package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/spf13/cobra"
)

// NewCapCmd builds `fuse cap <+N | -N | =N>` for the current project.
func NewCapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cap <+N | -N | =N>",
		Short: "Raise, lower, or set the spend cap for the current project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCap(cmd, args[0])
		},
	}
	return cmd
}

func runCap(cmd *cobra.Command, arg string) error {
	projectRoot, cfg, err := budget.LoadFromCwd()
	if err != nil {
		if errors.Is(err, budget.ErrNoConfig) {
			return fmt.Errorf("no .fuse.toml found — run `fuse init` first")
		}
		return err
	}

	newCap, err := applyCapDelta(cfg.CapUSD, arg)
	if err != nil {
		return err
	}
	old := cfg.CapUSD
	cfg.CapUSD = newCap

	dst := filepath.Join(projectRoot, budget.FileName)
	if err := budget.Save(dst, cfg); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "cap_usd: $%.2f → $%.2f (%s)\n", old, newCap, dst)
	return nil
}

// applyCapDelta parses +N / -N / =N (or N) and returns the new cap.
func applyCapDelta(current float64, arg string) (float64, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return 0, fmt.Errorf("cap argument required (e.g. +5 or =10)")
	}
	switch arg[0] {
	case '+':
		n, err := strconv.ParseFloat(strings.TrimSpace(arg[1:]), 64)
		if err != nil {
			return 0, fmt.Errorf("parse delta %q: %w", arg, err)
		}
		return roundedCap(current + n)
	case '-':
		n, err := strconv.ParseFloat(strings.TrimSpace(arg[1:]), 64)
		if err != nil {
			return 0, fmt.Errorf("parse delta %q: %w", arg, err)
		}
		v := current - n
		if v <= 0 {
			return 0, fmt.Errorf("cap would drop to $%.2f — must stay > $0", v)
		}
		return roundedCap(v)
	case '=':
		n, err := strconv.ParseFloat(strings.TrimSpace(arg[1:]), 64)
		if err != nil {
			return 0, fmt.Errorf("parse value %q: %w", arg, err)
		}
		if n <= 0 {
			return 0, fmt.Errorf("cap must be > $0 (got %.2f)", n)
		}
		return roundedCap(n)
	default:
		n, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			return 0, fmt.Errorf("parse value %q: %w", arg, err)
		}
		if n <= 0 {
			return 0, fmt.Errorf("cap must be > $0 (got %.2f)", n)
		}
		return roundedCap(n)
	}
}

// roundedCap converts a candidate cap to the stored, cent-rounded value and
// refuses one that rounding floors to $0.00. The pre-rounding guards above only
// reject values that are already non-positive, so `fuse cap =0.001` (or
// `-4.999`) passed them and returned 0 — a .fuse.toml budget.Load rejects.
// Persisting it would strand the project: init refuses to overwrite, and the
// deny path's own suggested `fuse cap +5` could not load the config either.
func roundedCap(v float64) (float64, error) {
	r := roundCents(v)
	if err := budget.ValidateCapUSD(r); err != nil {
		return 0, fmt.Errorf("cap rounds to $%.2f — %w", r, err)
	}
	return r, nil
}

func roundCents(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}
