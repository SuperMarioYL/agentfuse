package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitRejectsNonPositiveCap is the v0.15 regression for
// fix-init-cap-nonpositive-unusable-config. `fuse init` is the only writer of
// .fuse.toml that does not validate its cap: budget.Load and `fuse cap` both
// reject cap_usd <= 0, but init happily wrote cap_usd = 0 (or a negative) and
// exited 0. Every later command in that project then failed with
// "cap_usd must be > 0" — including `fuse cap +5`, the documented remediation
// the deny path itself suggests — leaving the user no in-CLI recovery.
//
// A valid cap must still write the file (the guard must not break the happy
// path).
func TestInitRejectsNonPositiveCap(t *testing.T) {
	for _, arg := range []string{"0", "-3"} {
		t.Run("cap="+arg, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			cmd := newInitCmd()
			cmd.SetArgs([]string{"--cap", arg})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("init --cap %s exited 0; every later command will fail on cap_usd must be > 0", arg)
			}
			if !strings.Contains(err.Error(), "cap") {
				t.Errorf("error should name the cap: %v", err)
			}
			// The unusable config must not be left behind: a leftover file is what
			// strands the user (init refuses to overwrite an existing .fuse.toml).
			if _, statErr := os.Stat(filepath.Join(dir, ".fuse.toml")); statErr == nil {
				t.Fatalf("init --cap %s wrote a .fuse.toml that budget.Load rejects", arg)
			}
		})
	}

	t.Run("valid cap still writes", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)

		cmd := newInitCmd()
		cmd.SetArgs([]string{"--cap", "5"})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true

		if err := cmd.Execute(); err != nil {
			t.Fatalf("init --cap 5 failed: %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".fuse.toml")); statErr != nil {
			t.Fatalf("init --cap 5 did not write .fuse.toml: %v", statErr)
		}
	})
}
