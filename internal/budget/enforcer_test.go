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
