# Changelog

All notable changes to AgentFuse are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions adhere to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.6.0] — 2026-07-27

A quiet cap-correctness/availability release. v0.5.0 closed the gzip
`Accept-Encoding` fail-open defect across the four non-Gemini handlers and
calibrated the unary accuracy harness, but a HIGH fail-open cap-correctness
defect remained in the Gemini JSON-array stream path, and two same-cluster
MEDIUM defects (`Config.Window` silently ignored; the upstream HTTP client had
no timeout) undermined the cap-correctness promise from the fail-closed
direction. v0.6.0 closes all three and continues the v0.3/v0.4/v0.5 quiet
release posture.

### Fixed
- **Gemini JSON-array streamGenerateContent under-billing**
  (`internal/proxy/gemini.go`): `parseGeminiUsage` only unmarshaled a single
  JSON object in its fast path and its SSE line-walker skipped every
  `[`-prefixed line, so a JSON-array `streamGenerateContent` body (one compact
  line `[{...},{...}]` or pretty `[ { ... } ]` — the shape a client gets without
  `alt=sse`) was dropped entirely: in=0, out=0, completionText="" and the
  per-side fallback billed promptTokens +
  `EstimateCompletion(model,"")=100` regardless of real output. A streamed
  Gemini completion with thousands of real output tokens billed 100, so
  realized spend could exceed the cap without tripping it (fail-open) — the
  same property the v0.5.0 gzip fix closes for unary traffic. `parseGeminiUsage`
  now detects a leading `[` and unmarshals the body as a `[]geminiResponse`,
  accumulating candidate text per frame and taking the last frame's
  `usageMetadata`; the SSE line-walker is reached only when the body is neither
  a single object nor an array.
- **`window = "daily"` silently enforced as a lifetime cap**
  (`internal/ledger/bolt.go`): `Config.Window` was accepted by `budget.Load` and
  surfaced in `fuse status` but `Reserve`/`ProjectTotal` never consulted it —
  `projectTotalLocked` summed every persisted day, so a daily cap was enforced
  as a lifetime project cap (over-deny from day 2 onward; a broken-promise
  correctness defect in the fail-closed path). The ledger now exposes
  `ReserveWindowed`/`WindowedTotal` that read only today's entry for
  `window="daily"` and the lifetime total for `"project"`/default; all five
  handlers (anthropic, openai, deepseek, gemini, openai_compat) reserve and
  build the `!allowed` reason off the windowed total. The bare `Reserve` entry
  point still behaves as `window="project"` for backwards compatibility.
- **Upstream HTTP client had no timeout** (`internal/proxy/server.go` + the five
  handlers): all handlers forwarded via `http.DefaultClient` (`Timeout=0`) and
  buffered the whole upstream body, so a stalled upstream held the handler
  goroutine — and its ledger reservation — with no TTL, starving the project
  budget until process restart. The `Server` now carries a shared
  `*http.Client{Timeout: 5*time.Minute}` used by all five handlers, so a stalled
  upstream errors out of the handler and runs the deferred `Release`, returning
  the reservation to the available budget.

## [0.4.0] — 2026-07-17

A correctness-completion release. v0.3.0 shipped the atomic `Reserve`/`CommitDelta`
ledger guard and the tiktoken accuracy harness, but the guard only landed in two
of the five provider handlers and the harness recorded the wrong estimator side.
v0.4.0 finishes the thesis and adds one read-only pricing-drift surface.

### Fixed
- **Cap race still open on 3/5 providers.** The v0.3.0 `fix-ledger-check-add-race`
  milestone introduced `Ledger.Reserve`/`CommitDelta` so the check→forward→add
  sequence is atomic and concurrent requests cannot collectively overshoot the cap,
  but only `anthropic.go` and `openai.go` were converted. `deepseek.go`,
  `gemini.go`, and `openai_compat.go` still used the racy `ProjectTotal → Decide
  → Add` path — so on DeepSeek, Gemini, and any OpenAI-compatible host (Groq,
  Mistral, xAI, Together, OpenRouter) concurrent requests could all pass the cap
  check before any `Add` landed and overshoot by up to (N-1) per-request costs.
  All three handlers now use the same `Reserve → forward → CommitDelta` (+deferred
  `Release`) discipline as the other two.
- **Accuracy harness recorded the wrong estimator side.** `anthropic.go` and
  `openai.go` called `tokens.RecordSample(provider, model,
  tokens.EstimatePrompt(model, completionText), outTok)` — feeding the completion
  text to the *prompt* estimator (which has no +100 round-up) and comparing against
  the upstream *output* token count. The harness is documented as measuring the
  tiktoken estimate for a completion vs the upstream-reported count; the estimator
  the ledger fallback actually bills with is `EstimateCompletion` (rounds UP to the
  nearest 100 tokens). The old call measured a different, structurally-biased-low
  estimator than the cap uses, so the §8 ">25% median error ⇒ redesign" kill
  criterion the harness was built to make evaluable was still not honestly
  measurable. Both handlers now call `EstimateCompletion(model, completionText)`.

### Added
- **`fuse prices --diff`** — read-only price-table drift visibility. Compares the
  merged table (bundled + `~/.fuse/prices.toml` overlay) against the bundled-only
  snapshot and prints **shadowed** entries (your override beats a bundled default —
  flags stale overrides whose bundled price has since moved) and **orphaned**
  entries (your entry has no bundled counterpart — the bundle retired that model).
  Surfaces the two root causes of "wrong cost" bugs the §8 kill criterion deferred
  to v0.4. Zero network; the trust premise is unchanged.

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
