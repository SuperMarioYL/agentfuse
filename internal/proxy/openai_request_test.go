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
	"github.com/SuperMarioYL/agentfuse/internal/tokens"
)

// The v0.14 request-side regressions for the openai handler's /v1/responses
// path. The v0.11 milestone widened RESPONSE parsing to the Responses API
// (parseOpenAIResponsesUsage); these tests guard the REQUEST side that was
// never widened: prompt extraction (fix-responses-prompt-fallback-empty-input)
// and the max-output-token field chain (fix-openai-max-completion-tokens-
// estimate).

// TestResponsesPromptText is the v0.14 regression for
// fix-responses-prompt-fallback-empty-input: openaiPromptText reads only the
// Chat-Completions `messages` / legacy `prompt` fields, so a /v1/responses
// body (input under `input` string-or-items + `instructions`) flattened to ""
// and the per-side input fallback billed EstimatePrompt(model, "") = 0 tokens
// — fail-open on the Codex path.
func TestResponsesPromptText(t *testing.T) {
	// Array-input variant: instructions + input items whose content is a
	// string OR an array of {type,text} parts (the Codex CLI shape).
	arrayBody := []byte(`{
	  "model": "gpt-5",
	  "instructions": "You are a coding agent. Be concise.",
	  "input": [
	    {"role": "developer", "content": [{"type": "input_text", "text": "Read the repo and fix the failing test."}]},
	    {"role": "user", "content": "The test fails on line 42; here is the file."}
	  ],
	  "stream": true
	}`)
	got := responsesPromptText(arrayBody)
	if !strings.Contains(got, "You are a coding agent. Be concise.") {
		t.Errorf("array-input: instructions not extracted: %q", got)
	}
	if !strings.Contains(got, "Read the repo and fix the failing test.") {
		t.Errorf("array-input: {type,text}-part content not extracted: %q", got)
	}
	if !strings.Contains(got, "The test fails on line 42") {
		t.Errorf("array-input: string-content item not extracted: %q", got)
	}
	if tokens.EstimatePrompt("gpt-5", got) <= 0 {
		t.Errorf("array-input: EstimatePrompt of extracted text = %d, want > 0 (the fallback billed 0 before the fix)", tokens.EstimatePrompt("gpt-5", got))
	}

	// String-input variant.
	stringBody := []byte(`{"model":"gpt-5","input":"summarize this document","stream":true}`)
	got = responsesPromptText(stringBody)
	if got != "summarize this document" {
		t.Errorf("string-input: got %q, want the input string", got)
	}

	// Non-JSON body degrades to the raw body (parity with openaiPromptText).
	if got := responsesPromptText([]byte("not json")); got != "not json" {
		t.Errorf("non-json: got %q, want raw body", got)
	}

	// The Chat-Completions extractor must keep handling its own shape.
	ccBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`)
	if got, want := openaiPromptText(ccBody), "user:hello\n"; got != want {
		t.Errorf("openaiPromptText(messages shape) = %q, want %q (regression?)", got, want)
	}
}

// TestOpenAIResponsesTruncatedStreamBillsInputFallback mirrors
// TestOpenAIResponsesStreamingBillsLedger but with the usage-bearing
// response.completed frame MISSING (the clean-TCP-FIN truncation scenario the
// v0.10 partial-usage fix targets): the input side must fall back to the
// tiktoken estimate of the REQUEST text, which was "" (billed 0 input tokens)
// before responsesPromptText existed.
func TestOpenAIResponsesTruncatedStreamBillsInputFallback(t *testing.T) {
	// response.created (no usage yet) + text deltas, then the connection
	// closes — response.completed never arrives, so the response carries no
	// usage at all and BOTH sides must come from the fallback.
	truncated := `event: response.created
data: {"type":"response.created","response":{"id":"resp_01","model":"gpt-5","status":"in_progress"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello ","item_id":"msg_0","output_index":0,"content_index":0}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"world","item_id":"msg_0","output_index":0,"content_index":0}

data: [DONE]

`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(truncated))
	}))
	defer upstream.Close()

	prev := OpenAIUpstream
	OpenAIUpstream = upstream.URL
	defer func() { OpenAIUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/responses-trunc", cfg, led,
		&account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	// A realistic Codex-shaped request: instructions + input items. The
	// prompt text is > 100 tokens so a passing input-side estimate is
	// unambiguous.
	body := []byte(`{
	  "model": "gpt-5",
	  "instructions": "You are a coding agent working in a repository. Read carefully, reason step by step, and keep answers concise. Prefer minimal diffs. Run tests before declaring success. Explain every change you make in terms of the failing behavior it fixes.",
	  "input": [
	    {"role": "user", "content": "The integration test suite fails intermittently on CI but never locally. Here is the flaky test, the fixture setup code, the CI workflow definition, and the last ten failure logs with full stack traces from three different runners."}
	  ],
	  "stream": true
	}`)
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

	got, err := led.ProjectTotal("/proj/responses-trunc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 {
		t.Fatalf("expected 1 request billed, got %d", got.Requests)
	}
	// Before the fix: promptText="" -> EstimatePrompt=0 -> input side billed
	// 0 tokens ($0 input on a truncated stream — fail-open).
	if got.TokensIn <= 0 {
		t.Fatalf("truncated Responses stream billed %d input tokens (fallback from an empty prompt text — fail-open)", got.TokensIn)
	}
	if got.TokensOut <= 0 {
		t.Fatalf("truncated Responses stream billed %d output tokens (delta text fallback missing?)", got.TokensOut)
	}
	if got.USD <= 0 {
		t.Fatalf("truncated Responses stream billed $0")
	}
}

// TestOpenAIMaxOutputTokens is the v0.14 regression for
// fix-openai-max-completion-tokens-estimate: the chain must honor the
// declared ceiling across all three wire generations — max_tokens (legacy),
// max_output_tokens (Responses), max_completion_tokens (current Chat
// Completions; the only max field o-series/gpt-5 clients send there).
func TestOpenAIMaxOutputTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"max_tokens wins", `{"model":"gpt-4o","max_tokens":1000}`, 1000},
		{"max_output_tokens when max_tokens absent", `{"model":"gpt-5","max_output_tokens":5000}`, 5000},
		{"max_completion_tokens only", `{"model":"o3","max_completion_tokens":12000}`, 12000},
		{"max_tokens precedes max_completion_tokens", `{"model":"gpt-4o","max_tokens":1000,"max_completion_tokens":12000}`, 1000},
		{"no ceiling declared", `{"model":"gpt-4o"}`, 0},
	}
	for _, c := range cases {
		var req openaiRequest
		if err := json.Unmarshal([]byte(c.body), &req); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		if got := openaiMaxOutputTokens(req); got != c.want {
			t.Errorf("%s: openaiMaxOutputTokens = %d, want %d", c.name, got, c.want)
		}
	}
}
