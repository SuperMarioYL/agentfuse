package account

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprintTruncates(t *testing.T) {
	got := Fingerprint("sk-ant-abc123XXXXXXXXXX")
	if len(got) != FingerprintLen {
		t.Fatalf("expected len %d, got %d (%q)", FingerprintLen, len(got), got)
	}
}

func TestFingerprintShortKey(t *testing.T) {
	got := Fingerprint("short")
	if got != "short" {
		t.Fatalf("short key should pass through, got %q", got)
	}
}

func TestLookupAndMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.toml")
	body := `
[accounts.personal]
provider = "anthropic"
api_key = "sk-ant-PERSONAL-aaaaaaaaaaaa"

[accounts.work]
provider = "anthropic"
api_key = "sk-ant-WORK-bbbbbbbbbbbb"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.FingerprintMatches("personal", "sk-ant-PERSONAL-aaaaaaaaaaaa") {
		t.Fatal("personal key should match itself")
	}
	if f.FingerprintMatches("personal", "sk-ant-WORK-bbbbbbbbbbbb") {
		t.Fatal("wrong-account key must not match")
	}
}

func TestLoadMissingFileReturnsEmpty(t *testing.T) {
	f, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 0 {
		t.Fatalf("expected empty, got %+v", f.Accounts)
	}
}
