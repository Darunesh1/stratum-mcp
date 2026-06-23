package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"stratum/config"
	"stratum/db"
	"stratum/impute"
	"stratum/openalex"
)

// MCPServer manages the official Model Context Protocol server state.
type MCPServer struct {
	name    string
	version string
	server  *mcp.Server
}

// NewMCPServer initializes a new MCP server instance.
func NewMCPServer(name, version string) *MCPServer {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    name,
		Version: version,
	}, nil)
	return &MCPServer{name: name, version: version, server: srv}
}

// ValidateArgs defines the input schema for the validate tool.
type ValidateArgs struct {
	ConfigPath string `json:"config_path,omitempty" jsonschema:"Optional path to the collection.yml config file (default: config/collection.yml)"`
}

// ValidateResult defines the output schema for the validate tool.
type ValidateResult struct {
	Valid  bool     `json:"valid" jsonschema:"Indicates if keywords and topics are structurally valid and exist in OpenAlex"`
	Errors []string `json:"errors" jsonschema:"List of validation error messages, empty if valid"`
}

// SearchArgs defines the input schema for the search tool.
type SearchArgs struct {
	ConfigPath   string `json:"config_path,omitempty" jsonschema:"Optional path to the collection.yml config file (default: config/collection.yml)"`
	CheckAnchors bool   `json:"check_anchors,omitempty" jsonschema:"Optional flag to check anchor DOI coverage (default: false)"`
}

// SearchResult defines the output schema for the search tool.
type SearchResult struct {
	TotalCount     int      `json:"total_count" jsonschema:"The total number of academic papers matching the query parameters in OpenAlex"`
	AnchorsTotal   int      `json:"anchors_total,omitempty" jsonschema:"Total number of anchors checked"`
	AnchorsMatched int      `json:"anchors_matched,omitempty" jsonschema:"Number of anchors found in search results"`
	AnchorsMissing []string `json:"anchors_missing,omitempty" jsonschema:"List of anchor DOIs missing from search results"`
}

// DownloadArgs defines the input schema for the download tool.
type DownloadArgs struct {
	ConfigPath  string `json:"config_path,omitempty" jsonschema:"Optional path to the collection.yml config file (default: config/collection.yml)"`
	OutputJSONL string `json:"output_jsonl,omitempty" jsonschema:"Optional path to write the downloaded JSONL file (defaults to config output location)"`
}

// DownloadResult defines the output schema for the download tool.
type DownloadResult struct {
	Status  string `json:"status" jsonschema:"Status message indicating success or failure"`
	Message string `json:"message" jsonschema:"Detail message outlining details of downloaded papers"`
}

// ConvertDBArgs defines the input schema for the convert_db tool.
type ConvertDBArgs struct {
	ConfigPath string `json:"config_path,omitempty" jsonschema:"Optional path to the collection.yml config file (default: config/collection.yml)"`
	JSONLPath  string `json:"jsonl_path,omitempty" jsonschema:"Optional path to the input JSONL file (defaults to latest downloaded)"`
}

// ConvertDBResult defines the output schema for the convert_db tool.
type ConvertDBResult struct {
	Status        string `json:"status" jsonschema:"Status message indicating success or failure"`
	PapersLoaded  int    `json:"papers_loaded" jsonschema:"Number of papers successfully loaded into DuckDB"`
	AuthorsLoaded int    `json:"authors_loaded" jsonschema:"Number of unique authors loaded"`
	InstsLoaded   int    `json:"institutions_loaded" jsonschema:"Number of unique institutions loaded"`
	Errors        int    `json:"errors" jsonschema:"Number of row errors encountered during ingestion"`
}

// ImputeArgs defines the input schema for the impute tool.
type ImputeArgs struct {
	ConfigPath string `json:"config_path,omitempty" jsonschema:"Optional path to the collection.yml config file (default: config/collection.yml)"`
	Pipeline   string `json:"pipeline,omitempty" jsonschema:"Pipeline stage to execute: crossref, llm, pdf, or all (default: all)"`
	Limit      int    `json:"limit,omitempty" jsonschema:"Optional limit for the number of PDF files to download and process"`
}

