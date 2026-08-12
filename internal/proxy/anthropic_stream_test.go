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

// anthropicSSEBody is a realistic Anthropic streaming response: message_start
// carries input_tokens + model, content_block_delta carries text, message_delta
// carries output_tokens. This is the Claude Code default (stream: true) and is
// exactly the body a single json.Unmarshal cannot parse.
const anthropicSSEBody = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","model":"claude-sonnet-4","usage":{"input_tokens":1200,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":620}}

event: message_stop
data: {"type":"message_stop"}

`

// TestAnthropicStreamingBillsLedger is the v0.3.0 regression test for the HIGH
// under-billing defect: a streamed (SSE) Anthropic response must update the
// ledger with the input/output tokens walked from the event frames, NOT bill $0.
func TestAnthropicStreamingBillsLedger(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(anthropicSSEBody))
	}))
	defer upstream.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/stream", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"claude-sonnet-4","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-test-1234567890")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	got, err := led.ProjectTotal("/proj/stream")
	if err != nil {
		t.Fatal(err)
	}
	// The defect billed $0 on streamed responses. Assert the tokens from the SSE
	// frames actually landed.
	if got.Requests != 1 {
		t.Fatalf("expected 1 request billed, got %d (streamed response under-billed)", got.Requests)
	}
	if got.TokensIn != 1200 || got.TokensOut != 620 {
		t.Fatalf("streamed usage not walked from SSE frames: in=%d out=%d (want 1200/620)",
			got.TokensIn, got.TokensOut)
	}
	if got.USD <= 0 {
		t.Fatalf("streamed response billed $0 — under-billing defect still present")
	}
}

// TestParseAnthropicUsage_UnaryAndStream unit-tests the parser on both shapes.
func TestParseAnthropicUsage_UnaryAndStream(t *testing.T) {
	// Unary JSON.
	unary := []byte(`{"model":"claude-sonnet-4","usage":{"input_tokens":100,"output_tokens":50}}`)
	in, out, _, model := parseAnthropicUsage(unary)
	if in != 100 || out != 50 || model != "claude-sonnet-4" {
		t.Fatalf("unary parse wrong: in=%d out=%d model=%s", in, out, model)
	}

	// Stream.
	in, out, text, model := parseAnthropicUsage([]byte(anthropicSSEBody))
	if in != 1200 || out != 620 {
		t.Fatalf("stream usage wrong: in=%d out=%d (want 1200/620)", in, out)
	}
	if model != "claude-sonnet-4" {
		t.Fatalf("stream model wrong: %s", model)
	}
	if text != "Hello world" {
		t.Fatalf("stream completion text wrong: %q", text)
	}
}

// TestAnthropicStreamNoUsageFallsBackToTiktoken verifies that when an SSE body
// carries content but NO usage block at all, the handler still bills via the
// local tiktoken fallback instead of $0.
func TestAnthropicStreamNoUsageFallsBackToTiktoken(t *testing.T) {
	noUsageSSE := `event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"some completion text that should be tokenized locally"}}

event: message_stop
data: {"type":"message_stop"}

`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(noUsageSSE))
	}))
	defer upstream.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/nousage", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"claude-sonnet-4","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"please write something"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("x-api-key", "sk-ant-test-1234567890")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	got, err := led.ProjectTotal("/proj/nousage")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 || got.TokensOut <= 0 || got.USD <= 0 {
		t.Fatalf("no-usage stream should bill via tiktoken fallback, got %+v", got)
	}
}

// anthropicPartialSSEBody is an Anthropic stream where the relay closed
// mid-stream AFTER the content_block_delta frames but BEFORE the final
// message_delta: message_start carried usage.input_tokens=1200 and the delta
// frames accumulated "Hello world", but usage.output_tokens never arrived.
// This is the fail-open defect class — the both-zero guard saw inTok>0 and
// skipped the per-side fallback, billing outTok=0 ($0 output) while a real
// completion had been streamed.
const anthropicPartialSSEBody = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","model":"claude-sonnet-4","usage":{"input_tokens":1200,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

`

// TestAnthropicPartialUsageFallsBackPerSide is the v0.10 regression for
// fix-anthropic-openai-partial-usage-bothzero-fallback (anthropic half). A
// partial-usage stream — message_start reports input_tokens but message_delta
// (output_tokens) is absent because a relay closed mid-stream — must bill the
// missing OUTPUT side via the local EstimateCompletion of the accumulated
// completion text, NOT 0. The INPUT side must keep the upstream-reported 1200
// (it was present, so the per-side fallback leaves it untouched), proving the
// guard fills only the missing side.
func TestAnthropicPartialUsageFallsBackPerSide(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(anthropicPartialSSEBody))
	}))
	defer upstream.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/partial", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"claude-sonnet-4","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-test-1234567890")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	got, err := led.ProjectTotal("/proj/partial")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 {
		t.Fatalf("expected 1 request billed, got %d", got.Requests)
	}
	// Input side was reported by message_start — must be preserved verbatim,
	// NOT re-estimated (proves the per-side guard leaves a present side alone).
	if got.TokensIn != 1200 {
		t.Fatalf("partial stream input side should keep upstream value 1200, got %d", got.TokensIn)
	}
	// Output side was absent — must be the EstimateCompletion of the
	// accumulated "Hello world", NOT 0 (the old both-zero guard billed 0 here).
	wantOut := tokens.EstimateCompletion("claude-sonnet-4", "Hello world")
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
