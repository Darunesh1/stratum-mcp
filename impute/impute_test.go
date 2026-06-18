package impute

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/duckdb/duckdb-go/v2"
)

// Helper to setup a temp DuckDB database for testing
func setupTestDB(t *testing.T) (string, *sql.DB) {
	tmpFile, err := os.CreateTemp("", "stratum_test_*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	dbPath := tmpFile.Name()
	tmpFile.Close()
	os.Remove(dbPath)

	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		os.Remove(dbPath)
		t.Fatalf("failed to open duckdb: %v", err)
	}

	queries := []string{
		`CREATE TABLE IF NOT EXISTS papers (
			id VARCHAR PRIMARY KEY, doi VARCHAR, title TEXT,
			publication_year INTEGER, publication_date VARCHAR, type VARCHAR,
			journal_name VARCHAR, journal_issn VARCHAR, is_core_journal BOOLEAN,
			publisher VARCHAR, is_oa BOOLEAN, oa_status VARCHAR, oa_url VARCHAR,
			cited_by_count INTEGER, citation_percentile DOUBLE,
			is_top_1_percent BOOLEAN, is_top_10_percent BOOLEAN, fwci DOUBLE,
			primary_topic_id VARCHAR, primary_topic_name VARCHAR, primary_topic_score DOUBLE,
			primary_topic_field VARCHAR, primary_topic_subfield VARCHAR, primary_topic_domain VARCHAR,
			institutions_distinct_count INTEGER, countries_distinct_count INTEGER,
			is_international BOOLEAN, abstract_text TEXT, updated_date VARCHAR
		)`,
		`CREATE TABLE IF NOT EXISTS authors (
			id VARCHAR PRIMARY KEY, display_name VARCHAR, orcid VARCHAR
		)`,
		`CREATE TABLE IF NOT EXISTS institutions (
			id VARCHAR PRIMARY KEY, display_name VARCHAR,
			country_code VARCHAR, type VARCHAR, ror_id VARCHAR,
			is_synthetic BOOLEAN DEFAULT FALSE
		)`,
		`CREATE TABLE IF NOT EXISTS countries (
			id INTEGER PRIMARY KEY, country_name VARCHAR, country_code VARCHAR UNIQUE, status INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS contributions (
			row_id INTEGER PRIMARY KEY, paper_id VARCHAR, author_id VARCHAR,
			institution_id VARCHAR, country_code VARCHAR, author_name VARCHAR,
			author_position VARCHAR, is_corresponding BOOLEAN, raw_affiliation_string VARCHAR
		)`,
	}

	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			os.Remove(dbPath)
			t.Fatalf("failed to create schema query %s: %v", q, err)
		}
	}

	return dbPath, db
}

func teardownTestDB(dbPath string, db *sql.DB) {
	if db != nil {
		db.Close()
	}
	os.Remove(dbPath)
	os.Remove(dbPath + ".tmp")
	os.Remove(dbPath + ".wal")
}

// RedirectTransport intercepts HTTP client calls and redirects them to the test server
type redirectTransport struct {
	targetURL     *url.URL
	origTransport http.RoundTripper
}

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Preserve host header or path if needed, but point URL to local mock server
	req.URL.Scheme = t.targetURL.Scheme
	req.URL.Host = t.targetURL.Host
	return t.origTransport.RoundTrip(req)
}

