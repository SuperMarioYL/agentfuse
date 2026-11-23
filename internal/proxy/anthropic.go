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
		// Anthropic reports prompt-cache traffic as counters SEPARATE from
		// input_tokens (which excludes cached tokens); they are billed at 1.25x
		// (write) / 0.1x (read) the input rate. v0.16:
		// fix-anthropic-cache-tokens-dropped.
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Model   string `json:"model"`
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"content"`
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
						"fuse init --account <name> or edit .fuse.toml's account field")
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
		// v0.16: fix-anthropic-counttokens-billed-and-reserved — /v1/messages/
		// count_tokens is a free endpoint (its response is a token count, with no
		// usage block), so it must be neither reserved against the cap nor billed;
		// v0.15 made the same call for Gemini's :countTokens.
		billable := !isAnthropicCountTokensPath(r.URL.Path)
		if billable {
			allowed, err := s.led.ReserveWindowed(s.projectRoot, s.cfg.Window, s.cfg.CapUSD, estimate)
			if err != nil {
				writeFuseError(w, http.StatusInternalServerError, "ledger reserve: "+err.Error(), "")
				return
			}
			if !allowed {
				// Fail-closed — stderr line + HTTP 402 with JSON body.
				// v0.7: fix-concurrent-deny-empty-reason — format the deny reason
				// from the projected total (persisted+reserved+estimate) the ledger
				// already denied on, not a persisted-only WindowedTotal via Decide
				// (which returns an empty Reason whenever reserved>0 drives the deny).
				_, _, projected, _ := s.led.ProjectedTotal(s.projectRoot, s.cfg.Window, estimate)
				decision := budget.DenyDecision(projected, s.cfg.CapUSD, estimate, s.projectRoot)
				fmt.Fprintf(os.Stderr, "agentfuse: %s — raise with: %s\n",
					decision.Reason, decision.SuggestedCmd)
				writeFuseError(w, http.StatusPaymentRequired, decision.Reason, decision.SuggestedCmd)
				return
			}
		}
		// committed flips true once the reservation is reconciled via CommitDelta;
		// any earlier return Releases it so it can't leak.
		committed := false
		defer func() {
			if billable && !committed {
				s.led.Release(s.projectRoot, estimate)
			}
		}()

		// 4. Forward upstream.
		// v0.8: fix-upstream-url-ignored-anthropic-openai — honor cfg.UpstreamURL
		// (a corporate gateway / Azure-OpenAI endpoint / self-hosted relay) the
		// same way deepseek/gemini/openai_compat already do, instead of always
		// hitting the package-level AnthropicUpstream default and silently
		// bypassing the configured gateway.
		upstreamBase := AnthropicUpstream
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

		// 5. Parse usage and update ledger (only on success, and only for
		// billable traffic — see the count_tokens note above). Anthropic streaming
		// (the Claude Code default) returns an SSE body, not one JSON object, so a
		// single json.Unmarshal of the whole body fails and the request would bill
		// $0 — letting streamed spend bypass the cap. parseAnthropicUsage handles
		// BOTH the unary JSON response and the SSE event stream, and we fall back
		// to a local tiktoken estimate when no usage block is present at all.
		if billable && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			usage, completionText, parsedModel := parseAnthropicUsage(respBody)
			model := parsedModel
			if model == "" {
				model = req.Model
			}
			// Record the accuracy sample only when BOTH sides are reported by the
			// upstream usage (the both-present branch) — the §8 >25% criterion
			// measures the local estimator against the real usage, which is
			// meaningless once a side has fallen back to a local estimate below
			// (comparing the estimator against itself).
			// v0.4: feed EstimateCompletion (the estimator the fallback bills with,
			// incl. the +100 round-up) on the completion text.
			// v0.5.0: guard on completionText != "" so an empty completion does not
			// record EstimateCompletion(model, "")=100 and false-trigger the kill
			// criterion.
			if usage.in > 0 && usage.out > 0 && completionText != "" {
				tokens.RecordSample("anthropic", model,
					tokens.EstimateCompletion(model, completionText), usage.out)
			}
			// v0.10: fix-anthropic-openai-partial-usage-bothzero-fallback — fall
			// back per-side, not on a both-zero guard. Anthropic streaming splits
			// usage across events: message_start carries usage.input_tokens and the
			// final message_delta carries usage.output_tokens. If a relay closes
			// the connection cleanly mid-stream (a clean TCP FIN yields partial
			// bytes with err=nil, and status was already 200 so the 2xx billing
			// branch runs), message_delta is absent and parseAnthropicUsage returns
			// inTok>0, outTok=0 with a non-empty completionText. The both-zero
			// guard skipped the fallback in that case and billed outTok=0 ($0
			// output) — fail-open: realized spend can exceed the cap without
			// tripping it. Estimate each missing side independently, mirroring
			// deepseek.go:143-148 / gemini.go:163-168 / openai_compat.go:132-141.
			if usage.in == 0 {
				usage.in = tokens.EstimatePrompt(model, promptText)
			}
			if usage.out == 0 {
				usage.out = tokens.EstimateCompletion(model, completionText)
			}
			// v0.16: fix-anthropic-cache-tokens-dropped — bill the prompt-cache
			// counters at their published multipliers and record the full input
			// token count (input + cache write + cache read), not just the
			// uncached remainder.
			usd := budget.CostFromAnthropicUsage(model, usage.in, usage.out, usage.cacheWrite, usage.cacheRead)
			ledgerIn := usage.in + usage.cacheWrite + usage.cacheRead
			if _, err := s.led.CommitDelta(s.projectRoot, estimate, ledgerIn, usage.out, usd); err != nil {
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
			// v0.16: fix-anthropic-cache-tokens-dropped — same two counters as
			// the unary Usage block; message_start carries them.
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Delta *struct {
		Text     string `json:"text"`     // content_block_delta (text_delta)
		Thinking string `json:"thinking"` // content_block_delta (thinking_delta)
	} `json:"delta"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		// v0.16: fix-anthropic-cache-tokens-dropped — some relays re-emit the
		// cache counters on message_delta rather than message_start.
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"` // message_delta
}

