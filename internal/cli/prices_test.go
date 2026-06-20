package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func runPricesCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := NewPricesCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("prices cmd failed: %v", err)
	}
	return buf.String()
}

func TestPricesPrintsTableAndSnapshot(t *testing.T) {
	out := runPricesCmd(t)
	if !strings.Contains(out, "bundled snapshot:") {
		t.Errorf("missing snapshot date line:\n%s", out)
	}
	if !strings.Contains(out, "effective table") {
		t.Errorf("missing effective table:\n%s", out)
	}
	if !strings.Contains(out, "fallback entry:") {
		t.Errorf("missing fallback entry line:\n%s", out)
	}
	if !strings.Contains(out, "estimator accuracy:") {
		t.Errorf("missing accuracy line:\n%s", out)
	}
}

func TestPricesCheckFlagsFallback(t *testing.T) {
	// A made-up model under a real provider has no explicit price → FALLBACK.
	out := runPricesCmd(t, "--check", "anthropic/claude-nonexistent-9000")
	if !strings.Contains(out, "FALLBACK") {
		t.Errorf("expected FALLBACK flag for unknown model:\n%s", out)
	}
}

// guard: NewPricesCmd returns a *cobra.Command named "prices".
func TestPricesCmdIdentity(t *testing.T) {
	var c *cobra.Command = NewPricesCmd()
	if c.Name() != "prices" {
		t.Fatalf("command name = %q, want prices", c.Name())
	}
}
