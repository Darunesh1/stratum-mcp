package tfidf

import (
	"testing"
)

func TestTokenize(t *testing.T) {
	text := "Green hydrogen is clean, renewable-energy, and sustainable."
	tokens := Tokenize(text)

	// Stop words ("is", "and") should be removed, case normalized to lower
	expected := []string{"green", "hydrogen", "clean", "renewable-energy", "sustainable"}
	if len(tokens) != len(expected) {
		t.Fatalf("expected %d tokens, got %d", len(expected), len(tokens))
	}
	for i, v := range expected {
		if tokens[i] != v {
			t.Errorf("expected token %d to be %q, got %q", i, v, tokens[i])
		}
	}
}

func TestGenerateNGrams(t *testing.T) {
	tokens := []string{"green", "hydrogen", "production"}
	grams := GenerateNGrams(tokens, 2, 3)

	expected := []string{
		"green hydrogen",
		"hydrogen production",
		"green hydrogen production",
	}

	if len(grams) != len(expected) {
		t.Fatalf("expected %d ngrams, got %d", len(expected), len(grams))
	}
	for i, v := range expected {
		if grams[i] != v {
			t.Errorf("expected ngram %d to be %q, got %q", i, v, grams[i])
		}
	}
}

func TestExtractKeywords(t *testing.T) {
	docs := []string{
		"Green hydrogen is the future of clean energy.",
		"Water splitting and electrolysis produces hydrogen.",
		"Clean energy and solar power are important.",
	}

	// Extract bigrams/trigrams (ngramMin=2, ngramMax=2)
	keywords := ExtractKeywords(docs, 2, 2, 1, 1.0, 5)

	if len(keywords) == 0 {
		t.Fatalf("expected to extract keywords, got 0")
	}

	// Verify order and basic presence
	foundCleanEnergy := false
	for _, kw := range keywords {
		if kw.Term == "clean energy" {
			foundCleanEnergy = true
		}
		if kw.Score <= 0 || kw.Score > 1.0 {
			t.Errorf("term %q has invalid score %f", kw.Term, kw.Score)
		}
	}

	if !foundCleanEnergy {
		t.Errorf("expected to find term 'clean energy'")
	}

	// Let's check with limit
	limitedKeywords := ExtractKeywords(docs, 1, 2, 1, 1.0, 2)
	if len(limitedKeywords) != 2 {
		t.Errorf("expected 2 keywords, got %d", len(limitedKeywords))
	}
}
