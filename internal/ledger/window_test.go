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

	// Two past LOCAL days — neither is today (local), so the daily window
	// must exclude both while the project window must sum them. v0.7: seeded in
	// the process LOCAL timezone (not UTC) so the day strings match the
	// local-date bucket key the ledger now uses; seeding UTC dates would flake
	// near midnight in non-UTC zones where today's local date == "yesterday UTC".
	twoDaysAgo := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")

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

// TestDailyWindowResetsAtLocalMidnight is the v0.7.0 regression test for the
// daily-window-UTC-reset defect (fix-daily-window-utc-reset). key() previously
// built the daily bucket with day.UTC().Format("2006-01-02"), so a
// window="daily" cap reset at UTC midnight rather than the user's local
// midnight for any non-UTC user, and `fuse status`'s "today:" line reported
// the UTC day's spend. With the .UTC() dropped, the bucket keys on the process
// (user) LOCAL date.
//
// Deterministic by design: a fake clock (setNow) is pinned to two moments that
// sit on the SAME UTC date but OPPOSITE local dates — late evening local
// 2026-07-30 23:00 (== 2026-07-31 04:00 UTC) and just after local midnight
// 2026-07-31 00:30 (== 2026-07-31 05:30 UTC), in a fixed UTC-5 zone. Under the
// old .UTC() key both moments share the 2026-07-31 UTC date, so spend placed
// at "yesterday late" would NOT reset across local midnight (the defect).
// Under the fix, "today" is the local 2026-07-31 bucket, which is empty — the
// daily cap reset at local midnight. The project window must NOT reset (it
// sums every persisted day regardless of the date key).
func TestDailyWindowResetsAtLocalMidnight(t *testing.T) {
	l := mustOpen(t)
	// Fixed non-UTC zone (UTC-5) so local midnight and UTC midnight differ.
	loc := time.FixedZone("TEST-UTC-5", -5*60*60)
	// Late evening local 2026-07-30 23:00 == 2026-07-31 04:00 UTC: under the
	// old .UTC() key this Add lands in the 2026-07-31 UTC bucket.
	yesterdayLate := time.Date(2026, 7, 30, 23, 0, 0, 0, loc)
	// Just after local midnight 2026-07-31 00:30 == 2026-07-31 05:30 UTC:
	// under the old .UTC() key this is STILL the 2026-07-31 UTC bucket, so the
	// spend above would NOT reset across local midnight (the defect). Under
	// the fix, "today" is the local 2026-07-31 bucket, which is empty.
	todayEarly := time.Date(2026, 7, 31, 0, 30, 0, 0, loc)

	const (
		proj      = "/tz/daily"
		capUSD    = 5.00
		pastSpend = 4.60 // persisted on the prior LOCAL day
		estimate  = 0.50
	)

	// Place spend while the clock reads late-evening local 2026-07-30.
	restore := l.setNow(yesterdayLate)
	if _, err := l.Add(proj, 0, 0, pastSpend); err != nil {
		t.Fatal(err)
	}
	// Sanity: while still on the prior local day, the daily window sees it.
	yest, err := l.WindowedTotal(proj, "daily")
	if err != nil {
		t.Fatal(err)
	}
	if yest.USD != pastSpend {
		t.Fatalf("sanity: daily window at yesterday-late should be $%.2f, got $%.2f", pastSpend, yest.USD)
	}
	restore()

	// Advance the clock across LOCAL midnight to 2026-07-31 00:30 local.
	restore = l.setNow(todayEarly)
	defer restore()

	// --- daily window: resets at LOCAL midnight ---
	tot, err := l.WindowedTotal(proj, "daily")
	if err != nil {
		t.Fatal(err)
	}
	if tot.USD != 0 {
		t.Fatalf("daily window should reset at LOCAL midnight: spend on local 2026-07-30 must be excluded on local 2026-07-31, got $%.2f — the bucket still keys on UTC", tot.USD)
	}
	// `fuse status` "today" tracks the LOCAL day for the same reason (Today
	// goes through the same key as the daily window). Under the old UTC key it
	// would report the UTC day's spend ($4.60), not the local day's ($0).
	today, err := l.Today(proj)
	if err != nil {
		t.Fatal(err)
	}
	if today.USD != 0 {
		t.Fatalf("Today should track the LOCAL day ($0 on local 2026-07-31), got $%.2f — status 'today' keys on UTC", today.USD)
	}
	// A daily cap reserve must now succeed: the window reset, so $0 persisted
	// + reserved + estimate stays under the cap. Under the old UTC key the
	// daily window would read $4.60 and this reserve would be denied
	// (projected $5.10 > $5.00) — the over-deny-from-day-2 defect.
	ok, err := l.ReserveWindowed(proj, "daily", capUSD, estimate)
	if err != nil || !ok {
		t.Fatalf("daily cap should allow after local-midnight reset: ok=%v err=%v", ok, err)
	}
	l.Release(proj, estimate)

	// --- project window: does NOT reset across LOCAL midnight ---
	projTot, err := l.WindowedTotal(proj, "project")
	if err != nil {
		t.Fatal(err)
	}
	if projTot.USD != pastSpend {
		t.Fatalf("project window should sum every persisted day ($%.2f), got $%.2f — project cap must NOT reset at local midnight", pastSpend, projTot.USD)
	}
	// Same past spend + estimate must now DENY on the project window: the
	// project cap does not reset, so the prior day's $4.60 is still counted.
	ok, err = l.ReserveWindowed(proj, "project", capUSD, estimate)
	if err != nil {
		t.Fatalf("project reserve errored: %v", err)
	}
	if ok {
		t.Fatalf("project cap must NOT reset across local midnight: $%.2f persisted + $%.2f estimate should be denied (projected $%.2f > cap $%.2f)",
			pastSpend, estimate, pastSpend+estimate, capUSD)
	}
}
