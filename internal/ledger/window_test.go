package ledger

import (
	"encoding/json"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// seedDay writes a raw per-(project, day) entry directly into the bolt bucket,
// bypassing Add (which always writes "today"). Lets the day-boundary test
// place spend on past days the daily window must ignore.
func seedDay(t *testing.T, l *Ledger, project, day string, usd float64) {
	t.Helper()
	err := l.db.Update(func(tx *bolt.Tx) error {
		raw, err := json.Marshal(Entry{USD: usd, Requests: 1})
		if err != nil {
			return err
		}
		return tx.Bucket(bucketName).Put(dayKey(project, day), raw)
	})
	if err != nil {
		t.Fatalf("seed day %s: %v", day, err)
	}
}

// TestWindowedCapResetsAcrossDayBoundary is the v0.6.0 regression test for the
// Config.Window="daily" silently-ignored defect. With only past-day spend
// seeded (today is empty), a "daily" cap must IGNORE every prior day and allow
// the request (the cap resets each day), while a "project" cap must count every
// persisted day and deny (it does not reset). Before v0.6.0 both windows used
// ProjectTotal, so a "daily" cap was enforced as a lifetime cap (over-deny from
// day 2 onward — a broken-promise correctness defect in the fail-closed path).
//
// Deterministic by design: today is never written, so the daily window reads an
// empty today regardless of the wall clock; the daily/project contrast depends
// only on the past-day entries, which are placed on explicit day strings.
func TestWindowedCapResetsAcrossDayBoundary(t *testing.T) {
	l := mustOpen(t)

	// Two past days — neither is "today", so the daily window must exclude both
	// while the project window must sum them.
	twoDaysAgo := time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02")
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	const (
		dailyWindow   = "daily"
		projectWindow = "project"
		dailyProj      = "/win/daily"
		projectProj    = "/win/project"
		capUSD         = 5.00
		estimate       = 0.50
		pastSpendA     = 2.00 // two days ago
		pastSpendB     = 2.60 // yesterday
		pastTotal      = pastSpendA + pastSpendB // 4.60
	)

	for _, proj := range []string{dailyProj, projectProj} {
		seedDay(t, l, proj, twoDaysAgo, pastSpendA)
		seedDay(t, l, proj, yesterday, pastSpendB)
	}

	// --- daily window: resets across the day boundary ---
	dailyTot, err := l.WindowedTotal(dailyProj, dailyWindow)
	if err != nil {
		t.Fatal(err)
	}
	if dailyTot.USD != 0 {
		t.Fatalf("daily window should count only today (empty => $0), got $%.2f — past-day spend leaked in",
			dailyTot.USD)
	}
	ok, err := l.ReserveWindowed(dailyProj, dailyWindow, capUSD, estimate)
	if err != nil || !ok {
		t.Fatalf("daily cap should reset across the day boundary and allow: ok=%v err=%v (over-deny defect — past spend should be ignored)",
			ok, err)
	}
	// Release so the in-flight reservation can't leak into a later assertion.
	l.Release(dailyProj, estimate)

	// --- project window: does NOT reset across the day boundary ---
	projTot, err := l.WindowedTotal(projectProj, projectWindow)
	if err != nil {
		t.Fatal(err)
	}
	if projTot.USD != pastTotal {
		t.Fatalf("project window should sum every persisted day ($%.2f), got $%.2f",
			pastTotal, projTot.USD)
	}
	ok, err = l.ReserveWindowed(projectProj, projectWindow, capUSD, estimate)
	if err != nil {
		t.Fatalf("project reserve errored: %v", err)
	}
	if ok {
		t.Fatalf("project cap must NOT reset: past spend $%.2f should be counted and the $%.2f reserve denied (projected $%.2f > cap $%.2f)",
			pastTotal, estimate, pastTotal+estimate, capUSD)
	}
}

// TestWindowedTotalDefaultsToProject confirms an empty/unknown window string
// falls through to the lifetime project total (backwards compat with v0.5.0
// configs that omit window, and with the bare Reserve entry point).
func TestWindowedTotalDefaultsToProject(t *testing.T) {
	l := mustOpen(t)
	const proj = "/win/default"
	seedDay(t, l, proj, time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"), 3.00)

	tot, err := l.WindowedTotal(proj, "")
	if err != nil {
		t.Fatal(err)
	}
	if tot.USD != 3.00 {
		t.Fatalf("empty window should default to project total ($3.00), got $%.2f", tot.USD)
	}
	// The bare v0.3.0 Reserve entry point must still behave as window="project".
	if _, err := l.Add(proj, 0, 0, 0); err != nil { // ensure today also has spend
		t.Fatal(err)
	}
	// past $3.00 + today ~$0 => projected with a $0.50 estimate is $3.50 <= $5.00
	ok, err := l.Reserve(proj, 5.00, 0.50)
	if err != nil || !ok {
		t.Fatalf("bare Reserve (project window) should allow: ok=%v err=%v", ok, err)
	}
}
