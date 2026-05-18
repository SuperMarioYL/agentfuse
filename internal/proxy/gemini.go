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

		// 3. Enforce.
		currentEntry, err := s.led.ProjectTotal(s.projectRoot)
		if err != nil {
			writeFuseError(w, http.StatusInternalServerError, "ledger read: "+err.Error(), "")
			return
		}
		decision := budget.Decide(currentEntry.USD, s.cfg.CapUSD, estimate, s.projectRoot)
		if !decision.Allow {
			fmt.Fprintf(os.Stderr, "agentfuse: %s — raise with: %s\n",
				decision.Reason, decision.SuggestedCmd)
			writeFuseError(w, http.StatusPaymentRequired, decision.Reason, decision.SuggestedCmd)
			return
		}

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

		resp, err := http.DefaultClient.Do(upReq)
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
			if inTok == 0 && outTok == 0 {
				// Fallback: re-tokenize prompt + completion locally.
				inTok = promptTokens
				outTok = tokens.EstimateCompletion(model, completionText)
			}
			usd := budget.CostFromUsageWithProvider("gemini", model, inTok, outTok)
			if _, err := s.led.Add(s.projectRoot, inTok, outTok, usd); err != nil {
				fmt.Fprintf(os.Stderr, "agentfuse: ledger update failed: %v\n", err)
			}
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
	// Fast path: a single JSON object (unary :generateContent).
	var single geminiResponse
	if err := json.Unmarshal(body, &single); err == nil && (single.UsageMetadata.PromptTokenCount > 0 || single.UsageMetadata.CandidatesTokenCount > 0 || len(single.Candidates) > 0) {
		return single.UsageMetadata.PromptTokenCount,
			single.UsageMetadata.CandidatesTokenCount,
			extractGeminiText(&single)
	}

	// Stream path: each frame is a JSON object. Final frame carries
	// usageMetadata; earlier frames carry incremental candidate text.
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
