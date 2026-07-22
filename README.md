# spore-host-mcp

[![CI](https://github.com/spore-host/spore-host-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/spore-host/spore-host-mcp/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/spore-host/spore-host-mcp)](https://goreportcard.com/report/github.com/spore-host/spore-host-mcp)
[![codecov](https://codecov.io/gh/spore-host/spore-host-mcp/branch/main/graph/badge.svg)](https://codecov.io/gh/spore-host/spore-host-mcp)
[![Go Reference](https://pkg.go.dev/badge/github.com/spore-host/spore-host-mcp.svg)](https://pkg.go.dev/github.com/spore-host/spore-host-mcp)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

MCP server exposing truffle and spawn as tools for AI assistants.

Works with Claude Desktop, Cursor, and any other client that supports the [Model Context Protocol](https://modelcontextprotocol.io).

## Installation

**macOS / Linux (Homebrew)**
```bash
brew install spore-host/tap/spore-host-mcp
```

**Windows (Scoop)**
```powershell
scoop bucket add spore-host https://github.com/spore-host/scoop-bucket
scoop install spore-host-mcp
```

**Direct download** — pre-built binaries on the [releases page](https://github.com/spore-host/spore-host-mcp/releases/latest).

## Setup

Add to your MCP client config:

```json
{
  "mcpServers": {
    "spore-host": {
      "command": "spore-host-mcp"
    }
  }
}
```

For Claude Desktop: `~/Library/Application Support/Claude/claude_desktop_config.json`  
For Cursor: `.cursor/mcp.json`

## Tools exposed

**truffle tools** — EC2 discovery (require AWS credentials; these call the EC2 and Service Quotas APIs):
- `truffle_find` — natural language instance search
- `truffle_spot_prices` — spot prices for an instance type
- `truffle_quota_check` — check EC2 service quotas

**spawn tools** — instance lifecycle (requires AWS credentials):
- `spawn_list` — list instances
- `spawn_status` — instance status, TTL, and absolute reap deadline
- `spawn_stop` — stop a running instance
- `spawn_terminate` — terminate an instance
- `spawn_extend` — extend TTL

There is **no launch tool, by design** — the server is read + manage-existing
only. Creating billable instances from an assistant is a boundary spore.host
doesn't cross automatically; the assistant helps you *construct* the `spawn
launch` command and you run it.

`spawn_terminate` is **two-phase**: the first call previews the exact instance
that would be destroyed, and only a second call with `confirm=true` actually
terminates it. An ambiguous name (matching more than one instance) is refused —
use the instance ID — so the assistant can never terminate the wrong box.

## Credentials

The server uses your ambient AWS credential chain — the same one the CLIs use
(`AWS_PROFILE`/`AWS_REGION`, `~/.aws/…`, or instance metadata). It also honors
the shared spore.host config base: `SPORE_PROFILE`/`SPORE_REGION` and the
`[spore]` table of `~/.config/spore/config.toml`. No MCP-specific setup is
needed if the CLI already works.

## Documentation

Full setup guide at **[spore.host/docs](https://spore.host/docs/guides/mcp-setup)**.

## License

Apache 2.0 — Copyright 2025-2026 Scott Friedman.