// anthropicUsage is the token usage a Messages response reports. The cache
// counters are separate from in (input_tokens excludes cached prompt tokens);
// they are carried through so the billing layer can price them at their own
// multipliers instead of dropping that spend.
type anthropicUsage struct {
	in, out               int
	cacheWrite, cacheRead int
}

// parseAnthropicUsage returns (usage, completionText, model). It handles BOTH a
// unary JSON response and an SSE event-stream body. For the stream path it walks
// every `data: { ... }` line, accumulates content_block_delta text, and takes the
// last non-zero token count it sees for each side (message_start for the input
// and cache counters, message_delta for output).
func parseAnthropicUsage(body []byte) (anthropicUsage, string, string) {
	// Unary path: try as one JSON object first.
	var unary anthropicResponse
	if err := json.Unmarshal(body, &unary); err == nil &&
		(unary.Usage.InputTokens > 0 || unary.Usage.OutputTokens > 0 ||
			unary.Usage.CacheCreationInputTokens > 0 || unary.Usage.CacheReadInputTokens > 0 ||
			unary.Model != "") {
		u := anthropicUsage{
			in:         unary.Usage.InputTokens,
			out:        unary.Usage.OutputTokens,
			cacheWrite: unary.Usage.CacheCreationInputTokens,
			cacheRead:  unary.Usage.CacheReadInputTokens,
		}
		if u.in > 0 || u.out > 0 || u.cacheWrite > 0 || u.cacheRead > 0 {
			// v0.5.0: fix-accuracy-harness-empty-completion-unary — the unary
			// branch previously returned "" for completionText, so RecordSample
			// measured EstimateCompletion(model, "")=100 on every unary response
			// regardless of real completion length, false-triggering the §8 >25%
			// kill criterion. Extract the content-block text so the harness
			// measures a real estimate on unary traffic too.
			return u, anthropicContentText(unary.Content), unary.Model
		}
	}

	// Stream path.
	var (
		u     anthropicUsage
		text  strings.Builder
		model string
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
					u.in = ev.Message.Usage.InputTokens
				}
				if ev.Message.Usage.OutputTokens > 0 {
					u.out = ev.Message.Usage.OutputTokens
				}
				if ev.Message.Usage.CacheCreationInputTokens > 0 {
					u.cacheWrite = ev.Message.Usage.CacheCreationInputTokens
				}
				if ev.Message.Usage.CacheReadInputTokens > 0 {
					u.cacheRead = ev.Message.Usage.CacheReadInputTokens
				}
			}
		}
		if ev.Delta != nil {
			if ev.Delta.Text != "" {
				text.WriteString(ev.Delta.Text)
			}
			// Anthropic extended-thinking streams emit the reasoning trace
			// under delta.thinking (delta.type="thinking_delta"), not
			// delta.text. Accumulate it so EstimateCompletion sees reasoning
			// + visible text and the per-side streaming fallback errs
			// conservative on thinking models — otherwise a truncated
			// thinking stream (message_delta never arrives) bills only the
			// visible text and realized spend can exceed the cap (fail-open).
			if ev.Delta.Thinking != "" {
				text.WriteString(ev.Delta.Thinking)
			}
		}
		if ev.Usage != nil {
			if ev.Usage.InputTokens > 0 {
				u.in = ev.Usage.InputTokens
			}
			if ev.Usage.OutputTokens > 0 {
				u.out = ev.Usage.OutputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				u.cacheWrite = ev.Usage.CacheCreationInputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				u.cacheRead = ev.Usage.CacheReadInputTokens
			}
		}
	}
	return u, text.String(), model
}

