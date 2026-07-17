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

		promptText := openaiPromptText(body)
		promptTokens := estimatePromptTokens(body)
		maxOut := req.MaxTokens
		if maxOut == 0 {
			maxOut = req.MaxOutputTokens
		}
		estimate := budget.EstimateRequestWithProvider("openai", req.Model, promptTokens, maxOut)

		// Atomic reserve so concurrent requests cannot collectively overshoot the cap.
		allowed, err := s.led.Reserve(s.projectRoot, s.cfg.CapUSD, estimate)
		if err != nil {
			writeFuseError(w, http.StatusInternalServerError, "ledger reserve: "+err.Error(), "")
			return
		}
		if !allowed {
			total, _ := s.led.ProjectTotal(s.projectRoot)
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

		// Chat Completions streaming returns SSE `data: {...}` frames terminated by
		// `data: [DONE]`, not one JSON object — a single json.Unmarshal of the whole
		// body fails and the request would bill $0, bypassing the cap. Reuse the
		// DeepSeek parser (identical OpenAI-compat wire shape) for BOTH the unary
		// and the streamed shapes, and fall back to a local tiktoken estimate when
		// no usage is present.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			inTok, outTok, completionText, parsedModel := parseDeepSeekUsage(respBody)
			model := parsedModel
			if model == "" {
				model = req.Model
			}
			if inTok == 0 && outTok == 0 {
				inTok = tokens.EstimatePrompt(model, promptText)
				outTok = tokens.EstimateCompletion(model, completionText)
			} else {
				// v0.4: measure EstimateCompletion (the estimator the fallback bills
				// with, incl. +100 round-up) on the completion text — previously
				// EstimatePrompt(completionText) compared against outTok, which is
				// the wrong side + structurally biased low (no round-up).
				tokens.RecordSample("openai", model,
					tokens.EstimateCompletion(model, completionText), outTok)
			}
			usd := budget.CostFromUsageWithProvider("openai", model, inTok, outTok)
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

func stripBearer(v string) string {
	const p = "Bearer "
	if strings.HasPrefix(v, p) {
		return strings.TrimSpace(v[len(p):])
	}
	return strings.TrimSpace(v)
}
