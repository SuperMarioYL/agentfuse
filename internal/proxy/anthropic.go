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

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/SuperMarioYL/agentfuse/internal/tokens"
)

// AnthropicUpstream is overridable for tests.
var AnthropicUpstream = "https://api.anthropic.com"

type anthropicRequest struct {
	Model     string `json:"model"`
	MaxTokens int    `json:"max_tokens"`
	Messages  []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

type anthropicResponse struct {
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Model string `json:"model"`
}

func anthropicHandler(s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeFuseError(w, http.StatusBadGateway, "read request: "+err.Error(), "")
			return
		}
		_ = r.Body.Close()

		// 1. Account guard.
		if s.cfg.Account != "" {
			inbound := r.Header.Get("x-api-key")
			if !s.accounts.FingerprintMatches(s.cfg.Account, inbound) {
				// Inject the configured account's key instead of refusing — the
				// guardrail rewrites rather than fails so a stray .env can't bill
				// the wrong account. If the named account doesn't exist, deny.
				acc, err := s.accounts.Lookup(s.cfg.Account)
				if err != nil {
					writeFuseError(w, http.StatusPreconditionFailed,
						fmt.Sprintf("named account %q not found in accounts.toml", s.cfg.Account),
						"fuse run --account <name> or edit ~/.fuse/accounts.toml")
					return
				}
				r.Header.Set("x-api-key", acc.APIKey)
			}
		}

		// 2. Parse request for model + token estimate.
		var req anthropicRequest
		_ = json.Unmarshal(body, &req)

		promptText := anthropicPromptText(body)
		promptTokens := estimatePromptTokens(body)
		estimate := budget.EstimateRequestWithProvider("anthropic", req.Model, promptTokens, req.MaxTokens)

		// 3. Enforce — atomically reserve the estimate so concurrent requests
		// cannot all pass the cap check and collectively overshoot. On any exit
		// path below we MUST release or commit the reservation exactly once.
		allowed, err := s.led.Reserve(s.projectRoot, s.cfg.CapUSD, estimate)
		if err != nil {
			writeFuseError(w, http.StatusInternalServerError, "ledger reserve: "+err.Error(), "")
			return
		}
		if !allowed {
			// Fail-closed — stderr line + HTTP 402 with JSON body.
			total, _ := s.led.ProjectTotal(s.projectRoot)
			decision := budget.Decide(total.USD, s.cfg.CapUSD, estimate, s.projectRoot)
			fmt.Fprintf(os.Stderr, "agentfuse: %s — raise with: %s\n",
				decision.Reason, decision.SuggestedCmd)
			writeFuseError(w, http.StatusPaymentRequired, decision.Reason, decision.SuggestedCmd)
			return
		}
		// committed flips true once the reservation is reconciled via CommitDelta;
		// any earlier return Releases it so it can't leak.
		committed := false
		defer func() {
			if !committed {
				s.led.Release(s.projectRoot, estimate)
			}
		}()

		// 4. Forward upstream.
		upstreamURL, err := url.Parse(AnthropicUpstream + r.URL.RequestURI())
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

		// 5. Parse usage and update ledger (only on success). Anthropic streaming
		// (the Claude Code default) returns an SSE body, not one JSON object, so a
		// single json.Unmarshal of the whole body fails and the request would bill
		// $0 — letting streamed spend bypass the cap. parseAnthropicUsage handles
		// BOTH the unary JSON response and the SSE event stream, and we fall back
		// to a local tiktoken estimate when no usage block is present at all.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			inTok, outTok, completionText, parsedModel := parseAnthropicUsage(respBody)
			model := parsedModel
			if model == "" {
				model = req.Model
			}
			if inTok == 0 && outTok == 0 {
				// No usage anywhere (truncated/odd stream) — re-tokenize locally so
				// the ledger still moves and the kill-switch still fires.
				inTok = tokens.EstimatePrompt(model, promptText)
				outTok = tokens.EstimateCompletion(model, completionText)
			} else {
				// Both upstream usage AND a local estimate are available — record
				// the comparison so the §8 >25% accuracy criterion is measurable.
				tokens.RecordSample("anthropic", model,
					tokens.EstimatePrompt(model, completionText), outTok)
			}
			usd := budget.CostFromUsageWithProvider("anthropic", model, inTok, outTok)
			if _, err := s.led.CommitDelta(s.projectRoot, estimate, inTok, outTok, usd); err != nil {
				fmt.Fprintf(os.Stderr, "agentfuse: ledger update failed: %v\n", err)
			}
			committed = true
		}

		// 6. Relay.
		copyHeader(w.Header(), resp.Header)
		w.Header().Set("x-fuse-account", account.Fingerprint(r.Header.Get("x-api-key")))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	})
}

