package codeintel

// SimHash implements 64-bit Charikar SimHash for near-duplicate detection.
//
// Design matches the TS SimHash class from
// donmai-libraries/packages/code-intelligence/src/memory/simhash.ts.
//
// Token hash: FNV-1a-like 64-bit hash (same constant as TS: 0xcbf29ce484222325,
// multiplier 0x100000001b3). The TS implementation uses BigInt operations that
// wrap at 64 bits; this port uses uint64 arithmetic which is identical.
//
// Hamming-distance threshold for near-duplicate detection: 3 (same default as TS).
//
// Intentional deviation: the TS tokenizer uses \p{P} (Unicode punctuation
// class); this port uses unicode.IsPunct which covers the same set for
// ASCII source and nearly all Unicode code points that appear in code files.

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// SimHashBits is the fingerprint width.
	SimHashBits = 64
	// SimHashDefaultThreshold is the default Hamming-distance threshold for
	// near-duplicate detection. Matches the TS default.
	SimHashDefaultThreshold = 3
)

// fnv1aLike64 computes a 64-bit FNV-1a-like hash matching the TS tokenHash
// function (constants 0xcbf29ce484222325 and 0x100000001b3).
func fnv1aLike64(s string) uint64 {
	const (
		offset = uint64(0xcbf29ce484222325)
		prime  = uint64(0x100000001b3)
	)
	h := offset
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		// The TS xors with charCodeAt, which is the Unicode code point.
		h ^= uint64(r) //nolint:gosec // intentional: Unicode code-point fits in uint64; no truncation risk.
		h *= prime
	}
	return h
}

// simHashTokenize splits text into tokens of length >= 2.
// Matches the TS SimHash.tokenize: lowercase, split on whitespace and Unicode
// punctuation, filter tokens shorter than 2 characters.
func simHashTokenize(text string) []string {
	lower := strings.ToLower(text)
	// Split on whitespace and punctuation.
	parts := reSimHashDelim.Split(lower, -1)
	out := parts[:0]
	for _, p := range parts {
		if len([]rune(p)) >= 2 {
			out = append(out, p)
		}
	}
	return out
}

// SimHashCompute returns the 64-bit SimHash fingerprint for text.
// Matches the TS SimHash.compute method.
func SimHashCompute(text string) uint64 {
	tokens := simHashTokenize(text)
	if len(tokens) == 0 {
		return 0
	}

	weights := [SimHashBits]float64{}
	for _, token := range tokens {
		h := fnv1aLike64(token)
		for i := 0; i < SimHashBits; i++ {
			if (h>>uint(i))&1 == 1 {
				weights[i]++
			} else {
				weights[i]--
			}
		}
	}

	var fingerprint uint64
	for i := 0; i < SimHashBits; i++ {
		if weights[i] > 0 {
			fingerprint |= uint64(1) << uint(i)
		}
	}
	return fingerprint
}

// SimHashHammingDistance returns the number of differing bits between a and b.
func SimHashHammingDistance(a, b uint64) int {
	xor := a ^ b
	count := 0
	for xor != 0 {
		count++
		xor &= xor - 1 // clear lowest set bit
	}
	return count
}

// ── DupEntry — in-memory store entry ─────────────────────────────────────────

// DupEntry stores the hashes for a single indexed content item.
type DupEntry struct {
	ID      string
	XXHash  string // hex, 16 chars
	SimHash uint64
}

// DupResult is the result of a CheckDuplicate call.
type DupResult struct {
	IsDuplicate     bool   `json:"isDuplicate"`
	MatchType       string `json:"matchType"`       // "exact", "near", "none"
	ExistingID      string `json:"existingId"`      // set when IsDuplicate=true
	HammingDistance int    `json:"hammingDistance"` // set when MatchType=="near"
}

// DupStore is an in-memory content deduplication store.
// Not safe for concurrent use; callers must synchronize externally if needed.
type DupStore struct {
	entries []DupEntry
}

