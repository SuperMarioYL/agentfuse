package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
)

// TestDeepSeekUnaryParsesUsage covers the non-streaming path where DeepSeek
// returns a JSON body with the usage block.
func TestDeepSeekUnaryParsesUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "ds-1",
			"model": "deepseek-chat",
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "hi"}},
			},
			"usage": map[string]int{
				"prompt_tokens":     500,
				"completion_tokens": 300,
				"total_tokens":      800,
			},
		})
	}))
	defer upstream.Close()

	prev := DeepSeekUpstream
	DeepSeekUpstream = upstream.URL
	defer func() { DeepSeekUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project", Provider: "deepseek"}
	s := New("/proj/ds-a", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/deepseek/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-ds-test-1234567890")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	got, err := led.ProjectTotal("/proj/ds-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.TokensIn != 500 || got.TokensOut != 300 || got.Requests != 1 {
		t.Fatalf("ledger not updated correctly: %+v", got)
	}
	// 500 * 0.00027/1k + 300 * 0.0011/1k = 0.000135 + 0.00033 = 0.000465
	if got.USD < 0.00045 || got.USD > 0.00048 {
		t.Fatalf("unexpected usd: %v", got.USD)
	}
}

// TestDeepSeekStreamWithUsageBlock covers DeepSeek's distinguishing behavior:
// it emits a final SSE frame with usage even when the client did not opt-in.
func TestDeepSeekStreamWithUsageBlock(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Two content frames + a final usage frame, no [DONE] for simplicity.
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"hi "}}],"model":"deepseek-chat"}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"there"}}],"model":"deepseek-chat"}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{}}],"model":"deepseek-chat","usage":{"prompt_tokens":42,"completion_tokens":17}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	prev := DeepSeekUpstream
	DeepSeekUpstream = upstream.URL
	defer func() { DeepSeekUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project", Provider: "deepseek"}
	s := New("/proj/ds-b", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/deepseek/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	got, err := led.ProjectTotal("/proj/ds-b")
	if err != nil {
		t.Fatal(err)
	}
	if got.TokensIn != 42 || got.TokensOut != 17 {
		t.Fatalf("ledger should use upstream usage block: %+v", got)
	}
}

// TestDeepSeekStreamFallback covers the surprise case where DeepSeek omits
// usage (e.g. a mid-stream HTTP error or a Together-style mirror).
func TestDeepSeekStreamFallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"some output"}}],"model":"deepseek-chat"}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	prev := DeepSeekUpstream
	DeepSeekUpstream = upstream.URL
	defer func() { DeepSeekUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project", Provider: "deepseek"}
	s := New("/proj/ds-c", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/deepseek/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	got, err := led.ProjectTotal("/proj/ds-c")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 || got.TokensOut < 100 {
		t.Fatalf("fallback should still write a ledger row with rounded-up completion: %+v", got)
	}
}
