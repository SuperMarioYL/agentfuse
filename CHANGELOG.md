# Changelog

All notable changes to AgentFuse are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions adhere to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] — 2026-05-17

First runnable scaffold. Milestones m1 + m2 + m3 from `mvp_plan.md` all land in
the same release because they form one user-visible product (intercept + cap +
account routing) and shipping them separately would dilute the kill-switch
demo GIF.

### Added

#### m1 — intercept and log
- `fuse run <cmd>` launches a local HTTP proxy on `127.0.0.1:<rand>` and `exec`s
  the wrapped child with `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL` patched in.
- Anthropic and OpenAI handlers forward to upstream, parse the `usage` block on
  the response, and write `(tokens_in, tokens_out, usd, requests)` into a
  bbolt-backed ledger at `~/.fuse/ledger.db` keyed by `(project_root, day)`.
- `fuse status` prints today and project-total spend for the current `cwd`'s
  project.

#### m2 — enforce hard-cap
- `.fuse.toml` schema (`cap_usd`, `window`, `account`) parsed by
  `internal/budget`.
- Pre-flight cost estimate (`prompt_tokens × input_price + completion_estimate
  × output_price`) is added to current ledger before the request leaves the
  proxy. If the projected spend exceeds `cap_usd`, the proxy returns HTTP 402
  with a JSON body the agent CLI surfaces verbatim, and an `agentfuse:` line
  is printed to stderr.
- `fuse cap +N | -N | =N` atomically rewrites `.fuse.toml` (write-to-tmp +
  rename).

#### m3 — run launcher with account routing
- `~/.fuse/accounts.toml` maps named accounts to API keys.
- When `.fuse.toml` specifies `account = "<name>"`, the proxy rewrites any
  inbound `x-api-key` (Anthropic) or `Authorization: Bearer …` (OpenAI) whose
  fingerprint (first 12 chars) doesn't match — so a stray `.env`-sourced key
  for a different account can't bill the wrong place.
- `fuse run` also injects the named account's key into the child's env,
  overriding `.env`-sourced keys.

### Infrastructure
- `go.mod` pinned to Go 1.24, cobra 1.8, bbolt 1.3, toml 1.4.
- Unit tests for `budget`, `ledger`, `account`, `cli/cap`, and integration
  tests for the Anthropic handler covering forward, fail-closed, and
  account-rewrite paths.
- GitHub Actions CI runs `go vet`, `go build`, and `go test ./...`.

### Known limitations (out of scope for 0.1, see `mvp_plan.md` §6)
- No streaming-response cost recompute beyond the final SSE `usage` event.
- No fine-grained per-tool quotas.
- No Windows native build (WSL2 only).
- No telemetry. Ever. (See positioning.)
