[English](./README.md) | **简体中文**

<p align="center">
  <a href="https://github.com/SuperMarioYL/agentfuse">
    <img src="https://capsule-render.vercel.app/api?type=waving&color=0:7C2D12,100:F59E0B&height=200&section=header&text=AgentFuse&fontSize=70&fontColor=ffffff&desc=%E7%BB%99%E7%BC%96%E7%A0%81%E6%99%BA%E8%83%BD%E4%BD%93%20CLI%20%E8%A3%85%E4%B8%80%E4%B8%AA%E6%9C%AC%E5%9C%B0%E7%9A%84%E7%86%94%E6%96%AD%E5%99%A8&descSize=14&descAlignY=68" alt="AgentFuse banner" />
  </a>
</p>

<p align="center">
  <a href="LICENSE"><img alt="License" src="https://img.shields.io/github/license/SuperMarioYL/agentfuse?color=blue" /></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/badge/go-1.24-00ADD8?logo=go&logoColor=white" /></a>
  <a href="https://github.com/SuperMarioYL/agentfuse/releases"><img alt="Latest release" src="https://img.shields.io/github/v/release/SuperMarioYL/agentfuse?include_prereleases&sort=semver&color=7C2D12&label=release" /></a>
  <a href="CHANGELOG.md"><img alt="Version" src="https://img.shields.io/badge/version-v0.2.0-F59E0B" /></a>
  <a href="https://github.com/SuperMarioYL/agentfuse/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/SuperMarioYL/agentfuse/actions/workflows/ci.yml/badge.svg" /></a>
  <a href="#v020--多供应商"><img alt="Providers" src="https://img.shields.io/badge/providers-Anthropic%20%7C%20OpenAI%20%7C%20Gemini%20%7C%20DeepSeek%20%7C%20OpenAI--compat-7C2D12" /></a>
</p>

<p align="center">
  <img src="https://readme-typing-svg.demolab.com?font=JetBrains+Mono&size=18&pause=1500&color=F59E0B&center=true&width=620&lines=Anthropic+%7C+OpenAI+%7C+Gemini+%7C+DeepSeek+%7C+OpenAI-compat;%E4%B8%80%E4%B8%AA%E4%BA%8C%E8%BF%9B%E5%88%B6+%C2%B7+%E4%B8%80%E6%9D%A1%E4%B8%8A%E9%99%90+%C2%B7+%E4%BA%94%E7%A7%8D+wire+format;%E6%9C%AC%E5%9C%B0+tokenizer+%E5%85%9C%E5%BA%95+%C2%B7+%E9%9B%B6%E9%81%A5%E6%B5%8B" alt="provider rotation" />
</p>

<p align="center"><em>一个 Go 单二进制：在你的 <code>claude</code>、<code>codex</code> 或任何编码智能体 CLI 和上游 API 之间，按项目目录硬性限额，本地运行、无后台守护、无任何遥测。</em></p>

---

## 目录

