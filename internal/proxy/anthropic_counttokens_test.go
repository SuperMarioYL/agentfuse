package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
)

// v0.16: fix-anthropic-counttokens-billed-and-reserved.
//
// POST /v1/messages/count_tokens returns a token count ({"input_tokens": N}) with
// no usage/candidates block, and Anthropic does not bill it. The handler treated
// it like any other request, so the per-side fallback billed a fabricated
// EstimatePrompt + EstimateCompletion("")=100 output tokens, counted the request,
// and the pre-flight reserve gated the free call — a project at its cap got a 402
// for a call that costs nothing. This is the Anthropic sibling of the Gemini
// :countTokens defect v0.15 fixed.
func TestAnthropicCountTokensNotBilledNorReserved(t *testing.T) {
	t.Run("not billed", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":42}`))
		}))
		defer upstream.Close()

		prev := AnthropicUpstream
		AnthropicUpstream = upstream.URL
		defer func() { AnthropicUpstream = prev }()

		led := mustOpenLedger(t)
		cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
		s := New("/proj/ct-free", cfg, led, &account.File{Accounts: map[string]account.Account{}})
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = s.Stop(ctx)
		}()

		body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}]}`)
		req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages/count_tokens", bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200", resp.StatusCode)
		}

		got, err := led.ProjectTotal("/proj/ct-free")
		if err != nil {
			t.Fatal(err)
		}
		if got.Requests != 0 || got.TokensIn != 0 || got.TokensOut != 0 || got.USD != 0 {
			t.Fatalf("count_tokens wrote to the ledger: %+v — a free endpoint must not be billed", got)
		}
	})

	t.Run("allowed at cap", func(t *testing.T) {
		called := false
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":42}`))
		}))
		defer upstream.Close()

		prev := AnthropicUpstream
		AnthropicUpstream = upstream.URL
		defer func() { AnthropicUpstream = prev }()

		led := mustOpenLedger(t)
		// Exactly at the cap: any positive estimate must deny, so a 200 here
		// proves count_tokens no longer reserves.
		if _, err := led.Add("/proj/ct-cap", 0, 0, 5.00); err != nil {
			t.Fatal(err)
		}
		cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
		s := New("/proj/ct-cap", cfg, led, &account.File{Accounts: map[string]account.Account{}})
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = s.Stop(ctx)
		}()

		body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}]}`)
		req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages/count_tokens", bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d, want 200 — the free endpoint was denied at the cap", resp.StatusCode)
		}
		if !called {
			t.Fatal("count_tokens was not forwarded upstream")
		}
	})

	t.Run("messages still billed", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"claude-sonnet-4","usage":{"input_tokens":100,"output_tokens":50},"content":[{"type":"text","text":"ok"}]}`))
		}))
		defer upstream.Close()

		prev := AnthropicUpstream
		AnthropicUpstream = upstream.URL
		defer func() { AnthropicUpstream = prev }()

		led := mustOpenLedger(t)
		cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
		s := New("/proj/ct-msg", cfg, led, &account.File{Accounts: map[string]account.Account{}})
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = s.Stop(ctx)
		}()

		body := []byte(`{"model":"claude-sonnet-4","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`)
		req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages", bytes.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", resp.StatusCode)
		}

		got, err := led.ProjectTotal("/proj/ct-msg")
		if err != nil {
			t.Fatal(err)
		}
		if got.Requests != 1 || got.TokensIn != 100 || got.TokensOut != 50 || got.USD <= 0 {
			t.Fatalf("/v1/messages must still bill its real usage, got %+v", got)
		}
	})
}
