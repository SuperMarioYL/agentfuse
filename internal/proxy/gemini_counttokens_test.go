package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
)

// TestGeminiCountTokensNotBilledNorReserved is the v0.15 regression for
// fix-gemini-counttokens-billed-and-reserved.
//
// Gemini's :countTokens endpoint returns `{"totalTokens": N}` — a token count,
// not billable usage (no usageMetadata, no candidates) — and Google does not
// charge for it. The handler treated it like any other request: the per-side
// fallback billed a fabricated promptTokens + EstimateCompletion(model,"")=100
// output tokens, incremented the request counter, and the pre-flight reserve
// gated it against the cap, so a free call was denied outright once the project
// was at its ceiling.
func TestGeminiCountTokensNotBilledNorReserved(t *testing.T) {
	countTokensUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"totalTokens": 42})
	}))
	defer countTokensUpstream.Close()

	genUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates":    []map[string]any{{"content": map[string]any{"parts": []map[string]any{{"text": "hello"}}}}},
			"usageMetadata": map[string]int{"promptTokenCount": 800, "candidatesTokenCount": 200},
		})
	}))
	defer genUpstream.Close()

	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	const capUSD = 5.00

	// start brings up a server for one subtest and returns its base URL.
	start := func(t *testing.T, ledProject string, upstreamURL string, seedUSD float64) (string, func(), func() (int, int, float64)) {
		t.Helper()
		led := mustOpenLedger(t)
		if seedUSD > 0 {
			if _, err := led.Add(ledProject, 1, 1, seedUSD); err != nil {
				t.Fatal(err)
			}
		}
		cfg := &budget.Config{CapUSD: capUSD, Window: "project", Provider: "gemini"}
		s := New(ledProject, cfg, led, &account.File{Accounts: map[string]account.Account{}})
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		read := func() (int, int, float64) {
			tot, err := led.ProjectTotal(ledProject)
			if err != nil {
				t.Fatal(err)
			}
			return tot.Requests, tot.TokensOut, tot.USD
		}
		stop := func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = s.Stop(ctx)
		}
		return "http://" + s.Addr(), stop, read
	}

	post := func(t *testing.T, base, path string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("not billed", func(t *testing.T) {
		prev := GeminiUpstream
		GeminiUpstream = countTokensUpstream.URL
		defer func() { GeminiUpstream = prev }()

		base, stop, read := start(t, "/proj/ct-bill", countTokensUpstream.URL, 0)
		defer stop()

		if code := post(t, base, "/gemini/v1beta/models/gemini-1.5-pro:countTokens"); code != http.StatusOK {
			t.Fatalf("status=%d, want 200", code)
		}
		reqs, out, usd := read()
		// Before the fix: 1 request, TokensOut=100 (EstimateCompletion("") round-up),
		// USD>0 for a free call that reported no usage.
		if reqs != 0 || out != 0 || usd != 0 {
			t.Fatalf(":countTokens was billed: reqs=%d tokensOut=%d usd=%v (want all zero)", reqs, out, usd)
		}
	})

	t.Run("not denied at cap", func(t *testing.T) {
		prev := GeminiUpstream
		GeminiUpstream = countTokensUpstream.URL
		defer func() { GeminiUpstream = prev }()

		// Seed exactly the cap so any reserve against the window denies.
		base, stop, _ := start(t, "/proj/ct-cap", countTokensUpstream.URL, capUSD)
		defer stop()

		if code := post(t, base, "/gemini/v1beta/models/gemini-1.5-pro:countTokens"); code != http.StatusOK {
			t.Fatalf("status=%d, want 200 — a free :countTokens call must not be denied by an exhausted cap", code)
		}
	})

	t.Run("generateContent still billed", func(t *testing.T) {
		prev := GeminiUpstream
		GeminiUpstream = genUpstream.URL
		defer func() { GeminiUpstream = prev }()

		base, stop, read := start(t, "/proj/ct-gen", genUpstream.URL, 0)
		defer stop()

		if code := post(t, base, "/gemini/v1beta/models/gemini-1.5-pro:generateContent"); code != http.StatusOK {
			t.Fatalf("status=%d, want 200", code)
		}
		reqs, out, usd := read()
		if reqs != 1 || out != 200 || usd <= 0 {
			t.Fatalf("billable :generateContent must still bill: reqs=%d tokensOut=%d usd=%v", reqs, out, usd)
		}
	})
}
