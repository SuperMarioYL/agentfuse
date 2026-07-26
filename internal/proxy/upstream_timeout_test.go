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

// TestStalledUpstreamReleasesReservation is the v0.6.0 regression test for the
// upstream-client-no-timeout defect: all five handlers previously forwarded via
// http.DefaultClient (Timeout=0) and buffered the whole upstream body, so a
// slow/stalled upstream held the handler goroutine — and its ledger
// reservation — with no TTL, starving the project budget until process
// restart. The Server now carries a shared *http.Client with a wall-clock
// Timeout; a stalled upstream call errors out of the handler, which runs the
// deferred Release so the budget returns to available.
func TestStalledUpstreamReleasesReservation(t *testing.T) {
	// Upstream sleeps past the proxy's upstream-client timeout, then returns a
	// normal 200+usage. Under the fix the proxy times out first (502 + Release);
	// under the old code (Timeout=0) the proxy would wait for the 200 and commit
	// ~$0.0105, leaking/stale-charging the reservation.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"claude-sonnet-4","usage":{"input_tokens":1000,"output_tokens":500},"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upstream.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	led := mustOpenLedger(t)
	cfg := &budget.Config{CapUSD: 1.00, Window: "project"}
	s := New("/proj/stall", cfg, led, &account.File{Accounts: map[string]account.Account{}})
	// Short upstream-client timeout so the test runs in milliseconds, not the
	// production 5 minutes — still well under the 250ms the upstream sleeps.
	s.upstream = &http.Client{Timeout: 50 * time.Millisecond}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	body := []byte(`{"model":"claude-sonnet-4","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-test-1234567890")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("inbound call: %v", err)
	}
	elapsed := time.Since(start)
	defer resp.Body.Close()

	// The proxy's own upstream timeout must fire and return 502, NOT wait for
	// the slow 200 (which would commit the reservation). Under the old code
	// (http.DefaultClient, Timeout=0) this would be a 200 after ~250ms.
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502 — stalled upstream must error out, not hang/commit (took %v)",
			resp.StatusCode, elapsed)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("handler did not return promptly after upstream timeout: %v (upstream-client timeout did not fire)", elapsed)
	}

	// The reservation must have been released by the deferred Release, so the
	// full budget is available again: a fresh near-cap reserve must succeed.
	// Under the old code the slow 200 would have committed ~$0.0105, so a $0.99
	// reserve (projected $1.0005 > $1.00 cap) would be denied — the stale
	// commit/leak the fix prevents.
	ok, err := led.ReserveWindowed("/proj/stall", "project", 1.00, 0.99)
	if err != nil {
		t.Fatalf("fresh reserve errored: %v", err)
	}
	if !ok {
		t.Fatalf("stalled upstream leaked its reservation: a $0.99 fresh reserve should succeed after Release (budget not returned to available)")
	}

	// And the timed-out call must not have been billed (it errored, never
	// committed). ProjectTotal reads persisted rows only, so the in-flight
	// fresh reserve above does not count.
	tot, err := led.ProjectTotal("/proj/stall")
	if err != nil {
		t.Fatal(err)
	}
	if tot.Requests != 0 || tot.USD != 0 {
		t.Fatalf("timed-out upstream must not be billed: %+v", tot)
	}
}
