package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"stratum/mcp"
)

func main() {
	fmt.Fprintln(os.Stderr, "Starting Stratum MCP Server (stdio mode)...")
	ctx := context.Background()
	server := mcp.NewMCPServer("stratum-mcp", "1.0.0")
	if err := server.RegisterTools(); err != nil {
		log.Fatalf("Failed to register MCP tools: %v", err)
	}
	if err := server.Start(ctx); err != nil {
		log.Fatalf("MCP server failed: %v", err)
	}
}
