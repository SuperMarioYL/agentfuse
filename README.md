# AgentFuse

> **AgentFuse is the local kill-switch proxy that caps per-project spend for coding-agent CLIs.**

A single Go binary that sits between `claude`, `codex`, or any other coding-agent CLI and the upstream API. Reads a `.fuse.toml` from your project root. **Fails closed** when the cap is hit — the next request never leaves your machine.

![status: alpha](https://img.shields.io/badge/status-alpha--v0.1-orange)
![lang: go](https://img.shields.io/badge/go-1.24-00ADD8)
![license: MIT](https://img.shields.io/badge/license-MIT-blue)

---

## Why

Three real failures, three real Reddit threads, in the same week:

| Pain | Thread |
| --- | --- |
| Stray `.env` routed `$187` of agent traffic to the wrong account | r/ClaudeAI — [PSA](https://reddit.com/r/ClaudeAI/comments/1tbaq2d/) (476↑) |
| Overnight agentic loops chewing through Max-plan budgets | r/ClaudeAI — [agentic 24/7](https://reddit.com/r/ClaudeAI/comments/1tcpxi2/) (440↑) |
| `claude --print` reclassified to credit-billed on **June 15** | r/ClaudeAI — [I'm cooked](https://reddit.com/r/ClaudeAI/comments/1tcetsd/) (597↑) |

AgentFuse is the kill-switch the PSA asked for.

## Install → first result in 3 steps

```bash
# 1. install (Go 1.24+)
go install github.com/agentfuse/fuse/cmd/fuse@latest

# 2. drop a config in your project (defaults to $5 cap)
cd ~/code/myproj && fuse init

# 3. run any coding-agent CLI through the proxy
fuse run claude
```

When cumulative spend hits the cap, the next request returns HTTP 402 with a one-line message printed to the agent's stderr:

```
agentfuse: budget exceeded for project /Users/you/code/myproj ($5.02 / $5.00) — raise with: fuse cap +5
```

## Live status

```bash
fuse status

# project:  /Users/you/code/myproj
# account:  personal
# cap:      $5.00 (project)
#
# today:    $0.4317  (12 req, 145020 in / 9802 out tokens)
# project:  $0.4317  (12 req)
# remaining: $4.5683
```

## Raise / lower the cap

```bash
fuse cap +5     # +$5
fuse cap =20    # set to $20
fuse cap -2     # -$2  (refused if it would drop to ≤ $0)
```

## How it works

```
┌─────────────────┐                  ┌───────────────────┐
│  fuse run claude│ ─── exec ──────► │  claude (child)   │
└─────────────────┘                  │  ANTHROPIC_BASE   │
                                     │  → 127.0.0.1:PORT │
                                     └─────────┬─────────┘
                                               │
                                ┌──────────────▼──────────────┐
                                │  AgentFuse proxy            │
                                │  1. account guard           │
                                │  2. pre-flight estimate     │
                                │  3. ledger check vs cap     │
                                │  4. ALLOW → forward         │
                                │     DENY  → HTTP 402        │
                                │  5. parse `usage`, write    │
                                │     ~/.fuse/ledger.db       │
                                └──────────────┬──────────────┘
                                               │
                                       api.anthropic.com
                                       api.openai.com
```

Everything is local:

- **Single binary.** No daemon. The proxy dies with the wrapped child.
- **Ledger at `~/.fuse/ledger.db`** (bbolt, single file, KV: `(project, day) → {tokens_in, tokens_out, usd, requests}`).
- **No telemetry.** This binary never talks to anything other than the provider you configured. `strings` the binary in 30 seconds if you don't trust it.

## Configuration

`.fuse.toml` (in the project root):

```toml
cap_usd = 5.0          # hard cap; pre-flight estimate counts toward this
window  = "project"    # "project" or "daily"
account = "personal"   # named account from ~/.fuse/accounts.toml
```

`~/.fuse/accounts.toml`:

```toml
[accounts.personal]
provider = "anthropic"
api_key  = "sk-ant-..."

[accounts.work]
provider = "anthropic"
api_key  = "sk-ant-..."
```

When `account` is set, AgentFuse:

1. Refuses or rewrites any inbound `x-api-key` (or `Authorization: Bearer …`) whose first 12 chars don't match the named account's fingerprint.
2. Injects the configured key into the child's env, **overriding** stray `.env`-sourced keys.

The original PSA: defused.

## Out of scope for v0.1

- Web UI / dashboard. CLI + stdout only.
- Multi-user / SSO / org policy. (LiteLLM territory.)
- Cloud spend aggregation across hosts.
- Providers beyond Anthropic + OpenAI.
- Streaming-response recompute beyond the final SSE `usage` event.
- Slack / email alerts. Stderr line + exit code.
- Windows native. WSL2 only.

## Provenance

This product was generated from [ai-radar scan `scan-2026-05-17-2013`](../../projects/scan-2026-05-17-2013/), candidate `t7neg04` (AgentFuse). The full plan, market forecast, and go-to-market live in `workspace/projects/scan-2026-05-17-2013/F-plan/winner-t7neg04/`.

## License

MIT — see [LICENSE](LICENSE).
