package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/SuperMarioYL/agentfuse/internal/ledger"
)

func mustOpenLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// TestAnthropicForwardsAndUpdatesLedger covers the m1 happy path: a request is
// forwarded, the response usage block is parsed, and the ledger increments.
func TestAnthropicForwardsAndUpdatesLedger(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"model":"claude-sonnet-4"`)) {
			t.Errorf("upstream did not see model field, body=%s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_01",
			"model": "claude-sonnet-4",
			"usage": map[string]int{
				"input_tokens":  1000,
				"output_tokens": 500,
			},
			"content": []map[string]string{{"type": "text", "text": "ok"}},
		})
	}))
	defer upstream.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/a", cfg, led, &account.File{Accounts: map[string]account.Account{}})
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
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-test-1234567890")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	got, err := led.ProjectTotal("/proj/a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 || got.TokensIn != 1000 || got.TokensOut != 500 {
		t.Fatalf("ledger not updated: %+v", got)
	}
	// usd = 1000 * 3/1M + 500 * 15/1M = 0.003 + 0.0075 = 0.0105
	if got.USD < 0.0104 || got.USD > 0.0106 {
		t.Fatalf("unexpected usd: %v", got.USD)
	}
}

// TestAnthropicFailsClosedWhenCapHit covers m2: pre-flight estimate trips the
// kill-switch and the request never reaches upstream.
func TestAnthropicFailsClosedWhenCapHit(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	led := mustOpenLedger(t)
	// Pre-charge the ledger past the cap.
	if _, err := led.Add("/proj/b", 0, 0, 5.50); err != nil {
		t.Fatal(err)
	}
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/b", cfg, led, &account.File{Accounts: map[string]account.Account{}})
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expected 402, got %d", resp.StatusCode)
	}
	if called {
		t.Fatal("upstream must NOT be called when over cap")
	}
	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "agentfuse_budget_denied") {
		t.Fatalf("response body missing fuse error type: %s", respBody)
	}
}

// TestAnthropicAccountGuardRewritesKey covers m3: when a stray .env-style
// inbound key doesn't match the configured account, the proxy injects the
// correct key instead of forwarding the wrong one.
func TestAnthropicAccountGuardRewritesKey(t *testing.T) {
	var seenKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "claude-sonnet-4",
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
		})
	}))
	defer upstream.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	led := mustOpenLedger(t)
	accts := &account.File{Accounts: map[string]account.Account{
		"personal": {Provider: "anthropic", APIKey: "sk-ant-PERSONAL-aaaaaaaaaaaa"},
		"work":     {Provider: "anthropic", APIKey: "sk-ant-WORK-bbbbbbbbbbbb"},
	}}
	cfg := &budget.Config{CapUSD: 5.00, Window: "project", Account: "personal"}
	s := New("/proj/c", cfg, led, accts)
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
	// Stray .env-sourced WORK key — must NOT reach upstream.
	req.Header.Set("x-api-key", "sk-ant-WORK-bbbbbbbbbbbb")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if seenKey != "sk-ant-PERSONAL-aaaaaaaaaaaa" {
		t.Fatalf("expected proxy to rewrite to personal key, upstream saw %q", seenKey)
	}
}