// ImputeResult defines the output schema for the impute tool.
type ImputeResult struct {
	Status  string `json:"status" jsonschema:"Status message indicating success or failure"`
	Message string `json:"message" jsonschema:"Detailed summary of actions taken and records updated"`
}

// RegisterTools registers all available collection, database, and imputation pipeline tools.
func (s *MCPServer) RegisterTools() error {
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "validate",
		Description: "Validate the keywords syntax and check if configured topics exist in OpenAlex.",
	}, s.handleValidate)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "search",
		Description: "Query OpenAlex to return the total count of academic papers matching current configuration filters.",
	}, s.handleSearch)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "download",
		Description: "Download papers matching configuration filters concurrently and save them to a JSONL file.",
	}, s.handleDownload)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "convert_db",
		Description: "Import downloaded JSONL paper records into DuckDB with schema initialization.",
	}, s.handleConvertDB)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "impute",
		Description: "Impute missing institution and country metadata using Crossref, LLMs, and PDF text extraction.",
	}, s.handleImpute)

	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "get_topics",
		Description: "Fetch the distribution of research topics and paper counts matching the keyword filters from OpenAlex.",
	}, s.handleGetTopics)

	return nil
}

// Start runs the MCP server on the stdio transport interface.
func (s *MCPServer) Start(ctx context.Context) error {
	return s.server.Run(ctx, &mcp.StdioTransport{})
}

// handleValidate validates the keywords syntax and checks if configured topics exist in OpenAlex.
func (s *MCPServer) handleValidate(ctx context.Context, req *mcp.CallToolRequest, args ValidateArgs) (*mcp.CallToolResult, ValidateResult, error) {
	configPath := args.ConfigPath
	if configPath == "" {
		configPath = "data/db/config.db"
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		errStr := "failed to load config: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, ValidateResult{Valid: false, Errors: []string{errStr}}, nil
	}

	var errors []string

	// 1. Validate keywords
	keywords := cfg.Keywords
	if keywords != "" {
		kwErrs := openalex.ValidateKeywords(keywords)
		errors = append(errors, kwErrs...)
	}

	// 2. Validate topics
	topics := cfg.Topics
	if len(topics) > 0 {
		var validTopics []string
		for _, topic := range topics {
			if !openalex.ValidateTopicFormat(topic) {
				errors = append(errors, "invalid topic format: "+topic)
			} else {
				validTopics = append(validTopics, topic)
			}
		}

		if len(validTopics) > 0 {
			client := openalex.NewClient(cfg.API.Keys, cfg.API.Email, cfg.Collection.PerPage, cfg.Collection.ConcurrentRequests, cfg.Collection.MaxRetries, cfg.Collection.RetryDelay)
			existsMap, err := openalex.ValidateTopicsExist(ctx, client, validTopics)
			if err != nil {
				errors = append(errors, "failed to check topics existence: "+err.Error())
			} else {
				for _, topic := range validTopics {
					if !existsMap[topic] {
						errors = append(errors, "topic does not exist in OpenAlex: "+topic)
					}
				}
			}
		}
	}

	valid := len(errors) == 0
	msg := fmt.Sprintf("Validation complete. Valid: %t. Errors: %d.", valid, len(errors))
	if !valid {
		msg += " Errors: " + strings.Join(errors, "; ")
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: msg},
		},
	}, ValidateResult{Valid: valid, Errors: errors}, nil
}

