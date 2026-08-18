package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/SuperMarioYL/agentfuse/internal/account"
	"github.com/SuperMarioYL/agentfuse/internal/budget"
	"github.com/SuperMarioYL/agentfuse/internal/ledger"
	"github.com/SuperMarioYL/agentfuse/internal/proxy"
	"github.com/spf13/cobra"
)

// NewRunCmd builds `fuse run <cmd> [args...]`.
//
// v0.11: fix-run-passthrough-flags-unknown — `run` registers no flags of its
// own, so cobra/pflag parsed every `--foo`/`-x` token after `run` as a fuse
// flag and aborted "unknown flag" before the child spawned (e.g. `fuse run
// claude --model opus`, `fuse run codex --resume`, `fuse run echo --foo` all
// exited 1 and never launched the agent). DisableFlagParsing makes cobra hand
// us every token after `run` verbatim; the manual len check replaces the old
// `cobra.MinimumNArgs(1)` validator (which cobra still runs under
// DisableFlagParsing, but the inline guard is the source of truth so a future
// flag addition can't reintroduce the parse). All tokens are forwarded to
// exec.Command(args[0], args[1:]...) untouched.
func NewRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "run <cmd> [args...]",
		Short:              "Run a coding-agent CLI through the local kill-switch proxy",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("fuse run requires a command to wrap (e.g. `fuse run claude`); `run` parses no flags — every token after `run` is forwarded to the child verbatim")
			}
			return runWrapped(cmd, args)
		},
	}
	return cmd
}

func runWrapped(cmd *cobra.Command, args []string) error {
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

	acctPath, err := account.DefaultPath()
	if err != nil {
		return err
	}
	accts, err := account.Load(acctPath)
	if err != nil {
		return err
	}

	srv := proxy.New(projectRoot, cfg, led, accts)
	if err := srv.Start(); err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	}()

	fmt.Fprintf(cmd.ErrOrStderr(),
		"agentfuse: proxy on %s — project=%s cap=$%.2f account=%q\n",
		srv.Addr(), projectRoot, cfg.CapUSD, cfg.Account)

	// Build child env. We always set the v0.1 anthropic+openai env vars so an
	// agent that hard-codes one of them keeps working, then we layer the
	// provider-specific overrides on top based on the active .fuse.toml.
	env := os.Environ()
	env = setEnv(env, "ANTHROPIC_BASE_URL", srv.AnthropicBaseURL())
	env = setEnv(env, "OPENAI_BASE_URL", srv.OpenAIBaseURL())
	env = setEnv(env, "OPENAI_API_BASE", srv.OpenAIBaseURL())

	switch cfg.Provider {
	case "gemini":
		env = setEnv(env, "GEMINI_API_BASE", srv.GeminiBaseURL())
		env = setEnv(env, "GOOGLE_API_BASE", srv.GeminiBaseURL())
	case "deepseek":
		env = setEnv(env, "DEEPSEEK_API_BASE", srv.DeepSeekBaseURL())
		// Many DeepSeek clients reuse the OpenAI_BASE_URL knob — point it at
		// the DeepSeek-specific handler so we don't accidentally route
		// DeepSeek traffic into the OpenAI handler.
		env = setEnv(env, "OPENAI_BASE_URL", srv.DeepSeekBaseURL())
		env = setEnv(env, "OPENAI_API_BASE", srv.DeepSeekBaseURL())
	case "openai_compat":
		env = setEnv(env, "OPENAI_BASE_URL", srv.OpenAICompatBaseURL())
		env = setEnv(env, "OPENAI_API_BASE", srv.OpenAICompatBaseURL())
	}

	// If the named account is known, inject its key — overriding any stray
	// .env-sourced key. The proxy will still re-check.
	if cfg.Account != "" {
		if a, err := accts.Lookup(cfg.Account); err == nil && a.APIKey != "" {
			env = setEnv(env, "ANTHROPIC_API_KEY", a.APIKey)
			env = setEnv(env, "OPENAI_API_KEY", a.APIKey)
			// v0.10: fix-run-gemini-managed-account-key-not-injected — the gemini
			// CLI / google-generativeai SDK reads its key from GEMINI_API_KEY or
			// GOOGLE_API_KEY, not from the anthropic/openai knobs set above. With
			// a clean environment the child had no key: it either errored
			// client-side before reaching the proxy, or fell back to Application
			// Default Credentials (Authorization: Bearer), which the gemini
			// account guard (gemini.go:68-86) does not inspect — so the configured
			// account's key was never used (broken managed-account promise for the
			// gemini provider). Inject both so the child authenticates with the
			// managed key and sends ?key=/x-goog-api-key the guard then verifies.
			if cfg.Provider == "gemini" {
				env = setEnv(env, "GEMINI_API_KEY", a.APIKey)
				env = setEnv(env, "GOOGLE_API_KEY", a.APIKey)
			}
		}
	}

	// Spawn child.
	child := exec.Command(args[0], args[1:]...)
	child.Env = env
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr

	if err := child.Start(); err != nil {
		return fmt.Errorf("start %s: %w", args[0], err)
	}

	// Forward signals so Ctrl-C reaches the child.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()

	for {
		select {
		case s := <-sigs:
			_ = child.Process.Signal(s)
		case err := <-done:
			signal.Stop(sigs)
			if err == nil {
				return nil
			}
			if ee, ok := err.(*exec.ExitError); ok {
				os.Exit(ee.ExitCode())
			}
			return err
		}
	}
}

func setEnv(env []string, key, val string) []string {
	prefix := key + "="
	out := env[:0]
	for _, kv := range env {
		if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+val)
}
