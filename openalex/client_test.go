package openalex

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"stratum/config"
)

func TestNewClient(t *testing.T) {
	client := NewClient([]string{"key1", "key2"}, "test@example.com", 100, 5, 3, 1)
	if client == nil {
		t.Fatal("expected client, got nil")
	}
	if len(client.apiKeys) != 2 || client.apiKeys[0] != "key1" {
		t.Errorf("unexpected apiKeys: %v", client.apiKeys)
	}
	if client.email != "test@example.com" {
		t.Errorf("unexpected email: %s", client.email)
	}
	if client.perPage != 100 {
		t.Errorf("unexpected perPage: %d", client.perPage)
	}
	if client.concurrentRequests != 5 {
		t.Errorf("unexpected concurrentRequests: %d", client.concurrentRequests)
	}
	if client.maxRetries != 3 {
		t.Errorf("unexpected maxRetries: %d", client.maxRetries)
	}
	if client.retryDelay != 1 {
		t.Errorf("unexpected retryDelay: %d", client.retryDelay)
	}
}

func TestValidateKeywords(t *testing.T) {
	tests := []struct {
		input    string
		expected int // expected number of errors
	}{
		{"quantum AND physics", 0},
		{"(quantum OR computing) AND NOT silicon", 0},
		{"\"quantum computation\" AND space", 0},
		{"\"cloning and characterization\" AND gene", 0}, // lowercase and inside quotes is allowed
		{"", 1}, // empty
		{"(quantum", 1}, // unbalanced parens
		{"quantum and physics", 1}, // lowercase operator
		{"quantum OR OR physics", 1}, // adjacent operators
		{"()", 1}, // empty parens
		{"\"\"", 1}, // empty quotes
		{"\"   \"", 1}, // empty quotes with whitespace
		{"AND quantum", 1}, // starts with operator
		{"quantum OR", 1}, // ends with operator
		{"(AND quantum)", 1}, // dangling operator inside paren
		{"(quantum OR)", 1}, // dangling operator inside paren
		{"quantum; OR physics", 1}, // invalid semicolon
		{"quantum `computing`", 1}, // invalid backtick
		{"quantum 'physics'", 1}, // invalid single quote
	}

	for _, tc := range tests {
		errs := ValidateKeywords(tc.input)
		if (tc.expected == 0 && len(errs) > 0) || (tc.expected > 0 && len(errs) == 0) {
			t.Errorf("input %q: expected errors: %t, got %d errors: %v", tc.input, tc.expected > 0, len(errs), errs)
		}
	}
}

func TestValidateTopicFormat(t *testing.T) {
	tests := []struct {
		input    string
		expected bool
	}{
		{"T12345", true},
		{"T00001", true},
		{"T123", false},
		{"T123456", false},
		{"12345", false},
		{"t12345", false},
		{"T1234a", false},
	}

	for _, tc := range tests {
		res := ValidateTopicFormat(tc.input)
		if res != tc.expected {
			t.Errorf("input %q: expected %v, got %v", tc.input, tc.expected, res)
		}
	}
}

func TestGetTotalCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/works" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("filter") != "title_and_abstract.search:quantum" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"meta":{"count":1234},"results":[]}`)
	}))
	defer server.Close()

	client := NewClient(nil, "test@example.com", 200, 2, 2, 1)
	client.baseURL = server.URL

	count, err := client.GetTotalCount(context.Background(), "title_and_abstract.search:quantum")
	if err != nil {
		t.Fatalf("GetTotalCount failed: %v", err)
	}
	if count != 1234 {
		t.Errorf("expected 1234, got %d", count)
	}
}

func TestFetchPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"meta":{"count":1,"next_cursor":"next_cursor_val"},"results":[{"id":"W1","doi":"doi1","title":"title1"}]}`)
	}))
	defer server.Close()

	client := NewClient(nil, "test@example.com", 200, 2, 2, 1)
	client.baseURL = server.URL

	resp, err := client.FetchPage(context.Background(), "filter", "*")
	if err != nil {
		t.Fatalf("FetchPage failed: %v", err)
	}
	if resp.Meta.Count != 1 || resp.Meta.NextCursor != "next_cursor_val" {
		t.Errorf("unexpected meta: %+v", resp.Meta)
	}
	if len(resp.Results) != 1 || resp.Results[0].ID != "W1" || resp.Results[0].Title != "title1" {
		t.Errorf("unexpected results: %+v", resp.Results)
	}
}

func TestValidateTopicsExist(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/T10001") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"id":"https://api.openalex.org/topics/T10001"}`)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintln(w, `{"error":"Not Found"}`)
		}
	}))
	defer server.Close()

	client := NewClient(nil, "test@example.com", 200, 2, 2, 1)
	client.baseURL = server.URL

	res, err := ValidateTopicsExist(context.Background(), client, []string{"T10001", "T99999"})
	if err != nil {
		t.Fatalf("ValidateTopicsExist failed: %v", err)
	}
	if !res["T10001"] {
		t.Error("expected T10001 to exist")
	}
	if res["T99999"] {
		t.Error("expected T99999 to not exist")
	}
}

func TestDownloadPapers(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "stratum_download_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Mock server that returns W1 for page 1, and empty results for page 2
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		w.Header().Set("Content-Type", "application/json")
		if cursor == "*" {
			fmt.Fprintln(w, `{"meta":{"count":1,"next_cursor":"cursor_end"},"results":[{"id":"W1","title":"Paper 1"}]}`)
		} else {
			fmt.Fprintln(w, `{"meta":{"count":1,"next_cursor":""},"results":[]}`)
		}
	}))
	defer server.Close()

	client := NewClient(nil, "test@example.com", 2, 2, 2, 1)
	client.baseURL = server.URL

	cfg := &config.AppConfig{
		Keywords: "quantum",
		Topics:   []string{"T10001", "T10002"},
		Collection: config.CollectionConfig{
			BatchSizeTopics:    1,
			PerPage:            2,
			ConcurrentRequests: 2,
			MaxRetries:         2,
			RetryDelay:         1,
		},
		Filters: config.FiltersConfig{
			DateFrom: "2020-01-01",
			DateTo:   "2023-12-31",
		},
	}

	outputJSONL := filepath.Join(tmpDir, "output.jsonl")
	progressChan := make(chan int, 100)

	err = client.DownloadPapers(context.Background(), cfg, outputJSONL, progressChan)
	if err != nil {
		t.Fatalf("DownloadPapers failed: %v", err)
	}

	// Check if output file exists and has content
	data, err := os.ReadFile(outputJSONL)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 { // We have 2 topics, batch size 1. So 2 batches, each gets W1. Total 2 lines.
		t.Errorf("expected 2 lines in output JSONL, got %d", len(lines))
	}

	// Check progress file is deleted
	progressPath := outputJSONL + ".download_progress.json"
	if _, err := os.Stat(progressPath); !os.IsNotExist(err) {
		t.Error("expected download progress file to be deleted")
	}
}

func TestFetchGroupBy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("group_by") != "primary_topic.id" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"group_by":[{"key":"https://openalex.org/T10001","key_display_name":"Topic 1","count":10}],"meta":{"count":1,"next_cursor":""}}`)
	}))
	defer server.Close()

	client := NewClient(nil, "test@example.com", 200, 2, 2, 1)
	client.baseURL = server.URL

	resp, err := client.FetchGroupBy(context.Background(), "filter", "primary_topic.id", "*")
	if err != nil {
		t.Fatalf("FetchGroupBy failed: %v", err)
	}
	if len(resp.GroupBy) != 1 || resp.GroupBy[0].Key != "https://openalex.org/T10001" || resp.GroupBy[0].Count != 10 {
		t.Errorf("unexpected group_by results: %+v", resp.GroupBy)
	}
}

func TestFetchTopicDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/T10001") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"id":"https://openalex.org/T10001","display_name":"Topic 1","description":"Test Topic","keywords":["kw1","kw2"],"domain":{"display_name":"Domain 1"},"field":{"display_name":"Field 1"},"subfield":{"display_name":"Subfield 1"}}`)
	}))
	defer server.Close()

	client := NewClient(nil, "test@example.com", 200, 2, 2, 1)
	client.baseURL = server.URL

	details, err := client.FetchTopicDetails(context.Background(), "T10001")
	if err != nil {
		t.Fatalf("FetchTopicDetails failed: %v", err)
	}
	if details.DisplayName != "Topic 1" || details.Description != "Test Topic" || details.Domain.DisplayName != "Domain 1" {
		t.Errorf("unexpected topic details: %+v", details)
	}
}

