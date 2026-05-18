package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
)

// TestOpenAICompatRequiresUpstreamURL covers the misconfig guardrail: without
// upstream_url the handler must refuse with HTTP 412 rather than 502 the user
// into a confusing upstream error.
func TestOpenAICompatRequiresUpstreamURL(t *testing.T) {
	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project", Provider: "openai_compat"}
	s := New("/proj/oc-a", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"llama-3.1-70b","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/openai_compat/v1/chat/completions", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d", resp.StatusCode)
	}
}

// TestOpenAICompatFallbackOnMissingUsage covers the headline case: a
// streamed response from a Groq/OpenRouter/Mistral-style host that omits the
// usage block — the proxy MUST still write a non-zero ledger row using
// tiktoken-go.
func TestOpenAICompatFallbackOnMissingUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"Lorem ipsum dolor sit amet, "}}],"model":"llama-3.1-70b"}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"consectetur adipiscing elit."}}],"model":"llama-3.1-70b"}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	led := mustOpenLedger(t)
	cfg := &budget.Config{
		CapUSD:      5.00,
		Window:      "project",
		Provider:    "openai_compat",
		UpstreamURL: upstream.URL,
	}
	s := New("/proj/oc-b", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"llama-3.1-70b","stream":true,"messages":[{"role":"user","content":"please write a paragraph of lorem ipsum"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/openai_compat/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	got, err := led.ProjectTotal("/proj/oc-b")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 {
		t.Fatalf("expected 1 request, got %+v", got)
	}
	if got.TokensIn <= 0 || got.TokensOut < 100 {
		t.Fatalf("tiktoken fallback should yield in>0 and out>=100, got %+v", got)
	}
	if got.USD <= 0 {
		t.Fatalf("usd must be > 0, got %v", got.USD)
	}
}

// TestOpenAICompatUnaryHonoursUsage covers the case where the OpenAI-compat
// host DID return usage — the proxy should prefer it over the local estimate.
func TestOpenAICompatUnaryHonoursUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "llama-3.1-70b",
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "hi"}},
			},
			"usage": map[string]int{
				"prompt_tokens":     1234,
				"completion_tokens": 567,
			},
		})
	}))
	defer upstream.Close()

	led := mustOpenLedger(t)
	cfg := &budget.Config{
		CapUSD:      5.00,
		Window:      "project",
		Provider:    "openai_compat",
		UpstreamURL: upstream.URL,
	}
	s := New("/proj/oc-c", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"llama-3.1-70b","messages":[{"role":"user","content":"hello"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/openai_compat/v1/chat/completions",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	got, err := led.ProjectTotal("/proj/oc-c")
	if err != nil {
		t.Fatal(err)
	}
	if got.TokensIn != 1234 || got.TokensOut != 567 {
		t.Fatalf("expected upstream usage to win: %+v", got)
	}
}

// TestOpenAICompatStripsAuthorizationCorrectly ensures the Authorization
// header is forwarded (rather than dropped) when no account guard is engaged.
func TestOpenAICompatStripsAuthorizationCorrectly(t *testing.T) {
	var seenAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "llama-3.1-70b",
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "ok"}},
			},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
		})
	}))
	defer upstream.Close()

	led := mustOpenLedger(t)
	cfg := &budget.Config{
		CapUSD:      5.00,
		Window:      "project",
		Provider:    "openai_compat",
		UpstreamURL: upstream.URL,
	}
	s := New("/proj/oc-d", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"llama-3.1-70b","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/openai_compat/v1/chat/completions",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer gsk-test-1234567890")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !strings.HasPrefix(seenAuth, "Bearer gsk-test-") {
		t.Fatalf("expected upstream to see forwarded Authorization, got %q", seenAuth)
	}
}
