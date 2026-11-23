package budget

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	in := &Config{CapUSD: 7.5, Window: "project", Account: "personal"}
	if err := Save(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if out.CapUSD != 7.5 || out.Account != "personal" {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
}

func TestFindProjectRootWalksUp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, FileName), []byte("cap_usd = 5.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got := FindProjectRoot(deep)
	if got != root {
		t.Fatalf("got %q want %q", got, root)
	}
}

func TestFindProjectRootReturnsEmptyWhenAbsent(t *testing.T) {
	if got := FindProjectRoot(t.TempDir()); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestLoadRejectsZeroCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("cap_usd = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for cap_usd=0")
	}
}

// TestLoadRejectsInvalidWindow guards fix-window-value-unvalidated: the ledger
// treats only the literal "daily" as today-only and falls every other value
// through to the lifetime project total, so a typo (e.g. "daly") must fail at
// load instead of silently enforcing a lifetime cap with no error (over-deny
// from day 2 onward — the opposite of the v0.6/v0.8 window fixes' intent).
func TestLoadRejectsInvalidWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("cap_usd = 5.0\nwindow = \"daly\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal(`expected error for invalid window value "daly"`)
	}
}

// TestLoadAcceptsDailyAndProjectWindows asserts the two documented window
// values still load (and that an empty window defaults to "project").
func TestLoadAcceptsDailyAndProjectWindows(t *testing.T) {
	cases := []struct{ in, want string }{
		{"daily", "daily"},
		{"project", "project"},
		{"", "project"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, FileName)
		body := "cap_usd = 5.0\n"
		if c.in != "" {
			body += fmt.Sprintf("window = %q\n", c.in)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("window=%q: expected load to succeed, got %v", c.in, err)
		}
		if cfg.Window != c.want {
			t.Fatalf("window=%q: got %q want %q", c.in, cfg.Window, c.want)
		}
	}
}

// TestSaveRejectsNonPositiveCap is the v0.16 regression for
// fix-cap-rounds-to-zero-unusable-config (writer half). `fuse cap` is the second
// writer of .fuse.toml, and it rounded a nominally positive cap (e.g. =0.001 or
// -4.999) to $0.00 before saving. Save is the config module's own write choke
// point, so it must refuse any config Load would reject instead of persisting a
// file that strands the project.
func TestSaveRejectsNonPositiveCap(t *testing.T) {
	for _, cap := range []float64{0, -3} {
		dir := t.TempDir()
		path := filepath.Join(dir, FileName)
		if err := Save(path, &Config{CapUSD: cap, Window: "project"}); err == nil {
			t.Fatalf("Save(cap_usd=%v) succeeded; budget.Load rejects that file", cap)
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("Save(cap_usd=%v) left a .fuse.toml behind that Load rejects", cap)
		}
	}
}

// TestLoadRejectsUnknownProvider is the v0.16 regression for
// fix-unknown-provider-silently-unmonitored. `fuse run` routes on Config.Provider
// with a five-case switch and no fallback, so a misspelled provider (e.g.
// "googel") loads without error, sets no provider base URL, and the agent CLI
// then talks straight to the provider — around the proxy, the ledger and the cap.
func TestLoadRejectsUnknownProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	body := "cap_usd = 5.0\nprovider = \"googel\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal(`Load accepted provider = "googel"; the launcher has no route for it`)
	}
	for _, name := range []string{"anthropic", "openai", "gemini", "deepseek", "openai_compat"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name the valid provider %q: %v", name, err)
		}
	}
}

// TestLoadAcceptsEveryRoutableProvider asserts the validated set is exactly what
// internal/cli/run.go routes (plus the empty -> anthropic default).
func TestLoadAcceptsEveryRoutableProvider(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "anthropic"},
		{"anthropic", "anthropic"},
		{"openai", "openai"},
		{"gemini", "gemini"},
		{"deepseek", "deepseek"},
		{"openai_compat", "openai_compat"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		body := "cap_usd = 5.0\n"
		if c.in != "" {
			body += fmt.Sprintf("provider = %q\n", c.in)
		}
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("provider=%q: expected load to succeed, got %v", c.in, err)
		}
		if cfg.Provider != c.want {
			t.Fatalf("provider=%q: got %q want %q", c.in, cfg.Provider, c.want)
		}
	}
}