// handleSearch queries OpenAlex to return the count of matching papers.
func (s *MCPServer) handleSearch(ctx context.Context, req *mcp.CallToolRequest, args SearchArgs) (*mcp.CallToolResult, SearchResult, error) {
	configPath := args.ConfigPath
	if configPath == "" {
		configPath = "data/db/config.db"
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		errStr := "failed to load config: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, SearchResult{TotalCount: 0}, nil
	}

	keywords := cfg.Keywords
	if errs := openalex.ValidateKeywords(keywords); len(errs) > 0 {
		errStr := "keyword validation failed: " + strings.Join(errs, "; ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, SearchResult{TotalCount: 0}, nil
	}
	topics := cfg.Topics

	client := openalex.NewClient(cfg.API.Keys, cfg.API.Email, cfg.Collection.PerPage, cfg.Collection.ConcurrentRequests, cfg.Collection.MaxRetries, cfg.Collection.RetryDelay)

	// Build the OpenAlex filter
	parts := []string{"title_and_abstract.search:" + keywords}
	if len(topics) > 0 {
		parts = append(parts, "primary_topic.id:"+strings.Join(topics, "|"))
	}
	dateFrom := cfg.Filters.DateFrom
	if dateFrom == "" {
		dateFrom = "2003-01-01"
	}
	dateTo := cfg.Filters.DateTo
	if dateTo == "" {
		dateTo = "2024-12-31"
	}
	parts = append(parts, "from_publication_date:"+dateFrom)
	parts = append(parts, "to_publication_date:"+dateTo)
	if len(cfg.Filters.DocTypes) > 0 {
		parts = append(parts, "type:"+strings.Join(cfg.Filters.DocTypes, "|"))
	}
	filter := strings.Join(parts, ",")

	count, err := client.GetTotalCount(ctx, filter)
	if err != nil {
		errStr := "OpenAlex search request failed: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, SearchResult{TotalCount: 0}, nil
	}

	var anchors []string
	var matchedCount int
	var missingDOIs []string

	if args.CheckAnchors {
		for _, a := range cfg.Anchors {
			norm := normalizeDOI(a)
			if norm != "" {
				anchors = append(anchors, norm)
			}
		}

		if len(anchors) > 0 {
			batchSize := 10
			matchedSet := make(map[string]bool)

			for i := 0; i < len(anchors); i += batchSize {
				end := i + batchSize
				if end > len(anchors) {
					end = len(anchors)
				}
				batch := anchors[i:end]
				batchFilter := strings.Join(batch, "|")

				combinedFilter := filter + ",doi:" + batchFilter

				resp, err := client.FetchPage(ctx, combinedFilter, "*")
				if err == nil && resp != nil {
					for _, w := range resp.Results {
						norm := normalizeDOI(w.DOI)
						if norm != "" {
							matchedSet[norm] = true
						}
					}
				}
			}

			for _, doi := range anchors {
				if matchedSet[doi] {
					matchedCount++
				} else {
					missingDOIs = append(missingDOIs, doi)
				}
			}
		}
	}

	text := fmt.Sprintf("Found %d papers matching configuration filters in OpenAlex.", count)
	if args.CheckAnchors {
		text += fmt.Sprintf(" Anchor match coverage: %d/%d matches.", matchedCount, len(anchors))
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
	}, SearchResult{
		TotalCount:     count,
		AnchorsTotal:   len(anchors),
		AnchorsMatched: matchedCount,
		AnchorsMissing: missingDOIs,
	}, nil
}

// handleDownload downloads papers matching query filters concurrently and saves them to JSONL.
func (s *MCPServer) handleDownload(ctx context.Context, req *mcp.CallToolRequest, args DownloadArgs) (*mcp.CallToolResult, DownloadResult, error) {
	configPath := args.ConfigPath
	if configPath == "" {
		configPath = "data/db/config.db"
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		errStr := "failed to load config: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, DownloadResult{Status: "error", Message: errStr}, nil
	}

	outputJSONL := args.OutputJSONL
	if outputJSONL == "" {
		if err := os.MkdirAll(cfg.Output.JSONLDir, 0755); err != nil {
			errStr := "failed to create JSONL output directory: " + err.Error()
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: errStr},
				},
				IsError: true,
			}, DownloadResult{Status: "error", Message: errStr}, nil
		}
		outputJSONL = filepath.Join(cfg.Output.JSONLDir, "collected_papers.jsonl")
	} else {
		if err := os.MkdirAll(filepath.Dir(outputJSONL), 0755); err != nil {
			errStr := "failed to create output directory: " + err.Error()
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: errStr},
				},
				IsError: true,
			}, DownloadResult{Status: "error", Message: errStr}, nil
		}
	}

	client := openalex.NewClient(cfg.API.Keys, cfg.API.Email, cfg.Collection.PerPage, cfg.Collection.ConcurrentRequests, cfg.Collection.MaxRetries, cfg.Collection.RetryDelay)

	progressChan := make(chan int, 100)
	go func() {
		for p := range progressChan {
			fmt.Fprintf(os.Stderr, "Download progress: %d papers fetched.\n", p)
		}
	}()

	err = client.DownloadPapers(ctx, cfg, outputJSONL, progressChan)
	close(progressChan)

	if err != nil {
		errStr := "download papers failed: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, DownloadResult{Status: "error", Message: errStr}, nil
	}

	msg := fmt.Sprintf("Download complete. Papers saved to %s.", outputJSONL)
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: msg},
		},
	}, DownloadResult{Status: "success", Message: msg}, nil
}

