package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewDBManagerAndClose(t *testing.T) {
	// Create temporary directory for test database
	tmpDir, err := os.MkdirTemp("", "stratum_db_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.duckdb")

	// Verify manager creation
	mgr, err := NewDBManager(dbPath)
	if err != nil {
		t.Fatalf("NewDBManager failed: %v", err)
	}

	if mgr == nil {
		t.Fatal("expected DBManager instance, got nil")
	}

	if mgr.dbPath != dbPath {
		t.Errorf("expected dbPath '%s', got '%s'", dbPath, mgr.dbPath)
	}

	if mgr.db == nil {
		t.Error("expected database connection, got nil")
	}

	// Verify manager close
	if err := mgr.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}

	// Verify database file exists
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Errorf("expected database file to exist at %s, but it does not", dbPath)
	}
}

func TestCreateSchema(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "stratum_db_schema_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.duckdb")

	mgr, err := NewDBManager(dbPath)
	if err != nil {
		t.Fatalf("NewDBManager failed: %v", err)
	}
	defer mgr.Close()

	if err := mgr.CreateSchema(); err != nil {
		t.Fatalf("CreateSchema failed: %v", err)
	}

	// Verify tables are created in main schema
	expectedTables := map[string]bool{
		"papers":        false,
		"authors":       false,
		"institutions":  false,
		"countries":     false,
		"contributions": false,
	}

	rows, err := mgr.db.Query("SELECT table_name FROM information_schema.tables WHERE table_schema = 'main'")
	if err != nil {
		t.Fatalf("failed to query information_schema: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("failed to scan table name: %v", err)
		}
		if _, ok := expectedTables[name]; ok {
			expectedTables[name] = true
		}
	}

	for k, found := range expectedTables {
		if !found {
			t.Errorf("expected table '%s' was not found in schema", k)
		}
	}
}