// Generates a valid minimal PDF containing the given text
func generateMinimalPDF(text string) []byte {
	header := "%PDF-1.4\n"
	streamContent := fmt.Sprintf("BT\n/F1 12 Tf\n72 712 Td\n(%s) Tj\nET", text)
	objs := []string{
		"1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n",
		"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n",
		"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>\nendobj\n",
		"4 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n",
		fmt.Sprintf("5 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(streamContent), streamContent),
	}

	var sb strings.Builder
	sb.WriteString(header)
	offsets := make([]int, len(objs))
	currentOffset := len(header)
	for i, obj := range objs {
		offsets[i] = currentOffset
		sb.WriteString(obj)
		currentOffset += len(obj)
	}

	xrefOffset := currentOffset
	sb.WriteString("xref\n0 6\n")
	sb.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets {
		sb.WriteString(fmt.Sprintf("%010d 00000 n \n", offset))
	}

	sb.WriteString(fmt.Sprintf("trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefOffset))
	return []byte(sb.String())
}

func TestNormalizeCountryCode(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"HK", "CN"},
		{"hk", "CN"},
		{"US", "US"},
		{"  ca  ", "CA"},
	}

	for _, tc := range tests {
		got := NormalizeCountryCode(tc.input)
		if got != tc.expected {
			t.Errorf("NormalizeCountryCode(%q) = %q; want %q", tc.input, got, tc.expected)
		}
	}
}

func TestInferCountryFromAffiliation(t *testing.T) {
	tests := []struct {
		affiliation string
		expectedCode string
		expectedStatus string
	}{
		{"Harvard University, Cambridge, USA", "US", "unambiguous"},
		{"Sorbonne, Paris, France", "FR", "unambiguous"},
		{"UK and Germany cooperation", "", "ambiguous"},
		{"Unknown department", "", "none"},
		{"", "", "none"},
	}

	for _, tc := range tests {
		got := InferCountryFromAffiliation(tc.affiliation)
		if got.CountryCode != tc.expectedCode || got.Status != tc.expectedStatus {
			t.Errorf("InferCountryFromAffiliation(%q) = (%q, %q); want (%q, %q)",
				tc.affiliation, got.CountryCode, got.Status, tc.expectedCode, tc.expectedStatus)
		}
	}
}

func TestSyntheticInstitutionID(t *testing.T) {
	id1 := SyntheticInstitutionID("Stanford University")
	id2 := SyntheticInstitutionID("stanford university  ")
	id3 := SyntheticInstitutionID("MIT")

	if id1 == "" || !strings.HasPrefix(id1, "IMP_") {
		t.Errorf("expected IMP_ prefix for ID: %q", id1)
	}
	if id1 != id2 {
		t.Errorf("expected ID to be stable under case and spacing: %q vs %q", id1, id2)
	}
	if id1 == id3 {
		t.Errorf("different names should result in different IDs")
	}
}

func TestInstitutionMatcher(t *testing.T) {
	records := []InstitutionRecord{
		{ID: "inst-1", DisplayName: "Harvard University", CountryCode: "US"},
		{ID: "inst-2", DisplayName: "Tsinghua University", CountryCode: "CN"},
	}

	matcher := NewInstitutionMatcher("test")
	err := matcher.Index(records)
	if err != nil {
		t.Fatalf("failed to index: %v", err)
	}

	// Test TopK
	matches, err := matcher.TopK("harvard", 2)
	if err != nil {
		t.Fatalf("TopK failed: %v", err)
	}
	if len(matches) == 0 {
		t.Fatalf("expected matches, got 0")
	}
	if matches[0].InstitutionID != "inst-1" {
		t.Errorf("expected inst-1 as best match, got %s", matches[0].InstitutionID)
	}

	// Test FindMatch with high threshold
	bestMatch := matcher.FindMatch("harvard university", 0.9)
	if bestMatch == nil {
		t.Errorf("expected non-nil match")
	} else if bestMatch.InstitutionID != "inst-1" {
		t.Errorf("expected inst-1, got %s", bestMatch.InstitutionID)
	}

	// Test FindMatch with no good matches
	noMatch := matcher.FindMatch("random hospital", 0.9)
	if noMatch != nil {
		t.Errorf("expected nil match, got %v", noMatch)
	}
}

func TestImputeCrossRef(t *testing.T) {
	dbPath, db := setupTestDB(t)
	defer teardownTestDB(dbPath, db)

	// Insert test data: paper with DOI, contribution with NULL raw_affiliation_string, and an author with ORCID
	_, err := db.Exec(`
		INSERT INTO papers (id, doi) VALUES ('p-123', '10.1002/test.123')
	`)
	if err != nil {
		t.Fatalf("failed to insert paper: %v", err)
	}

	_, err = db.Exec(`
		INSERT INTO authors (id, display_name, orcid) VALUES ('a-123', 'John Doe', '0000-0002-1825-0097')
	`)
	if err != nil {
		t.Fatalf("failed to insert author: %v", err)
	}

	_, err = db.Exec(`
		INSERT INTO contributions (row_id, paper_id, author_id, raw_affiliation_string, institution_id, country_code)
		VALUES (1, 'p-123', 'a-123', NULL, NULL, NULL)
	`)
	if err != nil {
		t.Fatalf("failed to insert contribution: %v", err)
	}

	// Mock Crossref server response
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "10.1002/test.123") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		payload := map[string]interface{}{
			"message": map[string]interface{}{
				"author": []map[string]interface{}{
					{
						"ORCID": "https://orcid.org/0000-0002-1825-0097",
						"affiliation": []map[string]interface{}{
							{"name": "Stanford University"},
						},
					},
				},
			},
		}
		json.NewEncoder(w).Encode(payload)
	}))
	defer ts.Close()

	// Redirect HTTP calls to test server
	mockURL, _ := url.Parse(ts.URL)
	origTransport := http.DefaultTransport
	http.DefaultTransport = &redirectTransport{targetURL: mockURL, origTransport: origTransport}
	defer func() { http.DefaultTransport = origTransport }()

	engine := NewImputationEngine(dbPath)
	ctx := context.Background()
	progress := make(chan int, 10)

	err = engine.ImputeCrossRef(ctx, progress)
	if err != nil {
		t.Fatalf("ImputeCrossRef failed: %v", err)
	}

	// Verify update
	var rawAff sql.NullString
	err = db.QueryRow("SELECT raw_affiliation_string FROM contributions WHERE row_id = 1").Scan(&rawAff)
	if err != nil {
		t.Fatalf("failed to query contribution: %v", err)
	}
	if !rawAff.Valid || rawAff.String != "Stanford University" {
		t.Errorf("expected raw_affiliation_string to be 'Stanford University', got %q", rawAff.String)
	}

	// Verify audit log
	var auditCount int
	err = db.QueryRow("SELECT COUNT(*) FROM country_imputation_audit").Scan(&auditCount)
	if err != nil {
		t.Fatalf("failed to query audit: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("expected 1 audit log entry, got %d", auditCount)
	}
}

