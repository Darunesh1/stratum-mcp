package impute

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/adrg/strutil/metrics"
	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/dslipak/pdf"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
	"google.golang.org/genai"
	"stratum/config"
)

// ImputationEngine coordinates imputation pipelines over missing institutions and countries.
type ImputationEngine struct {
	dbPath string
}

// NewImputationEngine initializes a new engine.
func NewImputationEngine(dbPath string) *ImputationEngine {
	return &ImputationEngine{dbPath: dbPath}
}

// NormalizeCountryCode standardizes country codes, e.g. mapping "HK" to "CN".
func NormalizeCountryCode(code string) string {
	c := strings.ToUpper(strings.TrimSpace(code))
	if c == "HK" {
		return "CN"
	}
	return c
}

// CountryInference holds the result of mapping a raw affiliation string to a country.
type CountryInference struct {
	CountryCode  string
	Status       string // unambiguous, ambiguous, none
	MatchedTerms []string
}

var countryAliases = map[string][]string{
	"US": {"united states", "united states of america", "usa", "u.s.a.", "u.s.a", "u.s.", "america"},
	"CN": {"china", "people's republic of china", "peoples republic of china", "pr china", "p.r. china", "p.r.c.", "p.r.c"},
	"JP": {"japan"},
	"DE": {"germany", "deutschland"},
	"GB": {"united kingdom", "uk", "u.k.", "u.k", "england", "scotland", "wales", "northern ireland", "great britain"},
	"FR": {"france"},
	"IT": {"italy"},
	"ES": {"spain"},
	"NL": {"netherlands", "the netherlands"},
	"CH": {"switzerland"},
	"SE": {"sweden"},
	"NO": {"norway"},
	"FI": {"finland"},
	"DK": {"denmark"},
	"BE": {"belgium"},
	"AT": {"austria"},
	"CA": {"canada"},
	"AU": {"australia"},
	"NZ": {"new zealand"},
	"IN": {"india"},
	"KR": {"south korea", "republic of korea", "korea"},
	"RU": {"russia", "russian federation"},
	"IL": {"israel"},
	"SG": {"singapore"},
	"TW": {"taiwan"},
	"BR": {"brazil"},
	"PL": {"poland"},
	"IR": {"iran", "iran, islamic republic of"},
	"IE": {"ireland"},
	"PT": {"portugal"},
	"GR": {"greece"},
	"CZ": {"czech republic", "czechia"},
	"HU": {"hungary"},
	"TR": {"turkey", "türkiye", "turkiye"},
	"MX": {"mexico"},
	"AR": {"argentina"},
	"CL": {"chile"},
	"ZA": {"south africa"},
	"HK": {"hong kong", "hong kong sar"},
}

func containsWordBoundary(text, term string) bool {
	textLower := strings.ToLower(text)
	termLower := strings.ToLower(term)

	startIdx := 0
	for {
		idx := strings.Index(textLower[startIdx:], termLower)
		if idx == -1 {
			return false
		}
		actualIdx := startIdx + idx

		beforeOk := true
		if actualIdx > 0 {
			charBefore := textLower[actualIdx-1]
			if (charBefore >= 'a' && charBefore <= 'z') || (charBefore >= '0' && charBefore <= '9') {
				beforeOk = false
			}
		}

		afterOk := true
		afterIdx := actualIdx + len(term)
		if afterIdx < len(textLower) {
			charAfter := textLower[afterIdx]
			if (charAfter >= 'a' && charAfter <= 'z') || (charAfter >= '0' && charAfter <= '9') {
				afterOk = false
			}
		}

		if beforeOk && afterOk {
			return true
		}
		startIdx = actualIdx + 1
	}
}

// InferCountryFromAffiliation uses country name aliases and regex word-boundary checks to identify a country.
func InferCountryFromAffiliation(rawAffiliation string) *CountryInference {
	text := strings.TrimSpace(rawAffiliation)
	if text == "" {
		return &CountryInference{
			CountryCode:  "",
			Status:       "none",
			MatchedTerms: []string{},
		}
	}

	matchedCodes := make(map[string]bool)
	var matchedTerms []string

	for code, terms := range countryAliases {
		for _, term := range terms {
			if containsWordBoundary(text, term) {
				matchedCodes[code] = true
				matchedTerms = append(matchedTerms, term)
			}
		}
	}

	if len(matchedCodes) == 1 {
		var code string
		for c := range matchedCodes {
			code = NormalizeCountryCode(c)
		}
		return &CountryInference{
			CountryCode:  code,
			Status:       "unambiguous",
			MatchedTerms: matchedTerms,
		}
	} else if len(matchedCodes) > 1 {
		return &CountryInference{
			CountryCode:  "",
			Status:       "ambiguous",
			MatchedTerms: matchedTerms,
		}
	}

	return &CountryInference{
		CountryCode:  "",
		Status:       "none",
		MatchedTerms: []string{},
	}
}

// SyntheticInstitutionID generates a stable hex ID prefixed with IMP_ from a display name.
func SyntheticInstitutionID(name string) string {
	cleaned := strings.TrimSpace(name)
	cleaned = strings.ToLower(cleaned)
	re := regexp.MustCompile(`\s+`)
	cleaned = re.ReplaceAllString(cleaned, " ")

	h := sha1.New()
	h.Write([]byte(cleaned))
	digest := fmt.Sprintf("%x", h.Sum(nil))
	if len(digest) > 10 {
		digest = digest[:10]
	}
	return "IMP_" + digest
}

