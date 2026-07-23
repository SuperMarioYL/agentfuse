package proxy

import (
	"bytes"
	"compress/gzip"
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

// TestGzipAcceptEncodingNotForwarded is the v0.5.0 regression for the HIGH
// fail-open cap-correctness defect fix-gzip-accept-encoding-forwarded.
//
// copyHeader forwarded the inbound "Accept-Encoding: gzip" (which Go/Node/fetch
// HTTP clients add by default) onto upReq.Header on all five handlers. Go's
// http transport only auto-decompresses a gzipped response when IT added the
// Accept-Encoding header; a caller-set value is passed through untouched, so a
// gzipped unary JSON body is read raw by io.ReadAll(resp.Body).
// parseAnthropicUsage then json.Unmarshals the gzip bytes into an error
// (in=0, out=0, completionText=""), the per-side fallback bills promptTokens +
// EstimateCompletion(model, "")=100, and realized spend exceeds the cap without
// tripping it (fail-open) — the exact property the product exists to prevent.
//
// This test reproduces the defect hermetically: the httptest upstream HONORS
// Accept-Encoding: gzip (the existing tests' upstreams never did, which is why
// the defect slipped past them). The inbound client request sets
// Accept-Encoding: gzip. Without the fix the proxy forwards it, the upstream
// gzips, the proxy reads raw \x1f\x8b bytes, parsing fails, and the ledger
// bills the fallback (outTok=100) instead of the real usage (outTok=500). With
// the fix copyHeader strips Accept-Encoding, the proxy's own transport manages
// gzip (re-adds Accept-Encoding, decompresses, strips Content-Encoding), and
// the usage parser sees plain JSON — the ledger bills the real 1000/500.
func TestGzipAcceptEncodingNotForwarded(t *testing.T) {
	var upstreamSawAcceptEncoding string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamSawAcceptEncoding = r.Header.Get("Accept-Encoding")
		payload, _ := json.Marshal(map[string]any{
			"id":    "msg_01",
			"model": "claude-sonnet-4",
			"usage": map[string]int{"input_tokens": 1000, "output_tokens": 500},
			"content": []map[string]string{
				{"type": "text", "text": "unary gzipped response"},
			},
		})
		if strings.Contains(upstreamSawAcceptEncoding, "gzip") {
			// Honor the advertised encoding — this is what a real upstream
			// (api.anthropic.com, api.openai.com, …) does for a non-streamed
			// request that advertised gzip.
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			_, _ = gz.Write(payload)
			_ = gz.Close()
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(buf.Bytes())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 5.00, Window: "project"}
	s := New("/proj/gzip", cfg, led, &account.File{Accounts: map[string]account.Account{}})
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
	// Simulate a caller (Go/Node/fetch default) that advertises gzip. Before
	// the fix, copyHeader forwarded this verbatim and the upstream gzipped the
	// unary JSON body, which Go's transport then handed to the handler raw
	// (no auto-decompress for a caller-set header).
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	// Sanity: the test is only meaningful if the upstream actually received a
	// gzip advertisement and gzipped the body. Without this, the defect would
	// not reproduce and the test would pass vacuously.
	if !strings.Contains(upstreamSawAcceptEncoding, "gzip") {
		t.Fatalf("test setup broken: upstream never saw Accept-Encoding: gzip (got %q) — defect not exercised", upstreamSawAcceptEncoding)
	}

	got, err := led.ProjectTotal("/proj/gzip")
	if err != nil {
		t.Fatal(err)
	}
	// The defect: a forwarded Accept-Encoding: gzip made the upstream gzip the
	// unary JSON, the parser read raw \x1f\x8b bytes, json.Unmarshal errored,
	// and the per-side fallback billed promptTokens + EstimateCompletion(model,
	// "")=100 instead of the real 1000/500. Asserting the REAL usage lands
	// proves the skip-list fix let the proxy transport manage gzip.
	if got.TokensIn != 1000 || got.TokensOut != 500 {
		t.Fatalf("gzipped unary response not parsed — ledger has in=%d out=%d (want 1000/500); "+
			"copyHeader is still forwarding caller-set Accept-Encoding and the parser is reading raw gzip bytes",
			got.TokensIn, got.TokensOut)
	}
	if got.USD <= 0 {
		t.Fatalf("gzipped unary response billed $0 — fail-open defect still present")
	}
}