// handleConvertDB imports downloaded JSONL paper records into DuckDB.
func (s *MCPServer) handleConvertDB(ctx context.Context, req *mcp.CallToolRequest, args ConvertDBArgs) (*mcp.CallToolResult, ConvertDBResult, error) {
	configPath := args.ConfigPath
	if configPath == "" {
		configPath = "data/db/config.db"
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		errStr := "failed to load config: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, ConvertDBResult{Status: "error"}, nil
	}

	jsonlPath := args.JSONLPath
	if jsonlPath == "" {
		jsonlPath = filepath.Join(cfg.Output.JSONLDir, "collected_papers.jsonl")
	}

	dbPath := filepath.Join(cfg.Output.DBDir, "stratum.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		errStr := "failed to create DB output directory: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, ConvertDBResult{Status: "error"}, nil
	}

	dbMgr, err := db.NewDBManager(dbPath)
	if err != nil {
		errStr := "failed to open DuckDB database: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, ConvertDBResult{Status: "error"}, nil
	}
	defer dbMgr.Close()

	if err := dbMgr.CreateSchema(); err != nil {
		errStr := "failed to create schema: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, ConvertDBResult{Status: "error"}, nil
	}

	progressChan := make(chan int, 100)
	go func() {
		for range progressChan {
			// Just drain
		}
	}()

	stats, err := dbMgr.LoadJSONL(jsonlPath, progressChan)
	close(progressChan)

	if err != nil {
		errStr := "failed to import JSONL into DuckDB: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, ConvertDBResult{Status: "error"}, nil
	}

	msg := fmt.Sprintf("Import complete. Loaded %d papers, %d authors, %d institutions into %s.", stats.Papers, stats.Authors, stats.Institutions, dbPath)
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: msg},
		},
	}, ConvertDBResult{
		Status:        "success",
		PapersLoaded:  stats.Papers,
		AuthorsLoaded: stats.Authors,
		InstsLoaded:   stats.Institutions,
		Errors:        stats.Errors,
	}, nil
}

