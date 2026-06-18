package config

import (
	"database/sql"
	"os"
	"regexp"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"
	"gopkg.in/yaml.v3"
)

// AppConfig represents the root configuration structure mapped to collection.yml or config.db.
type AppConfig struct {
	API        APIConfig        `yaml:"api" json:"api"`
	Filters    FiltersConfig    `yaml:"filters" json:"filters"`
	Keywords   string           `yaml:"keywords" json:"keywords"`
	Topics     []string         `yaml:"topics" json:"topics"`
	Anchors    []string         `yaml:"anchors" json:"anchors"`
	Collection CollectionConfig `yaml:"collection" json:"collection"`
	LLM        LLMConfig        `yaml:"llm" json:"llm"`
	Output     OutputConfig     `yaml:"output" json:"output"`
}

// APIConfig defines api keys, endpoint URL, and contact email.
type APIConfig struct {
	Keys    []string `yaml:"keys" json:"keys"`
	GroqKey string   `yaml:"groq_key" json:"groq_key"`
	Email   string   `yaml:"email" json:"email"`
	BaseURL string   `yaml:"base_url" json:"base_url"`
}

// FiltersConfig defines date limits and document types.
type FiltersConfig struct {
	DateFrom string   `yaml:"date_from" json:"date_from"`
	DateTo   string   `yaml:"date_to" json:"date_to"`
	DocTypes []string `yaml:"doc_types" json:"doc_types"`
}

// CollectionConfig defines request tuning options.
type CollectionConfig struct {
	BatchSizeTopics    int `yaml:"batch_size_topics" json:"batch_size_topics"`
	PerPage            int `yaml:"per_page" json:"per_page"`
	ConcurrentRequests int `yaml:"concurrent_requests" json:"concurrent_requests"`
	MaxRetries         int `yaml:"max_retries" json:"max_retries"`
	RetryDelay         int `yaml:"retry_delay" json:"retry_delay"`
}

// LLMConfig defines configuration for local Ollama or Gemini models.
type LLMConfig struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
	BaseURL  string `yaml:"base_url" json:"base_url"`
}

// OutputConfig defines output directory targets.
type OutputConfig struct {
	JSONLDir string `yaml:"jsonl_dir" json:"jsonl_dir"`
	DBDir    string `yaml:"db_dir" json:"db_dir"`
}

// LoadConfig reads the configuration. If path ends with .db or .duckdb, it loads it from a DuckDB database.
// Otherwise it parses from YAML.
func LoadConfig(path string) (*AppConfig, error) {
	if strings.HasSuffix(path, ".db") || strings.HasSuffix(path, ".duckdb") {
		return LoadConfigFromDB(path)
	}
	return LoadConfigFromYAML(path)
}

