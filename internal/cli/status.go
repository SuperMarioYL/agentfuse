package cli

import (
	"errors"
	"fmt"

	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/SuperMarioYL/agentfuse/internal/ledger"
	"github.com/spf13/cobra"
)

// NewStatusCmd builds `fuse status` — prints current spend for the cwd's project.
func NewStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show today and project-total spend for the current project",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd)
		},
	}
	return cmd
}

func runStatus(cmd *cobra.Command) error {
	projectRoot, cfg, err := budget.LoadFromCwd()
	if err != nil {
		if errors.Is(err, budget.ErrNoConfig) {
			return fmt.Errorf("no .fuse.toml found — run `fuse init` first")
		}
		return err
	}

	ledgerPath, err := ledger.DefaultPath()
	if err != nil {
		return err
	}
	led, err := ledger.Open(ledgerPath)
	if err != nil {
		return err
	}
	defer led.Close()

	today, err := led.Today(projectRoot)
	if err != nil {
		return err
	}
	total, err := led.ProjectTotal(projectRoot)
	if err != nil {
		return err
	}
	// v0.8: fix-status-daily-window-remaining — the enforced cap base is the
	// WINDOWED total (today-only for window="daily", lifetime for
	// window="project"/""), not the lifetime ProjectTotal. Previously remaining
	// and the "cap reached" line used the lifetime sum, so a daily-cap project
	// whose lifetime spend exceeded the (smaller) daily cap falsely reported
	// "cap reached / remaining $0" from day 2 onward even though the daily
	// window had reset at local midnight and the next request would be allowed
	// (ReserveWindowed only counts today's persisted entry for window="daily").
	// The lifetime `project:` line below stays as informational output.
	windowed, err := led.WindowedTotal(projectRoot, cfg.Window)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "project:  %s\n", projectRoot)
	fmt.Fprintf(out, "account:  %s\n", cfg.Account)
	fmt.Fprintf(out, "cap:      $%.2f (%s)\n", cfg.CapUSD, cfg.Window)
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "today:    $%.4f  (%d req, %d in / %d out tokens)\n",
		today.USD, today.Requests, today.TokensIn, today.TokensOut)
	fmt.Fprintf(out, "project:  $%.4f  (%d req)\n", total.USD, total.Requests)

	remaining := cfg.CapUSD - windowed.USD
	if remaining < 0 {
		remaining = 0
	}
	fmt.Fprintf(out, "remaining: $%.4f\n", remaining)
	if windowed.USD >= cfg.CapUSD {
		fmt.Fprintf(cmd.ErrOrStderr(), "agentfuse: cap reached — next request will be denied\n")
	}
	return nil
}
