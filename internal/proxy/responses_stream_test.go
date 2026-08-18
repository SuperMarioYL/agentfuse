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

// responsesSSEBody is a realistic Responses-API (POST /v1/responses, used by
// the Codex CLI for gpt-5/o3) streaming response. Text deltas arrive as a
// top-level `delta` STRING on response.output_text.delta frames; final usage
// arrives on response.completed under response.usage.{input_tokens,
// output_tokens}. parseDeepSeekUsage (the Chat-Completions walker) reads
// neither, so without the v0.11 fix this body billed in=0, out=0,
// completionText="".
const responsesSSEBody = `event: response.created
data: {"type":"response.created","response":{"id":"resp_01","model":"gpt-5","status":"in_progress","usage":{"input_tokens":128,"output_tokens":0}}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello ","item_id":"msg_0","output_index":0,"content_index":0}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"world","item_id":"msg_0","output_index":0,"content_index":0}

event: response.output_text.done
data: {"type":"response.output_text.done","text":"Hello world","item_id":"msg_0"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_01","model":"gpt-5","status":"completed","usage":{"input_tokens":128,"output_tokens":37,"total_tokens":165}}}

data: [DONE]

`

// TestParseOpenAIResponsesUsage_Unit exercises the parser on a streamed
// Responses body and the unary shape.
func TestParseOpenAIResponsesUsage_Unit(t *testing.T) {
	in, out, text, model := parseOpenAIResponsesUsage([]byte(responsesSSEBody))
	if in != 128 {
		t.Fatalf("stream input tokens wrong: got %d want 128 (response.usage.input_tokens not walked?)", in)
	}
	if out != 37 {
		t.Fatalf("stream output tokens wrong: got %d want 37 (response.usage.output_tokens not walked?)", out)
	}
	if model != "gpt-5" {
		t.Fatalf("stream model wrong: %q want gpt-5", model)
	}
	if text != "Hello world" {
		t.Fatalf("stream completion text wrong: %q want %q (delta strings not accumulated?)", text, "Hello world")
	}

	// Unary shape: a single JSON object with top-level usage + output text.
	unary := []byte(`{
		"model":"gpt-5",
		"usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33},
		"output":[{"type":"message","content":[{"type":"output_text","text":"hi there"}]}]
	}`)
	in, out, text, model = parseOpenAIResponsesUsage(unary)
	if in != 11 || out != 22 {
		t.Fatalf("unary usage wrong: in=%d out=%d want 11/22", in, out)
	}
	if model != "gpt-5" {
		t.Fatalf("unary model wrong: %q", model)
	}
	if text != "hi there" {
		t.Fatalf("unary completion text wrong: %q want %q", text, "hi there")
	}
}

// TestOpenAIResponsesStreamingBillsLedger is the v0.11 regression for
// fix-openai-responses-stream-misparse: a streamed /v1/responses request must
// bill the ledger from response.usage (non-zero in/out) instead of falling
// back to the 100-output-token fail-open cap.
func TestOpenAIResponsesStreamingBillsLedger(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(responsesSSEBody))
	}))
	defer upstream.Close()

	prev := OpenAIUpstream
	OpenAIUpstream = upstream.URL
	defer func() { OpenAIUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/responses-stream", cfg, led,
		&account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"gpt-5","input":"hi","stream":true}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/openai/v1/responses", bytes.NewReader(body))
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

	got, err := led.ProjectTotal("/proj/responses-stream")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 {
		t.Fatalf("expected 1 request billed, got %d (streamed Responses under-billed)", got.Requests)
	}
	// The defect billed in=0/out=0 then fell back to the 100-output-token cap.
	// Assert the REAL usage block landed instead of the fail-open fallback.
	if got.TokensIn != 128 {
		t.Fatalf("Responses stream input not walked from response.usage: got %d want 128", got.TokensIn)
	}
	if got.TokensOut != 37 {
		t.Fatalf("Responses stream output not walked from response.usage: got %d want 37 (fail-open 100-token fallback?)", got.TokensOut)
	}
	if got.USD <= 0 {
		t.Fatalf("streamed Responses billed $0 — under-billing defect still present")
	}
}

// TestIsOpenAIResponsesPath guards the routing predicate that selects the
// Responses parser: /v1/responses and /v1/responses/<id> branch correctly,
// /v1/chat/completions stays on the Chat-Completions walker.
func TestIsOpenAIResponsesPath(t *testing.T) {
	cases := []struct {
		p    string
		want bool
	}{
		{"/v1/responses", true},
		{"/v1/responses/resp_01", true},
		{"/v1/chat/completions", false},
		{"/v1/embeddings", false},
		{"/v1/responsesXYZ", false}, // prefix boundary: must not shadow other routes
	}
	for _, c := range cases {
		if got := isOpenAIResponsesPath(c.p); got != c.want {
			t.Errorf("isOpenAIResponsesPath(%q)=%v want %v", c.p, got, c.want)
		}
	}
}
