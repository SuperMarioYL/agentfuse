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

// openaiSSEBody is a Chat Completions streaming response: delta frames carrying
// content, then a final frame with usage (stream_options.include_usage), then
// the [DONE] sentinel. A single json.Unmarshal of this whole body fails.
const openaiSSEBody = `data: {"id":"c1","model":"gpt-4o","choices":[{"delta":{"content":"Hello "}}]}

data: {"id":"c1","model":"gpt-4o","choices":[{"delta":{"content":"there"}}]}

data: {"id":"c1","model":"gpt-4o","choices":[{"delta":{}}],"usage":{"prompt_tokens":900,"completion_tokens":400}}

data: [DONE]

`

// TestOpenAIStreamingBillsLedger is the v0.3.0 regression test for the HIGH
// under-billing defect on the OpenAI handler: a streamed (SSE) response must
// update the ledger from the final-frame usage, not bill $0.
func TestOpenAIStreamingBillsLedger(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(openaiSSEBody))
	}))
	defer upstream.Close()

	prev := OpenAIUpstream
	OpenAIUpstream = upstream.URL
	defer func() { OpenAIUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/oai-stream", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"gpt-4o","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/openai/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-1234567890")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	got, err := led.ProjectTotal("/proj/oai-stream")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 {
		t.Fatalf("expected 1 request billed, got %d (streamed response under-billed)", got.Requests)
	}
	if got.TokensIn != 900 || got.TokensOut != 400 {
		t.Fatalf("streamed usage not walked from SSE frames: in=%d out=%d (want 900/400)",
			got.TokensIn, got.TokensOut)
	}
	if got.USD <= 0 {
		t.Fatalf("streamed response billed $0 — under-billing defect still present")
	}
}