// isAnthropicCountTokensPath reports whether the (prefix-stripped) request path
// targets Anthropic's token-counting endpoint (POST /v1/messages/count_tokens).
// Its response is a token count with no usage block and Anthropic does not bill
// it, so it must be neither reserved against the cap nor billed — the same rule
// v0.15 applied to Gemini's :countTokens.
func isAnthropicCountTokensPath(p string) bool {
	return strings.HasSuffix(p, "/messages/count_tokens")
}

// anthropicContentText flattens the content blocks of a unary Anthropic
// response into one string, so the accuracy harness can EstimateCompletion on
// the real completion text instead of "" (which rounds up to 100 and
// false-triggers the §8 >25% kill criterion on unary traffic). Text blocks
// contribute their text; thinking blocks contribute their reasoning trace
// (billed as output_tokens by Anthropic); other blocks (tool_use, etc.)
// contribute nothing — they are not billed as completion tokens.
func anthropicContentText(blocks []struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
}) string {
	var b strings.Builder
	for _, blk := range blocks {
		switch blk.Type {
		case "text", "":
			b.WriteString(blk.Text)
		case "thinking":
			b.WriteString(blk.Thinking)
		}
	}
	return b.String()
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
		// Accept-Encoding is also dropped: Go's http transport only
		// auto-decompresses a gzipped response when IT added the
		// Accept-Encoding header. A caller-set value (Go/Node/fetch add
		// "gzip" by default) is forwarded verbatim, the upstream then
		// gzips the unary JSON body, and the transport passes the raw
		// \x1f\x8b bytes through untouched — so io.ReadAll(resp.Body)
		// yields gzip bytes, parseAnthropicUsage/parseDeepSeekUsage
		// json.Unmarshal them into an error (in=0, out=0,
		// completionText=""), and the per-side fallback bills
		// promptTokens + EstimateCompletion(model, "")=100 instead of
		// the real usage. For a unary response with thousands of real
		// output tokens that under-bills by orders of magnitude, so
		// realized spend can exceed the cap without tripping it
		// (fail-open) — the exact property this product exists to
		// prevent. Stripping it here lets the proxy's own transport
		// manage gzip (adds Accept-Encoding, decompresses, strips
		// Content-Encoding) for all five handlers that share this
		// copier. v0.5.0: fix-gzip-accept-encoding-forwarded.
		if k == "Connection" || k == "Keep-Alive" || k == "Proxy-Connection" ||
			k == "Te" || k == "Trailer" || k == "Transfer-Encoding" || k == "Upgrade" ||
			k == "Host" || k == "Content-Length" || k == "Accept-Encoding" {
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
