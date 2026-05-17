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
	return &Ledger{db: db}, nil
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