// InstitutionRecord represents an existing database institution entry for indexing.
type InstitutionRecord struct {
	ID          string
	DisplayName string
	CountryCode string
}

// InstitutionMatch represents the result of a similarity match query.
type InstitutionMatch struct {
	InstitutionID string
	DisplayName   string
	CountryCode   string
	Score         float64
}

// InstitutionMatcher wraps sentence-transformer embeddings to run similarity lookups.
type InstitutionMatcher struct {
	ModelName string
	Records   []InstitutionRecord
}

// NewInstitutionMatcher initializes the matcher.
func NewInstitutionMatcher(modelName string) *InstitutionMatcher {
	return &InstitutionMatcher{ModelName: modelName}
}

// Index generates or loads cached vector embeddings for the provided database records.
func (m *InstitutionMatcher) Index(records []InstitutionRecord) error {
	m.Records = records
	return nil
}

func normalizeTitle(raw string) string {
	if raw == "" {
		return ""
	}
	s := raw
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	if normalized, _, err := transform.String(t, s); err == nil {
		s = normalized
	}
	s = strings.ToLower(s)

	punctRE := regexp.MustCompile(`[^\pL\pN\s]+`)
	s = punctRE.ReplaceAllString(s, " ")

	wsRE := regexp.MustCompile(`\s+`)
	s = wsRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// FindMatch returns the best matched institution if above the threshold.
func (m *InstitutionMatcher) FindMatch(query string, threshold float64) *InstitutionMatch {
	matches, err := m.TopK(query, 1)
	if err != nil || len(matches) == 0 {
		return nil
	}
	best := matches[0]
	if best.Score >= threshold {
		return &best
	}
	return nil
}

// TopK returns the top K most similar institutions for a query.
func (m *InstitutionMatcher) TopK(query string, k int) ([]InstitutionMatch, error) {
	if len(m.Records) == 0 || query == "" {
		return nil, nil
	}

	normQuery := normalizeTitle(query)
	jw := metrics.NewJaroWinkler()

	var matches []InstitutionMatch
	for _, rec := range m.Records {
		normRec := normalizeTitle(rec.DisplayName)
		score := jw.Compare(normQuery, normRec)
		matches = append(matches, InstitutionMatch{
			InstitutionID: rec.ID,
			DisplayName:   rec.DisplayName,
			CountryCode:   rec.CountryCode,
			Score:         score,
		})
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Score > matches[j].Score
	})

	if len(matches) > k {
		matches = matches[:k]
	}

	return matches, nil
}

