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
	"sort"
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
//
// The v4 additions (FilePath, SymbolName, Line) carry symbol-granular match
// identity; existing fields keep their v2 names and meaning (ExistingID stays
// the matched file's path) so JSON consumers of the old shape keep working.
type DupResult struct {
	IsDuplicate     bool   `json:"isDuplicate"`
	MatchType       string `json:"matchType"`       // "exact", "near", "none"
	ExistingID      string `json:"existingId"`      // set when IsDuplicate=true; the matched file path
	HammingDistance int    `json:"hammingDistance"` // set when MatchType=="near"
	FilePath        string `json:"filePath,omitempty"`
	SymbolName      string `json:"symbolName,omitempty"` // set on a symbol-granular match
	Line            int    `json:"line,omitempty"`       // 1-based declaration line of the matched symbol
}

// DupMatch is one ranked duplicate site returned by FindDuplicateMatches.
// SymbolName/Line are zero for a file-level match.
type DupMatch struct {
	FilePath        string `json:"filePath"`
	SymbolName      string `json:"symbolName,omitempty"`
	Line            int    `json:"line,omitempty"`
	MatchType       string `json:"matchType"` // "exact" or "near"
	HammingDistance int    `json:"hammingDistance,omitempty"`
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

// symbolHashMinLines is the minimum body extent (declaration line through
// EndLine, inclusive) for a symbol to get its own dedup fingerprint. Shorter
// symbols are too token-poor for SimHash to discriminate (false-positive
// risk) and hashing every one-liner would bloat the index; whole-file hashing
// still covers them.
const symbolHashMinLines = 3

// ComputeSymbolHashes computes the symbol-granular dedup fingerprints for one
// file's extracted symbols (schema v4). For each symbol with a known body
// extent (EndLine set) spanning at least symbolHashMinLines lines, the body —
// the declaration line through EndLine — is normalised with the SAME
// normalization as whole-file dedup hashing (normalizeDupContent) and hashed
// with xxHash64 (exact tier) + SimHash (near tier). Symbols without EndLine
// (e.g. Python/Rust extractors don't emit body extents yet) are skipped and
// remain covered by whole-file hashing only.
func ComputeSymbolHashes(content string, symbols []CodeSymbol) []SymbolDup {
	if len(symbols) == 0 {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	var out []SymbolDup
	for _, s := range symbols {
		if s.EndLine == nil {
			continue
		}
		start, end := s.Line, *s.EndLine
		if start < 1 || end > len(lines) || end-start+1 < symbolHashMinLines {
			continue
		}
		normalized := normalizeDupContent(strings.Join(lines[start-1:end], "\n"))
		if normalized == "" {
			continue
		}
		out = append(out, SymbolDup{
			Name:        s.Name,
			Line:        s.Line,
			ContentHash: ContentXXHash64(normalized),
			SimHash:     SimHashCompute(normalized),
		})
	}
	return out
}

// FindDuplicateMatches performs a stateless duplicate check of content against
// BOTH the file-level and symbol-level fingerprints captured in the persisted
// index (schema v4), returning up to maxResults ranked duplicate sites.
//
// Two tiers at two granularities:
//
//   - Exact: the query's normalised-content xxHash64 is compared against each
//     file's persisted ContentHash (whole-file paste) and each symbol's
//     persisted ContentHash (function/type paste buried in a larger file).
//   - Near: the query's SimHash fingerprint is compared against the persisted
//     file-level and symbol-level SimHashes using Hamming distance; within
//     threshold ⇒ near-dup.
//
// Ranking: exact before near; within a tier, a symbol-level match wins over a
// file-level one (it pinpoints the duplicate — the agent needs no grep
// follow-up); near matches order by ascending Hamming distance; remaining ties
// break on (filePath, line) for determinism. Entries lacking fingerprints
// (pre-v4 or failed-to-hash) are skipped rather than producing spurious
// matches. maxResults <= 0 means 1.
func FindDuplicateMatches(content string, index []FileIndex, threshold, maxResults int) []DupMatch {
	if maxResults <= 0 {
		maxResults = 1
	}
	normalized := normalizeDupContent(content)
	contentHash := ContentXXHash64(normalized)
	contentFP := SimHashCompute(normalized)

	type candidate struct {
		match DupMatch
		tier  int // 0 sym-exact, 1 file-exact, 2 sym-near, 3 file-near
	}
	var cands []candidate

	for _, fi := range index {
		for _, sh := range fi.SymbolHashes {
			if sh.ContentHash == "" {
				continue
			}
			if sh.ContentHash == contentHash {
				cands = append(cands, candidate{tier: 0, match: DupMatch{
					FilePath: fi.FilePath, SymbolName: sh.Name, Line: sh.Line, MatchType: "exact",
				}})
				continue
			}
			if sh.SimHash == 0 {
				continue
			}
			if dist := SimHashHammingDistance(contentFP, sh.SimHash); dist <= threshold {
				cands = append(cands, candidate{tier: 2, match: DupMatch{
					FilePath: fi.FilePath, SymbolName: sh.Name, Line: sh.Line,
					MatchType: "near", HammingDistance: dist,
				}})
			}
		}
		if fi.ContentHash == "" {
			continue // no real-content fingerprint to compare against
		}
		if fi.ContentHash == contentHash {
			cands = append(cands, candidate{tier: 1, match: DupMatch{
				FilePath: fi.FilePath, MatchType: "exact",
			}})
			continue
		}
		if fi.SimHash == 0 {
			continue
		}
		if dist := SimHashHammingDistance(contentFP, fi.SimHash); dist <= threshold {
			cands = append(cands, candidate{tier: 3, match: DupMatch{
				FilePath: fi.FilePath, MatchType: "near", HammingDistance: dist,
			}})
		}
	}

	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.tier != b.tier {
			return a.tier < b.tier
		}
		if a.match.HammingDistance != b.match.HammingDistance {
			return a.match.HammingDistance < b.match.HammingDistance
		}
		if a.match.FilePath != b.match.FilePath {
			return a.match.FilePath < b.match.FilePath
		}
		return a.match.Line < b.match.Line
	})

	if maxResults > len(cands) {
		maxResults = len(cands)
	}
	out := make([]DupMatch, 0, maxResults)
	for _, c := range cands[:maxResults] {
		out = append(out, c.match)
	}
	return out
}

