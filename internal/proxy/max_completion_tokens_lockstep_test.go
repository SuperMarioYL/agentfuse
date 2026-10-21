package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
)

// The v0.15 regression for fix-max-completion-tokens-lockstep.
//
// v0.14 added openaiMaxOutputTokens (max_tokens -> max_output_tokens ->
// max_completion_tokens) and wired it into the openai handler ONLY. The two
// other Chat-Completions handlers (deepseek, openai_compat) kept the old
// two-field inline chain and dropped the third field, so a request whose only
// declared ceiling is max_completion_tokens (the field the Chat-Completions
// clients of today send) under-reserved: the ceiling fell back to
// "completion ≈ prompt", shrinking the reservation that stops concurrent
// requests from collectively overshooting the cap.
//
// Each subtest fires a request with a huge declared output ceiling against a
// small cap. With the field honored the pre-flight estimate exceeds the cap and
// the request is denied (402) BEFORE any upstream call; with the field dropped
// the estimate is a few micro-dollars and the request is forwarded (200). The
// max_tokens positive control proves the reserve path itself is wired — only
// the field-name chain differs.
func TestMaxCompletionTokensHonoredInReserveLockstep(t *testing.T) {
	// upstream counts calls so a denied request can assert it never flew.
	newUpstream := func(calls *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			*calls++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":   "deepseek-chat",
				"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "ok"}}},
				"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
			})
		}))
	}

	cases := []struct {
		name     string
		provider string
		route    string
		upstream string // package-level upstream var to swap
		body     string
	}{
		{
			name:     "deepseek max_completion_tokens",
			provider: "deepseek",
			route:    "/deepseek/v1/chat/completions",
			upstream: "deepseek",
			body:     `{"model":"deepseek-chat","max_completion_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:     "openai_compat max_completion_tokens",
			provider: "openai_compat",
			route:    "/openai_compat/v1/chat/completions",
			upstream: "openai_compat",
			body:     `{"model":"grok-2-mini","max_completion_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := 0
			upstream := newUpstream(&calls)
			defer upstream.Close()

			// The deepseek handler reads the package-level upstream var; the
			// openai_compat handler reads cfg.UpstreamURL (set below).
			if c.upstream == "deepseek" {
				prevDS := DeepSeekUpstream
				DeepSeekUpstream = upstream.URL
				defer func() { DeepSeekUpstream = prevDS }()
			}

			// Cap is far below the declared ceiling's cost (100k output tokens at
			// deepseek-chat 0.0011/1k = $0.11; grok-2-mini 0.001/1k = $0.10) but far
			// above the fallback estimate (completion ≈ prompt ≈ 2 tokens).
			led := mustOpenLedger(t)
			cfg := &budget.Config{CapUSD: 0.05, Window: "project", Provider: c.provider, UpstreamURL: upstream.URL}
			s := New("/proj/mct-"+c.provider, cfg, led, &account.File{Accounts: map[string]account.Account{}})
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = s.Stop(ctx)
			}()

			req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+c.route, bytes.NewReader([]byte(c.body)))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			// The declared 100k-token ceiling must be honored by the pre-flight
			// reserve: the projected cost exceeds the $0.05 cap, so the request is
			// denied fail-closed before it flies. Dropping max_completion_tokens
			// makes the estimate ~$0.000003 and the request is forwarded (200).
			if resp.StatusCode != http.StatusPaymentRequired {
				t.Fatalf("status=%d, want 402 — the declared max_completion_tokens ceiling was not honored by the pre-flight reserve (upstream calls=%d)", resp.StatusCode, calls)
			}
			if calls != 0 {
				t.Fatalf("denied request still reached upstream (calls=%d)", calls)
			}
			tot, err := led.ProjectTotal("/proj/mct-" + c.provider)
			if err != nil {
				t.Fatal(err)
			}
			if tot.Requests != 0 {
				t.Fatalf("denied request was billed: %+v", tot)
			}
		})
	}

	// Positive control: the SAME body with max_tokens (already honored by both
	// handlers) must also be denied — proving the reserve path and the cap math
	// are wired, and isolating the defect to the field-name chain.
	t.Run("control max_tokens is denied", func(t *testing.T) {
		calls := 0
		upstream := newUpstream(&calls)
		defer upstream.Close()
		prevDS := DeepSeekUpstream
		DeepSeekUpstream = upstream.URL
		defer func() { DeepSeekUpstream = prevDS }()

		led := mustOpenLedger(t)
		cfg := &budget.Config{CapUSD: 0.05, Window: "project", Provider: "deepseek"}
		s := New("/proj/mct-control", cfg, led, &account.File{Accounts: map[string]account.Account{}})
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = s.Stop(ctx)
		}()

		body := []byte(`{"model":"deepseek-chat","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`)
		req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/deepseek/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusPaymentRequired {
			t.Fatalf("control: status=%d, want 402 — max_tokens must already be honored", resp.StatusCode)
		}
	})
}
