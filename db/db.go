package db

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"
	"stratum/openalex"
)

// DBManager wraps the DuckDB connection and handles schema initialization and batch insertions.
type DBManager struct {
	dbPath string
	db     *sql.DB
}

// LoadStats records loaded rows and error counts during the JSONL processing.
type LoadStats struct {
	Papers        int `json:"papers"`
	Authors       int `json:"authors"`
	Institutions  int `json:"institutions"`
	Countries     int `json:"countries"`
	Contributions int `json:"contributions"`
	Errors        int `json:"errors"`
	Skipped       int `json:"skipped"`
}

// DashboardStats stores top-level aggregate values to power the web dashboard.
type DashboardStats struct {
	TotalPapers       int            `json:"total_papers"`
	TotalAuthors      int            `json:"total_authors"`
	TotalInstitutions int            `json:"total_institutions"`
	TotalCountries    int            `json:"total_countries"`
	PapersByYear      []YearStat     `json:"papers_by_year"`
	OAStatusCounts    []OAStatusStat `json:"oa_status_counts"`
	TopJournals       []JournalStat  `json:"top_journals"`
	CountryCounts     []CountryStat  `json:"country_counts"`
}

type YearStat struct {
	Year  int `json:"year"`
	Count int `json:"count"`
}

type OAStatusStat struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

type JournalStat struct {
	JournalName string `json:"journal_name"`
	Count       int    `json:"count"`
}

type CountryStat struct {
	CountryCode string `json:"country_code"`
	Count       int    `json:"count"`
}

// NewDBManager initializes a new database manager.
func NewDBManager(dbPath string) (*DBManager, error) {
	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &DBManager{
		dbPath: dbPath,
		db:     db,
	}, nil
}

// Close closes the underlying DuckDB database connection.
func (m *DBManager) Close() error {
	if m.db != nil {
		return m.db.Close()
	}
	return nil
}

// CreateSchema initializes the database tables (papers, authors, institutions, countries, contributions).
func (m *DBManager) CreateSchema() error {
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
		if _, err := m.db.Exec(q); err != nil {
			return err
		}
	}

	// Initialize contribution sequence seq_contrib
	var maxID int
	err := m.db.QueryRow("SELECT COALESCE(MAX(row_id), 0) FROM contributions").Scan(&maxID)
	if err != nil {
		maxID = 0
	}
	m.db.Exec("DROP SEQUENCE IF EXISTS seq_contrib")
	_, err = m.db.Exec(fmt.Sprintf("CREATE SEQUENCE seq_contrib START %d", maxID+1))
	return err
}

