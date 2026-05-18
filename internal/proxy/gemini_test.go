package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
)

// TestGeminiUnaryParsesUsageMetadata covers the happy path for
// :generateContent — a single JSON response with usageMetadata.
func TestGeminiUnaryParsesUsageMetadata(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/models/gemini-1.5-pro:generateContent") {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{
				{"content": map[string]any{
					"parts": []map[string]any{{"text": "hello"}},
				}},
			},
			"usageMetadata": map[string]int{
				"promptTokenCount":     800,
				"candidatesTokenCount": 200,
				"totalTokenCount":      1000,
			},
		})
	}))
	defer upstream.Close()

	prev := GeminiUpstream
	GeminiUpstream = upstream.URL
	defer func() { GeminiUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project", Provider: "gemini"}
	s := New("/proj/gemini-a", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi gemini"}]}]}`)
	url := "http://" + s.Addr() + "/gemini/v1beta/models/gemini-1.5-pro:generateContent"
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}

	got, err := led.ProjectTotal("/proj/gemini-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 || got.TokensIn != 800 || got.TokensOut != 200 {
		t.Fatalf("ledger not updated correctly: %+v", got)
	}
	// 800 * 0.00125/1k + 200 * 0.005/1k = 0.001 + 0.001 = 0.002
	if got.USD < 0.0019 || got.USD > 0.0021 {
		t.Fatalf("unexpected usd: %v", got.USD)
	}
}

// TestGeminiStreamFallsBackOnMissingUsage covers the streamGenerateContent
// path when usageMetadata is absent — the tiktoken estimator must take over.
func TestGeminiStreamFallsBackOnMissingUsage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Two frames, no usageMetadata anywhere.
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello "}]}}]}` + "\n"))
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"there"}]}}]}` + "\n"))
	}))
	defer upstream.Close()

	prev := GeminiUpstream
	GeminiUpstream = upstream.URL
	defer func() { GeminiUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project", Provider: "gemini"}
	s := New("/proj/gemini-b", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi gemini"}]}]}`)
	url := "http://" + s.Addr() + "/gemini/v1beta/models/gemini-1.5-flash:streamGenerateContent"
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	got, err := led.ProjectTotal("/proj/gemini-b")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 {
		t.Fatalf("expected 1 request, got %+v", got)
	}
	if got.TokensIn <= 0 {
		t.Fatalf("tiktoken fallback should produce >0 input tokens, got %+v", got)
	}
	if got.TokensOut < 100 {
		t.Fatalf("completion must round up to >=100 even for tiny output, got %d", got.TokensOut)
	}
	if got.USD <= 0 {
		t.Fatalf("usd must be > 0, got %v", got.USD)
	}
}

func TestGeminiModelFromPath(t *testing.T) {
	cases := map[string]string{
		"/v1beta/models/gemini-1.5-pro:generateContent":         "gemini-1.5-pro",
		"/v1/models/gemini-1.5-flash:streamGenerateContent":     "gemini-1.5-flash",
		"/v1beta/models/gemini-2.0-flash-exp:generateContent":   "gemini-2.0-flash-exp",
	}
	for path, want := range cases {
		if got := geminiModelFromPath(path); got != want {
			t.Errorf("path %s: got %q want %q", path, got, want)
		}
	}
}
