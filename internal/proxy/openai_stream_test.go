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
	"github.com/SuperMarioYL/agentfuse/internal/tokens"
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

// openaiPartialSSEBody is a Chat Completions stream where the relay closed
// after the content delta frames but the final usage frame reported only
// prompt_tokens (completion_tokens never arrived). The both-zero guard saw
// inTok>0 and skipped the per-side fallback, billing outTok=0 ($0 output)
// while a real completion ("Hello there") had been streamed — fail-open.
const openaiPartialSSEBody = `data: {"id":"c1","model":"gpt-4o","choices":[{"delta":{"content":"Hello "}}]}

data: {"id":"c1","model":"gpt-4o","choices":[{"delta":{"content":"there"}}]}

data: {"id":"c1","model":"gpt-4o","choices":[{"delta":{}}],"usage":{"prompt_tokens":900}}

data: [DONE]

`

// TestOpenAIPartialUsageFallsBackPerSide is the v0.10 regression for
// fix-anthropic-openai-partial-usage-bothzero-fallback (openai half). A
// partial-usage stream whose final usage frame carries prompt_tokens but no
// completion_tokens must bill the missing OUTPUT side via the local
// EstimateCompletion of the accumulated completion text, NOT 0. The INPUT side
// must keep the upstream-reported 900 (it was present, so the per-side fallback
// leaves it untouched).
func TestOpenAIPartialUsageFallsBackPerSide(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(openaiPartialSSEBody))
	}))
	defer upstream.Close()

	prev := OpenAIUpstream
	OpenAIUpstream = upstream.URL
	defer func() { OpenAIUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/oai-partial", cfg, led, &account.File{Accounts: map[string]account.Account{}})
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

	got, err := led.ProjectTotal("/proj/oai-partial")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 {
		t.Fatalf("expected 1 request billed, got %d", got.Requests)
	}
	// Input side was reported by the final usage frame — must be preserved
	// verbatim, NOT re-estimated.
	if got.TokensIn != 900 {
		t.Fatalf("partial stream input side should keep upstream value 900, got %d", got.TokensIn)
	}
	// Output side was absent — must be the EstimateCompletion of the
	// accumulated "Hello there", NOT 0 (the old both-zero guard billed 0 here).
	wantOut := tokens.EstimateCompletion("gpt-4o", "Hello there")
	if got.TokensOut != wantOut {
		t.Fatalf("partial stream output side should fall back to EstimateCompletion=%d, got %d "+
			"(both-zero guard billed 0?)", wantOut, got.TokensOut)
	}
	if got.TokensOut == 0 {
		t.Fatalf("partial stream billed outTok=0 — fail-open under-billing defect still present")
	}
	if got.USD <= 0 {
		t.Fatalf("partial stream billed $0 — under-billing defect still present")
	}
}