// handleImpute imputes missing institution and country metadata using Crossref, LLMs, and PDF text extraction.
func (s *MCPServer) handleImpute(ctx context.Context, req *mcp.CallToolRequest, args ImputeArgs) (*mcp.CallToolResult, ImputeResult, error) {
	configPath := args.ConfigPath
	if configPath == "" {
		configPath = "data/db/config.db"
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		errStr := "failed to load config: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, ImputeResult{Status: "error", Message: errStr}, nil
	}

	dbPath := filepath.Join(cfg.Output.DBDir, "stratum.db")
	engine := impute.NewImputationEngine(dbPath)

	pipeline := strings.ToLower(args.Pipeline)
	if pipeline == "" {
		pipeline = "all"
	}

	var results []string

	progressChan := make(chan int, 100)
	go func() {
		for range progressChan {
			// Just drain
		}
	}()
	defer close(progressChan)

	if pipeline == "crossref" || pipeline == "all" {
		fmt.Fprintln(os.Stderr, "Running CrossRef metadata imputation...")
		if err := engine.ImputeCrossRef(ctx, progressChan); err != nil {
			results = append(results, "CrossRef failed: "+err.Error())
		} else {
			results = append(results, "CrossRef imputation complete.")
		}
	}

	if pipeline == "llm" || pipeline == "all" {
		fmt.Fprintln(os.Stderr, "Running LLM affiliation metadata imputation...")
		provider := cfg.LLM.Provider
		model := cfg.LLM.Model
		baseURL := cfg.LLM.BaseURL
		if err := engine.ImputeLLM(ctx, provider, model, baseURL, progressChan); err != nil {
			results = append(results, "LLM imputation failed: "+err.Error())
		} else {
			results = append(results, "LLM imputation complete.")
		}
	}

	if pipeline == "pdf" || pipeline == "all" {
		fmt.Fprintln(os.Stderr, "Running PDF metadata extraction and imputation...")
		provider := cfg.LLM.Provider
		model := cfg.LLM.Model
		baseURL := cfg.LLM.BaseURL
		limit := args.Limit
		if limit <= 0 {
			limit = 10 // default limit
		}
		if err := engine.ImputePDF(ctx, provider, model, baseURL, limit, progressChan); err != nil {
			results = append(results, "PDF imputation failed: "+err.Error())
		} else {
			results = append(results, "PDF imputation complete.")
		}
	}

	summary := strings.Join(results, "\n")
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: summary},
		},
	}, ImputeResult{Status: "success", Message: summary}, nil
}

// GetTopicsArgs defines input schema for the get_topics tool.
type GetTopicsArgs struct {
	ConfigPath string `json:"config_path,omitempty" jsonschema:"Optional path to the config file (default: data/db/config.db)"`
	Details    bool   `json:"details,omitempty" jsonschema:"Optional flag to fetch name and description details for each topic"`
}

// GetTopicsResult defines output schema for the get_topics tool.
type GetTopicsResult struct {
	Markdown string `json:"markdown" jsonschema:"Markdown representation of the topics table"`
}

