package ledger

import (
	"path/filepath"
	"sync"
	"testing"
)

func mustOpen(t *testing.T) *Ledger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// TestReserveRejectsOverCap verifies the basic atomic decision: a reservation
// that would push projected spend over the cap is rejected.
func TestReserveRejectsOverCap(t *testing.T) {
	l := mustOpen(t)
	const proj = "/p"
	if _, err := l.Add(proj, 0, 0, 4.50); err != nil {
		t.Fatal(err)
	}
	ok, err := l.Reserve(proj, 5.00, 0.40)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("0.40 estimate on a 4.50 spend / 5.00 cap should be allowed")
	}
	// Now 4.50 persisted + 0.40 reserved = 4.90; a further 0.20 would hit 5.10 > 5.00.
	ok, err = l.Reserve(proj, 5.00, 0.20)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second reserve should be rejected: 4.50 + 0.40 reserved + 0.20 > 5.00")
	}
}

// TestConcurrentReserveDoesNotOvershoot is the race regression: N concurrent
// requests each reserving an estimate must NOT collectively exceed the cap.
// Before the Reserve/CommitDelta fix, all N could read the same under-cap total
// and pass, overshooting by up to (N-1) estimates.
func TestConcurrentReserveDoesNotOvershoot(t *testing.T) {
	l := mustOpen(t)
	const (
		proj     = "/race"
		cap      = 1.00
		estimate = 0.10 // up to 10 should fit under a 1.00 cap
		workers  = 50
	)

	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := l.Reserve(proj, cap, estimate)
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// At most floor(cap/estimate) = 10 may be granted; never more.
	if granted > 10 {
		t.Fatalf("cap overshoot: %d reservations granted, max allowed is 10", granted)
	}
	if granted == 0 {
		t.Fatal("expected at least some reservations to be granted")
	}
}

// TestCommitDeltaReleasesReservation verifies CommitDelta drops the reservation
// and persists the realized cost, so a committed request frees room for the next.
func TestCommitDeltaReleasesReservation(t *testing.T) {
	l := mustOpen(t)
	const proj = "/commit"
	ok, err := l.Reserve(proj, 5.00, 1.00)
	if err != nil || !ok {
		t.Fatalf("reserve failed: ok=%v err=%v", ok, err)
	}
	// Realized usage smaller than the estimate.
	if _, err := l.CommitDelta(proj, 1.00, 100, 50, 0.30); err != nil {
		t.Fatal(err)
	}
	tot, err := l.ProjectTotal(proj)
	if err != nil {
		t.Fatal(err)
	}
	if tot.USD < 0.29 || tot.USD > 0.31 || tot.TokensIn != 100 || tot.TokensOut != 50 {
		t.Fatalf("commit did not persist realized cost: %+v", tot)
	}
	// Reservation should be fully released — a fresh near-cap reserve now succeeds.
	ok, err = l.Reserve(proj, 5.00, 4.60)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("reservation leaked: 0.30 spent should leave room for 4.60")
	}
}

// TestReleaseFreesReservation verifies Release drops a reservation without billing.
func TestReleaseFreesReservation(t *testing.T) {
	l := mustOpen(t)
	const proj = "/release"
	ok, _ := l.Reserve(proj, 1.00, 0.90)
	if !ok {
		t.Fatal("first reserve should succeed")
	}
	l.Release(proj, 0.90)
	// Room is back: a 0.90 reserve succeeds again, and nothing was billed.
	ok, _ = l.Reserve(proj, 1.00, 0.90)
	if !ok {
		t.Fatal("release did not free the reservation")
	}
	tot, _ := l.ProjectTotal(proj)
	if tot.USD != 0 || tot.Requests != 0 {
		t.Fatalf("release must not bill: %+v", tot)
	}
}