- [为什么需要它](#为什么需要它)
- [安装 &amp; 上手](#安装--上手)
- [演示](#演示)
- [v0.2.0 — 多供应商](#v020--多供应商)
- [配置](#配置)
- [命令](#命令)
- [工作原理](#工作原理)
- [路线图](#路线图)
- [License &amp; 参与贡献](#license--参与贡献)

---

## 为什么需要它

不到一年，编码智能体 CLI 从"每轮按一下回车"变成了通宵跑的自动循环，可计费模型并没有跟上。一份不小心留下的 `.env` 就能把 **$187** 的请求路由到错的 Anthropic 账号（[这条 476↑ 的 PSA 帖子](https://reddit.com/r/ClaudeAI/comments/1tbaq2d/) 就是这个仓库的起点）。通宵的 agentic loop 把 Max 套餐烧穿。6 月 15 日的 `--print` 计费变更，把跑了好几个月的脚本从"订阅"一夜之间挪进了"按 credit 计费"。

AgentFuse 就是那条 PSA 想要的总闸：一个轻量本地代理，读取项目根目录的 `.fuse.toml`，**累计花费一旦触顶就直接 fail-closed**——下一个请求根本不会离开你的机器。无后台守护、无云端、无遥测，用 `strings` 30 秒就能审计完整二进制。

## 安装 &amp; 上手

<img align="right" width="48" src="https://raw.githubusercontent.com/tabler/tabler-icons/main/icons/rocket.svg" alt="" />

```bash
# 1. 安装（需要 Go 1.24+）
go install github.com/SuperMarioYL/agentfuse/cmd/fuse@latest

# 2. 在项目下写一份默认 $5 上限的配置
cd ~/code/myproj && fuse init

# 3. 用代理跑你的智能体 CLI
fuse run claude
```

<details>
<summary>触发上限时的标准输出示例</summary>

```text
$ fuse run claude
agentfuse: proxy on 127.0.0.1:54021 → api.anthropic.com (account=personal, cap=$5.00 project)
agentfuse: forwarding child stdio …

[claude 会话正常输出一段时间 …]

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

## 演示

> 📼 演示马上补。asciinema 录像位置见 [`assets/README.md`](./assets/README.md)。30 秒剪辑覆盖：`fuse init` → `fuse run claude` → 跑飞的循环 → 触顶 → HTTP 402 → `fuse cap +5` → 继续。

## v0.2.0 — 多供应商

<img align="right" width="48" src="https://raw.githubusercontent.com/tabler/tabler-icons/main/icons/world.svg" alt="" />

v0.1 只支持 Anthropic + OpenAI。v0.2 把同一把熔断器扩展到 **5 个 provider 家族**——同一个二进制、同一个 `fuse run`、同一份 `.fuse.toml`——并通过 [tiktoken-go](https://github.com/pkoukk/tiktoken-go) 在本地重新分词，把 v0.1 那个"流式响应不带 `usage`"的漏洞堵上。

| Provider          | `.fuse.toml` 里的 `provider`        | Wire 格式                            | usage 来源                                       |
| ----------------- | ----------------------------------- | ------------------------------------ | ------------------------------------------------ |
| Anthropic         | `"anthropic"`（默认值）             | Messages API + SSE `usage` 事件      | 上游 `usage` 区块                                |
| OpenAI            | `"openai"`                          | Chat Completions + `include_usage`   | 上游 `usage` 区块                                |
| Google Gemini     | `"gemini"`                          | `:generateContent` / `:stream…`      | `usageMetadata` → tiktoken 兜底                  |
| DeepSeek          | `"deepseek"`                        | OpenAI-compat 形态                   | 最终 SSE `usage`（即使没开 include_usage 也有） |
| OpenAI-compat     | `"openai_compat"` + `upstream_url`  | 任何 OpenAI 形态上游                  | 有就用上游 `usage`，没有就 **tiktoken 兜底**      |

例：把项目挂到 Groq 的 Llama-3.1，$5 上限。

```toml
# .fuse.toml
cap_usd      = 5.0
provider     = "openai_compat"
upstream_url = "https://api.groq.com/openai/v1"
account      = "personal"
```

```bash
fuse run aider --model llama-3.1-70b
# fuse: proxy on 127.0.0.1:51234 — provider=openai_compat upstream=api.groq.com cap=$5.00
# … 正常用 … 触顶时的行为和 v0.1 一模一样
```

三份开箱即用的示例配置放在 [`examples/`](./examples)：`.fuse.toml.gemini`、`.fuse.toml.deepseek`、`.fuse.toml.openai-compat`。

### 价格表 —— 外置、用户可覆盖、永远不联网

`assets/prices.toml` 在编译期通过 `//go:embed` 打进二进制（2026-05 快照）。如果你想覆盖某个 `(provider, model)` 的价格，不用重新编译，直接写一份 `~/.fuse/prices.toml`：

```toml
# ~/.fuse/prices.toml —— 按 (provider, model) 维度覆盖，用户键永远赢。
[openai_compat."llama-3.1-70b"]
input_usd_per_1k  = 0.00079   # 你协商到的价
output_usd_per_1k = 0.00099
```

二进制永远不从网络拉价格——这是刻意为之、不可妥协的设计。价格过期是用户的问题；一个会偷偷联网的熔断器是信任的问题。

## 配置

项目根目录放一份 `.fuse.toml`。AgentFuse 会从 `cwd` 一级一级往上找最近的那一份。

| 键             | 类型      | 默认值         | 含义                                                                     |
| -------------- | --------- | -------------- | ------------------------------------------------------------------------ |
| `cap_usd`      | `float`   | *必填*         | USD 硬上限。请求前的预估也会计入这条线，单次特别大的请求绝不会"漏"过去。 |
| `window`       | `string`  | `"project"`    | `"project"`（从 `fuse init` 起累计）或 `"daily"`（UTC 零点滚动）。       |
| `account`      | `string`  | `""`           | `~/.fuse/accounts.toml` 中的命名账号。设置后，AgentFuse 会拒绝指纹对不上的 inbound key，并把正确的那把 key 注入子进程，把误读到的 `.env` key 直接盖掉。 |
| `provider`     | `string`  | `"anthropic"`  | 取值之一：`"anthropic"` / `"openai"` / `"gemini"` / `"deepseek"` / `"openai_compat"`。v0.2 新增。 |
| `upstream_url` | `string`  | 对应 provider 默认 | 覆盖上游主机。`provider = "openai_compat"` **必填**；其它 provider 可选（例如把 Gemini 指到 Vertex AI）。 |

账号本身放在 `~/.fuse/accounts.toml`：

```toml
[accounts.personal]
provider = "anthropic"
api_key  = "sk-ant-..."

[accounts.work]
provider = "anthropic"
api_key  = "sk-ant-..."
```

## 命令

| 命令                          | 作用                                                                               |
| ----------------------------- | ---------------------------------------------------------------------------------- |
| `fuse init`                   | 在当前目录写一份 `.fuse.toml`（参数：`--cap`、`--account`）。                      |
| `fuse run <cmd>`              | 启动本地代理，把子进程 CLI 指向 `127.0.0.1:<rand>`，`exec <cmd>`，跟随子进程退出。 |
| `fuse cap +N` / `=N` / `-N`   | 原子修改上限（任何会让上限 ≤ 0 的变更都会被拒）。                                  |
| `fuse status`                 | 打印当前账号、上限、今日花费、项目累计花费、剩余预算。                             |

## 工作原理

```
┌─────────────────┐                  ┌───────────────────┐
│  fuse run claude│ ─── exec ──────► │  claude (child)   │
└─────────────────┘                  │  ANTHROPIC_BASE   │
                                     │  → 127.0.0.1:PORT │
                                     └─────────┬─────────┘
                                               │
                                ┌──────────────▼──────────────┐
                                │  AgentFuse proxy            │
                                │  1. 账号守卫                │
                                │  2. 请求前预估              │
                                │  3. 对照 ledger 检查上限    │
                                │  4. ALLOW → 转发            │
                                │     DENY  → HTTP 402        │
                                │  5. 解析 `usage`，写入       │
                                │     ~/.fuse/ledger.db       │
                                └──────────────┬──────────────┘
                                               │
                                       api.anthropic.com
                                       api.openai.com
```

全部本地：单二进制、无守护进程、ledger 是 `~/.fuse/ledger.db`（bbolt KV，`(project, day) → {tokens_in, tokens_out, usd, requests}`）。代理跟随子进程一起退出，binary 除了你配置的 provider 不会和任何外网通信。

## 路线图

- [x] **m1 — 拦截 &amp; 记账。** 本地代理透明转发 `claude` / `codex` 流量，解析 `usage`，按 cwd 写 token + USD ledger。`fuse status` 给出真实数字。
- [x] **m2 — 硬上限。** `.fuse.toml` 解析 + `cap_usd` 强制执行。请求前预估杜绝单次"漏掉"。触顶返回 HTTP 402。`fuse cap ±N` 原子修改。
- [x] **m3 — 启动器模式。** `~/.fuse/accounts.toml` 命名账号、指纹匹配、OpenAI provider 对齐。误读到的 `.env` key 再也不会把流量带到错的账号上。
- [x] **m4 — 扩宽楔形（v0.2）。** 新增 Gemini、DeepSeek、OpenAI-compat 三个 handler；用 tiktoken-go 给"流式无 usage"的上游兜底；价格表外置 `assets/prices.toml`，可通过 `~/.fuse/prices.toml` 覆盖。

依然不做：Web UI、多人 / SSO、跨主机花费聚合、细粒度 per-tool 限额、Slack / 邮件告警、Windows 原生（仅 WSL2），以及——绝不做的——任何远端价格拉取或遥测调用。

## License &amp; 参与贡献

MIT，详见 [LICENSE](LICENSE)。Issue 和 PR 都欢迎，地址：[github.com/SuperMarioYL/agentfuse](https://github.com/SuperMarioYL/agentfuse)。当下最有价值的贡献：拿一个真实的通宵循环跑一遍上面的 quickstart，把熔断触发（或者没触发）的细节开个 issue 告诉我们。

---

<p align="center"><sub>由 <a href="https://github.com/SuperMarioYL/ai-radar">ai-radar</a> 流水线生成 · scan-2026-05-17-2013 · 候选 <code>t7neg04</code></sub></p>
