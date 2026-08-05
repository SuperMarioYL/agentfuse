package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/SuperMarioYL/agentfuse/internal/ledger"

	bolt "go.etcd.io/bbolt"
)

// seedLedgerDay writes a raw (project, day) entry directly into the ledger's
// bolt file, bypassing ledger.Add (which always writes "today"). It mirrors the
// same-package ledger.seedDay test helper; the cli package cannot reach that
// helper, so it re-states the stable on-disk format (bucket "ledger", key
// "<yyyy-mm-dd>|<project>"). Used only to place past-day spend so the status
// daily-window fix is regression-testable across packages.
func seedLedgerDay(t *testing.T, path, project, day string, usd float64) {
	t.Helper()
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("open bolt to seed: %v", err)
	}
	defer db.Close()
	raw, err := json.Marshal(ledger.Entry{USD: usd, Requests: 1})
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("ledger"))
		if err != nil {
			return err
		}
		return b.Put([]byte(day+"|"+project), raw)
	}); err != nil {
		t.Fatalf("seed day %s: %v", day, err)
	}
}

// TestStatusDailyWindowRemainingIsWindowed is the v0.8.0 regression test for
// fix-status-daily-window-remaining: `fuse status` must compute `remaining`
// and the "cap reached" notice from the WINDOWED total (today-only for
// window="daily"), not the LIFETIME ProjectTotal. Before the fix, runStatus used
// led.ProjectTotal for both, so a daily-cap project whose LIFETIME spend
// exceeded the (smaller) daily cap falsely reported "remaining $0 / cap reached"
// from day 2 onward even though the daily window had reset at local midnight
// (ReserveWindowed only counts today's persisted entry) and the next request
// WOULD be allowed.
//
// Discriminating by design: lifetime spend ($6.00) exceeds the $5.00 cap while
// today's daily window is empty ($0). The OLD code printed remaining $0.0000 +
// the cap-reached notice; the fixed code prints remaining $5.0000 and no notice.
// The lifetime "project:" line stays as informational output ($6.0000).
func TestStatusDailyWindowRemainingIsWindowed(t *testing.T) {
	// Isolate the ledger: override HOME so ledger.DefaultPath() lands in a temp
	// dir, and chdir into a temp project dir carrying .fuse.toml (LoadFromCwd
	// walks up from cwd to find it).
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	tmpProject := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpProject); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	if err := budget.Save(filepath.Join(tmpProject, budget.FileName), &budget.Config{
		CapUSD: 5.00,
		Window: "daily",
	}); err != nil {
		t.Fatal(err)
	}

	// Resolve the project root the SAME way runStatus does (LoadFromCwd →
	// FindProjectRoot(os.Getwd())). On macOS os.Getwd() returns the
	// symlink-resolved physical path (/private/var/...) which differs from the
	// raw t.TempDir() value (/var/...); the seed's project key MUST match that
	// resolved string or ProjectTotal/Today won't find the seeded entry.
	projectRoot, _, err := budget.LoadFromCwd()
	if err != nil {
		t.Fatalf("load from cwd: %v", err)
	}

	ledgerPath, err := ledger.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	// Place $6.00 on a PAST local day (two days ago — never today, so the daily
	// window excludes it). Lifetime total ($6.00) > cap ($5.00); today = $0.
	twoDaysAgo := time.Now().AddDate(0, 0, -2).Format("2006-01-02")
	seedLedgerDay(t, ledgerPath, projectRoot, twoDaysAgo, 6.00)

	var out, errw bytes.Buffer
	cmd := NewStatusCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errw)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status execute: %v", err)
	}

	got := out.String()
	// remaining must be the WINDOWED (daily = today = $0) base: $5.00 - $0.00.
	// The OLD code used the lifetime $6.00 -> $0.0000.
	if !strings.Contains(got, "remaining: $5.0000") {
		t.Fatalf("daily-window remaining should be $5.0000 (cap - today, not cap - lifetime); got:\n%s", got)
	}
	// The informational lifetime line must still report the full $6.00.
	if !strings.Contains(got, "project:  $6.0000  (1 req)") {
		t.Fatalf("lifetime project line should still report $6.0000 (1 req); got:\n%s", got)
	}
	// The "cap reached" notice must NOT fire — the daily window reset at local
	// midnight and the next request WILL be allowed. The OLD code printed it
	// (lifetime $6.00 >= cap $5.00).
	if errw.Len() != 0 {
		t.Fatalf("daily window reset: no cap-reached notice expected, got stderr:\n%s", errw.String())
	}
}