// LoadJSONL parses a downloaded JSONL file and loads normalized records into DuckDB with progress updates.
func (m *DBManager) LoadJSONL(jsonlPath string, progressChan chan<- int) (*LoadStats, error) {
	file, err := os.Open(jsonlPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	stats := &LoadStats{}

	// Load existing caches
	countryCache := make(map[string]bool)
	authorCache := make(map[string]bool)
	institutionCache := make(map[string]bool)

	// Load existing country codes
	rows, _ := m.db.Query("SELECT country_code FROM countries WHERE country_code IS NOT NULL")
	if rows != nil {
		for rows.Next() {
			var cc string
			if err := rows.Scan(&cc); err == nil {
				countryCache[cc] = true
			}
		}
		rows.Close()
	}

	// Load existing authors
	rows, _ = m.db.Query("SELECT id FROM authors")
	if rows != nil {
		for rows.Next() {
			var aid string
			if err := rows.Scan(&aid); err == nil {
				authorCache[aid] = true
			}
		}
		rows.Close()
	}

	// Load existing institutions
	rows, _ = m.db.Query("SELECT id FROM institutions")
	if rows != nil {
		for rows.Next() {
			var iid string
			if err := rows.Scan(&iid); err == nil {
				institutionCache[iid] = true
			}
		}
		rows.Close()
	}

	// Get next country ID
	var maxCountryID int
	m.db.QueryRow("SELECT COALESCE(MAX(id), 0) FROM countries").Scan(&maxCountryID)

	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Prepare statements
	stmtPaper, err := tx.Prepare(`INSERT INTO papers (
		id, doi, title, publication_year, publication_date, type,
		journal_name, journal_issn, is_core_journal, publisher,
		is_oa, oa_status, oa_url, cited_by_count, citation_percentile,
		is_top_1_percent, is_top_10_percent, fwci,
		primary_topic_id, primary_topic_name, primary_topic_score,
		primary_topic_field, primary_topic_subfield, primary_topic_domain,
		institutions_distinct_count, countries_distinct_count, is_international,
		abstract_text, updated_date
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`)
	if err != nil {
		return nil, err
	}
	defer stmtPaper.Close()

	stmtAuthor, err := tx.Prepare(`INSERT INTO authors (id, display_name, orcid) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`)
	if err != nil {
		return nil, err
	}
	defer stmtAuthor.Close()

	stmtInstitution, err := tx.Prepare(`INSERT INTO institutions (id, display_name, country_code, type, ror_id, is_synthetic) VALUES (?, ?, ?, ?, ?, FALSE) ON CONFLICT DO NOTHING`)
	if err != nil {
		return nil, err
	}
	defer stmtInstitution.Close()

	stmtCountry, err := tx.Prepare(`INSERT INTO countries (id, country_name, country_code, status) VALUES (?, ?, ?, 1) ON CONFLICT (country_code) DO NOTHING`)
	if err != nil {
		return nil, err
	}
	defer stmtCountry.Close()

	stmtContribution, err := tx.Prepare(`INSERT INTO contributions (row_id, paper_id, author_id, institution_id, country_code, author_name, author_position, is_corresponding, raw_affiliation_string) VALUES (nextval('seq_contrib'), ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}
	defer stmtContribution.Close()

	ensureCountry := func(code string) string {
		if code == "" {
			return ""
		}
		norm := normalizeCountryCode(code)
		if norm == "" {
			return ""
		}
		if countryCache[norm] {
			return norm
		}
		maxCountryID++
		_, err := stmtCountry.Exec(maxCountryID, "["+norm+"]", norm)
		if err == nil {
			countryCache[norm] = true
			stats.Countries++
		}
		return norm
	}

	scanner := bufio.NewScanner(file)
	const maxCapacity = 10 * 1024 * 1024 // 10MB
	buf := make([]byte, maxCapacity)
	scanner.Buffer(buf, maxCapacity)

	lineCount := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineCount++

		var w openalex.Work
		if err := json.Unmarshal([]byte(line), &w); err != nil {
			stats.Errors++
			continue
		}

		paperID := extractID(w.ID)
		if paperID == "" {
			stats.Skipped++
			continue
		}

		var exists int
		tx.QueryRow("SELECT 1 FROM papers WHERE id = ? LIMIT 1", paperID).Scan(&exists)
		if exists == 1 {
			stats.Skipped++
			continue
		}

		abstract := reconstructAbstract(w.AbstractInvertedIndex)
		isInternational := w.CountriesDistinctCount > 1

		topicID := extractID(w.PrimaryTopic.ID)
		topicName := w.PrimaryTopic.DisplayName
		topicScore := w.PrimaryTopic.Score
		topicField := w.PrimaryTopic.Field.DisplayName
		topicSubfield := w.PrimaryTopic.Subfield.DisplayName
		topicDomain := w.PrimaryTopic.Domain.DisplayName

		journalName := w.PrimaryLocation.Source.DisplayName
		journalISSN := w.PrimaryLocation.Source.ISSN
		isCore := w.PrimaryLocation.Source.IsCore
		publisher := w.PrimaryLocation.Source.HostOrganization
		isOA := w.OpenAccess.IsOA
		oaStatus := w.OpenAccess.OAStatus
		oaURL := w.OpenAccess.OAURL

		_, err = stmtPaper.Exec(
			paperID, w.DOI, w.Title, w.PublicationYear, w.PublicationDate, w.Type,
			journalName, journalISSN, isCore, publisher,
			isOA, oaStatus, oaURL, w.CitedByCount, w.CitationPercentile.Value,
			w.CitationPercentile.IsInTop1Pct, w.CitationPercentile.IsInTop10Pct, w.FWCI,
			topicID, topicName, topicScore,
			topicField, topicSubfield, topicDomain,
			w.InstitutionsDistinctCount, w.CountriesDistinctCount, isInternational,
			abstract, w.UpdatedDate,
		)
		if err != nil {
			stats.Errors++
			continue
		}
		stats.Papers++

		for _, auth := range w.Authorships {
			authorID := extractID(auth.Author.ID)
			if authorID != "" && !authorCache[authorID] {
				_, err = stmtAuthor.Exec(authorID, auth.Author.DisplayName, auth.Author.ORCID)
				if err == nil {
					authorCache[authorID] = true
					stats.Authors++
				}
			}

			if len(auth.Institutions) > 0 {
				for idx, inst := range auth.Institutions {
					instID := extractID(inst.ID)
					var instCC string
					if instID != "" {
						instCC = ensureCountry(inst.CountryCode)
						if !institutionCache[instID] {
							_, err = stmtInstitution.Exec(instID, inst.DisplayName, instCC, inst.Type, inst.ROR)
							if err == nil {
								institutionCache[instID] = true
								stats.Institutions++
							}
						}
					}
					rawAff := ""
					if idx < len(auth.RawAffiliationString) {
						rawAff = auth.RawAffiliationString[idx]
					}
					_, err = stmtContribution.Exec(
						paperID,
						sql.NullString{String: authorID, Valid: authorID != ""},
						sql.NullString{String: instID, Valid: instID != ""},
						sql.NullString{String: instCC, Valid: instCC != ""},
						auth.RawAuthorName,
						auth.AuthorPosition,
						auth.IsCorresponding,
						sql.NullString{String: rawAff, Valid: rawAff != ""},
					)
					if err == nil {
						stats.Contributions++
					}
				}
			} else {
				var countryCode string
				if len(auth.Countries) > 0 {
					countryCode = ensureCountry(auth.Countries[0])
				}
				rawAff := ""
				if len(auth.RawAffiliationString) > 0 {
					for _, r := range auth.RawAffiliationString {
						if strings.TrimSpace(r) != "" {
							rawAff = strings.TrimSpace(r)
							break
						}
					}
				}
				_, err = stmtContribution.Exec(
					paperID,
					sql.NullString{String: authorID, Valid: authorID != ""},
					sql.NullString{String: "", Valid: false},
					sql.NullString{String: countryCode, Valid: countryCode != ""},
					auth.RawAuthorName,
					auth.AuthorPosition,
					auth.IsCorresponding,
					sql.NullString{String: rawAff, Valid: rawAff != ""},
				)
				if err == nil {
					stats.Contributions++
				}
			}
		}

		if progressChan != nil && lineCount%100 == 0 {
			progressChan <- lineCount
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	if progressChan != nil {
		progressChan <- lineCount
	}

	return stats, nil
}

func normalizeCountryCode(code string) string {
	c := strings.ToUpper(strings.TrimSpace(code))
	if c == "HK" {
		return "CN"
	}
	return c
}

func extractID(url string) string {
	if url == "" {
		return ""
	}
	parts := strings.Split(url, "/")
	return parts[len(parts)-1]
}

func reconstructAbstract(invertedIndex map[string][]int) string {
	if len(invertedIndex) == 0 {
		return ""
	}
	maxPos := -1
	for _, positions := range invertedIndex {
		for _, pos := range positions {
			if pos > maxPos {
				maxPos = pos
			}
		}
	}
	if maxPos < 0 {
		return ""
	}
	words := make([]string, maxPos+1)
	for word, positions := range invertedIndex {
		for _, pos := range positions {
			if pos >= 0 && pos <= maxPos {
				words[pos] = word
			}
		}
	}
	return strings.Join(words, " ")
}

// GetDashboardStats queries aggregates to get total counts and analytical breakdown statistics.
func (m *DBManager) GetDashboardStats() (*DashboardStats, error) {
	stats := &DashboardStats{}

	// 1. Total papers
	err := m.db.QueryRow("SELECT COUNT(*) FROM papers").Scan(&stats.TotalPapers)
	if err != nil {
		return nil, err
	}

	// 2. Total authors
	err = m.db.QueryRow("SELECT COUNT(*) FROM authors").Scan(&stats.TotalAuthors)
	if err != nil {
		return nil, err
	}

	// 3. Total institutions
	err = m.db.QueryRow("SELECT COUNT(*) FROM institutions").Scan(&stats.TotalInstitutions)
	if err != nil {
		return nil, err
	}

	// 4. Total countries
	err = m.db.QueryRow("SELECT COUNT(*) FROM countries").Scan(&stats.TotalCountries)
	if err != nil {
		return nil, err
	}

	// 5. Papers by year
	rowsYear, err := m.db.Query("SELECT publication_year, COUNT(*) FROM papers GROUP BY publication_year ORDER BY publication_year")
	if err == nil {
		defer rowsYear.Close()
		for rowsYear.Next() {
			var ys YearStat
			if err := rowsYear.Scan(&ys.Year, &ys.Count); err == nil {
				stats.PapersByYear = append(stats.PapersByYear, ys)
			}
		}
	}

	// 6. OA status counts
	rowsOA, err := m.db.Query("SELECT COALESCE(oa_status, 'unknown'), COUNT(*) FROM papers GROUP BY oa_status")
	if err == nil {
		defer rowsOA.Close()
		for rowsOA.Next() {
			var oas OAStatusStat
			if err := rowsOA.Scan(&oas.Status, &oas.Count); err == nil {
				stats.OAStatusCounts = append(stats.OAStatusCounts, oas)
			}
		}
	}

	// 7. Top journals
	rowsJournal, err := m.db.Query("SELECT journal_name, COUNT(*) FROM papers WHERE journal_name IS NOT NULL AND TRIM(journal_name) != '' GROUP BY journal_name ORDER BY COUNT(*) DESC LIMIT 10")
	if err == nil {
		defer rowsJournal.Close()
		for rowsJournal.Next() {
			var js JournalStat
			if err := rowsJournal.Scan(&js.JournalName, &js.Count); err == nil {
				stats.TopJournals = append(stats.TopJournals, js)
			}
		}
	}

	// 8. Country counts
	rowsCountry, err := m.db.Query("SELECT country_code, COUNT(*) FROM contributions WHERE country_code IS NOT NULL AND TRIM(country_code) != '' GROUP BY country_code ORDER BY country_code DESC LIMIT 15")
	if err == nil {
		defer rowsCountry.Close()
		for rowsCountry.Next() {
			var cs CountryStat
			if err := rowsCountry.Scan(&cs.CountryCode, &cs.Count); err == nil {
				stats.CountryCounts = append(stats.CountryCounts, cs)
			}
		}
	}

	return stats, nil
}

// RunQuery executes a custom SQL statement on the DuckDB database and returns a generic array of row maps.
func (m *DBManager) RunQuery(query string) ([]map[string]interface{}, error) {
	rows, err := m.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var result []map[string]interface{}

	for rows.Next() {
		values := make([]interface{}, len(cols))
		valuePtrs := make([]interface{}, len(cols))
		for i := range cols {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}

		rowMap := make(map[string]interface{})
		for i, colName := range cols {
			val := values[i]
			if b, ok := val.([]byte); ok {
				rowMap[colName] = string(b)
			} else {
				rowMap[colName] = val
			}
		}
		result = append(result, rowMap)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}
