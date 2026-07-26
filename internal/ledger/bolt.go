package ledger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Entry is the per-(project, day) aggregate.
type Entry struct {
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	USD       float64 `json:"usd"`
	Requests  int     `json:"requests"`
}

// Ledger is a bbolt-backed KV store keyed by (project, day).
type Ledger struct {
	db *bolt.DB
	mu sync.Mutex
	// reserved tracks in-flight, not-yet-committed estimated spend per project.
	// It exists so the budget check and the eventual Add are atomic with respect
	// to one another: Reserve re-reads the persisted total + adds the in-flight
	// reservations inside the same critical section, so N concurrent requests can
	// no longer all read the same under-cap total and collectively overshoot the
	// cap. Keyed by project root.
	reserved map[string]float64
}

var bucketName = []byte("ledger")

// DefaultPath returns ~/.fuse/ledger.db.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".fuse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "ledger.db"), nil
}

// Open the bbolt file. Caller must Close.
func Open(path string) (*Ledger, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open ledger %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(bucketName)
		return e
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Ledger{db: db, reserved: map[string]float64{}}, nil
}

// Close the underlying bolt db.
func (l *Ledger) Close() error {
	if l == nil || l.db == nil {
		return nil
	}
	return l.db.Close()
}

func key(project string, day time.Time) []byte {
	return []byte(day.UTC().Format("2006-01-02") + "|" + project)
}

func dayKey(project, day string) []byte {
	return []byte(day + "|" + project)
}

// Add atomically increments today's entry.
func (l *Ledger) Add(project string, tokensIn, tokensOut int, usd float64) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.addLocked(project, tokensIn, tokensOut, usd)
}

// addLocked is Add without taking the lock. Caller MUST hold l.mu.
func (l *Ledger) addLocked(project string, tokensIn, tokensOut int, usd float64) (Entry, error) {
	k := key(project, time.Now())
	var out Entry
	err := l.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		raw := b.Get(k)
		if raw != nil {
			if err := json.Unmarshal(raw, &out); err != nil {
				return err
			}
		}
		out.TokensIn += tokensIn
		out.TokensOut += tokensOut
		out.USD += usd
		out.Requests++
		enc, err := json.Marshal(out)
		if err != nil {
			return err
		}
		return b.Put(k, enc)
	})
	return out, err
}

// Today returns the entry for project today (or zero if none).
func (l *Ledger) Today(project string) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.todayLocked(project)
}

// todayLocked returns today's persisted entry for project (or zero if none).
// Caller MUST hold l.mu.
func (l *Ledger) todayLocked(project string) (Entry, error) {
	k := key(project, time.Now())
	var out Entry
	err := l.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketName).Get(k)
		if raw == nil {
			return nil
		}
		return json.Unmarshal(raw, &out)
	})
	return out, err
}

// ProjectTotal sums every day for the given project.
func (l *Ledger) ProjectTotal(project string) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.projectTotalLocked(project)
}

// projectTotalLocked sums every persisted day for project. Caller MUST hold
// l.mu. It deliberately does NOT include in-flight reservations — those are
// added by Reserve, which needs the persisted-only total as its base.
func (l *Ledger) projectTotalLocked(project string) (Entry, error) {
	suffix := []byte("|" + project)
	var sum Entry
	err := l.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketName).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if !hasSuffix(k, suffix) {
				continue
			}
			var e Entry
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			sum.TokensIn += e.TokensIn
			sum.TokensOut += e.TokensOut
			sum.USD += e.USD
			sum.Requests += e.Requests
		}
		return nil
	})
	return sum, err
}

// WindowedTotal returns the spend base for the cap check according to window:
// "daily" => only today's persisted entry; "project"/""/default => every
// persisted day for the project. It does NOT include in-flight reservations.
// Handlers use it for the !allowed Decide reason so the projected spend shown
// to the user matches the window ReserveWindowed actually enforced. Before
// v0.6.0 the handlers always used ProjectTotal, so a window="daily" cap was
// reported and enforced as a lifetime project cap (over-deny from day 2).
func (l *Ledger) WindowedTotal(project, window string) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.windowedTotalLocked(project, window)
}

