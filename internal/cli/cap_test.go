package cli

import "testing"

func TestApplyCapDelta(t *testing.T) {
	cases := []struct {
		current float64
		arg     string
		want    float64
		errok   bool
	}{
		{5.0, "+5", 10.0, false},
		{10.0, "-3", 7.0, false},
		{10.0, "=20", 20.0, false},
		{10.0, "15", 15.0, false},
		{5.0, "-10", 0, true}, // would drop to negative
		{5.0, "=0", 0, true},  // must stay > 0
		{5.0, "abc", 0, true}, // unparseable
		// v0.16: fix-cap-rounds-to-zero-unusable-config. These are positive
		// BEFORE roundCents floors them to whole cents, so they became $0.00 —
		// which budget.Load rejects, stranding the project (init refuses to
		// overwrite, and the deny path's own `fuse cap +5` would fail on load).
		// All three must error instead of returning 0.
		{5.0, "=0.001", 0, true},
		{5.0, "0.001", 0, true},
		{5.0, "-4.999", 0, true},
		{5.0, "=0.01", 0.01, false}, // smallest representable positive cap still writes
	}
	for _, c := range cases {
		got, err := applyCapDelta(c.current, c.arg)
		if c.errok {
			if err == nil {
				t.Errorf("applyCapDelta(%v, %q) expected error, got %v", c.current, c.arg, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("applyCapDelta(%v, %q) unexpected error: %v", c.current, c.arg, err)
			continue
		}
		if got != c.want {
			t.Errorf("applyCapDelta(%v, %q) = %v, want %v", c.current, c.arg, got, c.want)
		}
	}
}