func TestLoadJSONL(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "stratum_db_load_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.duckdb")
	mgr, err := NewDBManager(dbPath)
	if err != nil {
		t.Fatalf("NewDBManager failed: %v", err)
	}
	defer mgr.Close()

	if err := mgr.CreateSchema(); err != nil {
		t.Fatalf("CreateSchema failed: %v", err)
	}

	mockJSONL := `{"id": "https://api.openalex.org/works/W42839485", "doi": "https://doi.org/10.1103/physrevlett.120.010501", "title": "Quantum Supremacy Demo", "publication_year": 2018, "publication_date": "2018-01-02", "type": "article", "primary_location": {"source": {"display_name": "Physical Review Letters", "issn_l": "0031-9007", "is_core": true, "host_organization_name": "American Physical Society"}}, "open_access": {"is_oa": true, "oa_status": "green", "oa_url": "https://arxiv.org/pdf/1701.01234.pdf"}, "cited_by_count": 42, "citation_normalized_percentile": {"value": 0.99, "is_in_top_1_percent": true, "is_in_top_10_percent": true}, "fwci": 3.5, "primary_topic": {"id": "https://api.openalex.org/topics/T10020", "display_name": "Quantum Computation", "score": 0.98, "field": { "display_name": "Physics" }, "subfield": { "display_name": "Quantum Physics" }, "domain": { "display_name": "Physical Sciences" }}, "institutions_distinct_count": 2, "countries_distinct_count": 2, "abstract_inverted_index": {"We": [0], "demonstrate": [1], "quantum": [2], "supremacy.": [3]}, "updated_date": "2024-02-01", "authorships": [{"author": {"id": "https://api.openalex.org/authors/A111", "display_name": "Alice Smith", "orcid": "https://orcid.org/0000-0001-2345-6789"}, "institutions": [{"id": "https://api.openalex.org/institutions/I111", "display_name": "MIT", "country_code": "US", "type": "education", "ror": "https://ror.org/021nxhr62"}], "raw_affiliation_strings": ["Dept of Physics, MIT, USA"], "raw_author_name": "Alice Smith", "author_position": "first", "is_corresponding": true}, {"author": {"id": "https://api.openalex.org/authors/A222", "display_name": "Bob Jones", "orcid": ""}, "institutions": [{"id": "https://api.openalex.org/institutions/I222", "display_name": "Tsinghua University", "country_code": "CN", "type": "education", "ror": "https://ror.org/03cvepc15"}], "raw_affiliation_strings": ["Tsinghua University, Beijing, China"], "raw_author_name": "Bob Jones", "author_position": "last", "is_corresponding": false}]}`
	jsonlPath := filepath.Join(tmpDir, "openalex.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(mockJSONL), 0644); err != nil {
		t.Fatalf("failed to write mock jsonl: %v", err)
	}

	progressChan := make(chan int, 10)
	stats, err := mgr.LoadJSONL(jsonlPath, progressChan)
	if err != nil {
		t.Fatalf("LoadJSONL failed: %v", err)
	}

	if stats.Papers != 1 {
		t.Errorf("expected 1 paper loaded, got %d", stats.Papers)
	}
	if stats.Authors != 2 {
		t.Errorf("expected 2 authors loaded, got %d", stats.Authors)
	}
	if stats.Institutions != 2 {
		t.Errorf("expected 2 institutions loaded, got %d", stats.Institutions)
	}
	if stats.Countries != 2 { // US, CN
		t.Errorf("expected 2 countries loaded, got %d", stats.Countries)
	}
	if stats.Contributions != 2 {
		t.Errorf("expected 2 contributions loaded, got %d", stats.Contributions)
	}

	// Verify database record contents
	var title, abstract string
	var cited int
	err = mgr.db.QueryRow("SELECT title, abstract_text, cited_by_count FROM papers WHERE id = 'W42839485'").Scan(&title, &abstract, &cited)
	if err != nil {
		t.Fatalf("failed to query paper: %v", err)
	}
	if title != "Quantum Supremacy Demo" {
		t.Errorf("expected title 'Quantum Supremacy Demo', got '%s'", title)
	}
	if abstract != "We demonstrate quantum supremacy." {
		t.Errorf("expected reconstructed abstract 'We demonstrate quantum supremacy.', got '%s'", abstract)
	}
	if cited != 42 {
		t.Errorf("expected cited count 42, got %d", cited)
	}

	// Verify authors
	var authorName string
	err = mgr.db.QueryRow("SELECT display_name FROM authors WHERE id = 'A111'").Scan(&authorName)
	if err != nil {
		t.Fatalf("failed to query author: %v", err)
	}
	if authorName != "Alice Smith" {
		t.Errorf("expected author name 'Alice Smith', got '%s'", authorName)
	}

	// Verify contributions
	var instID, cc, rawAff string
	rows, err := mgr.db.Query("SELECT institution_id, country_code, raw_affiliation_string FROM contributions WHERE paper_id = 'W42839485' ORDER BY author_position")
	if err != nil {
		t.Fatalf("failed to query contributions: %v", err)
	}
	defer rows.Close()

	// First author: Alice (MIT, US, Dept of Physics...)
	if !rows.Next() {
		t.Fatal("expected first contribution row")
	}
	if err := rows.Scan(&instID, &cc, &rawAff); err != nil {
		t.Fatalf("scan contribution failed: %v", err)
	}
	if instID != "I111" || cc != "US" || rawAff != "Dept of Physics, MIT, USA" {
		t.Errorf("unexpected contribution values: instID=%s, cc=%s, rawAff=%s", instID, cc, rawAff)
	}

	// Second author: Bob (Tsinghua, CN)
	if !rows.Next() {
		t.Fatal("expected second contribution row")
	}
	if err := rows.Scan(&instID, &cc, &rawAff); err != nil {
		t.Fatalf("scan contribution failed: %v", err)
	}
	if instID != "I222" || cc != "CN" || rawAff != "Tsinghua University, Beijing, China" {
		t.Errorf("unexpected contribution values: instID=%s, cc=%s, rawAff=%s", instID, cc, rawAff)
	}

	// Test GetDashboardStats
	statsDash, err := mgr.GetDashboardStats()
	if err != nil {
		t.Fatalf("GetDashboardStats failed: %v", err)
	}

	if statsDash.TotalPapers != 1 {
		t.Errorf("expected 1 total paper in stats, got %d", statsDash.TotalPapers)
	}
	if statsDash.TotalAuthors != 2 {
		t.Errorf("expected 2 total authors in stats, got %d", statsDash.TotalAuthors)
	}
	if statsDash.TotalInstitutions != 2 {
		t.Errorf("expected 2 total institutions in stats, got %d", statsDash.TotalInstitutions)
	}
	if statsDash.TotalCountries != 2 {
		t.Errorf("expected 2 total countries in stats, got %d", statsDash.TotalCountries)
	}

	if len(statsDash.PapersByYear) != 1 || statsDash.PapersByYear[0].Year != 2018 || statsDash.PapersByYear[0].Count != 1 {
		t.Errorf("unexpected PapersByYear stats: %+v", statsDash.PapersByYear)
	}

	if len(statsDash.OAStatusCounts) != 1 || statsDash.OAStatusCounts[0].Status != "green" || statsDash.OAStatusCounts[0].Count != 1 {
		t.Errorf("unexpected OAStatusCounts stats: %+v", statsDash.OAStatusCounts)
	}

	if len(statsDash.TopJournals) != 1 || statsDash.TopJournals[0].JournalName != "Physical Review Letters" || statsDash.TopJournals[0].Count != 1 {
		t.Errorf("unexpected TopJournals stats: %+v", statsDash.TopJournals)
	}

	if len(statsDash.CountryCounts) != 2 {
		t.Errorf("expected 2 country counts, got %d: %+v", len(statsDash.CountryCounts), statsDash.CountryCounts)
	}

	// Test RunQuery
	res, err := mgr.RunQuery("SELECT publication_year, COUNT(*) as c FROM papers GROUP BY publication_year")
	if err != nil {
		t.Fatalf("RunQuery failed: %v", err)
	}
	if len(res) != 1 {
		t.Errorf("expected 1 result row, got %d", len(res))
	}
	row := res[0]
	var year int64
	switch v := row["publication_year"].(type) {
	case int64:
		year = v
	case int32:
		year = int64(v)
	case int:
		year = int64(v)
	default:
		t.Errorf("unexpected type for publication_year: %T", row["publication_year"])
	}
	if year != 2018 {
		t.Errorf("expected publication_year 2018, got %d", year)
	}

	var count int64
	switch v := row["c"].(type) {
	case int64:
		count = v
	case int32:
		count = int64(v)
	case int:
		count = int64(v)
	default:
		t.Errorf("unexpected type for c: %T", row["c"])
	}
	if count != 1 {
		t.Errorf("expected count 1, got %d", count)
	}
}
