package tfidf

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// StopWords is a predefined set of common English stop words
var StopWords = map[string]bool{
	"a": true, "about": true, "above": true, "after": true, "again": true, "against": true,
	"all": true, "am": true, "an": true, "and": true, "any": true, "are": true, "aren't": true,
	"as": true, "at": true, "be": true, "because": true, "been": true, "before": true,
	"being": true, "below": true, "between": true, "both": true, "but": true, "by": true,
	"can't": true, "cannot": true, "could": true, "couldn't": true, "did": true, "didn't": true,
	"do": true, "does": true, "doesn't": true, "doing": true, "don't": true, "down": true,
	"during": true, "each": true, "few": true, "for": true, "from": true, "further": true,
	"had": true, "hadn't": true, "has": true, "hasn't": true, "have": true, "haven't": true,
	"having": true, "he": true, "he'd": true, "he'll": true, "he's": true, "her": true,
	"here": true, "here's": true, "hers": true, "herself": true, "him": true, "himself": true,
	"his": true, "how": true, "how's": true, "i": true, "i'd": true, "i'll": true,
	"i'm": true, "i've": true, "if": true, "in": true, "into": true, "is": true, "isn't": true,
	"it": true, "it's": true, "its": true, "itself": true, "let's": true, "me": true,
	"more": true, "most": true, "mustn't": true, "my": true, "myself": true, "no": true,
	"nor": true, "not": true, "of": true, "off": true, "on": true, "once": true,
	"only": true, "or": true, "other": true, "ought": true, "our": true, "ours": true,
	"ourselves": true, "out": true, "over": true, "own": true, "same": true, "shan't": true,
	"she": true, "she'd": true, "she'll": true, "she's": true, "should": true, "shouldn't": true,
	"so": true, "some": true, "such": true, "than": true, "that": true, "that's": true,
	"the": true, "their": true, "theirs": true, "them": true, "themselves": true, "then": true,
	"there": true, "there's": true, "these": true, "they": true, "they'd": true, "they'll": true,
	"they're": true, "they've": true, "this": true, "those": true, "through": true, "to": true,
	"too": true, "under": true, "until": true, "up": true, "very": true, "was": true,
	"wasn't": true, "we": true, "we'd": true, "we'll": true, "we're": true, "we've": true,
	"were": true, "weren't": true, "what": true, "what's": true, "when": true, "when's": true,
	"where": true, "where's": true, "which": true, "while": true, "who": true, "who's": true,
	"whom": true, "why": true, "why's": true, "with": true, "won't": true, "would": true,
	"wouldn't": true, "you": true, "you'd": true, "you'll": true, "you're": true, "you've": true,
	"your": true, "yours": true, "yourself": true, "yourselves": true,
}

var wordRegexp = regexp.MustCompile(`[a-zA-Z][a-zA-Z\-]*`)

// Tokenize converts a text string into a list of lowercase alphanumeric words, filtering out stop words.
func Tokenize(text string) []string {
	text = strings.ToLower(text)
	matches := wordRegexp.FindAllString(text, -1)
	var tokens []string
	for _, m := range matches {
		m = strings.Trim(m, "-") // Trim trailing/leading hyphens
		if m != "" && !StopWords[m] {
			tokens = append(tokens, m)
		}
	}
	return tokens
}

// GenerateNGrams generates n-grams from a slice of tokens for sizes from ngramMin to ngramMax.
func GenerateNGrams(tokens []string, ngramMin, ngramMax int) []string {
	var ngrams []string
	n := len(tokens)
	for sz := ngramMin; sz <= ngramMax; sz++ {
		if sz > n {
			continue
		}
		for i := 0; i <= n-sz; i++ {
			gram := strings.Join(tokens[i:i+sz], " ")
			ngrams = append(ngrams, gram)
		}
	}
	return ngrams
}

// ScoredTerm represents a term and its computed mean TF-IDF score.
type ScoredTerm struct {
	Term  string  `json:"term"`
	Score float64 `json:"score"`
}

// ExtractKeywords processes a list of documents and returns the top terms scored by TF-IDF.
func ExtractKeywords(docs []string, ngramMin, ngramMax int, minDF int, maxDF float64, topN int) []ScoredTerm {
	if ngramMin < 1 {
		ngramMin = 1
	}
	if ngramMax < ngramMin {
		ngramMax = ngramMin
	}
	if topN <= 0 {
		topN = 50
	}

	numDocs := len(docs)
	if numDocs == 0 {
		return nil
	}

	// 1. Tokenize documents and generate n-grams
	docTermCounts := make([]map[string]int, numDocs)
	dfMap := make(map[string]int)

	for i, doc := range docs {
		tokens := Tokenize(doc)
		grams := GenerateNGrams(tokens, ngramMin, ngramMax)
		counts := make(map[string]int)
		for _, g := range grams {
			counts[g]++
		}
		docTermCounts[i] = counts
		for term := range counts {
			dfMap[term]++
		}
	}

	// 2. Filter terms by DF limits
	activeTerms := make(map[string]bool)
	for term, df := range dfMap {
		dfRatio := float64(df) / float64(numDocs)
		if df >= minDF && dfRatio <= maxDF {
			activeTerms[term] = true
		}
	}

	if len(activeTerms) == 0 {
		return nil
	}

	// 3. Compute IDF for active terms using smooth IDF: ln((1 + N)/(1 + DF)) + 1
	idfMap := make(map[string]float64)
	for term := range activeTerms {
		df := dfMap[term]
		idfMap[term] = math.Log(float64(1+numDocs)/float64(1+df)) + 1.0
	}

	// 4. Calculate TF-IDF per document and L2 normalize
	tfidfVectors := make([]map[string]float64, numDocs)
	for i, counts := range docTermCounts {
		weights := make(map[string]float64)
		var l2Sum float64
		for term, tf := range counts {
			if !activeTerms[term] {
				continue
			}
			val := float64(tf) * idfMap[term]
			weights[term] = val
			l2Sum += val * val
		}
		norm := math.Sqrt(l2Sum)
		if norm > 0 {
			for term, val := range weights {
				weights[term] = val / norm
			}
		}
		tfidfVectors[i] = weights
	}

	// 5. Calculate mean TF-IDF score for each term across all documents
	meanScores := make(map[string]float64)
	for _, vec := range tfidfVectors {
		for term, val := range vec {
			meanScores[term] += val
		}
	}

	var results []ScoredTerm
	for term, sum := range meanScores {
		results = append(results, ScoredTerm{
			Term:  term,
			Score: sum / float64(numDocs),
		})
	}

	// 6. Sort results by score descending, then alphabetically
	sort.Slice(results, func(i, j int) bool {
		if math.Abs(results[i].Score-results[j].Score) < 1e-9 {
			return results[i].Term < results[j].Term
		}
		return results[i].Score > results[j].Score
	})

	if len(results) > topN {
		results = results[:topN]
	}

	return results
}
