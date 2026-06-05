package codeintel

// bm25 implements Okapi BM25 ranking for full-text code search.
//
// Design matches the TS BM25 class from
// donmai-libraries/packages/code-intelligence/src/search/bm25.ts.
//
// Parameters:
//   - k1 (term-frequency saturation): 1.5
//   - b  (document-length normalisation): 0.75
//
// The tokenizer is a code-aware splitter that also expands camelCase and
// snake_case, matching the TS CodeTokenizer. Intentional deviations from TS:
//   - The TS inverted-index stores only one posting per (doc, term) pair; this
//     implementation matches that behaviour.
//   - Unicode punctuation splitting uses Go's unicode package rather than the
//     \p{P} class, which produces identical token sets for ASCII source and
//     near-identical sets for non-ASCII identifiers.

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// ── Tokenizer ────────────────────────────────────────────────────────────────

// tokenize splits text into lowercase tokens applying code-aware splitting:
//  1. Split on whitespace and delimiter characters (matching TS tokenizer).
//  2. Expand camelCase / PascalCase sub-tokens.
//  3. Expand snake_case / kebab-case sub-tokens.
//
// The resulting slice may contain duplicates (TS behaviour preserved).
func tokenize(text string) []string {
	var tokens []string

	// Split on delimiters (TS: /[\s.,:;()\[\]{}<>=!&|+*/\\@#$%^~`'"]+/)
	words := reDelimiters.Split(text, -1)
	for _, word := range words {
		if word == "" {
			continue
		}
		lower := strings.ToLower(word)
		tokens = append(tokens, lower)

		// camelCase / PascalCase expansion.
		parts := splitCamelCase(word)
		if len(parts) > 1 {
			for _, p := range parts {
				pl := strings.ToLower(p)
				if len([]rune(pl)) >= 2 && pl != lower {
					tokens = append(tokens, pl)
				}
			}
		}

		// snake_case / kebab-case expansion.
		snakeParts := reSnakeKebab.Split(word, -1)
		if len(snakeParts) > 1 {
			seen := make(map[string]bool)
			for _, t := range tokens {
				seen[t] = true
			}
			for _, p := range snakeParts {
				pl := strings.ToLower(p)
				if len([]rune(pl)) >= 2 && !seen[pl] {
					tokens = append(tokens, pl)
					seen[pl] = true
				}
			}
		}
	}
	return tokens
}

// splitCamelCase splits a word on camelCase / PascalCase boundaries.
// Matches the TS implementation using the two-pass regex approach.
func splitCamelCase(word string) []string {
	// Insert boundary between lowercase → uppercase.
	s := reCamelBoundaryLU.ReplaceAllString(word, "${1}\x00${2}")
	// Insert boundary between runs of uppercase letters + the transition letter.
	s = reCamelBoundaryUU.ReplaceAllString(s, "${1}\x00${2}")
	parts := strings.Split(s, "\x00")
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// symbolToText converts a CodeSymbol to the text that will be indexed.
// Matches the TS InvertedIndex.symbolToText method.
func symbolToText(sym CodeSymbol) string {
	parts := []string{sym.Name, string(sym.Kind)}
	if sym.Signature != "" {
		parts = append(parts, sym.Signature)
	}
	if sym.Documentation != "" {
		parts = append(parts, sym.Documentation)
	}
	if sym.FilePath != "" {
		parts = append(parts, sym.FilePath)
	}
	return strings.Join(parts, " ")
}

// ── Inverted index ───────────────────────────────────────────────────────────

type postingEntry struct {
	docID    int
	termFreq int
}

// invertedIndex maps lowercase tokens to posting lists.
type invertedIndex struct {
	index      map[string][]postingEntry
	docLengths []int
	avgDocLen  float64
	N          int // total document count
}

// buildInvertedIndex creates an index over the symbols slice.
func buildInvertedIndex(symbols []CodeSymbol) invertedIndex {
	idx := invertedIndex{
		index:      make(map[string][]postingEntry, len(symbols)*4),
		docLengths: make([]int, len(symbols)),
	}
	totalLen := 0

	for docID, sym := range symbols {
		text := symbolToText(sym)
		toks := tokenize(text)
		idx.docLengths[docID] = len(toks)
		totalLen += len(toks)

		// Term frequencies per document.
		tf := make(map[string]int, len(toks))
		for _, t := range toks {
			tf[t]++
		}
		for term, freq := range tf {
			idx.index[term] = append(idx.index[term], postingEntry{docID: docID, termFreq: freq})
		}
	}

	idx.N = len(symbols)
	if idx.N > 0 {
		idx.avgDocLen = float64(totalLen) / float64(idx.N)
	}
	return idx
}

// ── BM25 scoring ─────────────────────────────────────────────────────────────

// bm25Score returns document scores for a query against the inverted index.
// Documents are ranked by descending score.
func bm25Score(query string, idx invertedIndex) []scoredDoc {
	queryTokens := tokenize(query)
	if idx.N == 0 || len(queryTokens) == 0 {
		return nil
	}

	scores := make(map[int]float64, idx.N)
	N := float64(idx.N)
	avgdl := idx.avgDocLen

	for _, token := range queryTokens {
		postings, ok := idx.index[token]
		if !ok {
			continue
		}
		df := float64(len(postings))
		// IDF: log((N - df + 0.5) / (df + 0.5) + 1)
		idf := math.Log((N-df+0.5)/(df+0.5) + 1)

		for _, p := range postings {
			tf := float64(p.termFreq)
			dl := float64(idx.docLengths[p.docID])
			// BM25 TF component.
			tfNorm := (tf * (bm25K1 + 1)) / (tf + bm25K1*(1-bm25B+bm25B*(dl/avgdl)))
			scores[p.docID] += idf * tfNorm
		}
	}

	result := make([]scoredDoc, 0, len(scores))
	for docID, score := range scores {
		result = append(result, scoredDoc{docID: docID, score: score})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].score > result[j].score
	})
	return result
}

// scoredDoc holds a document ID and its BM25 score.
type scoredDoc struct {
	docID int
	score float64
}

// ── Compiled regexps ──────────────────────────────────────────────────────────

var (
	// Matching the TS tokenizer split pattern: [\s.,:;()\[\]{}<>=!&|+*/\\@#$%^~`'"]+
	// Go regex uses the equivalent character class. The vertical bar | is not
	// special inside a character class.
	reDelimiters = regexp.MustCompile(`[\s.,:;()\[\]{}<>=!&|+*/\\@#$%^~` + "`" + `'"]+`)

	// snake_case / kebab-case splitter.
	reSnakeKebab = regexp.MustCompile(`[_-]`)

	// camelCase: lowercase → Uppercase boundary: "helloWorld" → "hello\0World"
	reCamelBoundaryLU = regexp.MustCompile(`([a-z])([A-Z])`)

	// ALLCAPS + transition: "HTMLParser" → "HTML\0Parser"
	reCamelBoundaryUU = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
)
