package prices

import "sort"

// DriftReport describes price-table drift between the merged table (bundled +
// user overlay) and the bundled-only snapshot. `fuse prices --diff` prints it so
// the two root causes of "wrong cost" bugs are visible: a user override shadowing
// a bundled entry whose bundled price has since moved (stale override), and a user
// entry with no bundled counterpart (the bundle dropped that model — orphaned).
//
// This is read-only introspection — it makes drift visible, it does NOT fetch or
// rewrite anything. The trust premise (no network) is preserved.
type DriftReport struct {
	// Shadowed lists entries present in BOTH the merged table and the bundle
	// where the user overlay changed the price versus the bundled default.
	// Order: by key, ascending (deterministic for CLI output).
	Shadowed []ShadowedEntry
	// Orphaned lists entries present in the merged table but absent from the
	// bundle — a user override for a model the binary no longer ships a default
	// price for. Often a stale model name the bundle retired.
	// Order: by key, ascending (deterministic for CLI output).
	Orphaned []OrphanedEntry
}

// ShadowedEntry pairs the bundled price with the user-overridden (merged) price
// for one "provider/model" key.
type ShadowedEntry struct {
	Key          string // "provider/model"
	BundledPrice Entry
	MergedPrice  Entry
}

// OrphanedEntry is a user-only entry with no bundled counterpart.
type OrphanedEntry struct {
	Key   string // "provider/model"
	Price Entry
}

// Diff compares the receiver (the merged table — bundled + user overlay) against
// the bundled-only snapshot and returns a DriftReport.
//
// A merged entry is Shadowed when the same key exists in the bundle with a
// different price (user overrode a bundled default). A merged entry is Orphaned
// when it has no bundled counterpart (user added an entry the bundle does not know).
// Entries identical to the bundle are neither — they are the bundle unchanged.
//
// Either table may be nil; the result is then empty (no panic).
func (t *Table) Diff(bundled *Table) DriftReport {
	var report DriftReport
	if t == nil || bundled == nil {
		return report
	}
	for k, merged := range t.entries {
		bEntry, ok := bundled.entries[k]
		if !ok {
			report.Orphaned = append(report.Orphaned, OrphanedEntry{Key: k, Price: merged})
			continue
		}
		if bEntry != merged {
			report.Shadowed = append(report.Shadowed, ShadowedEntry{
				Key:          k,
				BundledPrice: bEntry,
				MergedPrice:  merged,
			})
		}
	}
	// Deterministic ordering for stable CLI output.
	sort.Slice(report.Shadowed, func(i, j int) bool {
		return report.Shadowed[i].Key < report.Shadowed[j].Key
	})
	sort.Slice(report.Orphaned, func(i, j int) bool {
		return report.Orphaned[i].Key < report.Orphaned[j].Key
	})
	return report
}
