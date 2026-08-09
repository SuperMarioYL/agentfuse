package budget

import (
	"fmt"
	"os"
	"path/filepath"
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
