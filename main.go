// Command spore-host-mcp is an MCP server exposing spawn (instance management)
// and truffle (EC2 capacity discovery) as tools for AI assistants.
//
// Install in Claude Desktop (~/.claude/claude_desktop_config.json):
//
//	{
//	  "mcpServers": {
//	    "spore-host": {
//	      "command": "/usr/local/bin/spore-host-mcp"
//	    }
//	  }
//	}
package main

import (
	"log"
	"os"

	"github.com/mark3labs/mcp-go/server"
)

// Version is injected at build time via -ldflags.
var Version = "dev"

func main() {
	s := server.NewMCPServer(
		"spore-host-mcp",
		Version,
		server.WithToolCapabilities(true),
	)

	registerSpawnTools(s)
	registerTruffleTools(s)

	errLog := log.New(os.Stderr, "spore-host-mcp: ", 0)
	if err := server.ServeStdio(s, server.WithErrorLogger(errLog)); err != nil {
		errLog.Fatalf("server error: %v", err)
	}
}