func ensureAuditTable(db *sql.DB) error {
	q := `CREATE TABLE IF NOT EXISTS country_imputation_audit (
		run_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		row_id INTEGER,
		paper_id VARCHAR,
		inferred_country_code VARCHAR,
		matched_terms TEXT,
		raw_affiliation_string TEXT,
		source VARCHAR,
		confidence DOUBLE,
		stage VARCHAR
	)`
	_, err := db.Exec(q)
	return err
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

// ImputeCrossRef fetches missing raw_affiliation_string entries from Crossref using paper DOIs.
func (e *ImputationEngine) ImputeCrossRef(ctx context.Context, progressChan chan<- int) error {
	dbConn, err := sql.Open("duckdb", e.dbPath)
	if err != nil {
		return err
	}
	defer dbConn.Close()

	if err := ensureAuditTable(dbConn); err != nil {
		return err
	}

	// 1. Load eligible papers
	rows, err := dbConn.Query(`
		SELECT DISTINCT p.id, p.doi
		FROM papers p
		JOIN contributions c ON c.paper_id = p.id
		WHERE p.doi IS NOT NULL
		  AND TRIM(p.doi) != ''
		  AND c.institution_id IS NULL
		  AND (c.raw_affiliation_string IS NULL OR TRIM(c.raw_affiliation_string) = '')
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type PaperInfo struct {
		ID  string
		DOI string
	}
	var papers []PaperInfo
	for rows.Next() {
		var p PaperInfo
		if err := rows.Scan(&p.ID, &p.DOI); err == nil {
			papers = append(papers, p)
		}
	}

	if len(papers) == 0 {
		return nil
	}

	email := "polite@example.com"
	if cfg, err := config.LoadConfig("config/collection.yml"); err == nil {
		if cfg.API.Email != "" {
			email = cfg.API.Email
		}
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	processed := 0

	for _, paper := range papers {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		normDOI := normalizeDOI(paper.DOI)
		crossrefURL := fmt.Sprintf("https://api.crossref.org/works/%s", url.QueryEscape(normDOI))

		req, err := http.NewRequestWithContext(ctx, "GET", crossrefURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", fmt.Sprintf("openalex-data-collection/0.1.0 (mailto:%s)", email))
		req.Header.Set("Accept", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			continue
		}

		var payload struct {
			Message struct {
				Author []struct {
					ORCID       string `json:"ORCID"`
					Affiliation []struct {
						Name string `json:"name"`
					} `json:"affiliation"`
				} `json:"author"`
			} `json:"message"`
		}

		err = json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if err != nil {
			continue
		}

		// Load contributions targets for this paper
		targetRows, err := dbConn.Query(`
			SELECT c.row_id, c.author_id, c.author_position, a.orcid, c.raw_affiliation_string
			FROM contributions c
			LEFT JOIN authors a ON a.id = c.author_id
			WHERE c.paper_id = ?
			ORDER BY c.row_id
		`, paper.ID)
		if err != nil {
			continue
		}

		type TargetRow struct {
			RowID          int
			AuthorID       sql.NullString
			AuthorPosition sql.NullString
			ORCID          sql.NullString
			RawAff         sql.NullString
		}
		var targets []TargetRow
		for targetRows.Next() {
			var t TargetRow
			if err := targetRows.Scan(&t.RowID, &t.AuthorID, &t.AuthorPosition, &t.ORCID, &t.RawAff); err == nil {
				targets = append(targets, t)
			}
		}
		targetRows.Close()

		// Match authors by ORCID first, then position index if lengths match
		type CrAuthor struct {
			ORCID  string
			Names  []string
		}
		var crAuthors []CrAuthor
		for _, auth := range payload.Message.Author {
			orcid := strings.TrimSpace(auth.ORCID)
			for _, prefix := range []string{"http://orcid.org/", "https://orcid.org/"} {
				if strings.HasPrefix(orcid, prefix) {
					orcid = orcid[len(prefix):]
					break
				}
			}
			var names []string
			for _, aff := range auth.Affiliation {
				if aff.Name != "" {
					names = append(names, aff.Name)
				}
			}
			if len(names) > 0 {
				crAuthors = append(crAuthors, CrAuthor{ORCID: orcid, Names: names})
			}
		}

		if len(targets) == 0 || len(crAuthors) == 0 {
			continue
		}

		matched := make(map[int]CrAuthor)
		crUsed := make(map[int]bool)

		// Pass 1: ORCID matching
		for _, t := range targets {
			if t.ORCID.Valid && t.ORCID.String != "" {
				for idx, cr := range crAuthors {
					if cr.ORCID == t.ORCID.String {
						matched[t.RowID] = cr
						crUsed[idx] = true
						break
					}
				}
			}
		}

		// Pass 2: Position index mapping if lengths match
		var remainingTargets []TargetRow
		for _, t := range targets {
			if _, ok := matched[t.RowID]; !ok {
				remainingTargets = append(remainingTargets, t)
			}
		}
		var remainingCr []CrAuthor
		for idx, cr := range crAuthors {
			if !crUsed[idx] {
				remainingCr = append(remainingCr, cr)
			}
		}

		if len(remainingTargets) == len(remainingCr) {
			for idx, t := range remainingTargets {
				matched[t.RowID] = remainingCr[idx]
			}
		}

		// Update matches in DuckDB
		tx, err := dbConn.Begin()
		if err != nil {
			continue
		}

		stmtUpdate, _ := tx.Prepare(`
			UPDATE contributions
			SET raw_affiliation_string = ?
			WHERE row_id = ? AND (raw_affiliation_string IS NULL OR TRIM(raw_affiliation_string) = '')
		`)
		stmtAudit, _ := tx.Prepare(`
			INSERT INTO country_imputation_audit (
				row_id, paper_id, inferred_country_code, matched_terms, raw_affiliation_string, source, confidence, stage
			) VALUES (?, ?, NULL, 'crossref', ?, 'crossref', 1.0, 'crossref')
		`)

		updatesCount := 0
		for rowID, cr := range matched {
			rawAff := strings.Join(cr.Names, " | ")
			res, err := stmtUpdate.Exec(rawAff, rowID)
			if err != nil {
				continue
			}
			rowsAffected, _ := res.RowsAffected()
			if rowsAffected > 0 {
				stmtAudit.Exec(rowID, paper.ID, rawAff)
				updatesCount++
			}
		}
		stmtUpdate.Close()
		stmtAudit.Close()
		tx.Commit()

		processed++
		if progressChan != nil {
			progressChan <- processed
		}
	}

	return nil
}

type LLMClient interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// OllamaClient triggers local Ollama completions.
type OllamaClient struct {
	BaseURL string
	Model   string
}

// Complete submits a prompt payload to the Ollama endpoint.
func (c *OllamaClient) Complete(ctx context.Context, prompt string) (string, error) {
	urlStr := c.BaseURL + "/api/generate"

	payload := map[string]interface{}{
		"model":  c.Model,
		"prompt": prompt,
		"stream": false,
		"options": map[string]interface{}{
			"temperature": 0.0,
		},
		"format": "json",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", urlStr, bytes.NewBuffer(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama error %d: %s", resp.StatusCode, string(respBody))
	}

	var res struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", err
	}

	return res.Response, nil
}

// GeminiClient wraps the google.golang.org/genai client.
type GeminiClient struct {
	APIKey string
	Model  string
}

// Complete submits a prompt payload to the Gemini API.
func (c *GeminiClient) Complete(ctx context.Context, prompt string) (string, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey: c.APIKey,
	})
	if err != nil {
		return "", err
	}

	result, err := client.Models.GenerateContent(ctx, c.Model, []*genai.Content{{
		Parts: []*genai.Part{{Text: prompt}},
	}}, nil)
	if err != nil {
		return "", err
	}

	if len(result.Candidates) == 0 || result.Candidates[0].Content == nil || len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini returned no content candidates")
	}

	return result.Candidates[0].Content.Parts[0].Text, nil
}

func parseJSONMarkdown(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		idx := strings.Index(s, "\n")
		if idx != -1 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}

func parseRowID(val interface{}) (int, string) {
	if val == nil {
		return 0, ""
	}
	switch v := val.(type) {
	case float64:
		return int(v), ""
	case int:
		return v, ""
	case string:
		var i int
		if _, err := fmt.Sscanf(v, "%d", &i); err == nil {
			return i, ""
		}
		return 0, v
	}
	return 0, ""
}

// ImputeLLM uses an LLM (Ollama or Gemini) to extract institution and country names from raw affiliation strings.
func (e *ImputationEngine) ImputeLLM(ctx context.Context, provider string, model string, baseURL string, progressChan chan<- int) error {
	dbConn, err := sql.Open("duckdb", e.dbPath)
	if err != nil {
		return err
	}
	defer dbConn.Close()

	if err := ensureAuditTable(dbConn); err != nil {
		return err
	}

	var apiKey string
	if cfg, err := config.LoadConfig("config/collection.yml"); err == nil {
		apiKey = cfg.API.GroqKey // Or generic key
	}

	var llm LLMClient
	if strings.ToLower(provider) == "gemini" {
		llm = &GeminiClient{APIKey: apiKey, Model: model}
	} else {
		if baseURL == "" {
			baseURL = "http://localhost:11434"
		}
		llm = &OllamaClient{BaseURL: baseURL, Model: model}
	}

	// Load existing institutions for indexing
	rows, err := dbConn.Query("SELECT id, display_name, country_code FROM institutions WHERE display_name IS NOT NULL")
	if err != nil {
		return err
	}
	defer rows.Close()

	var records []InstitutionRecord
	for rows.Next() {
		var r InstitutionRecord
		var cc sql.NullString
		if err := rows.Scan(&r.ID, &r.DisplayName, &cc); err == nil {
			r.CountryCode = cc.String
			records = append(records, r)
		}
	}
	matcher := NewInstitutionMatcher("default")
	matcher.Index(records)

	// ==========================================
	// STAGE 1: Extract institution & country
	// ==========================================
	type Stage1Row struct {
		RowID   int    `json:"row_id"`
		PaperID string `json:"paper_id"`
		RawAff  string `json:"raw_affiliation_string"`
	}

	rowsS1, err := dbConn.Query(`
		SELECT row_id, paper_id, raw_affiliation_string
		FROM contributions
		WHERE institution_id IS NULL
		  AND raw_affiliation_string IS NOT NULL
		  AND TRIM(raw_affiliation_string) != ''
	`)
	if err != nil {
		return err
	}
	defer rowsS1.Close()

	var s1Rows []Stage1Row
	for rowsS1.Next() {
		var r Stage1Row
		if err := rowsS1.Scan(&r.RowID, &r.PaperID, &r.RawAff); err == nil {
			s1Rows = append(s1Rows, r)
		}
	}

	processedCount := 0

	// Batch size 15 for LLM processing
	batchSize := 15
	for i := 0; i < len(s1Rows); i += batchSize {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		end := i + batchSize
		if end > len(s1Rows) {
			end = len(s1Rows)
		}
		batch := s1Rows[i:end]

		// Format JSON input
		var inputList []map[string]interface{}
		for _, r := range batch {
			inputList = append(inputList, map[string]interface{}{
				"row_id":                   r.RowID,
				"raw_affiliation_string": r.RawAff,
			})
		}
		inputJSON, _ := json.Marshal(inputList)

		prompt := fmt.Sprintf(
			"Task: from each affiliation string, extract the primary institution name (university, research institute, lab, or company) and its ISO-3166-1 alpha-2 country code.\n"+
				"Rules:\n"+
				"1) Keep row_id exactly as input.\n"+
				"2) institution_name should be the most specific identifiable organisation, without departments or addresses.\n"+
				"3) Return valid JSON containing a 'predictions' array. Each item in the array must contain: 'row_id' (int/string matching input), 'institution_name' (string or null), 'country_code' (string or null), and 'confidence' (float).\n"+
				"Input:\n%s", string(inputJSON),
		)

		respText, err := llm.Complete(ctx, prompt)
		if err != nil {
			continue
		}

		cleanedResp := parseJSONMarkdown(respText)

		var parsedResponse struct {
			Predictions []struct {
				RowID           interface{} `json:"row_id"`
				InstitutionName *string     `json:"institution_name"`
				CountryCode     *string     `json:"country_code"`
				Confidence      float64     `json:"confidence"`
			} `json:"predictions"`
		}

		if err := json.Unmarshal([]byte(cleanedResp), &parsedResponse); err != nil {
			continue
		}

		tx, err := dbConn.Begin()
		if err != nil {
			continue
		}

		stmtUpdate, _ := tx.Prepare(`
			UPDATE contributions
			SET institution_id = ?, country_code = COALESCE(country_code, ?)
			WHERE row_id = ?
		`)
		stmtInsertInst, _ := tx.Prepare(`
			INSERT INTO institutions (id, display_name, country_code, type, ror_id, is_synthetic)
			VALUES (?, ?, ?, 'education', NULL, TRUE)
			ON CONFLICT DO NOTHING
		`)
		stmtAudit, _ := tx.Prepare(`
			INSERT INTO country_imputation_audit (
				row_id, paper_id, inferred_country_code, matched_terms, raw_affiliation_string, source, confidence, stage
			) VALUES (?, ?, ?, ?, ?, 'llm', ?, 'stage1')
		`)

		for _, pred := range parsedResponse.Predictions {
			rid, _ := parseRowID(pred.RowID)
			if rid == 0 {
				continue
			}

			// Find matching paper ID in current batch
			var paperID, rawAff string
			for _, r := range batch {
				if r.RowID == rid {
					paperID = r.PaperID
					rawAff = r.RawAff
					break
				}
			}

			var instID string
			var cc string
			if pred.CountryCode != nil {
				cc = NormalizeCountryCode(*pred.CountryCode)
			}

			if pred.InstitutionName != nil && *pred.InstitutionName != "" {
				name := *pred.InstitutionName
				// Try match
				match := matcher.FindMatch(name, 0.85)
				if match != nil {
					instID = match.InstitutionID
					if cc == "" {
						cc = match.CountryCode
					}
				} else {
					instID = SyntheticInstitutionID(name)
					// Insert new synthetic institution
					stmtInsertInst.Exec(instID, name, sql.NullString{String: cc, Valid: cc != ""})
				}
			}

			if instID != "" || cc != "" {
				var instVal interface{} = nil
				if instID != "" {
					instVal = instID
				}
				var ccVal interface{} = nil
				if cc != "" {
					ccVal = cc
				}
				stmtUpdate.Exec(instVal, ccVal, rid)
				stmtAudit.Exec(rid, paperID, ccVal, "llm_stage1", rawAff, pred.Confidence)
			}
		}

		stmtUpdate.Close()
		stmtInsertInst.Close()
		stmtAudit.Close()
		tx.Commit()

		processedCount += len(batch)
		if progressChan != nil {
			progressChan <- processedCount
		}
	}

	// ==========================================
	// STAGE 2: backfill missing institution country
	// ==========================================
	type Stage2Row struct {
		ID          string
		DisplayName string
	}
	rowsS2, err := dbConn.Query("SELECT id, display_name FROM institutions WHERE country_code IS NULL")
	if err == nil {
		var s2Rows []Stage2Row
		for rowsS2.Next() {
			var r Stage2Row
			if err := rowsS2.Scan(&r.ID, &r.DisplayName); err == nil {
				s2Rows = append(s2Rows, r)
			}
		}
		rowsS2.Close()

		tx, _ := dbConn.Begin()
		stmtUpdateInst, _ := tx.Prepare("UPDATE institutions SET country_code = ? WHERE id = ?")
		stmtUpdateContrib, _ := tx.Prepare("UPDATE contributions SET country_code = COALESCE(country_code, ?) WHERE institution_id = ?")

		for _, r := range s2Rows {
			inf := InferCountryFromAffiliation(r.DisplayName)
			if inf.Status == "unambiguous" {
				stmtUpdateInst.Exec(inf.CountryCode, r.ID)
				stmtUpdateContrib.Exec(inf.CountryCode, r.ID)
			}
		}
		stmtUpdateInst.Close()
		stmtUpdateContrib.Close()
		tx.Commit()
	}

	// ==========================================
	// STAGE 3: Country inference from affiliation
	// ==========================================
	type Stage3Row struct {
		RowID   int
		PaperID string
		RawAff  string
	}
	rowsS3, err := dbConn.Query(`
		SELECT row_id, paper_id, raw_affiliation_string
		FROM contributions
		WHERE country_code IS NULL
		  AND raw_affiliation_string IS NOT NULL
		  AND TRIM(raw_affiliation_string) != ''
	`)
	if err == nil {
		var s3Rows []Stage3Row
		for rowsS3.Next() {
			var r Stage3Row
			if err := rowsS3.Scan(&r.RowID, &r.PaperID, &r.RawAff); err == nil {
				s3Rows = append(s3Rows, r)
			}
		}
		rowsS3.Close()

		// First, do offline rule matching
		var unresolved []Stage3Row
		tx, _ := dbConn.Begin()
		stmtUpdate, _ := tx.Prepare("UPDATE contributions SET country_code = ? WHERE row_id = ?")
		stmtAudit, _ := tx.Prepare(`
			INSERT INTO country_imputation_audit (
				row_id, paper_id, inferred_country_code, matched_terms, raw_affiliation_string, source, confidence, stage
			) VALUES (?, ?, ?, ?, ?, 'rule', 1.0, 'stage3_rule')
		`)

		for _, r := range s3Rows {
			inf := InferCountryFromAffiliation(r.RawAff)
			if inf.Status == "unambiguous" {
				stmtUpdate.Exec(inf.CountryCode, r.RowID)
				stmtAudit.Exec(r.RowID, r.PaperID, inf.CountryCode, strings.Join(inf.MatchedTerms, "|"), r.RawAff)
			} else {
				unresolved = append(unresolved, r)
			}
		}
		stmtUpdate.Close()
		stmtAudit.Close()
		tx.Commit()

		// Run remaining unresolved via LLM
		for i := 0; i < len(unresolved); i += batchSize {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			end := i + batchSize
			if end > len(unresolved) {
				end = len(unresolved)
			}
			batch := unresolved[i:end]

			var inputList []map[string]interface{}
			for _, r := range batch {
				inputList = append(inputList, map[string]interface{}{
					"row_id":                   r.RowID,
					"raw_affiliation_string": r.RawAff,
				})
			}
			inputJSON, _ := json.Marshal(inputList)

			prompt := fmt.Sprintf(
				"Task: from each affiliation string, extract the ISO-3166-1 alpha-2 country code.\n"+
					"Rules:\n"+
					"1) Keep row_id exactly as input.\n"+
					"2) Return valid JSON containing a 'predictions' array. Each item must contain: 'row_id' (int/string matching input), 'country_code' (string or null), 'status' (unambiguous, ambiguous, none), and 'confidence' (float).\n"+
					"Input:\n%s", string(inputJSON),
			)

			respText, err := llm.Complete(ctx, prompt)
			if err != nil {
				continue
			}

			cleanedResp := parseJSONMarkdown(respText)

			var parsedResponse struct {
				Predictions []struct {
					RowID       interface{} `json:"row_id"`
					CountryCode *string     `json:"country_code"`
					Status      string      `json:"status"`
					Confidence  float64     `json:"confidence"`
				} `json:"predictions"`
			}

			if err := json.Unmarshal([]byte(cleanedResp), &parsedResponse); err != nil {
				continue
			}

			tx, err := dbConn.Begin()
			if err != nil {
				continue
			}

			stmtUpdate, _ := tx.Prepare("UPDATE contributions SET country_code = ? WHERE row_id = ?")
			stmtAudit, _ := tx.Prepare(`
				INSERT INTO country_imputation_audit (
					row_id, paper_id, inferred_country_code, matched_terms, raw_affiliation_string, source, confidence, stage
				) VALUES (?, ?, ?, ?, ?, 'llm', ?, 'stage3_llm')
			`)

			for _, pred := range parsedResponse.Predictions {
				rid, _ := parseRowID(pred.RowID)
				if rid == 0 {
					continue
				}

				var paperID, rawAff string
				for _, r := range batch {
					if r.RowID == rid {
						paperID = r.PaperID
						rawAff = r.RawAff
						break
					}
				}

				if pred.CountryCode != nil && *pred.CountryCode != "" && strings.ToLower(pred.Status) == "unambiguous" {
					cc := NormalizeCountryCode(*pred.CountryCode)
					stmtUpdate.Exec(cc, rid)
					stmtAudit.Exec(rid, paperID, cc, "llm_stage3", rawAff, pred.Confidence)
				}
			}
			stmtUpdate.Close()
			stmtAudit.Close()
			tx.Commit()
		}
	}

	return nil
}

func extractPageText(path string, pageNum int) (string, error) {
	r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}

	if pageNum < 1 || pageNum > r.NumPage() {
		return "", fmt.Errorf("page out of bounds")
	}

	p := r.Page(pageNum)
	if p.V.IsNull() {
		return "", fmt.Errorf("page null")
	}

	content := p.Content()
	texts := content.Text

	sort.Slice(texts, func(i, j int) bool {
		if absFloat(texts[i].Y-texts[j].Y) < 2.0 {
			return texts[i].X < texts[j].X
		}
		return texts[i].Y > texts[j].Y
	})

	var sb strings.Builder
	var lastY float64
	for idx, text := range texts {
		if idx > 0 && absFloat(text.Y-lastY) >= 2.0 {
			sb.WriteString("\n")
		} else if idx > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(text.S)
		lastY = text.Y
	}
	return sb.String(), nil
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func unpaywallPDFURL(ctx context.Context, httpClient *http.Client, doi, email string) string {
	crossrefURL := fmt.Sprintf("https://api.unpaywall.org/v2/%s?email=%s", url.QueryEscape(doi), url.QueryEscape(email))
	req, err := http.NewRequestWithContext(ctx, "GET", crossrefURL, nil)
	if err != nil {
		return ""
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var payload struct {
		BestOALocation struct {
			URLForPDF string `json:"url_for_pdf"`
		} `json:"best_oa_location"`
		OALocations []struct {
			URLForPDF string `json:"url_for_pdf"`
		} `json:"oa_locations"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}

	if payload.BestOALocation.URLForPDF != "" {
		return payload.BestOALocation.URLForPDF
	}

	for _, loc := range payload.OALocations {
		if loc.URLForPDF != "" {
			return loc.URLForPDF
		}
	}
	return ""
}

func openalexPDFURL(ctx context.Context, httpClient *http.Client, doi, email string) string {
	openalexURL := fmt.Sprintf("https://api.openalex.org/works/doi:%s", url.QueryEscape(doi))
	req, err := http.NewRequestWithContext(ctx, "GET", openalexURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", fmt.Sprintf("openalex-data-collection/0.1.0 (mailto:%s)", email))

	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var payload struct {
		BestOALocation struct {
			PDFURL string `json:"pdf_url"`
		} `json:"best_oa_location"`
		PrimaryLocation struct {
			PDFURL string `json:"pdf_url"`
		} `json:"primary_location"`
		Locations []struct {
			PDFURL string `json:"pdf_url"`
		} `json:"locations"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}

	if payload.BestOALocation.PDFURL != "" {
		return payload.BestOALocation.PDFURL
	}
	if payload.PrimaryLocation.PDFURL != "" {
		return payload.PrimaryLocation.PDFURL
	}
	for _, loc := range payload.Locations {
		if loc.PDFURL != "" {
			return loc.PDFURL
		}
	}
	return ""
}

// ImputePDF downloads the paper PDF (e.g. via arXiv/Unpaywall), extracts text, and extracts affiliation using LLM.
func (e *ImputationEngine) ImputePDF(ctx context.Context, provider string, model string, baseURL string, limit int, progressChan chan<- int) error {
	dbConn, err := sql.Open("duckdb", e.dbPath)
	if err != nil {
		return err
	}
	defer dbConn.Close()

	if err := ensureAuditTable(dbConn); err != nil {
		return err
	}

	limitSQL := ""
	if limit > 0 {
		limitSQL = fmt.Sprintf("LIMIT %d", limit)
	}

	rows, err := dbConn.Query(fmt.Sprintf(`
		SELECT DISTINCT p.id, p.doi
		FROM papers p
		JOIN contributions c ON c.paper_id = p.id
		WHERE p.doi IS NOT NULL
		  AND TRIM(p.doi) != ''
		  AND c.institution_id IS NULL
		  AND (c.raw_affiliation_string IS NULL OR TRIM(c.raw_affiliation_string) = '')
		%s
	`, limitSQL))
	if err != nil {
		return err
	}
	defer rows.Close()

	type PaperInfo struct {
		ID  string
		DOI string
	}
	var papers []PaperInfo
	for rows.Next() {
		var p PaperInfo
		if err := rows.Scan(&p.ID, &p.DOI); err == nil {
			papers = append(papers, p)
		}
	}

	if len(papers) == 0 {
		return nil
	}

	email := "polite@example.com"
	var apiKey string
	if cfg, err := config.LoadConfig("config/collection.yml"); err == nil {
		if cfg.API.Email != "" {
			email = cfg.API.Email
		}
		apiKey = cfg.API.GroqKey
	}

	var llm LLMClient
	if strings.ToLower(provider) == "gemini" {
		llm = &GeminiClient{APIKey: apiKey, Model: model}
	} else {
		if baseURL == "" {
			baseURL = "http://localhost:11434"
		}
		llm = &OllamaClient{BaseURL: baseURL, Model: model}
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	cacheDir := "data/cache/pdf"
	os.MkdirAll(cacheDir, 0755)

	processed := 0

	for _, paper := range papers {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		normDOI := normalizeDOI(paper.DOI)
		var pdfURL string

		// Resolve PDF URL: arXiv -> Unpaywall -> OpenAlex
		if strings.HasPrefix(strings.ToLower(normDOI), "10.48550/arxiv.") {
			arxivID := normDOI[len("10.48550/arxiv."):]
			pdfURL = fmt.Sprintf("https://arxiv.org/pdf/%s.pdf", arxivID)
		}

		if pdfURL == "" {
			pdfURL = unpaywallPDFURL(ctx, httpClient, normDOI, email)
		}
		if pdfURL == "" {
			pdfURL = openalexPDFURL(ctx, httpClient, normDOI, email)
		}

		if pdfURL == "" {
			continue
		}

		// Download PDF
		h := sha1.New()
		h.Write([]byte(normDOI))
		pdfPath := filepath.Join(cacheDir, fmt.Sprintf("%x.pdf", h.Sum(nil)))

		downloaded := false
		if _, err := os.Stat(pdfPath); err == nil {
			downloaded = true
		} else {
			req, err := http.NewRequestWithContext(ctx, "GET", pdfURL, nil)
			if err == nil {
				resp, err := httpClient.Do(req)
				if err == nil && resp.StatusCode == http.StatusOK {
					ctype := strings.ToLower(resp.Header.Get("Content-Type"))
					if strings.Contains(ctype, "pdf") || strings.Contains(ctype, "octet-stream") {
						out, err := os.Create(pdfPath)
						if err == nil {
							_, err = io.Copy(out, resp.Body)
							out.Close()
							if err == nil {
								downloaded = true
							}
						}
					}
					resp.Body.Close()
				}
			}
		}

		if !downloaded {
			continue
		}

		// Extract first-page text
		pageText, err := extractPageText(pdfPath, 1)
		if err != nil || strings.TrimSpace(pageText) == "" {
			continue
		}

		// Prompt LLM to extract authors & affiliations
		prompt := fmt.Sprintf(
			"You are given the first page of an academic paper. Extract every author together with the primary institution they are affiliated with on this paper and that institution's ISO-3166-1 alpha-2 country code.\n"+
				"Rules:\n"+
				"1) 'position' is the zero-based order of the author in the byline.\n"+
				"2) 'institution' should be the most specific identifiable organisation; skip department names and postal addresses.\n"+
				"3) Return valid JSON containing an 'authors' array. Each item must contain: 'position' (int), 'given' (string or null), 'family' (string or null), 'orcid' (string or null), 'institution' (string or null), 'country_code' (string or null), and 'confidence' (float).\n"+
				"Page text:\n-----\n%s\n-----", pageText,
		)

		respText, err := llm.Complete(ctx, prompt)
		if err != nil {
			continue
		}

		cleanedResp := parseJSONMarkdown(respText)

		var parsedResponse struct {
			Authors []struct {
				Position    int     `json:"position"`
				Given       *string `json:"given"`
				Family      *string `json:"family"`
				ORCID       *string `json:"orcid"`
				Institution *string `json:"institution"`
				CountryCode *string `json:"country_code"`
				Confidence  float64 `json:"confidence"`
			} `json:"authors"`
		}

		if err := json.Unmarshal([]byte(cleanedResp), &parsedResponse); err != nil {
			continue
		}

		// Match LLM extracted authors with contributions targets
		targetRows, err := dbConn.Query(`
			SELECT c.row_id, c.author_id, c.author_position, a.orcid, c.institution_id, c.country_code
			FROM contributions c
			LEFT JOIN authors a ON a.id = c.author_id
			WHERE c.paper_id = ?
			ORDER BY c.row_id
		`, paper.ID)
		if err != nil {
			continue
		}

		type TargetRow struct {
			RowID          int
			AuthorID       sql.NullString
			AuthorPosition sql.NullString
			ORCID          sql.NullString
			InstID         sql.NullString
			CountryCode    sql.NullString
		}
		var targets []TargetRow
		for targetRows.Next() {
			var t TargetRow
			if err := targetRows.Scan(&t.RowID, &t.AuthorID, &t.AuthorPosition, &t.ORCID, &t.InstID, &t.CountryCode); err == nil {
				targets = append(targets, t)
			}
		}
		targetRows.Close()

		if len(targets) == 0 || len(parsedResponse.Authors) == 0 {
			continue
		}

		// Perform matching similar to CrossRef (ORCID first, then position index if count aligns)
		type LlmAuth struct {
			Position    int
			ORCID       string
			Institution string
			CountryCode string
			Confidence  float64
		}
		var llmAuthors []LlmAuth
		for _, a := range parsedResponse.Authors {
			orcid := ""
			if a.ORCID != nil {
				orcid = *a.ORCID
			}
			inst := ""
			if a.Institution != nil {
				inst = *a.Institution
			}
			cc := ""
			if a.CountryCode != nil {
				cc = *a.CountryCode
			}
			llmAuthors = append(llmAuthors, LlmAuth{
				Position:    a.Position,
				ORCID:       orcid,
				Institution: inst,
				CountryCode: cc,
				Confidence:  a.Confidence,
			})
		}

		matched := make(map[int]LlmAuth)
		llmUsed := make(map[int]bool)

		// 1. ORCID matching
		for _, t := range targets {
			if t.ORCID.Valid && t.ORCID.String != "" {
				for idx, la := range llmAuthors {
					if la.ORCID == t.ORCID.String {
						matched[t.RowID] = la
						llmUsed[idx] = true
						break
					}
				}
			}
		}

		// 2. Position matching
		var remainingTargets []TargetRow
		for _, t := range targets {
			if _, ok := matched[t.RowID]; !ok {
				remainingTargets = append(remainingTargets, t)
			}
		}
		var remainingLlm []LlmAuth
		for idx, la := range llmAuthors {
			if !llmUsed[idx] {
				remainingLlm = append(remainingLlm, la)
			}
		}

		if len(remainingTargets) == len(remainingLlm) {
			for idx, t := range remainingTargets {
				matched[t.RowID] = remainingLlm[idx]
			}
		}

		// Load matcher for this stage
		rowsInst, _ := dbConn.Query("SELECT id, display_name, country_code FROM institutions WHERE display_name IS NOT NULL")
		var recordsInst []InstitutionRecord
		if rowsInst != nil {
			for rowsInst.Next() {
				var r InstitutionRecord
				var cc sql.NullString
				if err := rowsInst.Scan(&r.ID, &r.DisplayName, &cc); err == nil {
					r.CountryCode = cc.String
					recordsInst = append(recordsInst, r)
				}
			}
			rowsInst.Close()
		}
		matcherInst := NewInstitutionMatcher("default")
		matcherInst.Index(recordsInst)

		tx, err := dbConn.Begin()
		if err != nil {
			continue
		}

		stmtUpdate, _ := tx.Prepare(`
			UPDATE contributions
			SET institution_id = ?, country_code = COALESCE(country_code, ?)
			WHERE row_id = ?
		`)
		stmtInsertInst, _ := tx.Prepare(`
			INSERT INTO institutions (id, display_name, country_code, type, ror_id, is_synthetic)
			VALUES (?, ?, ?, 'education', NULL, TRUE)
			ON CONFLICT DO NOTHING
		`)
		stmtAudit, _ := tx.Prepare(`
			INSERT INTO country_imputation_audit (
				row_id, paper_id, inferred_country_code, matched_terms, raw_affiliation_string, source, confidence, stage
			) VALUES (?, ?, ?, ?, ?, 'pdf', ?, 'pdf')
		`)

		for rowID, la := range matched {
			var instID string
			cc := NormalizeCountryCode(la.CountryCode)

			if la.Institution != "" {
				match := matcherInst.FindMatch(la.Institution, 0.85)
				if match != nil {
					instID = match.InstitutionID
					if cc == "" {
						cc = match.CountryCode
					}
				} else {
					instID = SyntheticInstitutionID(la.Institution)
					stmtInsertInst.Exec(instID, la.Institution, sql.NullString{String: cc, Valid: cc != ""})
				}
			}

			if instID != "" || cc != "" {
				var instVal interface{} = nil
				if instID != "" {
					instVal = instID
				}
				var ccVal interface{} = nil
				if cc != "" {
					ccVal = cc
				}
				stmtUpdate.Exec(instVal, ccVal, rowID)
				stmtAudit.Exec(rowID, paper.ID, ccVal, "pdf_stage", la.Institution, la.Confidence)
			}
		}

		stmtUpdate.Close()
		stmtInsertInst.Close()
		stmtAudit.Close()
		tx.Commit()

		processed++
		if progressChan != nil {
			progressChan <- processed
		}
	}

	return nil
}
