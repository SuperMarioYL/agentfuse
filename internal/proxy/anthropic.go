package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
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

		promptTokens := estimatePromptTokens(body)
		estimate := budget.EstimateRequest(req.Model, promptTokens, req.MaxTokens)

		// 3. Enforce.
		currentEntry, err := s.led.ProjectTotal(s.projectRoot)
		if err != nil {
			writeFuseError(w, http.StatusInternalServerError, "ledger read: "+err.Error(), "")
			return
		}
		decision := budget.Decide(currentEntry.USD, s.cfg.CapUSD, estimate, s.projectRoot)
		if !decision.Allow {
			// Fail-closed — stderr line + HTTP 402 with JSON body.
			fmt.Fprintf(os.Stderr, "agentfuse: %s — raise with: %s\n",
				decision.Reason, decision.SuggestedCmd)
			writeFuseError(w, http.StatusPaymentRequired, decision.Reason, decision.SuggestedCmd)
			return
		}

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

		// 5. Parse usage and update ledger (only on success).
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var parsed anthropicResponse
			if err := json.Unmarshal(respBody, &parsed); err == nil {
				model := parsed.Model
				if model == "" {
					model = req.Model
				}
				usd := budget.CostFromUsage(model, parsed.Usage.InputTokens, parsed.Usage.OutputTokens)
				if _, err := s.led.Add(s.projectRoot, parsed.Usage.InputTokens, parsed.Usage.OutputTokens, usd); err != nil {
					fmt.Fprintf(os.Stderr, "agentfuse: ledger update failed: %v\n", err)
				}
			}
		}

		// 6. Relay.
		copyHeader(w.Header(), resp.Header)
		w.Header().Set("x-fuse-account", account.Fingerprint(r.Header.Get("x-api-key")))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	})
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
