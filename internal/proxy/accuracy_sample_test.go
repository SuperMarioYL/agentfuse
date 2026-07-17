package proxy

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/SuperMarioYL/agentfuse/internal/tokens"
)

// TestAnthropicRecordSampleUsesCompletionEstimator is the v0.4 regression for
// the accuracy-harness wrong-estimator-side fix. v0.3.0 wired RecordSample in the
// "else" branch (both upstream usage AND a local estimate available) but passed
// tokens.EstimatePrompt(model, completionText) — the PROMPT estimator (no +100
// round-up) run on the COMPLETION text, compared against outTok (OUTPUT tokens).
// The harness is documented as measuring the tiktoken estimate for a completion
// vs the upstream-reported count, and the estimator the ledger fallback actually
// bills with is EstimateCompletion (rounds UP to the nearest 100 tokens). v0.4
// corrects the call to EstimateCompletion. This test fires one streamed response
// with both usage and completion text, then asserts the recorded sample's implied
// EstTokens equals EstimateCompletion(model, completionText) — i.e. the harness
// now measures the estimator the cap actually bills with.
func TestAnthropicRecordSampleUsesCompletionEstimator(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(anthropicSSEBody))
	}))
	defer upstream.Close()
	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	// Isolate the global recorder: swap in a fresh one for the duration of this
	// test so the single streamed request is the only sample present. DefaultRecorder
	// is an exported package var (*AccuracyRecorder), safe to reassign under Go's
	// serial-within-package test execution.
	saved := tokens.DefaultRecorder
	tokens.DefaultRecorder = tokens.NewAccuracyRecorder()
	defer func() { tokens.DefaultRecorder = saved }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/acc", cfg, led, &account.File{Accounts: map[string]account.Account{}})
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

	st := tokens.DefaultRecorder.Stats()
	if st.N != 1 {
		t.Fatalf("expected exactly 1 accuracy sample, got N=%d", st.N)
	}
	// The SSE fixture reports output_tokens=620 and completion text "Hello world".
	const outTok = 620
	expectedEst := tokens.EstimateCompletion("claude-sonnet-4", "Hello world")
	// With one sample, median abs% = |est - actual| / actual * 100. Asserting it
	// equals the value recomputed from EstimateCompletion proves the recorded
	// EstTokens == EstimateCompletion(model, completionText) — i.e. the harness
	// measures the estimator the fallback bills with, NOT EstimatePrompt (which
	// would differ by the +100 completion round-up).
	expectedAbsPct := math.Abs(float64(expectedEst-outTok)) / float64(outTok) * 100
	if math.Abs(st.MedianAbsPct-expectedAbsPct) > 0.01 {
		t.Fatalf("recorded sample implies a different estimator than EstimateCompletion: "+
			"medianAbs%%=%.4f expected(=|EstimateCompletion(out)|/out*100)=%.4f (EstimateCompletion=%d, outTok=%d) "+
			"— wrong estimator side still wired (EstimatePrompt instead of EstimateCompletion?)",
			st.MedianAbsPct, expectedAbsPct, expectedEst, outTok)
	}
}
