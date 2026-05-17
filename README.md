**English** | [简体中文](./README.zh-CN.md)

<p align="center">
  <a href="https://github.com/SuperMarioYL/agentfuse">
    <img src="https://capsule-render.vercel.app/api?type=waving&color=0:7C2D12,100:F59E0B&height=200&section=header&text=AgentFuse&fontSize=70&fontColor=ffffff&desc=The%20local%20kill-switch%20proxy%20for%20coding-agent%20CLIs&descSize=16&descAlignY=68" alt="AgentFuse banner" />
  </a>
</p>

<p align="center">
  <a href="LICENSE"><img alt="License" src="https://img.shields.io/github/license/SuperMarioYL/agentfuse?color=blue" /></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/badge/go-1.24-00ADD8?logo=go&logoColor=white" /></a>
  <a href="https://github.com/SuperMarioYL/agentfuse/releases"><img alt="Latest release" src="https://img.shields.io/github/v/release/SuperMarioYL/agentfuse?include_prereleases&sort=semver&color=7C2D12&label=release" /></a>
  <a href="https://github.com/SuperMarioYL/agentfuse/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/SuperMarioYL/agentfuse/actions/workflows/ci.yml/badge.svg" /></a>
</p>

<p align="center"><em>A single Go binary that hard-caps what your <code>claude</code>, <code>codex</code>, or any coding-agent CLI is allowed to spend — per project, locally, with no daemon and no telemetry.</em></p>

---

## Table of contents

- [Why this exists](#why-this-exists)
- [Install &amp; quickstart](#install--quickstart)
- [Demo](#demo)
- [Configuration](#configuration)
- [Commands](#commands)
- [How it works](#how-it-works)
- [Roadmap](#roadmap)
- [License &amp; contributing](#license--contributing)

---

## Why this exists

Coding-agent CLIs went from "press Enter for each turn" to overnight autonomous loops in under a year, and the billing model has not caught up. A single stray `.env` was enough to route **$187** of agent traffic to the wrong Anthropic account ([the PSA thread that started this repo, 476↑](https://reddit.com/r/ClaudeAI/comments/1tbaq2d/)). Overnight loops chew through Max-plan budgets. The June 15 `--print` reclassification flips scripts that ran for months from "subscription" to "credit-billed."

AgentFuse is the kill-switch that PSA asked for: a tiny local proxy that reads a `.fuse.toml` from your project root and **fails closed** the moment cumulative spend hits the cap. The next request never leaves your machine. No daemon. No cloud. Audit it with `strings` in thirty seconds.

## Install &amp; quickstart

<img align="right" width="48" src="https://raw.githubusercontent.com/tabler/tabler-icons/main/icons/rocket.svg" alt="" />

```bash
# 1. install (Go 1.24+)
go install github.com/SuperMarioYL/agentfuse/cmd/fuse@latest

# 2. drop a $5 cap into your project
cd ~/code/myproj && fuse init

# 3. run your coding-agent CLI through the proxy
fuse run claude
```

<details>
<summary>Sample stdout when the cap fires</summary>

```text
$ fuse run claude
agentfuse: proxy on 127.0.0.1:54021 → api.anthropic.com (account=personal, cap=$5.00 project)
agentfuse: forwarding child stdio …

[claude session prints normally for a while …]

agentfuse: budget exceeded for project /Users/you/code/myproj ($5.02 / $5.00) — raise with: fuse cap +5
claude: API request denied (HTTP 402): budget exceeded. exiting.
```

```text
$ fuse status
project:    /Users/you/code/myproj
account:    personal
cap:        $5.00 (project window)

today:      $0.4317   (12 req, 145020 in / 9802 out tokens)
project:    $5.0231   (318 req)
remaining:  $0.0000   ✗ over cap — next request will be denied
```

</details>

## Demo

> 📼 Demo coming soon — recorded asciinema cast lives at [`assets/README.md`](./assets/README.md). The 30-second cut shows `fuse init` → `fuse run claude` → a runaway loop → cap fires → HTTP 402 → `fuse cap +5` → resume.

## Configuration

A `.fuse.toml` at your project root. AgentFuse walks up from `cwd` to find the nearest one.

| Key       | Type      | Default     | Meaning                                                                 |
| --------- | --------- | ----------- | ----------------------------------------------------------------------- |
| `cap_usd` | `float`   | *required*  | Hard cap in USD. Pre-flight estimates count toward this — single oversized requests cannot sneak in under the line. |
| `window`  | `string`  | `"project"` | `"project"` (cumulative since `fuse init`) or `"daily"` (rolls at midnight UTC). |
| `account` | `string`  | `""`        | Named account from `~/.fuse/accounts.toml`. When set, AgentFuse refuses inbound keys that don't match this account's fingerprint and injects the right one, overriding stray `.env` keys. |

Accounts live separately in `~/.fuse/accounts.toml`:

```toml
[accounts.personal]
provider = "anthropic"
api_key  = "sk-ant-..."

[accounts.work]
provider = "anthropic"
api_key  = "sk-ant-..."
```

## Commands

| Command        | What it does                                                                 |
| -------------- | ---------------------------------------------------------------------------- |
| `fuse init`    | Write a `.fuse.toml` in the current directory (flags: `--cap`, `--account`). |
| `fuse run <cmd>` | Start the local proxy, point the child CLI at `127.0.0.1:<rand>`, exec `<cmd>`, exit when it does. |
| `fuse cap +N` / `=N` / `-N` | Mutate the cap atomically (refuses any change that would land at ≤ 0). |
| `fuse status`  | Print current account, cap, today's spend, project total, and remaining budget. |

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

Everything is local: single binary, no daemon, ledger at `~/.fuse/ledger.db` (bbolt, KV: `(project, day) → {tokens_in, tokens_out, usd, requests}`). The proxy dies with the wrapped child. The binary never talks to anything other than the provider you configured.

## Roadmap

- [x] **m1 — intercept &amp; log.** Local proxy transparently forwards `claude`/`codex` traffic, parses the `usage` block, writes per-cwd token + USD ledger. `fuse status` shows real numbers.
- [ ] **m2 — enforce hard-cap.** `.fuse.toml` parsing + `cap_usd` enforcement. Pre-flight estimate from `prompt_tokens` prevents single-request overshoot. HTTP 402 on deny. `fuse cap ±N` mutates atomically.
- [ ] **m3 — run launcher.** Named accounts in `~/.fuse/accounts.toml`, fingerprint matching, OpenAI provider parity. Stray `.env` keys cannot route traffic to the wrong account.

Out of scope for v0.1: web UI, multi-user / SSO, cloud spend aggregation, providers beyond Anthropic + OpenAI, streaming recompute past the final SSE `usage` event, Slack / email alerts, Windows native (WSL2 only).

## License &amp; contributing

MIT — see [LICENSE](LICENSE). Issues and PRs welcome at [github.com/SuperMarioYL/agentfuse](https://github.com/SuperMarioYL/agentfuse). The fastest way to help right now: try the quickstart on a real overnight loop and file an issue when the kill-switch fires (or fails to).

---

<p align="center"><sub>Generated by the <a href="https://github.com/SuperMarioYL/ai-radar">ai-radar</a> pipeline · scan-2026-05-17-2013 · candidate <code>t7neg04</code></sub></p>
