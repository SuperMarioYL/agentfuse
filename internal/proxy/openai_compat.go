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

// openaiCompatHandler is the catch-all for any host that speaks the OpenAI
// Chat Completions wire format (Groq, Mistral, xAI, Together, OpenRouter, etc).
// It requires upstream_url in .fuse.toml, and falls through to tiktoken
// fallback whenever the upstream response omits a usage block. That's the
// headline difference versus the dedicated deepseek handler: a generic
// OpenAI-compat host is much more likely to drop usage on streamed responses.
func openaiCompatHandler(s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeFuseError(w, http.StatusBadGateway, "read request: "+err.Error(), "")
			return
		}
		_ = r.Body.Close()

		if s.cfg.UpstreamURL == "" {
			writeFuseError(w, http.StatusPreconditionFailed,
				"openai_compat provider requires upstream_url in .fuse.toml",
				"add  upstream_url = \"https://api.groq.com/openai/v1\"  (or your host) to .fuse.toml")
			return
		}

		// 1. Account guard.
		if s.cfg.Account != "" {
			inbound := stripBearer(r.Header.Get("Authorization"))
			if !s.accounts.FingerprintMatches(s.cfg.Account, inbound) {
				acc, err := s.accounts.Lookup(s.cfg.Account)
				if err != nil {
					writeFuseError(w, http.StatusPreconditionFailed,
						fmt.Sprintf("named account %q not found in accounts.toml", s.cfg.Account),
						"fuse init --account <name> or edit .fuse.toml's account field")
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
		estimate := budget.EstimateRequestWithProvider("openai_compat", model, promptTokens, maxOut)

		// Atomic reserve so concurrent requests cannot collectively overshoot
		// the cap. v0.4: openai_compat was previously on the racy ProjectTotal->Decide->Add
		// path — the v0.3.0 Reserve/CommitDelta fix only landed in anthropic+openai,
		// so 3/5 providers (deepseek/gemini/openai_compat) could still overshoot under
		// concurrency. Now matches the atomic discipline of the other handlers.
		allowed, err := s.led.ReserveWindowed(s.projectRoot, s.cfg.Window, s.cfg.CapUSD, estimate)
		if err != nil {
			writeFuseError(w, http.StatusInternalServerError, "ledger reserve: "+err.Error(), "")
			return
		}
		if !allowed {
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
		committed := false
		defer func() {
			if !committed {
				s.led.Release(s.projectRoot, estimate)
			}
		}()

		upstreamBase := strings.TrimRight(s.cfg.UpstreamURL, "/")
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

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			// Re-use the DeepSeek parser — same OpenAI-compat wire shape.
			inTok, outTok, completionText, parsedModel := parseDeepSeekUsage(respBody)
			if parsedModel != "" {
				model = parsedModel
			}
			// Fall back per-side so a partial final frame (only one of in/out
			// present) does not under-bill the missing half. The expected branch
			// for generic openai_compat is both missing; per-side also covers the
			// truncated-stream case.
			if inTok == 0 || outTok == 0 {
				if inTok == 0 {
					inTok = promptTokens
				}
				if outTok == 0 {
					outTok = tokens.EstimateCompletion(model, completionText)
				}
				fmt.Fprintf(os.Stderr, "agentfuse: openai_compat upstream usage incomplete; tiktoken fallback model=%s in=%d out=%d\n",
					model, inTok, outTok)
			}
			usd := budget.CostFromUsageWithProvider("openai_compat", model, inTok, outTok)
			if _, err := s.led.CommitDelta(s.projectRoot, estimate, inTok, outTok, usd); err != nil {
				fmt.Fprintf(os.Stderr, "agentfuse: ledger update failed: %v\n", err)
			}
			committed = true
		}

		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	})
}