// CheckDuplicateContent performs a stateless duplicate check of content
// against the persisted index and returns the single top match as a
// DupResult. Since schema v4 the comparison spans both file-level and
// symbol-level fingerprints (see FindDuplicateMatches for tiers and ranking);
// a symbol-level match carries SymbolName/Line so the caller can point at the
// exact duplicate site inside a larger file.
//
// Parameters:
//   - content is the content to check.
//   - index is the list of FileIndex entries (from the persisted index.json).
//   - threshold is the SimHash Hamming-distance threshold.
func CheckDuplicateContent(content string, index []FileIndex, threshold int) DupResult {
	matches := FindDuplicateMatches(content, index, threshold, 1)
	if len(matches) == 0 {
		return DupResult{MatchType: "none"}
	}
	m := matches[0]
	return DupResult{
		IsDuplicate:     true,
		MatchType:       m.MatchType,
		ExistingID:      m.FilePath,
		HammingDistance: m.HammingDistance,
		FilePath:        m.FilePath,
		SymbolName:      m.SymbolName,
		Line:            m.Line,
	}
}

// ── Compiled regexps ──────────────────────────────────────────────────────────

// reSimHashDelim matches whitespace and Unicode punctuation for tokenization.
// Intentional deviation from TS \p{P}: Go's regexp2/unicode package provides
// `\pP` which is identical for all Unicode punctuation.
// Using a regexp that matches spaces + the Unicode P category.
var reSimHashDelim = regexp.MustCompile(`[\s\pP]+`)
