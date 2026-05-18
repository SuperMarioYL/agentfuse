// Package prices loads the AgentFuse price table: a (provider, model) -> per-1k
// USD lookup used by the budget enforcer when computing exact cost from a
// post-response usage block (or a tiktoken-estimated equivalent).
//
// The bundled snapshot ships embedded in the binary. Users override per-machine
// by editing ~/.fuse/prices.toml — user keys win, missing keys fall back to the
// bundle, missing both falls back to a conservative high-price entry so the
// kill-switch errs toward DENY.
package prices

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Entry is the per-(provider, model) per-1k pricing.
type Entry struct {
	InputUSDPer1K  float64 `toml:"input_usd_per_1k"`
	OutputUSDPer1K float64 `toml:"output_usd_per_1k"`
}

// Table is the in-memory price book. Keys are lowercase
// "<provider>/<model-prefix>".
type Table struct {
	entries map[string]Entry
}

// FallbackEntry is the price used when no provider+model match is found. It is
// deliberately on the high end so unknown models err toward DENY rather than
// silently slip past the cap.
var FallbackEntry = Entry{InputUSDPer1K: 0.005, OutputUSDPer1K: 0.020}

// rawFile mirrors the on-disk TOML shape, [provider.model] -> Entry.
type rawFile map[string]map[string]Entry

// LoadBundled parses only the embedded snapshot. Useful for tests.
func LoadBundled() (*Table, error) {
	return loadFromBytes(bundledTOML)
}

// DefaultUserPath returns ~/.fuse/prices.toml. The file is optional.
func DefaultUserPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".fuse")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "prices.toml"), nil
}

// Load merges the bundled snapshot with the user override at userPath. If
// userPath is empty or does not exist, only the bundle is loaded. User keys
// always win over bundled keys.
func Load(userPath string) (*Table, error) {
	t, err := loadFromBytes(bundledTOML)
	if err != nil {
		return nil, fmt.Errorf("load bundled prices: %w", err)
	}
	if userPath == "" {
		return t, nil
	}
	data, err := os.ReadFile(userPath)
	if err != nil {
		if os.IsNotExist(err) {
			return t, nil
		}
		return nil, fmt.Errorf("read %s: %w", userPath, err)
	}
	if err := t.mergeBytes(data); err != nil {
		return nil, fmt.Errorf("merge %s: %w", userPath, err)
	}
	return t, nil
}

// LoadDefault is Load(DefaultUserPath()).
func LoadDefault() (*Table, error) {
	p, err := DefaultUserPath()
	if err != nil {
		// Treat HOME lookup failure as bundle-only — never network, never panic.
		return LoadBundled()
	}
	return Load(p)
}

func loadFromBytes(data []byte) (*Table, error) {
	t := &Table{entries: map[string]Entry{}}
	if err := t.mergeBytes(data); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Table) mergeBytes(data []byte) error {
	var raw rawFile
	if err := toml.Unmarshal(data, &raw); err != nil {
		return err
	}
	for provider, models := range raw {
		for model, entry := range models {
			t.entries[indexKey(provider, model)] = entry
		}
	}
	return nil
}

func indexKey(provider, model string) string {
	return strings.ToLower(provider) + "/" + strings.ToLower(model)
}

// Lookup returns the Entry for the longest model-prefix match under provider.
// The match is case-insensitive. If nothing matches, FallbackEntry is returned
// with ok=false so callers can log if they want.
func (t *Table) Lookup(provider, model string) (Entry, bool) {
	p := strings.ToLower(provider)
	m := strings.ToLower(model)
	var bestEntry Entry
	bestLen := -1
	for k, e := range t.entries {
		slash := strings.IndexByte(k, '/')
		if slash < 0 {
			continue
		}
		if k[:slash] != p {
			continue
		}
		mp := k[slash+1:]
		if !strings.HasPrefix(m, mp) {
			continue
		}
		if len(mp) > bestLen {
			bestLen = len(mp)
			bestEntry = e
		}
	}
	if bestLen < 0 {
		return FallbackEntry, false
	}
	return bestEntry, true
}

// Cost returns USD for an (input, output) token pair under the (provider,
// model) lookup. Falls back to FallbackEntry when missing — the caller does
// NOT need to inspect ok separately to get a safe answer.
func (t *Table) Cost(provider, model string, inputTokens, outputTokens int) float64 {
	e, _ := t.Lookup(provider, model)
	return float64(inputTokens)*e.InputUSDPer1K/1000.0 +
		float64(outputTokens)*e.OutputUSDPer1K/1000.0
}

// All returns a copy of the in-memory entries map, keyed by "provider/model".
// Intended for tests and `fuse status --prices` style introspection.
func (t *Table) All() map[string]Entry {
	out := make(map[string]Entry, len(t.entries))
	for k, v := range t.entries {
		out[k] = v
	}
	return out
}
