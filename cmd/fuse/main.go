package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/SuperMarioYL/agentfuse/internal/cli"
	"github.com/spf13/cobra"
)

var version = "0.15.0"

func main() {
	root := &cobra.Command{
		Use:   "fuse",
		Short: "Local kill-switch proxy that caps per-project spend for coding-agent CLIs",
		Long: `AgentFuse intercepts requests from coding-agent CLIs (Claude Code, Codex) and
fails closed when a project's spend cap is reached. Configuration lives in a
.fuse.toml at the project root; the ledger lives at ~/.fuse/ledger.db.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.AddCommand(newInitCmd())
	root.AddCommand(cli.NewRunCmd())
	root.AddCommand(cli.NewCapCmd())
	root.AddCommand(cli.NewStatusCmd())
	root.AddCommand(cli.NewPricesCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func newInitCmd() *cobra.Command {
	var capUSD float64
	var account string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a .fuse.toml in the current directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			// v0.15: fix-init-cap-nonpositive-unusable-config — init is the only
			// writer of .fuse.toml that did not validate its cap. `fuse init --cap 0`
			// (or a negative) exited 0 and wrote a config budget.Load rejects, so
			// every later command in that project failed with "cap_usd must be > 0"
			// — including `fuse cap +5`, the remediation the deny path itself
			// suggests — leaving no in-CLI recovery. Validate before writing.
			if err := budget.ValidateCapUSD(capUSD); err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			dst := filepath.Join(cwd, ".fuse.toml")
			if _, err := os.Stat(dst); err == nil {
				return fmt.Errorf(".fuse.toml already exists at %s", dst)
			}
			cfg := budget.Config{CapUSD: capUSD, Window: "project", Account: account}
			if err := budget.Save(dst, &cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (cap=$%.2f, account=%q)\n", dst, capUSD, account)
			return nil
		},
	}
	cmd.Flags().Float64Var(&capUSD, "cap", 5.0, "spend cap in USD for this project")
	cmd.Flags().StringVar(&account, "account", "personal", "named account to route through")
	return cmd
}
