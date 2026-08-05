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

// TestAnthropicHonoursUpstreamURL is the v0.8.0 regression test for
// fix-upstream-url-ignored-anthropic-openai: when cfg.UpstreamURL is set the
// anthropic handler MUST route to it (a corporate gateway / Azure-OpenAI
// endpoint / self-hosted relay), not silently hit the package-level
// AnthropicUpstream default (api.anthropic.com). The fix mirrors the existing
// deepseek/gemini/openai_compat `if s.cfg.UpstreamURL != ""` override.
func TestAnthropicHonoursUpstreamURL(t *testing.T) {
	var realHit, decoyHit bool
	real := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		realHit = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "claude-sonnet-4",
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
		})
	}))
	defer real.Close()
	// Decoy stands in for the package-level default (api.anthropic.com). If the
	// handler ignores cfg.UpstreamURL it hits the decoy and the test fails.
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		decoyHit = true
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer decoy.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = decoy.URL
	defer func() { AnthropicUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project", UpstreamURL: real.URL}
	s := New("/proj/anth-up", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"claude-sonnet-4","max_tokens":256,"messages":[]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("x-api-key", "sk-ant-test-1234567890")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d — upstream_url-routed request did not succeed", resp.StatusCode)
	}
	if decoyHit {
		t.Fatal("anthropic handler hit the AnthropicUpstream default decoy — cfg.UpstreamURL was ignored")
	}
	if !realHit {
		t.Fatal("anthropic handler did not route to cfg.UpstreamURL")
	}
}

// TestOpenAIHonoursUpstreamURL is the v0.8.0 regression test for
// fix-upstream-url-ignored-anthropic-openai on the openai handler: when
// cfg.UpstreamURL is set the openai handler MUST route to it, not the
// package-level OpenAIUpstream default (api.openai.com).
func TestOpenAIHonoursUpstreamURL(t *testing.T) {
	var realHit, decoyHit bool
	real := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		realHit = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "gpt-4o",
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "ok"}},
			},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	defer real.Close()
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		decoyHit = true
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer decoy.Close()

	prev := OpenAIUpstream
	OpenAIUpstream = decoy.URL
	defer func() { OpenAIUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project", UpstreamURL: real.URL}
	s := New("/proj/oai-up", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"gpt-4o","max_tokens":256,"messages":[]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/openai/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-1234567890")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d — upstream_url-routed request did not succeed", resp.StatusCode)
	}
	if decoyHit {
		t.Fatal("openai handler hit the OpenAIUpstream default decoy — cfg.UpstreamURL was ignored")
	}
	if !realHit {
		t.Fatal("openai handler did not route to cfg.UpstreamURL")
	}
}
