package codeintel

// hybrid.go implements Go-native hybrid code search: BM25 top-K candidates
// rescored with Voyage embeddings (cosine similarity blended with BM25) and
// optionally reordered with Cohere cross-encoder rerank.
//
// This is the founder-locked Q2 v1 scope (see
// runs/2026-07-04-code-intel-capability/01-architecture.md decision D3.4):
// a direct Go HTTP client pair (no vector store, no HNSW, no whole-corpus
// embedding), bounded to per-query cost, env-key-gated with graceful
// degradation to pure BM25.
//
// # Cost profile (per search-code call, cache-cold)
//
//   - 1 Voyage embeddings call: query text, input_type=query.
//   - 1 Voyage embeddings call: up to hybridTopK candidate texts batched
//     into a single request, input_type=document (Voyage's batch limit is
//     128; hybridTopK is well under that).
//   - 1 Cohere rerank call (only if COHERE_API_KEY is set): up to
//     hybridTopK candidate texts in one request.
//
// Total: at most 3 HTTP calls, independent of corpus size (a repo with
// 100,000 symbols costs exactly the same 3 calls as one with 100). Cache-warm
// calls (same content already embedded by a prior search in this process)
// can drop to 1 call (query embed only) or 0 (identical query repeated).
//
// # Gating / degradation contract
//
//   - No VOYAGE_AI_API_KEY: isHybridEnabled() is false, applyHybridSearch
//     returns candidates completely unchanged and makes zero network calls.
//   - VOYAGE_AI_API_KEY set but the Voyage call fails (network error,
//     non-200, or exceeds hybridHTTPTimeout): exactly one stderr warning is
//     emitted and the original BM25-ordered candidates are returned
//     unchanged. This is never a hard error.
//   - COHERE_API_KEY unset, or the Cohere call fails: the Voyage-rescored
//     order is kept as final; at most one additional stderr warning is
//     emitted for a Cohere failure.
//
// # Security
//
// Candidate text (symbol name/signature/doc/file path) leaves the process
// ONLY when the operator has opted in by setting VOYAGE_AI_API_KEY. Neither
// client ever logs request bodies, response bodies, or key material —
// failures are summarized as "status N" / "request failed" only.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// searchCandidate mirrors the anonymous `result` struct declared inside
// SearchCodeNative (native.go:549-553) field-for-field — same field names,
// types, and order — so that integrating this hook there is a rename of the
// local type to this package-level one plus one guarded call, not a
// restructuring. See integration notes in the codeintel-v1-hybrid lane
// handoff for the exact patch.
type searchCandidate struct {
	symbol    CodeSymbol
	score     float64
	matchType string
}

const (
	// hybridTopK bounds how many BM25 candidates ever get embedded/reranked.
	// Per the design constraint (K~30-50), chosen at the middle of that
	// range. This is the single lever that keeps hybrid search's cost
	// independent of corpus size.
	hybridTopK = 40

	// hybridAlpha weights the Voyage cosine-similarity signal against the
	// (min-max normalized) BM25 signal in the blended score:
	//
	//	blended = alpha*normalizedVectorScore + (1-alpha)*normalizedBM25Score
	//
	// 0.45 matches the JS reference HybridSearchEngine's default alpha
	// (donmai-libraries/packages/code-intelligence/src/search/hybrid-search.ts),
	// slightly favoring lexical BM25 over pure semantic similarity for code
	// search, where exact identifier matches remain a strong signal.
	hybridAlpha = 0.45
)

// hybridHTTPTimeout bounds every Voyage/Cohere HTTP call. It is a var (not a
// const) so tests can shrink it to exercise the timeout-fallback path
// without a real 10s wait. Production code never mutates it.
var hybridHTTPTimeout = 10 * time.Second

// voyageAPIKey returns the Voyage API key from the environment, preferring
// VOYAGE_API_KEY (Voyage's own SDK convention, and what the platform/rensei
// credential path provisions) and falling back to VOYAGE_AI_API_KEY (the name
// this CLI's help text historically documented). Returning the first non-empty
// of the two keeps every existing deployment working while accepting the
// standard name the credential store actually ships.
func voyageAPIKey() string {
	if k := strings.TrimSpace(os.Getenv("VOYAGE_API_KEY")); k != "" {
		return k
	}
	return strings.TrimSpace(os.Getenv("VOYAGE_AI_API_KEY"))
}

// isHybridEnabled reports whether Voyage-based rescoring should run at all.
// This is the single gate: no key, no network, byte-identical BM25 output.
func isHybridEnabled() bool {
	return voyageAPIKey() != ""
}

