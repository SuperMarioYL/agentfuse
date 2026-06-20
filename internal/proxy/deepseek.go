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

// DeepSeekUpstream is overridable for tests.
var DeepSeekUpstream = "https://api.deepseek.com"

// deepseekStreamFrame is the per-chunk SSE shape. DeepSeek is OpenAI-compat
// in wire format: each `data: { ... }` line is one frame. Crucially DeepSeek
// emits the `usage` block on the FINAL frame even when the client did not pass
// `stream_options.include_usage = true` — that's the headline difference
// versus generic openai_compat handling.
type deepseekStreamFrame struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Model string `json:"model"`
}

func deepseekHandler(s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeFuseError(w, http.StatusBadGateway, "read request: "+err.Error(), "")
			return
		}
		_ = r.Body.Close()

		// 1. Account guard — DeepSeek uses Authorization: Bearer.
		if s.cfg.Account != "" {
			inbound := stripBearer(r.Header.Get("Authorization"))
			if !s.accounts.FingerprintMatches(s.cfg.Account, inbound) {
				acc, err := s.accounts.Lookup(s.cfg.Account)
				if err != nil {
					writeFuseError(w, http.StatusPreconditionFailed,
						fmt.Sprintf("named account %q not found in accounts.toml", s.cfg.Account),
						"fuse run --account <name> or edit ~/.fuse/accounts.toml")
					return
				}
				r.Header.Set("Authorization", "Bearer "+acc.APIKey)
			}
		}

		var req openaiRequest
		_ = json.Unmarshal(body, &req)
		model := req.Model

		promptText := openaiPromptText(body)
		promptTokens := tokens.EstimatePrompt(model, promptText)
		maxOut := req.MaxTokens
		if maxOut == 0 {
			maxOut = req.MaxOutputTokens
		}
		estimate := budget.EstimateRequestWithProvider("deepseek", model, promptTokens, maxOut)

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

		upstreamBase := DeepSeekUpstream
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

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			inTok, outTok, completionText, parsedModel := parseDeepSeekUsage(respBody)
			if parsedModel != "" {
				model = parsedModel
			}
			// Fall back per-side: a final frame that carries only prompt_tokens
			// (truncated stream, or a provider that emits prompt usage early but
			// drops completion usage) leaves one side at 0. Guarding on BOTH being
			// zero would bill that side as 0 tokens — an under-bill. Estimate each
			// missing side independently.
			if inTok == 0 {
				inTok = promptTokens
			}
			if outTok == 0 {
				outTok = tokens.EstimateCompletion(model, completionText)
			}
			usd := budget.CostFromUsageWithProvider("deepseek", model, inTok, outTok)
			if _, err := s.led.Add(s.projectRoot, inTok, outTok, usd); err != nil {
				fmt.Fprintf(os.Stderr, "agentfuse: ledger update failed: %v\n", err)
			}
		}

		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	})
}

// parseDeepSeekUsage returns (in, out, completionText, model). It handles both
// unary JSON and SSE-stream bodies. For the stream path it walks every
// `data: { ... }` line, accumulates delta.content, and takes the last usage
// block it sees.
func parseDeepSeekUsage(body []byte) (int, int, string, string) {
	// Unary path: try as one JSON object first.
	var unary openaiResponse
	if err := json.Unmarshal(body, &unary); err == nil &&
		(unary.Usage.PromptTokens > 0 || unary.Usage.CompletionTokens > 0 || unary.Model != "") {
		inTok := unary.Usage.PromptTokens
		if inTok == 0 {
			inTok = unary.Usage.InputTokens
		}
		outTok := unary.Usage.CompletionTokens
		if outTok == 0 {
			outTok = unary.Usage.OutputTokens
		}
		if inTok > 0 || outTok > 0 {
			return inTok, outTok, "", unary.Model
		}
	}

	// Stream path.
	var (
		lastUsageIn, lastUsageOut int
		text                      strings.Builder
		model                     string
	)
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		line = bytes.TrimPrefix(line, []byte("data:"))
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		var frame deepseekStreamFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			continue
		}
		if frame.Model != "" {
			model = frame.Model
		}
		for _, c := range frame.Choices {
			text.WriteString(c.Delta.Content)
		}
		if frame.Usage != nil {
			lastUsageIn = frame.Usage.PromptTokens
			lastUsageOut = frame.Usage.CompletionTokens
		}
	}
	return lastUsageIn, lastUsageOut, text.String(), model
}

// openaiPromptText flattens an OpenAI-shape request body into one big string
// for tiktoken estimation. Shared by deepseek + openai_compat.
func openaiPromptText(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return string(body)
	}
	var b strings.Builder
	if req.Prompt != "" {
		b.WriteString(req.Prompt)
		b.WriteByte('\n')
	}
	for _, m := range req.Messages {
		b.WriteString(m.Role)
		b.WriteByte(':')
		// content can be a string OR an array of {type,text}.
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			b.WriteString(s)
		} else {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(m.Content, &parts); err == nil {
				for _, p := range parts {
					b.WriteString(p.Text)
				}
			} else {
				b.Write(m.Content)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