// handleGetTopics queries OpenAlex for topic groups and formats them into a markdown table.
func (s *MCPServer) handleGetTopics(ctx context.Context, req *mcp.CallToolRequest, args GetTopicsArgs) (*mcp.CallToolResult, GetTopicsResult, error) {
	configPath := args.ConfigPath
	if configPath == "" {
		configPath = "data/db/config.db"
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		errStr := "failed to load config: " + err.Error()
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, GetTopicsResult{Markdown: ""}, nil
	}

	keywords := cfg.Keywords
	if errs := openalex.ValidateKeywords(keywords); len(errs) > 0 {
		errStr := "keyword validation failed: " + strings.Join(errs, "; ")
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: errStr},
			},
			IsError: true,
		}, GetTopicsResult{Markdown: ""}, nil
	}
	topics := cfg.Topics

	client := openalex.NewClient(cfg.API.Keys, cfg.API.Email, cfg.Collection.PerPage, cfg.Collection.ConcurrentRequests, cfg.Collection.MaxRetries, cfg.Collection.RetryDelay)

	// Build the OpenAlex filter
	parts := []string{"title_and_abstract.search:" + keywords}
	if len(topics) > 0 {
		parts = append(parts, "primary_topic.id:"+strings.Join(topics, "|"))
	}
	dateFrom := cfg.Filters.DateFrom
	if dateFrom == "" {
		dateFrom = "2003-01-01"
	}
	dateTo := cfg.Filters.DateTo
	if dateTo == "" {
		dateTo = "2024-12-31"
	}
	parts = append(parts, "from_publication_date:"+dateFrom)
	parts = append(parts, "to_publication_date:"+dateTo)
	if len(cfg.Filters.DocTypes) > 0 {
		parts = append(parts, "type:"+strings.Join(cfg.Filters.DocTypes, "|"))
	}
	filter := strings.Join(parts, ",")

	// Fetch all topic groups using cursor pagination
	var allGroups []openalex.GroupByItem
	cursor := "*"
	for {
		resp, err := client.FetchGroupBy(ctx, filter, "primary_topic.id", cursor)
		if err != nil {
			errStr := "OpenAlex request failed: " + err.Error()
			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: errStr},
				},
				IsError: true,
			}, GetTopicsResult{Markdown: ""}, nil
		}
		if resp == nil || len(resp.GroupBy) == 0 {
			break
		}
		allGroups = append(allGroups, resp.GroupBy...)
		if resp.Meta.NextCursor == "" || resp.Meta.NextCursor == cursor {
			break
		}
		cursor = resp.Meta.NextCursor
	}

	if len(allGroups) == 0 {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "No topics found matching the current keyword configurations."},
			},
		}, GetTopicsResult{Markdown: "No topics found."}, nil
	}

	// Sort groups by count descending
	sort.Slice(allGroups, func(i, j int) bool {
		return allGroups[i].Count > allGroups[j].Count
	})

	// Calculate total papers
	var totalPapers int
	for _, g := range allGroups {
		totalPapers += g.Count
	}

	type EnrichedTopic struct {
		TopicID     string
		DisplayName string
		Description string
		Count       int
		Percentage  float64
	}

	enriched := make([]EnrichedTopic, len(allGroups))
	var wg sync.WaitGroup

	for i, g := range allGroups {
		wg.Add(1)
		go func(idx int, item openalex.GroupByItem) {
			defer wg.Done()

			percentage := 0.0
			if totalPapers > 0 {
				percentage = float64(item.Count) / float64(totalPapers) * 100
			}

			topicID := item.Key
			if lastSlash := strings.LastIndex(topicID, "/"); lastSlash != -1 {
				topicID = topicID[lastSlash+1:]
			}

			eTopic := EnrichedTopic{
				TopicID:     topicID,
				DisplayName: item.KeyDisplayName,
				Count:       item.Count,
				Percentage:  percentage,
			}

			if args.Details {
				details, err := client.FetchTopicDetails(ctx, topicID)
				if err == nil && details != nil {
					if details.DisplayName != "" {
						eTopic.DisplayName = details.DisplayName
					}
					eTopic.Description = details.Description
				}
			}

			enriched[idx] = eTopic
		}(i, g)
	}
	wg.Wait()

	// Format results into a markdown table
	var md strings.Builder
	md.WriteString(fmt.Sprintf("## Topics found in search results (%d topics, %d papers total)\n\n", len(enriched), totalPapers))
	md.WriteString("| Topic ID | Topic Name | Description | Paper Count | Percentage |\n")
	md.WriteString("| :--- | :--- | :--- | :---: | :---: |\n")

	for _, t := range enriched {
		desc := t.Description
		if len(desc) > 80 {
			desc = desc[:77] + "..."
		}
		if desc == "" {
			desc = "—"
		}
		md.WriteString(fmt.Sprintf("| `%s` | %s | %s | %d | %.2f%% |\n", t.TopicID, t.DisplayName, desc, t.Count, t.Percentage))
	}

	markdownStr := md.String()

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: markdownStr},
		},
	}, GetTopicsResult{Markdown: markdownStr}, nil
}

func normalizeDOI(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.ToLower(s)
	for _, prefix := range []string{"https://doi.org/", "http://doi.org/", "doi:"} {
		if strings.HasPrefix(s, prefix) {
			s = s[len(prefix):]
			break
		}
	}
	return s
}