// isRerankEnabled reports whether the Cohere rerank pass should run on top
// of Voyage-rescored results. Independent of isHybridEnabled at the type
// level, but applyHybridSearch only ever consults it after Voyage has
// already succeeded (rerank never runs BM25-only).
func isRerankEnabled() bool {
	return strings.TrimSpace(os.Getenv("COHERE_API_KEY")) != ""
}

// ── in-memory embedding cache ────────────────────────────────────────────────

// embedCache is a process-lifetime cache from content hash to embedding
// vector, so a long-lived MCP-server process never re-embeds candidate text
// it has already embedded (e.g. the same symbol showing up across repeated
// searches). Keyed by "<inputType>:<sha256(text)>" so query-mode and
// document-mode embeddings of coincidentally identical text never collide
// (Voyage's asymmetric encoding produces different vectors for each).
var (
	embedCacheMu sync.Mutex
	embedCache   = map[string][]float32{}
)

func contentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func embedCacheKey(inputType, text string) string {
	return inputType + ":" + contentHash(text)
}

func embedCacheGet(inputType, text string) ([]float32, bool) {
	embedCacheMu.Lock()
	defer embedCacheMu.Unlock()
	v, ok := embedCache[embedCacheKey(inputType, text)]
	return v, ok
}

func embedCacheSet(inputType, text string, vec []float32) {
	embedCacheMu.Lock()
	defer embedCacheMu.Unlock()
	embedCache[embedCacheKey(inputType, text)] = vec
}

// ── candidate text ───────────────────────────────────────────────────────────

// candidateText builds the text used for both embedding and reranking from a
// symbol's metadata. Deliberately self-contained (does not reuse bm25.go's
// symbolToText) so this file has no coupling to BM25 internals.
func candidateText(sym CodeSymbol) string {
	var parts []string
	if sym.Signature != "" {
		parts = append(parts, sym.Signature)
	}
	if sym.Documentation != "" {
		parts = append(parts, sym.Documentation)
	}
	parts = append(parts, string(sym.Kind)+" "+sym.Name)
	if sym.FilePath != "" {
		parts = append(parts, sym.FilePath)
	}
	return strings.Join(parts, "\n")
}

// ── scoring helpers ──────────────────────────────────────────────────────────

func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// minMaxNormalize rescales values to [0, 1]. A constant (or empty) input
// normalizes to all-zeroes rather than dividing by zero.
func minMaxNormalize(values []float64) []float64 {
	out := make([]float64, len(values))
	if len(values) == 0 {
		return out
	}
	lo, hi := values[0], values[0]
	for _, v := range values {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	span := hi - lo
	for i, v := range values {
		if span == 0 {
			out[i] = 0
			continue
		}
		out[i] = (v - lo) / span
	}
	return out
}

// warnf emits exactly one stderr line describing a graceful-degradation
// event. Never includes request/response bodies or key material — callers
// pass only a short, static reason plus a status code / error class.
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "codeintel: hybrid search: "+format+"\n", args...)
}

// ── orchestration ────────────────────────────────────────────────────────────

// applyHybridSearch is the hook SearchCodeNative calls after its own BM25
// scoring/boosting/sort. It is a no-op (identity, zero network) unless
// VOYAGE_AI_API_KEY is set. When enabled:
//
//  1. Split candidates into head (top hybridTopK, already BM25-sorted) and
//     tail (everything else, left completely untouched).
//  2. Embed the query and the head's candidate texts via Voyage (batched,
//     cache-checked), cosine-score against the query vector, and blend with
//     the head's existing (min-max normalized) BM25 score.
//  3. Re-sort head by the blended score.
//  4. If COHERE_API_KEY is set, rerank head via Cohere and adopt that order
//     (relevance score becomes the final score); on any Cohere failure, keep
//     the Voyage-blended order.
//  5. Return head ++ tail, same length as the input.
//
// Any Voyage failure (including timeout) aborts the whole hybrid pass and
// returns the original candidates completely unchanged, after exactly one
// stderr warning.
func applyHybridSearch(query string, candidates []searchCandidate) []searchCandidate {
	if !isHybridEnabled() || len(candidates) == 0 {
		return candidates
	}

	k := hybridTopK
	if k > len(candidates) {
		k = len(candidates)
	}

	// Copy so we never mutate the caller's backing array while rescoring —
	// the caller (native.go) already holds the slice it passed us.
	working := make([]searchCandidate, len(candidates))
	copy(working, candidates)
	head := working[:k]
	tail := working[k:]

	httpClient := &http.Client{Timeout: hybridHTTPTimeout}
	ctx, cancel := context.WithTimeout(context.Background(), hybridHTTPTimeout)
	defer cancel()

	apiKey := voyageAPIKey()
	vectors, err := embedHead(ctx, httpClient, apiKey, query, head)
	if err != nil {
		warnf("voyage embeddings unavailable, falling back to BM25-only results (%v)", err)
		return candidates
	}

	blendHeadScores(head, vectors)

	if isRerankEnabled() {
		if err := rerankHead(ctx, httpClient, os.Getenv("COHERE_API_KEY"), query, head); err != nil {
			warnf("cohere rerank unavailable, keeping voyage-scored order (%v)", err)
			// Fall through: head is still sorted by the blended score below.
		}
	}

	sort.SliceStable(head, func(i, j int) bool {
		return head[i].score > head[j].score
	})

	out := make([]searchCandidate, 0, len(working))
	out = append(out, head...)
	out = append(out, tail...)
	return out
}

