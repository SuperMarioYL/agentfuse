package prices

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadBundledKnownKeys(t *testing.T) {
	tbl, err := LoadBundled()
	if err != nil {
		t.Fatalf("load bundled: %v", err)
	}
	cases := []struct {
		provider, model string
		wantIn          float64
	}{
		{"anthropic", "claude-3-5-sonnet", 0.003},
		{"openai", "gpt-4o-mini", 0.00015},
		{"gemini", "gemini-1.5-flash", 0.000075},
		{"deepseek", "deepseek-chat", 0.00027},
		{"openai_compat", "llama-3.1-70b", 0.00059},
	}
	for _, tc := range cases {
		got, ok := tbl.Lookup(tc.provider, tc.model)
		if !ok {
			t.Errorf("expected hit for %s/%s, got fallback", tc.provider, tc.model)
			continue
		}
		if got.InputUSDPer1K != tc.wantIn {
			t.Errorf("%s/%s input: got %v want %v",
				tc.provider, tc.model, got.InputUSDPer1K, tc.wantIn)
		}
	}
}

func TestLongestPrefixWins(t *testing.T) {
	tbl, err := LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	// Suffixed real-world model name should land on the longest matching key.
	got, ok := tbl.Lookup("anthropic", "claude-3-5-sonnet-20241022")
	if !ok {
		t.Fatal("expected hit")
	}
	if got.InputUSDPer1K != 0.003 || got.OutputUSDPer1K != 0.015 {
		t.Fatalf("unexpected entry: %+v", got)
	}
}

func TestUnknownFallsBack(t *testing.T) {
	tbl, err := LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := tbl.Lookup("anthropic", "not-a-real-model-xyz")
	if ok {
		t.Fatalf("expected fallback, got %+v", got)
	}
	if got != FallbackEntry {
		t.Fatalf("expected FallbackEntry, got %+v", got)
	}
}

func TestUserOverrideWins(t *testing.T) {
	dir := t.TempDir()
	override := filepath.Join(dir, "prices.toml")
	// User says "actually claude-3-5-sonnet is now $0.01 in / $0.05 out".
	body := []byte(`[anthropic.claude-3-5-sonnet]
input_usd_per_1k  = 0.01
output_usd_per_1k = 0.05
`)
	if err := os.WriteFile(override, body, 0o600); err != nil {
		t.Fatal(err)
	}
	tbl, err := Load(override)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := tbl.Lookup("anthropic", "claude-3-5-sonnet")
	if !ok {
		t.Fatal("expected hit")
	}
	if got.InputUSDPer1K != 0.01 || got.OutputUSDPer1K != 0.05 {
		t.Fatalf("user override didn't win: %+v", got)
	}
	// Untouched bundled key must still be present.
	if _, ok := tbl.Lookup("openai", "gpt-4o"); !ok {
		t.Fatal("bundled key was wiped by user override")
	}
}

func TestLoadMissingUserFileOK(t *testing.T) {
	tbl, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("missing user file should not error: %v", err)
	}
	if _, ok := tbl.Lookup("openai", "gpt-4o"); !ok {
		t.Fatal("bundle should still be loaded when user file is absent")
	}
}

func TestCostHelper(t *testing.T) {
	tbl, err := LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	// 1000 in @ $0.003/1k = $0.003; 500 out @ $0.015/1k = $0.0075. Total: $0.0105.
	got := tbl.Cost("anthropic", "claude-3-5-sonnet", 1000, 500)
	want := 0.0105
	if got < want-1e-6 || got > want+1e-6 {
		t.Fatalf("got %v want %v", got, want)
	}
}
