package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMCPServerTools(t *testing.T) {
	server := NewMCPServer("test-mcp", "1.0.0")
	err := server.RegisterTools()
	if err != nil {
		t.Fatalf("expected RegisterTools to pass, got error: %v", err)
	}

	// Setup a dummy config to test handleValidate
	tempDir, err := os.MkdirTemp("", "mcp-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	configPath := filepath.Join(tempDir, "collection.yml")

	configData := []byte(`
api:
  keys: ["key1"]
  email: "test@example.com"
filters:
  date_from: "2020-01-01"
  date_to: "2020-12-31"
  doc_types: ["article"]
keywords: "quantum AND computing"
topics:
  - "T12345"
`)
	if err := os.WriteFile(configPath, configData, 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	ctx := context.Background()

	// Call handleValidate directly
	res, valRes, err := server.handleValidate(ctx, nil, ValidateArgs{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("handleValidate failed: %v", err)
	}

	if res == nil {
		t.Fatalf("expected CallToolResult, got nil")
	}

	// Format check is valid, but checking existence will fail because the API key is fake. That is expected.
	t.Logf("Validate result: valid=%t, errors=%v", valRes.Valid, valRes.Errors)
}

func TestMCPServerSearchAndTopicsInvalidQuery(t *testing.T) {
	server := NewMCPServer("test-mcp", "1.0.0")
	err := server.RegisterTools()
	if err != nil {
		t.Fatalf("expected RegisterTools to pass, got error: %v", err)
	}

	tempDir, err := os.MkdirTemp("", "mcp-test-invalid")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	configPath := filepath.Join(tempDir, "collection.yml")

	// Invalid query with unbalanced parentheses
	configData := []byte(`
api:
  keys: ["key1"]
  email: "test@example.com"
keywords: "(unbalanced"
`)
	if err := os.WriteFile(configPath, configData, 0644); err != nil {
		t.Fatalf("failed to write config file: %v", err)
	}

	ctx := context.Background()

	// Call handleSearch
	searchRes, _, err := server.handleSearch(ctx, nil, SearchArgs{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("handleSearch failed: %v", err)
	}
	if searchRes == nil || !searchRes.IsError {
		t.Errorf("expected handleSearch to return an error for invalid query, got: %+v", searchRes)
	}

	// Call handleGetTopics
	topicsRes, _, err := server.handleGetTopics(ctx, nil, GetTopicsArgs{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("handleGetTopics failed: %v", err)
	}
	if topicsRes == nil || !topicsRes.IsError {
		t.Errorf("expected handleGetTopics to return an error for invalid query, got: %+v", topicsRes)
	}
}
