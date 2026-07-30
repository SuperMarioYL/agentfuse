package proxy

import (
	"bytes"
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

// fuseErrorBody mirrors the JSON writeFuseError emits, so a test can decode the
// 402 body and assert the deny carried a non-empty message + remediation.
type fuseErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	SuggestedCommand string `json:"suggested_command"`
}

// TestConcurrentDenyCarriesNonEmptyReason is the v0.7.0 regression test for the
// concurrent-deny empty-reason defect (fix-concurrent-deny-empty-reason).
//
// ReserveWindowed denies when persisted + reserved[project] + estimate > cap,
// but the !allowed branch previously recomputed the deny reason from a
// persisted-only WindowedTotal via budget.Decide. Whenever the deny was driven
// by in-flight reservations rather than persisted spend alone — i.e.
// persisted + estimate <= cap but persisted + reserved + estimate > cap, which
// requires reserved > 0 — Decide saw currentUSD + estimate <= cap and returned
// Allow=true with an EMPTY Reason and SuggestedCmd. The handler then wrote an
// HTTP 402 whose body was {"error":{"type":"agentfuse_budget_denied","message":""}}
// with no suggested_command, and printed an empty remediation line to stderr.
// The cap still fired (fail-closed held), but the user/agent CLI got a deny with
// no explanation.
//
// This test holds one in-flight reservation (so reserved > 0) while persisted
// spend alone is well under the cap, then fires a second request whose
// estimate pushes the PROJECTED total over the cap but leaves the persisted +
// estimate total under it. It asserts the second request is denied (402,
// upstream never called — fail-closed intact) AND that the 402 body carries a
// non-empty message and a non-empty suggested_command. Under the old
// persisted-only Decide path the message would be "" and suggested_command
// absent. Mirrors TestAnthropicFailsClosedWhenCapHit, which pre-charges
// persisted past the cap (reserved==0) and so never exercises the reserved>0
// branch this defect lives in.
func TestConcurrentDenyCarriesNonEmptyReason(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	prev := AnthropicUpstream
	AnthropicUpstream = upstream.URL
	defer func() { AnthropicUpstream = prev }()

	led := mustOpenLedger(t)
	const (
		proj      = "/deny/concurrent"
		capUSD    = 5.00
		persisted = 1.00 // well under cap; the deny must be driven by reserved, not persisted
		// Held in-flight reservation (not committed/released during the test).
		// persisted(1.00) + reserved(3.50) = 4.50; a request whose estimate
		// > 0.50 projects past the $5.00 cap and is denied — but persisted +
		// estimate stays under cap, so the persisted-only Decide path the bug
		// used returns Allow=true (empty reason).
		heldResv = 3.50
	)
	if _, err := led.Add(proj, 0, 0, persisted); err != nil {
		t.Fatal(err)
	}
	ok, err := led.ReserveWindowed(proj, "project", capUSD, heldResv)
	if err != nil || !ok {
		t.Fatalf("hold in-flight reservation failed: ok=%v err=%v", ok, err)
	}

	cfg := &budget.Config{CapUSD: capUSD, Window: "project"}
	s := New(proj, cfg, led, &account.File{Accounts: map[string]account.Account{}})
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	}()

	// claude-sonnet-4 output is $15/1M => max_tokens=80000 estimates ~$1.20.
	// projected = 1.00 + 3.50 + 1.20 = 5.70 > 5.00 (deny); persisted + estimate
	// = 2.20 <= 5.00 (the persisted-only Decide path returns empty here).
	body := []byte(`{"model":"claude-sonnet-4","max_tokens":80000,"messages":[]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/anthropic/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-test-1234567890")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Fail-closed: the cap still fires.
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("expected 402 (fail-closed), got %d", resp.StatusCode)
	}
	if called {
		t.Fatal("upstream must NOT be called when over cap (fail-closed)")
	}

	raw, _ := io.ReadAll(resp.Body)
	var fb fuseErrorBody
	if err := json.Unmarshal(raw, &fb); err != nil {
		t.Fatalf("decode 402 body: %v (raw=%s)", err, raw)
	}
	if fb.Error.Type != "agentfuse_budget_denied" {
		t.Fatalf("error type: got %q want agentfuse_budget_denied (raw=%s)", fb.Error.Type, raw)
	}
	// Headline assertion: the deny message must be NON-EMPTY. Under the old
	// persisted-only Decide path this was "" whenever reserved>0 drove the deny.
	if strings.TrimSpace(fb.Error.Message) == "" {
		t.Fatalf("concurrent deny carried an EMPTY message — the !allowed branch recomputed the reason from a persisted-only total (raw=%s)", raw)
	}
	// And the remediation command must be present and non-empty (was absent
	// under the old path because SuggestedCmd was "").
	if strings.TrimSpace(fb.SuggestedCommand) == "" {
		t.Fatalf("concurrent deny carried no suggested_command (raw=%s)", raw)
	}
	if !strings.Contains(fb.SuggestedCommand, "fuse cap") {
		t.Fatalf("suggested_command %q does not mention raising the cap", fb.SuggestedCommand)
	}
	t.Logf("concurrent deny message=%q suggested_command=%q", fb.Error.Message, fb.SuggestedCommand)
}
