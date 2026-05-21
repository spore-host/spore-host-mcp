# spore-host-mcp

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

**truffle tools** — EC2 discovery (no credentials required):
- `truffle_search` — search instance types
- `truffle_find` — natural language search
- `truffle_spot` — spot prices

**spawn tools** — instance lifecycle (requires AWS credentials):
- `spawn_list` — list running instances
- `spawn_launch` — launch an instance
- `spawn_connect` — connect to an instance
- `spawn_status` — instance status and TTL
- `spawn_extend` — extend TTL

## Documentation

Full setup guide at **[spore.host/docs](https://spore.host/docs/guides/mcp-setup)**.

## License

Apache 2.0 — Copyright 2025-2026 Scott Friedman.