// SaveConfig saves the configuration. If path ends with .db or .duckdb, it saves it to a DuckDB database.
// Otherwise it serializes to YAML.
func SaveConfig(path string, cfg *AppConfig) error {
	if strings.HasSuffix(path, ".db") || strings.HasSuffix(path, ".duckdb") {
		return SaveConfigToDB(path, cfg)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// initConfigTable initializes configuration database schema and inserts default row if missing.
func initConfigTable(dbConn *sql.DB) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS config (
			id INTEGER PRIMARY KEY,
			email VARCHAR,
			api_keys VARCHAR,
			date_from VARCHAR,
			date_to VARCHAR,
			doc_types VARCHAR,
			batch_size_topics INTEGER,
			per_page INTEGER,
			concurrent_requests INTEGER,
			max_retries INTEGER,
			retry_delay INTEGER,
			llm_provider VARCHAR,
			llm_model VARCHAR,
			llm_base_url VARCHAR,
			keywords VARCHAR,
			topics VARCHAR,
			anchors VARCHAR
		);`,
		`CREATE TABLE IF NOT EXISTS config_history (
			version INTEGER PRIMARY KEY,
			timestamp VARCHAR,
			label VARCHAR,
			keywords VARCHAR,
			topics VARCHAR,
			anchors VARCHAR
		);`,
	}

	for _, q := range queries {
		if _, err := dbConn.Exec(q); err != nil {
			return err
		}
	}

	var count int
	err := dbConn.QueryRow("SELECT COUNT(*) FROM config").Scan(&count)
	if err != nil {
		return err
	}

	if count == 0 {
		_, err = dbConn.Exec(`INSERT INTO config (
			id, email, api_keys, date_from, date_to, doc_types,
			batch_size_topics, per_page, concurrent_requests, max_retries, retry_delay,
			llm_provider, llm_model, llm_base_url, keywords, topics, anchors
		) VALUES (
			1, 'sathyarajasekar5873@gmail.com', '28leglCF5hY0mVmVYXSNNm', '2003-01-01', '2024-12-31', 'article,review,proceedings-article',
			10, 200, 10, 5, 2,
			'ollama', 'sorc/qwen3.5-instruct:2b', 'http://localhost:11434', '', '', ''
		)`)
		if err != nil {
			return err
		}
	}

	return nil
}

// LoadConfigFromDB reads config row from a DuckDB file.
func LoadConfigFromDB(dbPath string) (*AppConfig, error) {
	dbConn, err := sql.Open("duckdb", dbPath)
	if err != nil {
		return nil, err
	}
	defer dbConn.Close()

	if err := initConfigTable(dbConn); err != nil {
		return nil, err
	}

	var email, apiKeys, dateFrom, dateTo, docTypes, llmProvider, llmModel, llmBaseURL, keywords, topics, anchors string
	var batchSizeTopics, perPage, concurrentRequests, maxRetries, retryDelay int

	row := dbConn.QueryRow(`SELECT 
		email, api_keys, date_from, date_to, doc_types,
		batch_size_topics, per_page, concurrent_requests, max_retries, retry_delay,
		llm_provider, llm_model, llm_base_url, keywords, topics, anchors
		FROM config WHERE id = 1`)
	err = row.Scan(
		&email, &apiKeys, &dateFrom, &dateTo, &docTypes,
		&batchSizeTopics, &perPage, &concurrentRequests, &maxRetries, &retryDelay,
		&llmProvider, &llmModel, &llmBaseURL, &keywords, &topics, &anchors,
	)
	if err != nil {
		return nil, err
	}

	var keys []string
	if apiKeys != "" {
		for _, k := range strings.Split(apiKeys, ",") {
			trimmed := strings.TrimSpace(k)
			if trimmed != "" {
				keys = append(keys, trimmed)
			}
		}
	}

	var docTypeList []string
	if docTypes != "" {
		for _, dt := range strings.Split(docTypes, ",") {
			trimmed := strings.TrimSpace(dt)
			if trimmed != "" {
				docTypeList = append(docTypeList, trimmed)
			}
		}
	}

	var topicsList []string
	if topics != "" {
		for _, t := range strings.Split(topics, "\n") {
			trimmed := strings.TrimSpace(t)
			if trimmed != "" {
				topicsList = append(topicsList, trimmed)
			}
		}
	}

	var anchorsList []string
	if anchors != "" {
		for _, a := range strings.Split(anchors, "\n") {
			trimmed := strings.TrimSpace(a)
			if trimmed != "" {
				anchorsList = append(anchorsList, trimmed)
			}
		}
	}

	cfg := &AppConfig{
		API: APIConfig{
			Keys:    keys,
			Email:   email,
			BaseURL: "https://api.openalex.org/works",
		},
		Filters: FiltersConfig{
			DateFrom: dateFrom,
			DateTo:   dateTo,
			DocTypes: docTypeList,
		},
		Keywords: keywords,
		Topics:   topicsList,
		Anchors:  anchorsList,
		Collection: CollectionConfig{
			BatchSizeTopics:    batchSizeTopics,
			PerPage:            perPage,
			ConcurrentRequests: concurrentRequests,
			MaxRetries:         maxRetries,
			RetryDelay:         retryDelay,
		},
		LLM: LLMConfig{
			Provider: llmProvider,
			Model:    llmModel,
			BaseURL:  llmBaseURL,
		},
	}
	return cfg, nil
}

// SaveConfigToDB serializes config properties back to config.db.
func SaveConfigToDB(dbPath string, cfg *AppConfig) error {
	dbConn, err := sql.Open("duckdb", dbPath)
	if err != nil {
		return err
	}
	defer dbConn.Close()

	if err := initConfigTable(dbConn); err != nil {
		return err
	}

	apiKeys := strings.Join(cfg.API.Keys, ",")
	docTypes := strings.Join(cfg.Filters.DocTypes, ",")
	topics := strings.Join(cfg.Topics, "\n")
	anchors := strings.Join(cfg.Anchors, "\n")

	_, err = dbConn.Exec(`UPDATE config SET 
		email = ?, api_keys = ?, date_from = ?, date_to = ?, doc_types = ?,
		batch_size_topics = ?, per_page = ?, concurrent_requests = ?, max_retries = ?, retry_delay = ?,
		llm_provider = ?, llm_model = ?, llm_base_url = ?, keywords = ?, topics = ?, anchors = ?
		WHERE id = 1`,
		cfg.API.Email, apiKeys, cfg.Filters.DateFrom, cfg.Filters.DateTo, docTypes,
		cfg.Collection.BatchSizeTopics, cfg.Collection.PerPage, cfg.Collection.ConcurrentRequests, cfg.Collection.MaxRetries, cfg.Collection.RetryDelay,
		cfg.LLM.Provider, cfg.LLM.Model, cfg.LLM.BaseURL, cfg.Keywords, topics, anchors,
	)
	return err
}

// LoadConfigFromYAML parses the legacy collection.yml.
func LoadConfigFromYAML(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var legacy struct {
		API        APIConfig        `yaml:"api"`
		Filters    FiltersConfig    `yaml:"filters"`
		Keywords   string           `yaml:"keywords_file"`
		Topics     string           `yaml:"topics_file"`
		Anchors    string           `yaml:"anchor_file"`
		Collection CollectionConfig `yaml:"collection"`
		LLM        LLMConfig        `yaml:"llm"`
		Output     OutputConfig     `yaml:"output"`
	}
	if err := yaml.Unmarshal(data, &legacy); err != nil {
		return nil, err
	}

	// Clean/clean whitespace for keywords
	var keywords string
	if legacy.Keywords != "" {
		if kd, err := os.ReadFile(legacy.Keywords); err == nil {
			re := regexp.MustCompile(`\s+`)
			keywords = strings.TrimSpace(re.ReplaceAllString(string(kd), " "))
		}
	}

	var topics []string
	if legacy.Topics != "" {
		if td, err := os.ReadFile(legacy.Topics); err == nil {
			for _, line := range strings.Split(string(td), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					topics = append(topics, line)
				}
			}
		}
	}

	var anchors []string
	if legacy.Anchors != "" {
		if ad, err := os.ReadFile(legacy.Anchors); err == nil {
			for _, line := range strings.Split(string(ad), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					anchors = append(anchors, line)
				}
			}
		}
	}

	cfg := &AppConfig{
		API:        legacy.API,
		Filters:    legacy.Filters,
		Keywords:   keywords,
		Topics:     topics,
		Anchors:    anchors,
		Collection: legacy.Collection,
		LLM:        legacy.LLM,
		Output:     legacy.Output,
	}
	return cfg, nil
}
