package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
)

// geminiJSONArrayBody is a streamGenerateContent response in JSON-array mode
// (a client that calls :streamGenerateContent WITHOUT alt=sse gets this shape):
// one compact line that is a JSON array of frames, the last carrying
// usageMetadata. A single json.Unmarshal of the whole body into a JSON object
// fails, and the SSE line-walker skips every line starting with '[' — so before
// the v0.6.0 fix the whole array was dropped (in=0, out=0, completionText="")
// and the per-side fallback billed EstimateCompletion(model,"")=100 regardless
// of the real 200 candidate tokens (fail-open).
const geminiJSONArrayBody = `[{"candidates":[{"content":{"parts":[{"text":"Hello "}]}}]},{"candidates":[{"content":{"parts":[{"text":"there"}]}}]},{"usageMetadata":{"promptTokenCount":800,"candidatesTokenCount":200,"totalTokenCount":1000}}]`

// TestGeminiJSONArrayStreamBillsLedger is the v0.6.0 regression test for the
// HIGH fail-open cap-correctness defect: a Gemini JSON-array
// streamGenerateContent body must be billed with the real prompt/candidate
// counts from the final frame's usageMetadata, not the 100-token fail-open
// fallback.
func TestGeminiJSONArrayStreamBillsLedger(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(geminiJSONArrayBody))
	}))
	defer upstream.Close()

	prev := GeminiUpstream
	GeminiUpstream = upstream.URL
	defer func() { GeminiUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project", Provider: "gemini"}
	s := New("/proj/gemini-json-array", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi gemini stream"}]}]}`)
	url := "http://" + s.Addr() + "/gemini/v1beta/models/gemini-1.5-pro:streamGenerateContent"
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

	got, err := led.ProjectTotal("/proj/gemini-json-array")
	if err != nil {
		t.Fatal(err)
	}
	if got.Requests != 1 {
		t.Fatalf("expected 1 request billed, got %d (JSON-array stream dropped entirely)", got.Requests)
	}
	// Real prompt count from usageMetadata — under the bug this fell through to
	// the tiktoken estimate of "hi gemini stream" (~5), never 800.
	if got.TokensIn != 800 {
		t.Fatalf("JSON-array stream not parsed for prompt tokens: in=%d (want 800 — fail-open fallback would be ~5)",
			got.TokensIn)
	}
	// Real candidate count from usageMetadata — under the bug the whole array
	// was skipped so completionText="" and outTok fell to EstimateCompletion("",
	// 100). 200 (not 100) proves the array was walked.
	if got.TokensOut != 200 {
		t.Fatalf("JSON-array stream billed fail-open 100 output tokens: out=%d (want 200)",
			got.TokensOut)
	}
	if got.USD <= 0 {
		t.Fatalf("JSON-array stream billed $0 — fail-open defect still present: %+v", got)
	}
}
