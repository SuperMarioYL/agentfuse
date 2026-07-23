package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/SuperMarioYL/agentfuse/internal/tokens"
)

// TestUnaryCompletionExtractedForAccuracyHarness is the v0.5.0 regression for
// fix-accuracy-harness-empty-completion-unary (extraction half).
//
// The v0.4 RecordSample fix feeds EstimateCompletion(model, completionText) to
// the harness, but the UNARY branch of parseAnthropicUsage returned "" for
// completionText — it never extracted the content from a unary JSON response.
// EstimateCompletion(model, "") = roundUp(0, 100) = 100, so every unary
// Anthropic/OpenAI response recorded est=100 regardless of real completion
// length. For any outTok outside ~75-125, |error| > 25%, false-triggering the
// §8 kill criterion the v0.4 harness fix was built to make evaluable.
//
// This test fires a unary Anthropic response with real usage AND real content
// text (tokenizing to >100, so the roundUp-to-100 floor no longer masks the
// defect). It asserts the recorded sample measures EstimateCompletion(model,
// contentText) — NOT 100. Without the extraction fix the recorded est is 100
// (abs error ~83% for a ~600-token completion → ExceedsThreshold=true), so the
// assertions go red; with the fix the recorded est equals the recomputed
// EstimateCompletion (0% error → no false trigger), so they go green.
func TestUnaryCompletionExtractedForAccuracyHarness(t *testing.T) {
	// Long enough that the raw tiktoken count exceeds 100, so
	// EstimateCompletion rounds up to a value >100 — without this the broken
	// path (est=100) and the honest path would coincide and the test would
	// pass vacuously.
	contentText := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 60)
	expectedEst := tokens.EstimateCompletion("claude-sonnet-4", contentText)
	if expectedEst <= 100 {
		t.Fatalf("test content tokenized to %d (<=100); pick longer content so the roundUp floor does not mask the defect", expectedEst)
	}
	// Set the upstream "real" output equal to the estimator's rounded value, so
	// the honest path records 0% error. The point is to prove the RECORDED est
	// moved off the broken 100 onto the real EstimateCompletion(contentText);
	// the absolute numbers are recomputed from the same estimator the handler
	// uses, so the assertion is about which estimator side was wired.
	outTok := expectedEst

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_01",
			"model": "claude-sonnet-4",
			"usage": map[string]int{"input_tokens": 100, "output_tokens": outTok},
			"content": []map[string]string{
				{"type": "text", "text": contentText},
			},
		})
	}))
	defer upstream.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	// Isolate the global recorder so this single unary request is the only
	// sample present. DefaultRecorder is an exported *AccuracyRecorder; safe to
	// reassign under Go's serial-within-package test execution.
	saved := tokens.DefaultRecorder
	tokens.DefaultRecorder = tokens.NewAccuracyRecorder()
	defer func() { tokens.DefaultRecorder = saved }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/unary-acc", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-test-1234567890")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	st := tokens.DefaultRecorder.Stats()
	if st.N != 1 {
		t.Fatalf("expected exactly 1 accuracy sample on a unary response with content, got N=%d "+
			"(extraction/guard regression — completionText was empty so RecordSample was skipped)", st.N)
	}
	// With the fix, the recorded EstTokens == EstimateCompletion(model,
	// contentText) == outTok, so median |error|% == 0. Without the fix the
	// recorded est == 100 (the roundUp n<=0 -> step branch on empty
	// completionText), giving |100-outTok|/outTok*100 (~83% for a ~600-token
	// completion) — the assertion fails AND ExceedsThreshold is true.
	expectedAbsPct := math.Abs(float64(expectedEst-outTok)) / float64(outTok) * 100
	if math.Abs(st.MedianAbsPct-expectedAbsPct) > 0.01 {
		t.Fatalf("unary response recorded the wrong estimator: medianAbs%%=%.4f, "+
			"expected(=|EstimateCompletion(contentText)-outTok|/outTok*100)=%.4f "+
			"(EstimateCompletion(contentText)=%d, outTok=%d) — unary parseAnthropicUsage "+
			"branch is still returning empty completionText (est would be 100)",
			st.MedianAbsPct, expectedAbsPct, expectedEst, outTok)
	}
	if st.ExceedsThreshold {
		t.Fatalf("unary response false-triggered the §8 >25%% kill criterion (medianAbs%%=%.4f) — "+
			"the harness is recording est=100 (empty-completion roundUp) instead of the real EstimateCompletion(contentText)",
			st.MedianAbsPct)
	}
}

// TestUnaryNoCompletionSkipsRecordSample is the v0.5.0 regression for
// fix-accuracy-harness-empty-completion-unary (guard half).
//
// When a unary response carries usage but genuinely no completion content
// (no content blocks / empty choices), completionText is still "" even after
// the extraction fix. Recording that sample would measure
// EstimateCompletion(model, "")=100 vs outTok — a meaningless comparison that
// false-triggers the §8 >25% kill criterion for any outTok outside ~75-125.
// The v0.5 guard skips RecordSample when completionText is still "". This test
// fires a unary Anthropic response with usage but no content and asserts NO
// sample is recorded. Without the guard the sample is recorded (N=1, est=100);
// with the guard it is skipped (N=0).
func TestUnaryNoCompletionSkipsRecordSample(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Usage present, but NO content field — a real shape for some
		// tool-call-only or empty-completion responses.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_02",
			"model": "claude-sonnet-4",
			"usage": map[string]int{"input_tokens": 100, "output_tokens": 300},
		})
	}))
	defer upstream.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	saved := tokens.DefaultRecorder
	tokens.DefaultRecorder = tokens.NewAccuracyRecorder()
	defer func() { tokens.DefaultRecorder = saved }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/unary-nocontent", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-test-1234567890")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	st := tokens.DefaultRecorder.Stats()
	if st.N != 0 {
		t.Fatalf("expected NO accuracy sample for an empty-completion unary response, got N=%d "+
			"(medianAbs%%=%.4f) — RecordSample is not guarded on empty completionText, so it "+
			"recorded est=EstimateCompletion(model,\"\")=100 and false-triggers the §8 >25%% kill criterion",
			st.N, st.MedianAbsPct)
	}
	if st.ExceedsThreshold {
		t.Fatalf("empty-completion unary response false-triggered the §8 >25%% kill criterion — " +
			"the empty-completion guard is missing")
	}
}
