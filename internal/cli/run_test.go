package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runRoot dispatches `fuse <sub> [args...]` against a synthetic root that
// mirrors cmd/fuse/main.go's wiring, returning the captured stdout/stderr and
// the Execute error. RunE on the run subcommand is swapped for a capturing
// closure so we never spawn a child process or touch the ledger.
func runRootCaptureArgs(t *testing.T, args []string) ([]string, error) {
	t.Helper()
	root := &cobra.Command{Use: "fuse", SilenceUsage: true, SilenceErrors: true}
	run := NewRunCmd()
	var got []string
	run.RunE = func(_ *cobra.Command, a []string) error {
		got = a
		return nil
	}
	root.AddCommand(run)
	root.SetArgs(args)
	var buf bytes.Buffer
	root.SetErr(&buf)
	err := root.Execute()
	return got, err
}

// TestRunCmdForwardsFlagsVerbatim is the v0.11 regression for
// fix-run-passthrough-flags-unknown: `run` registers no flags, so before the
// fix cobra/pflag parsed every `--foo`/`-x` token after `run` as a fuse flag
// and aborted "unknown flag" before the child spawned. With
// DisableFlagParsing, every token after `run` reaches RunE verbatim and is
// forwarded to exec.Command(args[0], args[1:]...) untouched.
func TestRunCmdForwardsFlagsVerbatim(t *testing.T) {
	got, err := runRootCaptureArgs(t, []string{
		"run", "claude", "--model", "opus", "--resume", "--foo", "-x", "bar",
	})
	if err != nil {
		t.Fatalf("unexpected error (flag parsing still active?): %v", err)
	}
	want := []string{"claude", "--model", "opus", "--resume", "--foo", "-x", "bar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args not forwarded verbatim: got %v want %v (cobra intercepted a flag?)", got, want)
	}
}

// TestRunCmdForwardsLeadingDashArgs exercises the simpler `fuse run echo --foo`
// shape directly.
func TestRunCmdForwardsLeadingDashArgs(t *testing.T) {
	got, err := runRootCaptureArgs(t, []string{"run", "echo", "--foo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"echo", "--foo"}) {
		t.Fatalf("expected [echo --foo] forwarded verbatim, got %v", got)
	}
}

// TestRunCmdNoArgsReturnsClearError verifies the manual len>=1 guard that
// replaces the old `cobra.MinimumNArgs(1)` validator: `fuse run` with no
// command must return a clear, actionable error rather than panicking on
// args[0] in exec.Command. Exercised against the REAL RunE (no capture swap)
// so the guard in NewRunCmd itself is what's tested.
func TestRunCmdNoArgsReturnsClearError(t *testing.T) {
	root := &cobra.Command{Use: "fuse", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(NewRunCmd())
	root.SetArgs([]string{"run"})
	var buf bytes.Buffer
	root.SetErr(&buf)
	err := root.Execute()
	if err == nil {
		t.Fatal("expected a clear error for `fuse run` with no command, got nil (would panic on args[0])")
	}
	if !strings.Contains(err.Error(), "requires a command") {
		t.Fatalf("error should clearly state a command is required, got: %v", err)
	}
	// Must NOT be the cobra "unknown flag" misfire the fix removes.
	if strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("error is a flag-parse misfire, not the missing-command guard: %v", err)
	}
}

// TestRunCmdHasDisableFlagParsing is a structural guard against regression: if a
// future change re-enables flag parsing on `run`, the passthrough fix is
// silently undone (every `--foo` aborts "unknown flag" before spawn again).
func TestRunCmdHasDisableFlagParsing(t *testing.T) {
	var cmd *cobra.Command = NewRunCmd()
	if !cmd.DisableFlagParsing {
		t.Fatalf("run command must set DisableFlagParsing=true (got false) — `fuse run <cmd> --flag` would abort 'unknown flag'")
	}
	// The Args validator is removed: cobra still honors it under
	// DisableFlagParsing, but the inline len check is the source of truth so a
	// future flag addition can't reintroduce the parse.
	if cmd.Args != nil {
		t.Fatalf("run command must not set an Args validator (got %T) — the inline len(args)<1 guard owns validation", cmd.Args)
	}
}
