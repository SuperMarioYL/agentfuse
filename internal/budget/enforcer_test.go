package budget

import "testing"

func TestPriceForKnownModel(t *testing.T) {
	p := PriceFor("claude-sonnet-4-5-20251001")
	if p.InputPer1M != 3.00 || p.OutputPer1M != 15.00 {
		t.Fatalf("unexpected price: %+v", p)
	}
}

func TestPriceForUnknownFallsBack(t *testing.T) {
	p := PriceFor("totally-fake-model-9000")
	if p != fallbackPrice {
		t.Fatalf("expected fallback, got %+v", p)
	}
}

func TestEstimateConservativeForUnknownInputs(t *testing.T) {
	// With promptTokens=0 we should still produce a non-trivial estimate.
	got := EstimateRequest("claude-opus-4", 0, 0)
	if got <= 0 {
		t.Fatalf("estimate must be positive for unknown inputs, got %v", got)
	}
}

func TestDecideAllowsUnderCap(t *testing.T) {
	d := Decide(1.00, 5.00, 0.50, "/tmp/proj")
	if !d.Allow {
		t.Fatalf("expected ALLOW, got DENY: %+v", d)
	}
}

func TestDecideDeniesAtCap(t *testing.T) {
	d := Decide(4.80, 5.00, 0.50, "/tmp/proj")
	if d.Allow {
		t.Fatalf("expected DENY, got ALLOW: %+v", d)
	}
	if d.SuggestedCmd == "" {
		t.Fatal("DENY should suggest a recovery command")
	}
}

func TestCostFromUsage(t *testing.T) {
	usd := CostFromUsage("gpt-4o", 1_000_000, 500_000)
	want := 2.50 + 5.00
	if usd < want-1e-6 || usd > want+1e-6 {
		t.Fatalf("got %v want %v", usd, want)
	}
}

// TestDenyDecisionAlwaysNonEmpty is the v0.7.0 unit test for the
// fix-concurrent-deny-empty-reason deny formatter. DenyDecision is the
// !allowed-branch counterpart to Decide: it never consults Allow, so it
// ALWAYS returns a non-empty Reason + SuggestedCmd — even when the projected
// total it is handed is at or below the cap, which is exactly the case where
// the persisted-only Decide path returns an empty reason (the concurrent-deny
// defect: persisted + estimate <= cap but persisted + reserved + estimate >
// cap, which requires reserved > 0).
func TestDenyDecisionAlwaysNonEmpty(t *testing.T) {
	d := DenyDecision(5.70, 5.00, 1.20, "/proj")
	if d.Allow {
		t.Fatal("DenyDecision must return Allow=false")
	}
	if d.Reason == "" {
		t.Fatal("DenyDecision must return a non-empty Reason")
	}
	if d.SuggestedCmd != "fuse cap +5" {
		t.Fatalf("SuggestedCmd: got %q want %q", d.SuggestedCmd, "fuse cap +5")
	}
	if d.CapUSD != 5.00 || d.EstimateUSD != 1.20 || d.CurrentUSD != 4.50 {
		t.Fatalf("Decision fields wrong: %+v", d)
	}
	// Headline guarantee: even when projected <= cap (a TOCTOU edge where a
	// reservation committed between the deny and the reason format), the reason
	// is STILL non-empty. The defect was an empty 402 message, not a
	// slightly-stale projected number.
	d2 := DenyDecision(4.00, 5.00, 1.00, "/proj")
	if d2.Allow || d2.Reason == "" || d2.SuggestedCmd == "" {
		t.Fatalf("DenyDecision must be non-empty even when projected<=cap: %+v", d2)
	}
}
