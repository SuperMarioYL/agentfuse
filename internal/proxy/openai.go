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
)

// OpenAIUpstream is overridable for tests.
var OpenAIUpstream = "https://api.openai.com"

type openaiRequest struct {
	Model           string `json:"model"`
	MaxTokens       int    `json:"max_tokens"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

type openaiResponse struct {
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
	} `json:"usage"`
	Model string `json:"model"`
}

func openaiHandler(s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeFuseError(w, http.StatusBadGateway, "read request: "+err.Error(), "")
			return
		}
		_ = r.Body.Close()

		// Account guard — rewrite Authorization header if a managed account is configured.
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

		promptTokens := estimatePromptTokens(body)
		maxOut := req.MaxTokens
		if maxOut == 0 {
			maxOut = req.MaxOutputTokens
		}
		estimate := budget.EstimateRequest(req.Model, promptTokens, maxOut)

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

		upstreamURL, err := url.Parse(OpenAIUpstream + r.URL.RequestURI())
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
			var parsed openaiResponse
			if err := json.Unmarshal(respBody, &parsed); err == nil {
				model := parsed.Model
				if model == "" {
					model = req.Model
				}
				inTok := parsed.Usage.PromptTokens
				if inTok == 0 {
					inTok = parsed.Usage.InputTokens
				}
				outTok := parsed.Usage.CompletionTokens
				if outTok == 0 {
					outTok = parsed.Usage.OutputTokens
				}
				usd := budget.CostFromUsage(model, inTok, outTok)
				if _, err := s.led.Add(s.projectRoot, inTok, outTok, usd); err != nil {
					fmt.Fprintf(os.Stderr, "agentfuse: ledger update failed: %v\n", err)
				}
			}
		}

		copyHeader(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(respBody)
	})
}

func stripBearer(v string) string {
	const p = "Bearer "
	if strings.HasPrefix(v, p) {
		return strings.TrimSpace(v[len(p):])
	}
	return strings.TrimSpace(v)
}
