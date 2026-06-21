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
- `spawn_status` — instance status and TTL
- `spawn_stop` — stop a running instance
- `spawn_terminate` — terminate an instance
- `spawn_extend` — extend TTL

## Documentation

Full setup guide at **[spore.host/docs](https://spore.host/docs/guides/mcp-setup)**.

## License

Apache 2.0 — Copyright 2025-2026 Scott Friedman.
