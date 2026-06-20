package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/SuperMarioYL/agentfuse/internal/prices"
	"github.com/SuperMarioYL/agentfuse/internal/tokens"
	"github.com/spf13/cobra"
)

// NewPricesCmd builds `fuse prices` — a READ-ONLY introspection/diagnostic
// subcommand. It prints the effective resolved price table (bundled snapshot +
// ~/.fuse/prices.toml user overlay), the bundled snapshot date, which requested
// models fall back to the conservative FallbackEntry, and the tiktoken-vs-usage
// accuracy-harness median error.
//
// It is NOT a remote price-fetcher and is NOT `fuse prices update` — those stay
// out of scope to preserve the "this binary never talks to anything but the
// upstream you point it at" trust premise. This command performs zero network.
func NewPricesCmd() *cobra.Command {
	var lookups []string
	cmd := &cobra.Command{
		Use:   "prices",
		Short: "Show the effective price table, snapshot date, and estimator accuracy (read-only)",
		Long: `prices prints the price table AgentFuse resolved at startup: the bundled
snapshot deep-merged with your ~/.fuse/prices.toml overrides (user keys win).

It also reports the bundled snapshot date, flags any --check models that resolve
to the conservative FallbackEntry (no explicit price found), and surfaces the
median tiktoken-vs-upstream error from the accuracy harness.

This command makes NO network calls. To change prices, edit ~/.fuse/prices.toml.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPrices(cmd, lookups)
		},
	}
	cmd.Flags().StringSliceVar(&lookups, "check", nil,
		"model(s) to resolve and flag if they hit FallbackEntry (e.g. --check anthropic/claude-opus-5)")
	return cmd
}

func runPrices(cmd *cobra.Command, lookups []string) error {
	out := cmd.OutOrStdout()

	userPath, _ := prices.DefaultUserPath()
	table, err := prices.Load(userPath)
	if err != nil {
		return fmt.Errorf("load price table: %w", err)
	}

	fmt.Fprintf(out, "bundled snapshot: %s\n", prices.SnapshotDate)
	if userPath != "" {
		fmt.Fprintf(out, "user overlay:     %s\n", userPath)
	}
	fmt.Fprintf(out, "fallback entry:   $%.4f in / $%.4f out per 1k (used for unlisted models)\n",
		prices.FallbackEntry.InputUSDPer1K, prices.FallbackEntry.OutputUSDPer1K)
	fmt.Fprintln(out, "")

	// Effective resolved table, sorted by "provider/model".
	all := table.All()
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fmt.Fprintf(out, "effective table (%d entries):\n", len(keys))
	fmt.Fprintf(out, "  %-40s %12s %12s\n", "provider/model", "in/1k", "out/1k")
	for _, k := range keys {
		e := all[k]
		fmt.Fprintf(out, "  %-40s %12.4f %12.4f\n", k, e.InputUSDPer1K, e.OutputUSDPer1K)
	}

	// --check: resolve specific models and flag FallbackEntry hits.
	if len(lookups) > 0 {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "model resolution:")
		for _, q := range lookups {
			provider, model := splitProviderModel(q)
			e, ok := table.Lookup(provider, model)
			status := "resolved"
			if !ok {
				status = "FALLBACK (no explicit price — billed at conservative FallbackEntry)"
			}
			fmt.Fprintf(out, "  %-40s in=%.4f out=%.4f  %s\n",
				provider+"/"+model, e.InputUSDPer1K, e.OutputUSDPer1K, status)
		}
	}

	// Accuracy harness median error (populated only after live requests in this
	// process; on a cold CLI invocation N will be 0).
	st := tokens.Stats()
	fmt.Fprintln(out, "")
	if st.N == 0 {
		fmt.Fprintf(out, "estimator accuracy: no samples yet "+
			"(needs live requests where upstream reported usage; threshold is %.0f%% median abs error)\n",
			tokens.AccuracyThresholdPct)
	} else {
		flag := "OK"
		if st.ExceedsThreshold {
			flag = fmt.Sprintf("OVER %.0f%% THRESHOLD — estimator should be redesigned", tokens.AccuracyThresholdPct)
		}
		fmt.Fprintf(out, "estimator accuracy: n=%d  median signed=%.1f%%  median abs=%.1f%%  [%s]\n",
			st.N, st.MedianSignedPct, st.MedianAbsPct, flag)
	}
	return nil
}

// splitProviderModel splits a "provider/model" query. When no slash is present
// the whole string is treated as the model under an empty provider (Lookup will
// then return FallbackEntry, which is the honest answer).
func splitProviderModel(q string) (string, string) {
	if i := strings.IndexByte(q, '/'); i >= 0 {
		return q[:i], q[i+1:]
	}
	return "", q
}