// embedHead embeds the query (input_type=query) and every head candidate's
// text (input_type=document, batched into a single request for cache
// misses), consulting embedCache first. Returns one vector per head
// candidate plus the query vector, or an error if either Voyage call fails.
func embedHead(ctx context.Context, httpClient *http.Client, apiKey, query string, head []searchCandidate) (queryAndDocs struct {
	query []float32
	docs  [][]float32
}, err error,
) {
	// Query embedding (rarely cached, since queries vary, but a cache hit
	// on a repeated identical query costs nothing extra).
	if v, ok := embedCacheGet("query", query); ok {
		queryAndDocs.query = v
	} else {
		vecs, embedErr := voyageEmbed(ctx, httpClient, apiKey, []string{query}, "query")
		if embedErr != nil {
			err = fmt.Errorf("query embed: %w", embedErr)
			return
		}
		queryAndDocs.query = vecs[0]
		embedCacheSet("query", query, vecs[0])
	}

	// Candidate (document) embeddings: gather cache misses, embed them in
	// one batched call, and merge back in order.
	texts := make([]string, len(head))
	for i, c := range head {
		texts[i] = candidateText(c.symbol)
	}

	docs := make([][]float32, len(head))
	var missIdx []int
	var missTexts []string
	for i, t := range texts {
		if v, ok := embedCacheGet("document", t); ok {
			docs[i] = v
			continue
		}
		missIdx = append(missIdx, i)
		missTexts = append(missTexts, t)
	}

	if len(missTexts) > 0 {
		vecs, embedErr := voyageEmbed(ctx, httpClient, apiKey, missTexts, "document")
		if embedErr != nil {
			err = fmt.Errorf("document embed: %w", embedErr)
			return
		}
		for j, idx := range missIdx {
			docs[idx] = vecs[j]
			embedCacheSet("document", texts[idx], vecs[j])
		}
	}

	queryAndDocs.docs = docs
	return
}

// blendHeadScores overwrites each head candidate's score with the CCS
// (Convex Combination Score) blend of its normalized BM25 score and its
// normalized cosine similarity to the query, and tags matchType "hybrid".
func blendHeadScores(head []searchCandidate, vectors struct {
	query []float32
	docs  [][]float32
},
) {
	bm25Raw := make([]float64, len(head))
	vecRaw := make([]float64, len(head))
	for i, c := range head {
		bm25Raw[i] = c.score
		vecRaw[i] = cosineSimilarity(vectors.query, vectors.docs[i])
	}
	bm25Norm := minMaxNormalize(bm25Raw)
	vecNorm := minMaxNormalize(vecRaw)

	for i := range head {
		head[i].score = hybridAlpha*vecNorm[i] + (1-hybridAlpha)*bm25Norm[i]
		head[i].matchType = "hybrid"
	}
}

// rerankHead calls Cohere over the head's candidate texts and, on success,
// overwrites each candidate's score with its reranked relevance score
// (higher = more relevant). Leaves head untouched on any error.
func rerankHead(ctx context.Context, httpClient *http.Client, apiKey, query string, head []searchCandidate) error {
	texts := make([]string, len(head))
	for i, c := range head {
		texts[i] = candidateText(c.symbol)
	}

	results, err := cohereRerank(ctx, httpClient, apiKey, query, texts)
	if err != nil {
		return err
	}

	for _, r := range results {
		if r.Index < 0 || r.Index >= len(head) {
			continue
		}
		head[r.Index].score = r.Score
	}
	return nil
}
