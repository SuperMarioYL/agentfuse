package budget

import (
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