// windowedTotalLocked is the locked variant of WindowedTotal. Caller MUST hold
// l.mu. "daily" reads only today's entry; anything else falls through to the
// lifetime project total.
func (l *Ledger) windowedTotalLocked(project, window string) (Entry, error) {
	if window == "daily" {
		return l.todayLocked(project)
	}
	return l.projectTotalLocked(project)
}

// Reserve atomically performs the check-then-spend decision: under one lock it
// re-reads the persisted project total, adds every in-flight reservation for the
// same project, and rejects (ok=false) if total+inflight+estimateUSD would
// exceed capUSD. On success it records the reservation and returns ok=true; the
// caller MUST later call exactly one of CommitDelta (with the realized cost) or
// Release (to drop the reservation without billing, e.g. on an upstream error).
//
// This closes the read-then-Add race in the handlers: previously each request
// read ProjectTotal, decided, forwarded, then Add'ed — with no lock spanning the
// decision, N concurrent requests could all pass the cap check before any Add
// landed and collectively overshoot the cap by up to (N-1) per-request costs.
// Reserve atomically performs the check-then-spend decision using the LIFETIME
// project total as the cap base (window="project"). Retained for backwards
// compatibility with the v0.3.0 callers/tests; new callers should use
// ReserveWindowed so Config.Window ("daily"|"project") is honored.
func (l *Ledger) Reserve(project string, capUSD, estimateUSD float64) (bool, error) {
	return l.ReserveWindowed(project, "project", capUSD, estimateUSD)
}

// ReserveWindowed is the window-aware atomic check-then-spend: under one lock
// it reads the cap base for window ("daily" => only today's persisted entry;
// "project"/""/default => every persisted day for the project), adds every
// in-flight reservation for the same project, and rejects (ok=false) if
// base+inflight+estimateUSD would exceed capUSD. On success it records the
// reservation and returns ok=true; the caller MUST later call exactly one of
// CommitDelta (with the realized cost) or Release.
//
// Before v0.6.0 Reserve always used the lifetime project total, so a
// window="daily" cap was enforced as a lifetime project cap (over-deny from
// day 2 onward) — a broken-promise correctness defect in the fail-closed path.
func (l *Ledger) ReserveWindowed(project, window string, capUSD, estimateUSD float64) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	persisted, err := l.windowedTotalLocked(project, window)
	if err != nil {
		return false, err
	}
	projected := persisted.USD + l.reserved[project] + estimateUSD
	if projected > capUSD {
		return false, nil
	}
	l.reserved[project] += estimateUSD
	return true, nil
}

// Release drops a previously-Reserved estimate without billing it. Use it when
// the upstream call failed (non-2xx / transport error) so the reservation does
// not leak and starve later requests. The reservation floors at zero.
func (l *Ledger) Release(project string, estimateUSD float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reserved[project] -= estimateUSD
	if l.reserved[project] <= 0 {
		delete(l.reserved, project)
	}
}

// CommitDelta reconciles a reservation with the realized usage: it drops the
// reserved estimate and persists the actual (tokensIn, tokensOut, usd) via the
// same atomic increment Add uses, all under one lock. Pass the SAME estimateUSD
// that was handed to Reserve so the reservation is fully released.
func (l *Ledger) CommitDelta(project string, estimateUSD float64, tokensIn, tokensOut int, usd float64) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reserved[project] -= estimateUSD
	if l.reserved[project] <= 0 {
		delete(l.reserved, project)
	}
	return l.addLocked(project, tokensIn, tokensOut, usd)
}

// GetDay returns the entry for an explicit project+day (yyyy-mm-dd).
func (l *Ledger) GetDay(project, day string) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out Entry
	err := l.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketName).Get(dayKey(project, day))
		if raw == nil {
			return nil
		}
		return json.Unmarshal(raw, &out)
	})
	return out, err
}

func hasSuffix(b, suf []byte) bool {
	if len(b) < len(suf) {
		return false
	}
	for i := range suf {
		if b[len(b)-len(suf)+i] != suf[i] {
			return false
		}
	}
	return true
}
