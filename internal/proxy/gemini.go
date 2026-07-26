package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/SuperMarioYL/agentfuse/internal/tokens"
)

// GeminiUpstream is overridable for tests and for users who point at an
// alternate region or proxy. Default is the official google generativelanguage
// endpoint.
var GeminiUpstream = "https://generativelanguage.googleapis.com"

// geminiRequest mirrors just enough of the :generateContent body to estimate
// prompt size and detect the model. The wire format differs from OpenAI: the
// model is in the URL path (.../models/gemini-1.5-pro:generateContent), and
// the prompt is a tree of `contents[].parts[].text`.
type geminiRequest struct {
	Contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"contents"`
	GenerationConfig struct {
		MaxOutputTokens int `json:"maxOutputTokens"`
	} `json:"generationConfig"`
}

// geminiResponse covers the usageMetadata block on both unary and final-stream
// frames. Gemini consistently emits usageMetadata even on stream responses,
// but we still hold a tiktoken fallback in case a particular endpoint omits it
// (e.g. some Vertex regional endpoints).
type geminiResponse struct {
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

func geminiHandler(s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeFuseError(w, http.StatusBadGateway, "read request: "+err.Error(), "")
			return
		}
		_ = r.Body.Close()

		// 1. Account guard — Gemini uses ?key=... query param or x-goog-api-key
		// header. We rewrite both when a managed account is configured.
		if s.cfg.Account != "" {
			inboundKey := r.URL.Query().Get("key")
			if inboundKey == "" {
				inboundKey = r.Header.Get("x-goog-api-key")
			}
			if !s.accounts.FingerprintMatches(s.cfg.Account, inboundKey) {
				acc, err := s.accounts.Lookup(s.cfg.Account)
				if err != nil {
					writeFuseError(w, http.StatusPreconditionFailed,
						fmt.Sprintf("named account %q not found in accounts.toml", s.cfg.Account),
						"fuse run --account <name> or edit ~/.fuse/accounts.toml")
					return
				}
				q := r.URL.Query()
				q.Set("key", acc.APIKey)
				r.URL.RawQuery = q.Encode()
				r.Header.Set("x-goog-api-key", acc.APIKey)
			}
		}

		// 2. Extract model from URL path + estimate prompt size.
		model := geminiModelFromPath(r.URL.Path)
		var req geminiRequest
		_ = json.Unmarshal(body, &req)

		promptText := geminiPromptText(&req)
		promptTokens := tokens.EstimatePrompt(model, promptText)
		maxOut := req.GenerationConfig.MaxOutputTokens
		estimate := budget.EstimateRequestWithProvider("gemini", model, promptTokens, maxOut)

		// Atomic reserve so concurrent requests cannot collectively overshoot
		// the cap. v0.4: gemini was previously on the racy ProjectTotal->Decide->Add
		// path — the v0.3.0 Reserve/CommitDelta fix only landed in anthropic+openai,
		// so 3/5 providers (deepseek/gemini/openai_compat) could still overshoot under
		// concurrency. Now matches the atomic discipline of the other handlers.
		allowed, err := s.led.ReserveWindowed(s.projectRoot, s.cfg.Window, s.cfg.CapUSD, estimate)
		if err != nil {
			writeFuseError(w, http.StatusInternalServerError, "ledger reserve: "+err.Error(), "")
			return
		}
		if !allowed {
			total, _ := s.led.WindowedTotal(s.projectRoot, s.cfg.Window)
			decision := budget.Decide(total.USD, s.cfg.CapUSD, estimate, s.projectRoot)
			fmt.Fprintf(os.Stderr, "agentfuse: %s — raise with: %s\n",
				decision.Reason, decision.SuggestedCmd)
			writeFuseError(w, http.StatusPaymentRequired, decision.Reason, decision.SuggestedCmd)
			return
		}
		committed := false
		defer func() {
			if !committed {
				s.led.Release(s.projectRoot, estimate)
			}
		}()

		// 4. Forward upstream.
		upstreamBase := GeminiUpstream
		if s.cfg.UpstreamURL != "" {
			upstreamBase = strings.TrimRight(s.cfg.UpstreamURL, "/")
		}
		upstreamURL, err := url.Parse(upstreamBase + r.URL.RequestURI())
		if err != nil {
			writeFuseError(w, http.StatusBadGateway, "parse upstream url: "+err.Error(), "")
			return
		}
		upReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), bytes.NewReader(body))
		if err != nil {
			writeFuseError(w, http.StatusBadGateway, "build upstream request: "+err.Error(), "")
			return
		}
		copyHeader(upReq.Header, r.Header)
		upReq.Host = upstreamURL.Host

		resp, err := s.upstream.Do(upReq)
		if err != nil {
			writeFuseError(w, http.StatusBadGateway, "upstream call: "+err.Error(), "")
			return
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			writeFuseError(w, http.StatusBadGateway, "read upstream body: "+err.Error(), "")
			return
		}

		// 5. Parse usage and update ledger (only on success).
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			inTok, outTok, completionText := parseGeminiUsage(respBody)
			// Fall back per-side so a partial usage block (only one count present)
			// does not under-bill the missing half.
			if inTok == 0 {
				inTok = promptTokens
			}
			if outTok == 0 {
				outTok = tokens.EstimateCompletion(model, completionText)
			}
			usd := budget.CostFromUsageWithProvider("gemini", model, inTok, outTok)
			if _, err := s.led.CommitDelta(s.projectRoot, estimate, inTok, outTok, usd); err != nil {
				fmt.Fprintf(os.Stderr, "agentfuse: ledger update failed: %v\n", err)
			}
			committed = true
		}

		// 6. Relay.
		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	})
}

// geminiModelFromPath extracts "gemini-1.5-pro" from
// "/v1beta/models/gemini-1.5-pro:generateContent".
func geminiModelFromPath(p string) string {
	// Look for ".../models/<name>:<method>"
	idx := strings.LastIndex(p, "/models/")
	if idx < 0 {
		return ""
	}
	tail := p[idx+len("/models/"):]
	colon := strings.IndexByte(tail, ':')
	if colon < 0 {
		return tail
	}
	return tail[:colon]
}

func geminiPromptText(req *geminiRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			b.WriteString(p.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// parseGeminiUsage returns (in, out, fallbackCompletionText). When the body is
// an SSE stream of JSON frames (one per line, possibly prefixed with "data:"),
// the last frame's usageMetadata wins; the concatenated candidate text is
// returned for tiktoken fallback when usage is absent.
func parseGeminiUsage(body []byte) (int, int, string) {
	// Fast path 1: a single JSON object (unary :generateContent).
	var single geminiResponse
	if err := json.Unmarshal(body, &single); err == nil && (single.UsageMetadata.PromptTokenCount > 0 || single.UsageMetadata.CandidatesTokenCount > 0 || len(single.Candidates) > 0) {
		return single.UsageMetadata.PromptTokenCount,
			single.UsageMetadata.CandidatesTokenCount,
			extractGeminiText(&single)
	}

	// Fast path 2: a JSON-array streamGenerateContent body — one compact line
	// `[{...},{...}]` or pretty-printed `[ { ... } ]`. A client that calls
	// streamGenerateContent WITHOUT alt=sse gets this shape: one JSON array of
	// frames, the last carrying usageMetadata. The SSE line-walker below skips
	// every line starting with `[` (its skip-rule), so without this path the
	// whole array is dropped -> in=0, out=0, completionText="" -> the per-side
	// fallback bills promptTokens + EstimateCompletion(model,"")=100 regardless
	// of real output, a fail-open cap-correctness defect (realized spend can
	// exceed the cap without tripping it). Same class as the v0.5.0 gzip fix.
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("[")) {
		var frames []geminiResponse
		if err := json.Unmarshal(trimmed, &frames); err == nil {
			var (
				lastUsage geminiResponse
				fullText  strings.Builder
			)
			for i := range frames {
				if frames[i].UsageMetadata.CandidatesTokenCount > 0 || frames[i].UsageMetadata.PromptTokenCount > 0 {
					lastUsage = frames[i]
				}
				fullText.WriteString(extractGeminiText(&frames[i]))
			}
			return lastUsage.UsageMetadata.PromptTokenCount,
				lastUsage.UsageMetadata.CandidatesTokenCount,
				fullText.String()
		}
	}

	// Stream path: each frame is a JSON object (SSE, one per line, possibly
	// prefixed with "data:"). Final frame carries usageMetadata; earlier frames
	// carry incremental candidate text. Reached only when the body is neither a
	// single JSON object nor a JSON array.
	var (
		lastUsage geminiResponse
		fullText  strings.Builder
	)
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:"))
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.HasPrefix(line, []byte("[")) || bytes.HasPrefix(line, []byte(",")) || bytes.HasPrefix(line, []byte("]")) {
			continue
		}
		var frame geminiResponse
		if err := json.Unmarshal(line, &frame); err != nil {
			continue
		}
		if frame.UsageMetadata.CandidatesTokenCount > 0 || frame.UsageMetadata.PromptTokenCount > 0 {
			lastUsage = frame
		}
		fullText.WriteString(extractGeminiText(&frame))
	}
	return lastUsage.UsageMetadata.PromptTokenCount,
		lastUsage.UsageMetadata.CandidatesTokenCount,
		fullText.String()
}

func extractGeminiText(r *geminiResponse) string {
	var b strings.Builder
	for _, c := range r.Candidates {
		for _, p := range c.Content.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
