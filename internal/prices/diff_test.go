package prices

import "testing"

// TestDiffShadowsAndOrphans asserts Table.Diff surfaces the two drift classes the
// §8 kill criterion defers to v0.4: a user override shadowing a bundled entry
// (stale-override risk) and a user entry with no bundled counterpart (orphaned /
// stale model). The bundled table has [gemini.gemini-1.5-pro] and [gemini.gemini-2.0-flash];
// the merged table overrides gemini-1.5-pro's price and adds an unknown
// gemini-9.9-nope entry. Diff must yield exactly one Shadowed + one Orphaned.
func TestDiffShadowsAndOrphans(t *testing.T) {
	bundled := &Table{entries: map[string]Entry{
		"gemini/gemini-1.5-pro":   {InputUSDPer1K: 0.00125, OutputUSDPer1K: 0.005},
		"gemini/gemini-2.0-flash": {InputUSDPer1K: 0.0001, OutputUSDPer1K: 0.0004},
	}}
	merged := &Table{entries: map[string]Entry{
		"gemini/gemini-1.5-pro":   {InputUSDPer1K: 0.002, OutputUSDPer1K: 0.008},   // overridden -> shadowed
		"gemini/gemini-2.0-flash": {InputUSDPer1K: 0.0001, OutputUSDPer1K: 0.0004}, // identical -> neither
		"gemini/gemini-9.9-nope":  {InputUSDPer1K: 0.5, OutputUSDPer1K: 1.0},       // user-only -> orphaned
	}}

	report := merged.Diff(bundled)
	if len(report.Shadowed) != 1 {
		t.Fatalf("expected 1 shadowed entry, got %d: %+v", len(report.Shadowed), report.Shadowed)
	}
	if report.Shadowed[0].Key != "gemini/gemini-1.5-pro" {
		t.Fatalf("shadowed key wrong: %s", report.Shadowed[0].Key)
	}
	if report.Shadowed[0].BundledPrice.InputUSDPer1K != 0.00125 ||
		report.Shadowed[0].MergedPrice.InputUSDPer1K != 0.002 {
		t.Fatalf("shadowed price pair wrong: bundled=%+v merged=%+v",
			report.Shadowed[0].BundledPrice, report.Shadowed[0].MergedPrice)
	}
	if len(report.Orphaned) != 1 {
		t.Fatalf("expected 1 orphaned entry, got %d: %+v", len(report.Orphaned), report.Orphaned)
	}
	if report.Orphaned[0].Key != "gemini/gemini-9.9-nope" {
		t.Fatalf("orphaned key wrong: %s", report.Orphaned[0].Key)
	}
}

// TestDiffNoDriftWhenMergedEqualsBundled asserts Diff returns empty when the merged
// table is identical to the bundle (no overrides, no user-only entries).
func TestDiffNoDriftWhenMergedEqualsBundled(t *testing.T) {
	bundled := &Table{entries: map[string]Entry{
		"deepseek/deepseek-chat": {InputUSDPer1K: 0.00027, OutputUSDPer1K: 0.0011},
	}}
	merged := &Table{entries: map[string]Entry{
		"deepseek/deepseek-chat": {InputUSDPer1K: 0.00027, OutputUSDPer1K: 0.0011},
	}}
	report := merged.Diff(bundled)
	if len(report.Shadowed) != 0 || len(report.Orphaned) != 0 {
		t.Fatalf("expected no drift, got shadowed=%d orphaned=%d",
			len(report.Shadowed), len(report.Orphaned))
	}
}

// TestDiffNilSafe asserts neither a nil receiver nor a nil bundled arg panics.
func TestDiffNilSafe(t *testing.T) {
	var nil1 *Table
	bundled := &Table{entries: map[string]Entry{"a/b": {}}}
	if r := nil1.Diff(bundled); len(r.Shadowed) != 0 || len(r.Orphaned) != 0 {
		t.Fatal("nil receiver must yield empty report")
	}
	merged := &Table{entries: map[string]Entry{"a/b": {}}}
	if r := merged.Diff(nil); len(r.Shadowed) != 0 || len(r.Orphaned) != 0 {
		t.Fatal("nil bundled must yield empty report")
	}
}

// TestDiffAgainstLoadBundledSmoke is an integration check that Diff runs against
// the real bundled snapshot (merged==bundled -> no drift).
func TestDiffAgainstLoadBundledSmoke(t *testing.T) {
	b, err := LoadBundled()
	if err != nil {
		t.Fatalf("LoadBundled: %v", err)
	}
	report := b.Diff(b)
	if len(report.Shadowed) != 0 || len(report.Orphaned) != 0 {
		t.Fatalf("bundled vs itself must have no drift: shadowed=%d orphaned=%d",
			len(report.Shadowed), len(report.Orphaned))
	}
}
