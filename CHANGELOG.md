# Changelog

All notable changes to AgentFuse are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions adhere to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] — 2026-06-20

A fail-closed correctness pass. v0.2.0 widened the wedge to five provider
families, but the streaming paths under-billed: a streamed Anthropic or OpenAI
response was parsed with a single `json.Unmarshal` of the whole SSE body, which
fails, so the usage/ledger block was skipped and the request billed **$0** — the
cap never fired on streamed spend, defeating the product's core "never under-bill,
fail-closed at the cap" guarantee. This release fixes that and four related
cap-critical defects, then adds two diagnostics that make the two still-open §8
kill criteria measurable.

### Fixed

- **Anthropic streaming under-billing** (`internal/proxy/anthropic.go`): the
  handler now detects an SSE body and walks the `data:` event frames
  (`message_start` for input tokens, `message_delta` for output tokens,
  `content_block_delta` for the completion text), taking the real usage instead
  of billing $0. When no usage block is present at all, it falls back to a local
  tiktoken estimate before touching the ledger — matching the Gemini/DeepSeek/
  OpenAI-compat handlers.
- **OpenAI streaming under-billing** (`internal/proxy/openai.go`): reuses the
  DeepSeek SSE-frame parser (identical OpenAI-compat wire shape) to read the
  final-frame usage from streamed Chat Completions, with a tiktoken fallback when
  usage is absent.
- **Partial-usage fallback** (`deepseek.go`, `openai_compat.go`, `gemini.go`):
  the tiktoken fallback now runs per-side. Previously it only fired when BOTH the
  input and output token counts were zero, so a truncated stream that reported
  only `prompt_tokens` billed the completion as 0 output tokens. Each side is now
  estimated independently when missing.
- **Check-then-spend race** (`internal/ledger/bolt.go`): added an atomic
  `Reserve` / `CommitDelta` / `Release` discipline. Each handler now reserves its
  estimate inside one critical section that re-reads the project total plus all
  in-flight reservations and rejects if the cap would be exceeded; the reservation
  is reconciled with realized usage after the response. Previously the cap check
  and the ledger `Add` were separate locked ops, so N concurrent requests (coding
  agents fan out many) could all read the same under-cap total and collectively
  overshoot the cap.
- **v0.1 static-map fallback price** (`anthropic.go`, `openai.go`): both handlers
  now route cap-critical cost through `CostFromUsageWithProvider`, so the
  user-overridable price table and its conservative `FallbackEntry` govern the
  cost of unknown models, instead of the cheaper static-map fallback that could
  under-price a genuinely expensive new model and overshoot the true cap.

### Added

- **Tiktoken accuracy harness** (`internal/tokens/accuracy.go`): records the
  tiktoken estimate alongside the upstream-reported usage whenever both are
  present on the same response, and computes the median signed/absolute
  percentage error — making the §8 "median error > 25% → redesign the estimator"
  kill criterion evaluable for the first time. Measurement only; the estimator
  math is unchanged.
- **`fuse prices` introspection subcommand** (`internal/cli/prices.go`): a
  read-only diagnostic that prints the effective resolved price table (bundled
  snapshot + `~/.fuse/prices.toml` overlay), the bundled snapshot date, the
  conservative `FallbackEntry`, which `--check` models resolve to that fallback,
  and the accuracy-harness median error. **No network** — it is not a remote
  price-fetcher and not `fuse prices update`; those stay out of scope to preserve
  the trust premise.

## [0.2.0] — 2026-05-18

Single milestone `m4_v2_widen` from `mvp_plan_v0.2.0.md`. Adds three new
provider handlers, a local tokenizer fallback for streams that omit the
upstream `usage` block, and an externalized price table the user can override
without recompiling.

### Added

#### m4 — widen the wedge to five provider families
- **Gemini handler** (`internal/proxy/gemini.go`): forwards
  `:generateContent` and `:streamGenerateContent` to
  `generativelanguage.googleapis.com` (or a user-supplied `upstream_url`),
  parses `usageMetadata` on both unary and SSE-stream responses, and rewrites
  the `?key=` query param + `x-goog-api-key` header when an account guard is
  configured.
- **DeepSeek handler** (`internal/proxy/deepseek.go`): OpenAI-shape wire
  format with one twist — DeepSeek emits `usage` on the final SSE frame even
  when the client did NOT pass `stream_options.include_usage = true`. The
  handler prefers that block when present and falls back to tiktoken when
  absent.
- **OpenAI-compat catch-all** (`internal/proxy/openai_compat.go`): routed via
  `provider = "openai_compat"` + a required `upstream_url` in `.fuse.toml`.
  Designed for Groq / Mistral / xAI / Together / OpenRouter and any future
  OpenAI-shape host. Falls through to the tiktoken fallback whenever upstream
  omits `usage` (which is the common case on Groq + Together streams).

#### Local tokenizer fallback
- New `internal/tokens` package wraps `github.com/pkoukk/tiktoken-go`.
  `EstimatePrompt` returns the BPE-encoded prompt size; `EstimateCompletion`
  does the same for the completion and **rounds UP to the nearest 100
  tokens** — under-billing is unacceptable for a kill-switch, an over-estimate
  by a few percent is the price of safety.

#### Externalized price table
- New `internal/prices` package + `assets/prices.toml`. The bundled snapshot
  is embedded into the binary via `//go:embed`; the user can override any key
  by dropping `~/.fuse/prices.toml`. User keys always win, missing keys fall
  back to the bundle, and missing-in-both falls back to a conservative
  high-tier `FallbackEntry` so unknown models err toward DENY rather than
  silently slip past the cap.
- Initial table covers Anthropic, OpenAI, Gemini, DeepSeek, and OpenAI-compat
  aliases (Llama 3.1 / 3.3, Mistral Large, Mixtral, Grok-2, Qwen 2.5).

#### Configuration + launcher
- `.fuse.toml` schema extended with `provider` (defaults to `"anthropic"` for
  v0.1 configs) and an optional `upstream_url`.
- `fuse run` now sets `GEMINI_API_BASE` / `GOOGLE_API_BASE`,
  `DEEPSEEK_API_BASE`, or an `openai_compat`-routed `OPENAI_BASE_URL`
  depending on the configured provider — wrapping any of Aider, Cline,
  Continue, or a curl-loop transparently.
- Example configs: `examples/.fuse.toml.gemini`,
  `examples/.fuse.toml.deepseek`, `examples/.fuse.toml.openai-compat`.

### Tests
- `internal/proxy/gemini_test.go`, `deepseek_test.go`,
  `openai_compat_test.go` (request → upstream stub → ledger row).
- `internal/tokens/tiktoken_test.go` (round-up rule + encoding selection).
- `internal/prices/config_test.go` (bundle load, longest-prefix match, user
  override merge).

### Infrastructure
- Added `github.com/pkoukk/tiktoken-go v0.1.7` to `go.mod`.

### Known limitations (still out of scope per `mvp_plan_v0.2.0.md` §6)
- No remote price-fetcher — users edit `~/.fuse/prices.toml` by hand.
- No web UI or hosted dashboard.
- No per-tool fine-grained quotas.
- No Windows native build (WSL2 only).

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
