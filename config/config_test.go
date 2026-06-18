package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigYAML(t *testing.T) {
	// Create temporary directory for test files
	tmpDir, err := os.MkdirTemp("", "stratum_config_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create legacy mock txt files
	_ = os.MkdirAll(filepath.Join(tmpDir, "config"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "config", "keywords.txt"), []byte("(quantum AND computing)"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "config", "topics.txt"), []byte("T10020\nT10682"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "config", "anchor.txt"), []byte("10.1038/nphys1170"), 0644)

	yamlContent := `
api:
  keys:
    - "key1"
    - "key2"
  groq_key: "gsk_test"
  email: "test@domain.com"
  base_url: "https://api.openalex.org"
filters:
  date_from: "2020-01-01"
  date_to: "2024-01-01"
  doc_types:
    - "article"
keywords_file: "` + filepath.Join(tmpDir, "config", "keywords.txt") + `"
topics_file: "` + filepath.Join(tmpDir, "config", "topics.txt") + `"
anchor_file: "` + filepath.Join(tmpDir, "config", "anchor.txt") + `"
collection:
  batch_size_topics: 5
  per_page: 100
  concurrent_requests: 2
  max_retries: 3
  retry_delay: 1
llm:
  provider: "ollama"
  model: "qwen-test"
  base_url: "http://localhost:11434"
output:
  jsonl_dir: "data/raw/"
  db_dir: "data/db/"
`
	configPath := filepath.Join(tmpDir, "collection.yml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write test config file: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if cfg.API.Email != "test@domain.com" {
		t.Errorf("expected email 'test@domain.com', got '%s'", cfg.API.Email)
	}
	if len(cfg.API.Keys) != 2 || cfg.API.Keys[0] != "key1" || cfg.API.Keys[1] != "key2" {
		t.Errorf("unexpected API keys: %v", cfg.API.Keys)
	}
	if cfg.Filters.DateFrom != "2020-01-01" {
		t.Errorf("expected DateFrom '2020-01-01', got '%s'", cfg.Filters.DateFrom)
	}
	if cfg.Collection.PerPage != 100 {
		t.Errorf("expected PerPage 100, got %d", cfg.Collection.PerPage)
	}
	if cfg.LLM.Model != "qwen-test" {
		t.Errorf("expected LLM model 'qwen-test', got '%s'", cfg.LLM.Model)
	}
	if cfg.Keywords != "(quantum AND computing)" {
		t.Errorf("expected resolved keywords '(quantum AND computing)', got '%s'", cfg.Keywords)
	}
	if len(cfg.Topics) != 2 || cfg.Topics[0] != "T10020" {
		t.Errorf("unexpected resolved topics: %v", cfg.Topics)
	}
	if len(cfg.Anchors) != 1 || cfg.Anchors[0] != "10.1038/nphys1170" {
		t.Errorf("unexpected resolved anchors: %v", cfg.Anchors)
	}
}

func TestSaveAndLoadDBConfig(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "stratum_config_db_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "config.db")

	cfg := &AppConfig{
		API: APIConfig{
			Keys:    []string{"savekey"},
			GroqKey: "groqkey",
			Email:   "save@test.com",
			BaseURL: "https://base.org",
		},
		Filters: FiltersConfig{
			DateFrom: "2019-12-31",
			DateTo:   "2023-12-31",
			DocTypes: []string{"proceedings"},
		},
		Keywords: "(ai AND computing)",
		Topics:   []string{"T10001", "T10002"},
		Anchors:  []string{"10.1234/test"},
		Collection: CollectionConfig{
			BatchSizeTopics:    10,
			PerPage:            200,
			ConcurrentRequests: 5,
			MaxRetries:         4,
			RetryDelay:         2,
		},
		LLM: LLMConfig{
			Provider: "groq",
			Model:    "llama-save",
			BaseURL:  "https://api.groq.com",
		},
	}

	if err := SaveConfig(dbPath, cfg); err != nil {
		t.Fatalf("SaveConfig to DB failed: %v", err)
	}

	loaded, err := LoadConfig(dbPath)
	if err != nil {
		t.Fatalf("LoadConfig from DB failed: %v", err)
	}

	if loaded.API.Email != "save@test.com" {
		t.Errorf("expected email 'save@test.com', got '%s'", loaded.API.Email)
	}
	if len(loaded.API.Keys) != 1 || loaded.API.Keys[0] != "savekey" {
		t.Errorf("unexpected keys: %v", loaded.API.Keys)
	}
	if loaded.Keywords != "(ai AND computing)" {
		t.Errorf("unexpected keywords: %s", loaded.Keywords)
	}
	if len(loaded.Topics) != 2 || loaded.Topics[0] != "T10001" {
		t.Errorf("unexpected topics: %v", loaded.Topics)
	}
	if len(loaded.Anchors) != 1 || loaded.Anchors[0] != "10.1234/test" {
		t.Errorf("unexpected anchors: %v", loaded.Anchors)
	}
	if loaded.Collection.PerPage != 200 {
		t.Errorf("expected PerPage 200, got %d", loaded.Collection.PerPage)
	}
	if loaded.LLM.Model != "llama-save" {
		t.Errorf("expected LLM model 'llama-save', got '%s'", loaded.LLM.Model)
	}
}
