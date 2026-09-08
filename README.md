[简体中文](./README.zh-CN.md) · [Website](https://agentfuse.lei6393.com) · [GitHub](https://github.com/SuperMarioYL/agentfuse)

<picture>
  <source media="(max-width: 600px) and (prefers-color-scheme: dark)" srcset="./assets/presentation/hero-mobile-dark.svg">
  <source media="(max-width: 600px)" srcset="./assets/presentation/hero-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="./assets/presentation/hero-dark.svg">
  <img src="./assets/presentation/hero-light.svg" width="960" alt="Hero diagram">
</picture>

# AgentFuse

**Give each agent project a spending boundary**

AgentFuse wraps a command with a local API proxy, checks its configured budget before forwarding requests, and records usage in a local ledger. Inspect the price table before relying on the cost estimate.

## Why use it

An autonomous coding session can make many API calls before you inspect the bill. A project budget makes the next request an explicit decision: the proxy reserves estimated cost, forwards accepted calls and reconciles the returned usage.

- **Project or daily windows** — Choose a cumulative project cap or a window that resets at local midnight.
- **Inspect the estimate** — Read bundled prices, overrides, unknown-model fallbacks and overlay drift without making a provider call.
- **Keep a local ledger** — bbolt stores request counts, token usage and configured dollar costs for status reporting.

## Architecture

<picture>
  <source media="(max-width: 600px) and (prefers-color-scheme: dark)" srcset="./assets/presentation/architecture-mobile-dark.svg">
  <source media="(max-width: 600px)" srcset="./assets/presentation/architecture-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="./assets/presentation/architecture-dark.svg">
  <img src="./assets/presentation/architecture-light.svg" width="960" alt="Architecture diagram">
</picture>

The launcher rewrites provider base URLs for its child. The proxy applies account checks and reserves budget in the bbolt ledger, then sends accepted requests through the provider handler. Usage from the response reconciles the reservation; local token estimation covers supported missing-usage cases. The price resolver owns model-prefix lookup and override precedence.

| Component | Responsibility |
| --- | --- |
| `Launcher` | internal/cli/run.go |
| `Proxy` | account and provider handlers |
| `Ledger` | atomic budget reservations |
| `Price resolver` | bundled table and overrides |

## Install and quickstart

Go 1.24 or newer. The example needs no credentials.

```bash
git clone https://github.com/SuperMarioYL/agentfuse.git
cd agentfuse
go build -o bin/fuse ./cmd/fuse
```

The supplied Go example runs the production bundled-price resolver for gpt-4o and an unlisted model. Token counts are explicit example inputs, not a live response.

```bash
go run ./examples/presentation
```

## Recorded demo

<picture>
  <source media="(max-width: 600px) and (prefers-color-scheme: dark)" srcset="./assets/presentation/process-mobile-dark.svg">
  <source media="(max-width: 600px)" srcset="./assets/presentation/process-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="./assets/presentation/process-dark.svg">
  <img src="./assets/presentation/process-light.svg" width="960" alt="Process diagram">
</picture>

Two local model lookups; configured costs only, no API billing measurement.

```text
Bundled price snapshot: 2026-05
openai/gpt-4o explicit=true input_per_1k=0.0025 output_per_1k=0.0100
  configured cost for 1000 input + 100 output tokens: $0.0035
openai/unlisted-demo-model explicit=false input_per_1k=0.0050 output_per_1k=0.0200
  configured cost for 1000 input + 100 output tokens: $0.0070
Scope: local price-table arithmetic; no provider request or measured spend.
```

The complete command and output are recorded in [docs/demo-results.json](./docs/demo-results.json). Inputs and reproduction code are included in the repository.

## Usage

After building, add this repository’s bin directory to PATH or use the binary’s full path. In a chosen project, init creates .fuse.toml; cap changes its limit; status reads the ledger. For a configured API client, fuse run <command> starts the proxy and exits with the child. A named account requires its matching account configuration.

```bash
cd /path/to/your/project
fuse init --cap 5
fuse prices --check openai/gpt-4o --diff
fuse status
fuse cap +5
```

## Configuration

The nearest .fuse.toml owns cap_usd, window (project/daily), account, provider and optional upstream_url. openai_compat requires an upstream URL. ~/.fuse/accounts.toml holds named account credentials; ~/.fuse/prices.toml overrides model rates; ~/.fuse/ledger.db holds usage. The bundled price snapshot is historical and never updates itself from the network.

```toml
cap_usd = 5.0
window = "project"
provider = "openai"
account = ""
```

## Integrations and responsibilities

<picture>
  <source media="(max-width: 600px) and (prefers-color-scheme: dark)" srcset="./assets/presentation/integrations-mobile-dark.svg">
  <source media="(max-width: 600px)" srcset="./assets/presentation/integrations-mobile-light.svg">
  <source media="(prefers-color-scheme: dark)" srcset="./assets/presentation/integrations-dark.svg">
  <img src="./assets/presentation/integrations-light.svg" width="960" alt="Integrations diagram">
</picture>

The provider handlers translate request and response shapes; your selected client must honor the base URL configured by the launcher. AgentFuse accounts for traffic that passes through its proxy. The local price table is separate from the provider invoice.

| Route | Implemented role |
| --- | --- |
| Anthropic | Messages API handler |
| OpenAI | Chat Completions and Responses |
| Gemini | GenerateContent handlers |
| DeepSeek / compatible | OpenAI-shaped API routes |
| TOML + bbolt | configuration and local ledger |

## Limits and next steps

- Budget decisions depend on configured rates, estimated output and supported usage parsing; they do not guarantee the final provider bill.
- The recorded example exercises price resolution only; it does not test live proxy traffic or account credentials.
- Calls that bypass the proxy are outside its ledger and budget.

The current CLI includes multi-provider routing, budget reservations, price overrides and drift inspection. Future work should improve measured estimator accuracy and client compatibility. Cloud aggregation, SSO, alerts and a web management UI are outside the implemented CLI. See CHANGELOG.md for release details.

## License and contributions

See [LICENSE](./LICENSE). When reporting an issue, include a minimal input, the command, and the observed output.
