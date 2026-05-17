#!/usr/bin/env bash
# What using AgentFuse looks like, in three lines.
# Prereq: `go install github.com/agentfuse/fuse/cmd/fuse@latest` (or `go build -o ./fuse ./cmd/fuse`)
set -euo pipefail

cd "$(mktemp -d)"

# 1. drop a config with a $5 cap on the personal account
fuse init --cap 5 --account personal

# 2. start your coding agent through the proxy. AgentFuse exits when the child does.
fuse run claude

# 3. while it runs, peek at spend from another terminal:
#    fuse status
#
# When the cap is reached, the next request returns HTTP 402 and stderr shows:
#   agentfuse: budget exceeded for project ... ($5.02 / $5.00) — raise with: fuse cap +5