// normalizeDupContent normalises content for consistent hashing, matching the
// TS DedupPipeline.normalize method:
//   - CRLF → LF
//   - tabs → two spaces
//   - trailing whitespace stripped per line
//   - trimmed overall
func normalizeDupContent(content string) string {
	s := strings.ReplaceAll(content, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\t", "  ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// CheckDuplicate checks content against the store for exact and near duplicates.
// Returns a DupResult. Does NOT store the content — call Store to add it.
func (d *DupStore) CheckDuplicate(content string, threshold int) DupResult {
	normalized := normalizeDupContent(content)
	hash := ContentXXHash64(normalized)

	// Tier 1: exact via xxHash64.
	for _, e := range d.entries {
		if e.XXHash == hash {
			return DupResult{
				IsDuplicate: true,
				MatchType:   "exact",
				ExistingID:  e.ID,
			}
		}
	}

	// Tier 2: near via SimHash.
	fp := SimHashCompute(normalized)
	for _, e := range d.entries {
		dist := SimHashHammingDistance(fp, e.SimHash)
		if dist <= threshold {
			return DupResult{
				IsDuplicate:     true,
				MatchType:       "near",
				ExistingID:      e.ID,
				HammingDistance: dist,
			}
		}
	}

	return DupResult{MatchType: "none"}
}

// Store adds content to the store.
func (d *DupStore) Store(id, content string) {
	normalized := normalizeDupContent(content)
	d.entries = append(d.entries, DupEntry{
		ID:      id,
		XXHash:  ContentXXHash64(normalized),
		SimHash: SimHashCompute(normalized),
	})
}

// CheckDuplicateContent performs a stateless duplicate check against an
// existing index using xxHash64 + SimHash. It operates on individual file
// content items without a persistent store.
//
// Parameters:
//   - content is the content to check.
//   - index is the list of FileIndex entries (from the persisted index.json)
//     representing the corpus to search.
//   - threshold is the SimHash Hamming-distance threshold.
func CheckDuplicateContent(content string, index []FileIndex, threshold int) DupResult {
	normalized := normalizeDupContent(content)
	contentHash := ContentXXHash64(normalized)
	contentFP := SimHashCompute(normalized)

	for _, fi := range index {
		// We index by git blob hash (SHA1), so we cannot do exact xxHash64
		// matching against the file hashes directly.  Instead we compute
		// the SimHash fingerprint of the file symbols as a corpus.  For
		// exact matching we compare the xxHash64 of the normalised content
		// against the normalised xxHash64 of the file contents — however the
		// index only stores git blob SHA1.  So for S3 we use SimHash only
		// across files and xxHash64 for within-request duplication.
		// That matches the TS DedupPipeline which holds an in-memory store.
		//
		// Intentional deviation from TS: the TS pipeline holds state across
		// calls (InMemoryStore); the native Go path uses the persisted index
		// file list as the corpus.  This means:
		//   - Exact dedup: we compare contentHash against a per-call xxHash64
		//     computed from the symbols text of each file.
		//   - Near dedup: SimHash(content) vs SimHash(file symbol text).
		//
		// The TS DedupPipeline is designed for memory entries, not code files.
		// We apply the same two-tier strategy but over code symbols.

		fileText := fileIndexToText(fi)
		fileHash := ContentXXHash64(fileText)
		if fileHash == contentHash {
			return DupResult{
				IsDuplicate: true,
				MatchType:   "exact",
				ExistingID:  fi.FilePath,
			}
		}

		fileFP := SimHashCompute(fileText)
		dist := SimHashHammingDistance(contentFP, fileFP)
		if dist <= threshold {
			return DupResult{
				IsDuplicate:     true,
				MatchType:       "near",
				ExistingID:      fi.FilePath,
				HammingDistance: dist,
			}
		}
	}

	return DupResult{MatchType: "none"}
}

// fileIndexToText converts a FileIndex into a text string for SimHash computation.
func fileIndexToText(fi FileIndex) string {
	parts := make([]string, 0, len(fi.Symbols)+1)
	parts = append(parts, fi.FilePath)
	for _, sym := range fi.Symbols {
		parts = append(parts, symbolToText(sym))
	}
	return strings.Join(parts, " ")
}

// ── Compiled regexps ──────────────────────────────────────────────────────────

// reSimHashDelim matches whitespace and Unicode punctuation for tokenization.
// Intentional deviation from TS \p{P}: Go's regexp2/unicode package provides
// `\pP` which is identical for all Unicode punctuation.
// Using a regexp that matches spaces + the Unicode P category.
var reSimHashDelim = regexp.MustCompile(`[\s\pP]+`)