func TestImputeLLM(t *testing.T) {
	dbPath, db := setupTestDB(t)
	defer teardownTestDB(dbPath, db)

	// Setup database records
	// Contribution 1: to be imputed via LLM (Stage 1)
	_, err := db.Exec(`
		INSERT INTO contributions (row_id, paper_id, author_id, raw_affiliation_string, institution_id, country_code)
		VALUES (1, 'p-1', 'a-1', 'Stanford University, USA', NULL, NULL)
	`)
	if err != nil {
		t.Fatalf("failed to insert contribution 1: %v", err)
	}

	// Institution for Stage 2 backfill test
	_, err = db.Exec(`
		INSERT INTO institutions (id, display_name, country_code, type, is_synthetic)
		VALUES ('inst-tokyo', 'University of Tokyo, Japan', NULL, 'education', FALSE)
	`)
	if err != nil {
		t.Fatalf("failed to insert institution: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO contributions (row_id, paper_id, author_id, raw_affiliation_string, institution_id, country_code)
		VALUES (2, 'p-2', 'a-2', 'University of Tokyo, Japan', 'inst-tokyo', NULL)
	`)
	if err != nil {
		t.Fatalf("failed to insert contribution 2: %v", err)
	}

	// Contribution 3: Stage 3 rule matching (offline)
	_, err = db.Exec(`
		INSERT INTO contributions (row_id, paper_id, author_id, raw_affiliation_string, institution_id, country_code)
		VALUES (3, 'p-3', 'a-3', 'Sorbonne University, Paris, France', NULL, NULL)
	`)
	if err != nil {
		t.Fatalf("failed to insert contribution 3: %v", err)
	}

	// Contribution 4: Stage 3 LLM fallback matching (offline none, resolved online)
	_, err = db.Exec(`
		INSERT INTO contributions (row_id, paper_id, author_id, raw_affiliation_string, institution_id, country_code)
		VALUES (4, 'p-4', 'a-4', 'Obscure organization text', NULL, NULL)
	`)
	if err != nil {
		t.Fatalf("failed to insert contribution 4: %v", err)
	}

	// Mock Ollama Server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Read request prompt
		var reqBody struct {
			Prompt string `json:"prompt"`
		}
		json.NewDecoder(r.Body).Decode(&reqBody)

		var respText string
		if strings.Contains(reqBody.Prompt, "extract the primary institution name") {
			// Stage 1 output
			predJSON, _ := json.Marshal(map[string]interface{}{
				"predictions": []map[string]interface{}{
					{
						"row_id":           1,
						"institution_name": "Stanford University",
						"country_code":     "US",
						"confidence":       0.95,
					},
				},
			})
			respText = string(predJSON)
		} else if strings.Contains(reqBody.Prompt, "extract the ISO-3166-1 alpha-2 country code") {
			// Stage 3 output
			predJSON, _ := json.Marshal(map[string]interface{}{
				"predictions": []map[string]interface{}{
					{
						"row_id":       4,
						"country_code": "CA",
						"status":       "unambiguous",
						"confidence":   0.88,
					},
				},
			})
			respText = string(predJSON)
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"response": respText,
		})
	}))
	defer ts.Close()

	engine := NewImputationEngine(dbPath)
	ctx := context.Background()
	progress := make(chan int, 10)

	err = engine.ImputeLLM(ctx, "ollama", "llama3", ts.URL, progress)
	if err != nil {
		t.Fatalf("ImputeLLM failed: %v", err)
	}

	// Verify Stage 1
	var instID, countryCode sql.NullString
	err = db.QueryRow("SELECT institution_id, country_code FROM contributions WHERE row_id = 1").Scan(&instID, &countryCode)
	if err != nil {
		t.Fatalf("failed to query row 1: %v", err)
	}
	if !instID.Valid || !strings.HasPrefix(instID.String, "IMP_") {
		t.Errorf("expected synthetic institution ID starting with IMP_, got %q", instID.String)
	}
	if !countryCode.Valid || countryCode.String != "US" {
		t.Errorf("expected country_code US, got %q", countryCode.String)
	}

	// Verify Stage 2 (Tokyo backfill)
	var instCountry sql.NullString
	err = db.QueryRow("SELECT country_code FROM institutions WHERE id = 'inst-tokyo'").Scan(&instCountry)
	if err != nil {
		t.Fatalf("failed to query institution tokyo: %v", err)
	}
	if !instCountry.Valid || instCountry.String != "JP" {
		t.Errorf("expected Tokyo institution country code JP, got %q", instCountry.String)
	}

	var contribCountry sql.NullString
	err = db.QueryRow("SELECT country_code FROM contributions WHERE row_id = 2").Scan(&contribCountry)
	if err != nil {
		t.Fatalf("failed to query Tokyo contribution: %v", err)
	}
	if !contribCountry.Valid || contribCountry.String != "JP" {
		t.Errorf("expected Tokyo contribution country code JP, got %q", contribCountry.String)
	}

	// Verify Stage 3 offline rule match (France)
	var c3Country sql.NullString
	err = db.QueryRow("SELECT country_code FROM contributions WHERE row_id = 3").Scan(&c3Country)
	if err != nil {
		t.Fatalf("failed to query France contribution: %v", err)
	}
	if !c3Country.Valid || c3Country.String != "FR" {
		t.Errorf("expected France offline rule match FR, got %q", c3Country.String)
	}

	// Verify Stage 3 LLM fallback match (CA)
	var c4Country sql.NullString
	err = db.QueryRow("SELECT country_code FROM contributions WHERE row_id = 4").Scan(&c4Country)
	if err != nil {
		t.Fatalf("failed to query fallback contribution: %v", err)
	}
	if !c4Country.Valid || c4Country.String != "CA" {
		t.Errorf("expected fallback LLM match CA, got %q", c4Country.String)
	}
}

func TestImputePDF(t *testing.T) {
	dbPath, db := setupTestDB(t)
	defer teardownTestDB(dbPath, db)

	// Ensure cleaning up the cache directory generated by the PDF download
	defer os.RemoveAll("data")

	// Insert paper and contribution
	_, err := db.Exec(`
		INSERT INTO papers (id, doi) VALUES ('p-pdf', '10.48550/arxiv.2101.12345')
	`)
	if err != nil {
		t.Fatalf("failed to insert paper: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO contributions (row_id, paper_id, author_id, raw_affiliation_string, institution_id, country_code)
		VALUES (10, 'p-pdf', 'a-pdf', NULL, NULL, NULL)
	`)
	if err != nil {
		t.Fatalf("failed to insert contribution: %v", err)
	}

	// Mock Server serving both PDF and LLM requests
	pdfBytes := generateMinimalPDF("John Doe, Stanford University, US")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".pdf") {
			// Serve mock PDF
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(pdfBytes)
			return
		}

		if r.URL.Path == "/api/generate" {
			w.Header().Set("Content-Type", "application/json")
			var reqBody struct {
				Prompt string `json:"prompt"`
			}
			json.NewDecoder(r.Body).Decode(&reqBody)

			var respText string
			if strings.Contains(reqBody.Prompt, "You are given the first page") {
				author := "John"
				family := "Doe"
				orcid := ""
				inst := "Stanford University"
				cc := "US"
				predJSON, _ := json.Marshal(map[string]interface{}{
					"authors": []map[string]interface{}{
						{
							"position":     0,
							"given":        &author,
							"family":       &family,
							"orcid":        &orcid,
							"institution":  &inst,
							"country_code": &cc,
							"confidence":   0.99,
						},
					},
				})
				respText = string(predJSON)
			}

			json.NewEncoder(w).Encode(map[string]interface{}{
				"response": respText,
			})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	// Redirect HTTP calls to test server
	mockURL, _ := url.Parse(ts.URL)
	origTransport := http.DefaultTransport
	http.DefaultTransport = &redirectTransport{targetURL: mockURL, origTransport: origTransport}
	defer func() { http.DefaultTransport = origTransport }()

	engine := NewImputationEngine(dbPath)
	ctx := context.Background()
	progress := make(chan int, 10)

	err = engine.ImputePDF(ctx, "ollama", "llama3", ts.URL, 0, progress)
	if err != nil {
		t.Fatalf("ImputePDF failed: %v", err)
	}

	// Verify that the contribution was updated
	var instID, countryCode sql.NullString
	err = db.QueryRow("SELECT institution_id, country_code FROM contributions WHERE row_id = 10").Scan(&instID, &countryCode)
	if err != nil {
		t.Fatalf("failed to query contribution: %v", err)
	}
	if !instID.Valid || !strings.HasPrefix(instID.String, "IMP_") {
		t.Errorf("expected synthetic institution ID starting with IMP_, got %q", instID.String)
	}
	if !countryCode.Valid || countryCode.String != "US" {
		t.Errorf("expected country_code US, got %q", countryCode.String)
	}
}
