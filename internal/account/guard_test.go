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

// TestFingerprintMatchesFullKeyNotPrefix guards the same-prefix/different-suffix
// collision class that a 12-char prefix compare cannot distinguish: real
// same-provider keys share their first FingerprintLen=12 chars (Anthropic
// sk-ant-api03-…, OpenAI sk-proj-…), so two DIFFERENT keys collide on the
// prefix and the guard would forward a stray personal key verbatim instead of
// rewriting it with the managed account's key (fail-open for account routing).
// The existing TestLookupAndMatch masks the defect because its contrived keys
// place distinguishing material inside the first 12 chars.
func TestFingerprintMatchesFullKeyNotPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.toml")
	const managed = "sk-ant-api03-managed-account-key-AAAA"
	body := `
[accounts.work]
provider = "anthropic"
api_key = "` + managed + `"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// A stray personal key with the SAME first 12 chars (sk-ant-api03) but a
	// DIFFERENT suffix must NOT match the managed work account.
	stray := "sk-ant-api03-personal-account-key-ZZZZ"
	if f.FingerprintMatches("work", stray) {
		t.Fatalf("same-prefix/different-suffix key must not match managed account; both share prefix %q", Fingerprint(stray))
	}

	// The legitimate managed key must still match itself exactly.
	if !f.FingerprintMatches("work", managed) {
		t.Fatal("the account's own stored key must still match")
	}

	// Fixture sanity: the collision is real — both keys share the 12-char
	// prefix, so a prefix-only compare would have matched them (the bug).
	if Fingerprint(stray) != Fingerprint(managed) {
		t.Fatalf("test fixture broken: stray and managed keys should share the 12-char prefix, got %q vs %q",
			Fingerprint(stray), Fingerprint(managed))
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
