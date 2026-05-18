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
		estimate := budget.EstimateRequestWithProvider("openai_compat", model, promptTokens, maxOut)

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
			// Re-use the DeepSeek parser — same OpenAI-compat wire shape.
			inTok, outTok, completionText, parsedModel := parseDeepSeekUsage(respBody)
			if parsedModel != "" {
				model = parsedModel
			}
			if inTok == 0 && outTok == 0 {
				// The expected branch for generic openai_compat: upstream
				// omitted usage. Re-tokenize locally.
				inTok = promptTokens
				outTok = tokens.EstimateCompletion(model, completionText)
				fmt.Fprintf(os.Stderr, "agentfuse: openai_compat upstream omitted usage; tiktoken fallback model=%s in=%d out=%d\n",
					model, inTok, outTok)
			}
			usd := budget.CostFromUsageWithProvider("openai_compat", model, inTok, outTok)
			if _, err := s.led.Add(s.projectRoot, inTok, outTok, usd); err != nil {
				fmt.Fprintf(os.Stderr, "agentfuse: ledger update failed: %v\n", err)
			}
		}

		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	})
}
