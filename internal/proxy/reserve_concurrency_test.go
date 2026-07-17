package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/SuperMarioYL/agentfuse/internal/ledger"
)

// TestHandlerReserveDoesNotOvershoot is the v0.4 race-fix regression at the
// handler level. v0.3.0 only converted anthropic.go + openai.go to the atomic
// Reserve/CommitDelta guard; deepseek.go, gemini.go, and openai_compat.go were
// left on the racy ProjectTotal->Decide->Add path, so on those 3/5 providers N
// concurrent requests could all read the same under-cap persisted total, all
// pass Decide, all forward, and all Add — collectively overshooting the cap by
// up to (N-1) per-request costs. Each subtest fires 50 concurrent 2xx requests
// whose per-request actual cost * 50 far exceeds the cap, and asserts (a) the
// total billed never exceeds the cap (+ small float slack) and (b) the cap
// actually engaged under concurrency (fewer than N granted). Before the fix
// every subtest would bill 50*actual >> cap with 0 rejections.
func TestHandlerReserveDoesNotOvershoot(t *testing.T) {
	const capUSD = 0.50
	const workers = 50

	// deepseek-chat: output 0.0011/1k. 50_000 completion tokens => ~$0.055/req
	// => 50 reqs => $2.75, far over the $0.50 cap. estimate (max_tokens=100_000)
	// => ~$0.11/req < cap, so floor(cap/estimate)=4 may be granted concurrently.
	t.Run("deepseek", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model": "deepseek-chat",
				"choices": []map[string]any{
					{"message": map[string]string{"role": "assistant", "content": "ok"}},
				},
				"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 50000, "total_tokens": 50001},
			})
		}))
		defer upstream.Close()
		prev := DeepSeekUpstream
		DeepSeekUpstream = upstream.URL
		defer func() { DeepSeekUpstream = prev }()

		led := mustOpenLedger(t)
		cfg := &budget.Config{CapUSD: capUSD, Window: "project", Provider: "deepseek"}
		s := New("/race/deepseek", cfg, led, &account.File{Accounts: map[string]account.Account{}})
		startAndStop(t, s)

		body := []byte(`{"model":"deepseek-chat","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`)
		granted := fireConcurrent(t, s, "/deepseek/v1/chat/completions", body, workers)
		assertCapHeld(t, led, "/race/deepseek", capUSD, granted, workers)
	})

	// openai_compat grok-2-mini: output 0.001/1k. 50_000 completion => ~$0.05/req
	// => 50 reqs => $2.50 >> cap. estimate (max_tokens=100_000) => ~$0.10/req < cap.
	t.Run("openai_compat", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model": "grok-2-mini",
				"choices": []map[string]any{
					{"message": map[string]string{"role": "assistant", "content": "ok"}},
				},
				"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 50000, "total_tokens": 50001},
			})
		}))
		defer upstream.Close()

		led := mustOpenLedger(t)
		cfg := &budget.Config{CapUSD: capUSD, Window: "project", Provider: "openai_compat", UpstreamURL: upstream.URL}
		s := New("/race/openaicompat", cfg, led, &account.File{Accounts: map[string]account.Account{}})
		startAndStop(t, s)

		body := []byte(`{"model":"grok-2-mini","max_tokens":100000,"messages":[{"role":"user","content":"hi"}]}`)
		granted := fireConcurrent(t, s, "/openai_compat/v1/chat/completions", body, workers)
		assertCapHeld(t, led, "/race/openaicompat", capUSD, granted, workers)
	})

	// gemini-1.5-flash: output 0.0003/1k. 50_000 candidatesTokenCount => ~$0.015/req
	// => 50 reqs => $0.75 >> cap. estimate (maxOutputTokens=100_000) => ~$0.03/req < cap.
	t.Run("gemini", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"usageMetadata": map[string]int{
					"promptTokenCount":     1,
					"candidatesTokenCount": 50000,
					"totalTokenCount":      50001,
				},
				"candidates": []map[string]any{
					{"content": map[string]any{"parts": []map[string]string{{"text": "ok"}}}},
				},
			})
		}))
		defer upstream.Close()
		prev := GeminiUpstream
		GeminiUpstream = upstream.URL
		defer func() { GeminiUpstream = prev }()

		led := mustOpenLedger(t)
		cfg := &budget.Config{CapUSD: capUSD, Window: "project", Provider: "gemini"}
		s := New("/race/gemini", cfg, led, &account.File{Accounts: map[string]account.Account{}})
		startAndStop(t, s)

		body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":100000}}`)
		granted := fireConcurrent(t, s, "/gemini/v1beta/models/gemini-1.5-flash:generateContent", body, workers)
		assertCapHeld(t, led, "/race/gemini", capUSD, granted, workers)
	})
}

// startAndStop starts a server and registers graceful shutdown on test cleanup.
func startAndStop(t *testing.T, s *Server) {
	t.Helper()
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
}

// fireConcurrent fires n concurrent POSTs at path with body and returns the
// number that received HTTP 200 (i.e. were granted by the budget guard).
func fireConcurrent(t *testing.T, s *Server, path string, body []byte, n int) int {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+path, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("req: %v", err)
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return granted
}

// assertCapHeld asserts the v0.4 race-fix invariants for one handler subtest:
// (1) the total billed must not exceed the cap (+ small float slack for the
// one in-flight estimate that may commit at the boundary); (2) the cap engaged
// under concurrency — fewer than N requests were granted. Before the
// Reserve/CommitDelta propagation, billed == N*actual >> cap and granted == N.
func assertCapHeld(t *testing.T, led *ledger.Ledger, project string, capUSD float64, granted, n int) {
	t.Helper()
	got, err := led.ProjectTotal(project)
	if err != nil {
		t.Fatalf("ProjectTotal: %v", err)
	}
	// +0.05 slack = one estimate boundary of float noise; the invariant is that
	// realized spend never exceeds the cap by a request's worth. Before the fix
	// 50*actual (deepseek ~$2.75, grok ~$2.50, gemini ~$0.75) all blow past this.
	if got.USD > capUSD+0.05 {
		t.Fatalf("%s: billed $%.4f exceeds cap $%.2f (+slack) — race-fix not propagated; granted=%d/%d",
			project, got.USD, capUSD, granted, n)
	}
	if granted >= n {
		t.Fatalf("%s: all %d concurrent requests granted (cap never engaged under concurrency) — racy Decide path still in use",
			project, n)
	}
	if granted == 0 {
		t.Fatalf("%s: zero requests granted — estimate per-request >= cap, test misconfigured", project)
	}
	t.Logf("%s: granted=%d/%d billed=$%.4f cap=$%.2f", project, granted, n, got.USD, capUSD)
}