// anthropicStreamEvent is the union of the SSE event shapes we care about.
// Anthropic streaming emits, in order: message_start (carries usage.input_tokens
// and the model), repeated content_block_delta (carries delta.text), and a final
// message_delta (carries usage.output_tokens). We accumulate the text and take
// the last-seen input/output token counts.
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Model string `json:"model"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Delta *struct {
		Text string `json:"text"` // content_block_delta
	} `json:"delta"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"` // message_delta
}

// parseAnthropicUsage returns (inputTokens, outputTokens, completionText, model).
// It handles BOTH a unary JSON response and an SSE event-stream body. For the
// stream path it walks every `data: { ... }` line, accumulates content_block_delta
// text, and takes the last input/output token counts it sees (message_start for
// input, message_delta for output).
func parseAnthropicUsage(body []byte) (int, int, string, string) {
	// Unary path: try as one JSON object first.
	var unary anthropicResponse
	if err := json.Unmarshal(body, &unary); err == nil &&
		(unary.Usage.InputTokens > 0 || unary.Usage.OutputTokens > 0 || unary.Model != "") {
		if unary.Usage.InputTokens > 0 || unary.Usage.OutputTokens > 0 {
			return unary.Usage.InputTokens, unary.Usage.OutputTokens, "", unary.Model
		}
	}

	// Stream path.
	var (
		inTok, outTok int
		text          strings.Builder
		model         string
	)
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		line = bytes.TrimPrefix(line, []byte("data:"))
		line = bytes.TrimSpace(line)
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) || line[0] != '{' {
			continue
		}
		var ev anthropicStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Message != nil {
			if ev.Message.Model != "" {
				model = ev.Message.Model
			}
			if ev.Message.Usage != nil {
				if ev.Message.Usage.InputTokens > 0 {
					inTok = ev.Message.Usage.InputTokens
				}
				if ev.Message.Usage.OutputTokens > 0 {
					outTok = ev.Message.Usage.OutputTokens
				}
			}
		}
		if ev.Delta != nil && ev.Delta.Text != "" {
			text.WriteString(ev.Delta.Text)
		}
		if ev.Usage != nil {
			if ev.Usage.InputTokens > 0 {
				inTok = ev.Usage.InputTokens
			}
			if ev.Usage.OutputTokens > 0 {
				outTok = ev.Usage.OutputTokens
			}
		}
	}
	return inTok, outTok, text.String(), model
}

// anthropicPromptText flattens an Anthropic Messages request body into one big
// string for tiktoken estimation when the upstream omits usage entirely.
func anthropicPromptText(body []byte) string {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return string(body)
	}
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Role)
		b.WriteByte(':')
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

// estimatePromptTokens is a coarse stand-in: ~4 chars per token.
// The exact value isn't critical because the post-response usage block is the
// source of truth — pre-flight only needs to be conservative enough to deny
// pathological single requests before they fly.
func estimatePromptTokens(body []byte) int {
	return len(body) / 4
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		// Drop hop-by-hop and host headers; let Go re-set transport.
		if k == "Connection" || k == "Keep-Alive" || k == "Proxy-Connection" ||
			k == "Te" || k == "Trailer" || k == "Transfer-Encoding" || k == "Upgrade" ||
			k == "Host" || k == "Content-Length" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// writeFuseError writes a JSON body the agent CLI surfaces verbatim.
func writeFuseError(w http.ResponseWriter, status int, msg, suggested string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]any{
		"error": map[string]string{
			"type":    "agentfuse_budget_denied",
			"message": msg,
		},
	}
	if suggested != "" {
		payload["suggested_command"] = suggested
	}
	_ = json.NewEncoder(w).Encode(payload)
}
