package codeintel

// hybrid_test.go exercises the Go-native hybrid search hook: BM25 candidates
// rescored by Voyage embeddings and optionally reordered by Cohere rerank,
// gated entirely on env keys with graceful degradation to pure BM25.
//
// Both third-party APIs are faked with httptest servers; package-level
// endpoint vars (voyageAPIURL / cohereAPIURL) are overridden per-test so no
// real network egress ever happens in CI.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── test fixtures ────────────────────────────────────────────────────────────

func mkCandidates(n int) []searchCandidate {
	out := make([]searchCandidate, n)
	for i := 0; i < n; i++ {
		out[i] = searchCandidate{
			symbol: CodeSymbol{
				Name:      fmt.Sprintf("symbol%d", i),
				Kind:      KindFunction,
				FilePath:  fmt.Sprintf("pkg/file%d.go", i),
				Line:      i + 1,
				Signature: fmt.Sprintf("func symbol%d()", i),
			},
			score:     float64(n - i), // descending BM25 score, as native.go produces
			matchType: "bm25",
		}
	}
	return out
}

// withEnv sets an env var for the duration of the test and restores it after.
func withEnv(t *testing.T, key, val string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if val == "" {
		os.Unsetenv(key)
	} else {
		os.Setenv(key, val)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(key, prev)
		} else {
			os.Unsetenv(key)
		}
	})
}

// withEndpoint overrides a package-level API URL var for the test duration.
func withEndpoint(t *testing.T, target *string, val string) {
	t.Helper()
	prev := *target
	*target = val
	t.Cleanup(func() { *target = prev })
}

// resetEmbedCache clears the package-level embedding cache so tests that
// share deterministic candidate text (mkCandidates is fully deterministic)
// don't observe cache hits left behind by a previous test. Production code
// never needs this — the cache is meant to persist for the process
// lifetime.
func resetEmbedCache(t *testing.T) {
	t.Helper()
	embedCacheMu.Lock()
	embedCache = map[string][]float32{}
	embedCacheMu.Unlock()
}

// captureStderr redirects os.Stderr for the duration of fn and returns what
// was written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	fn()
	os.Stderr = orig
	w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

// fakeVoyageServer returns an httptest server that counts requests and
// returns an embedding per input text using the supplied vector function.
func fakeVoyageServer(t *testing.T, reqCount *int64, vecFor func(text, inputType string) []float32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(reqCount, 1)
		var body struct {
			Model     string   `json:"model"`
			Input     []string `json:"input"`
			InputType string   `json:"input_type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		type datum struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		resp := struct {
			Data  []datum `json:"data"`
			Model string  `json:"model"`
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}{Model: body.Model}
		for i, text := range body.Input {
			resp.Data = append(resp.Data, datum{Embedding: vecFor(text, body.InputType), Index: i})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// fakeCohereServer returns an httptest server that counts requests and
// reorders documents by the supplied score function.
func fakeCohereServer(t *testing.T, reqCount *int64, scoreFor func(text string) float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(reqCount, 1)
		var body struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		type result struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}
		resp := struct {
			Results []result `json:"results"`
		}{}
		for i, doc := range body.Documents {
			resp.Results = append(resp.Results, result{Index: i, RelevanceScore: scoreFor(doc)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// unitVec returns a unit-length embedding with weight concentrated on dim.
func unitVec(dim, size int) []float32 {
	v := make([]float32, size)
	v[dim] = 1
	return v
}

// ── tests ────────────────────────────────────────────────────────────────────

func TestIsHybridEnabled(t *testing.T) {
	withEnv(t, "VOYAGE_AI_API_KEY", "")
	if isHybridEnabled() {
		t.Fatal("expected hybrid disabled when VOYAGE_AI_API_KEY is unset")
	}
	withEnv(t, "VOYAGE_AI_API_KEY", "test-key-123")
	if !isHybridEnabled() {
		t.Fatal("expected hybrid enabled when VOYAGE_AI_API_KEY is set")
	}
}

// Key-absent passthrough: candidates come back byte-identical (same order,
// same scores) and zero network calls are made — proven by pointing the
// endpoints at an address nothing should ever dial.
func TestApplyHybridSearch_KeyAbsent_Passthrough(t *testing.T) {
	resetEmbedCache(t)
	withEnv(t, "VOYAGE_AI_API_KEY", "")
	withEnv(t, "COHERE_API_KEY", "")
	withEndpoint(t, &voyageAPIURL, "http://127.0.0.1:1/unreachable")
	withEndpoint(t, &cohereAPIURL, "http://127.0.0.1:1/unreachable")

	in := mkCandidates(5)
	out := applyHybridSearch("find the widget", in)

	if len(out) != len(in) {
		t.Fatalf("length changed: got %d want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("candidate %d mutated: got %+v want %+v", i, out[i], in[i])
		}
	}
}

// Happy path: Voyage rescores so a low-BM25 candidate that is semantically
// closest to the query rises, and Cohere rerank confirms/adjusts the final
// order.
func TestApplyHybridSearch_HappyPath_Reorders(t *testing.T) {
	resetEmbedCache(t)
	var voyageReqs, cohereReqs int64

	// symbol4 (lowest BM25 rank in mkCandidates) is made the closest semantic
	// match to the query by sharing its embedding dimension.
	voyage := fakeVoyageServer(t, &voyageReqs, func(text, inputType string) []float32 {
		if inputType == "query" {
			return unitVec(4, 8)
		}
		if strings.Contains(text, "symbol4") {
			return unitVec(4, 8)
		}
		return unitVec(0, 8)
	})
	defer voyage.Close()

	cohere := fakeCohereServer(t, &cohereReqs, func(text string) float64 {
		if strings.Contains(text, "symbol4") {
			return 0.99
		}
		return 0.1
	})
	defer cohere.Close()

	withEnv(t, "VOYAGE_AI_API_KEY", "test-voyage-key")
	withEnv(t, "COHERE_API_KEY", "test-cohere-key")
	withEndpoint(t, &voyageAPIURL, voyage.URL)
	withEndpoint(t, &cohereAPIURL, cohere.URL)

	in := mkCandidates(5) // symbol0..symbol4, BM25-descending (symbol0 best)
	out := applyHybridSearch("anything", in)

	if len(out) != len(in) {
		t.Fatalf("length changed: got %d want %d", len(out), len(in))
	}
	if out[0].symbol.Name != "symbol4" {
		t.Fatalf("expected symbol4 promoted to top by hybrid rescoring+rerank, got order: %v",
			namesOf(out))
	}
	if voyageReqs == 0 {
		t.Fatal("expected at least one Voyage request")
	}
	if cohereReqs == 0 {
		t.Fatal("expected at least one Cohere request")
	}
}

func namesOf(cands []searchCandidate) []string {
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.symbol.Name
	}
	return names
}

// API failure (500) falls back to the original BM25 order with exactly one
// stderr warning, never a hard error.
func TestApplyHybridSearch_APIFailure_FallsBackToBM25(t *testing.T) {
	resetEmbedCache(t)
	var voyageReqs int64
	voyage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&voyageReqs, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"internal error"}`))
	}))
	defer voyage.Close()

	withEnv(t, "VOYAGE_AI_API_KEY", "test-voyage-key")
	withEnv(t, "COHERE_API_KEY", "")
	withEndpoint(t, &voyageAPIURL, voyage.URL)

	in := mkCandidates(5)
	var out []searchCandidate
	stderr := captureStderr(t, func() {
		out = applyHybridSearch("anything", in)
	})

	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("expected BM25 fallback order unchanged, candidate %d: got %+v want %+v", i, out[i], in[i])
		}
	}
	warnLines := countNonEmptyLines(stderr)
	if warnLines != 1 {
		t.Fatalf("expected exactly one stderr warning line, got %d: %q", warnLines, stderr)
	}
}

// A slow/hanging Voyage server must not hang the caller: the client's own
// timeout bounds the wait, and the result still falls back to BM25.
func TestApplyHybridSearch_Timeout_FallsBackAndDoesNotHang(t *testing.T) {
	resetEmbedCache(t)
	voyage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer voyage.Close()

	withEnv(t, "VOYAGE_AI_API_KEY", "test-voyage-key")
	withEnv(t, "COHERE_API_KEY", "")
	withEndpoint(t, &voyageAPIURL, voyage.URL)

	prevTimeout := hybridHTTPTimeout
	hybridHTTPTimeout = 50 * time.Millisecond
	t.Cleanup(func() { hybridHTTPTimeout = prevTimeout })

	in := mkCandidates(3)
	start := time.Now()
	var out []searchCandidate
	_ = captureStderr(t, func() {
		out = applyHybridSearch("anything", in)
	})
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("applyHybridSearch took too long (%s) — looks like it hung", elapsed)
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("expected BM25 fallback on timeout, candidate %d: got %+v want %+v", i, out[i], in[i])
		}
	}
}

// A long-lived process must not re-embed unchanged candidate content: a
// second call with the same query+candidates should hit the in-memory
// content-hash cache and issue zero additional Voyage requests.
func TestApplyHybridSearch_CacheHitAvoidsReEmbedding(t *testing.T) {
	resetEmbedCache(t)
	var voyageReqs int64
	voyage := fakeVoyageServer(t, &voyageReqs, func(text, inputType string) []float32 {
		return unitVec(0, 4)
	})
	defer voyage.Close()

	withEnv(t, "VOYAGE_AI_API_KEY", "test-voyage-key")
	withEnv(t, "COHERE_API_KEY", "")
	withEndpoint(t, &voyageAPIURL, voyage.URL)

	in := mkCandidates(4)
	_ = applyHybridSearch("cache me", in)
	firstCount := atomic.LoadInt64(&voyageReqs)
	if firstCount == 0 {
		t.Fatal("expected first call to hit Voyage at least once")
	}

	_ = applyHybridSearch("cache me", in)
	secondCount := atomic.LoadInt64(&voyageReqs)

	if secondCount != firstCount {
		t.Fatalf("expected cache to prevent additional requests: first=%d second=%d", firstCount, secondCount)
	}
}

// Errors and warnings must never contain the API key material.
func TestApplyHybridSearch_NoKeyMaterialInError(t *testing.T) {
	resetEmbedCache(t)
	const secretKey = "sk-supersecret-voyage-key-DO-NOT-LEAK"
	voyage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Echo the Authorization header back into the error body to prove
		// our error path doesn't surface it even if a server misbehaves.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"detail":"saw auth header: ` + r.Header.Get("Authorization") + `"}`))
	}))
	defer voyage.Close()

	withEnv(t, "VOYAGE_AI_API_KEY", secretKey)
	withEnv(t, "COHERE_API_KEY", "")
	withEndpoint(t, &voyageAPIURL, voyage.URL)

	in := mkCandidates(3)
	stderr := captureStderr(t, func() {
		applyHybridSearch("anything", in)
	})

	if strings.Contains(stderr, secretKey) {
		t.Fatalf("stderr leaked API key material: %q", stderr)
	}
}

// This test would catch an accidental whole-corpus embedding: even with a
// large candidate set, the number of Voyage/Cohere requests stays bounded
// (independent of corpus size) because only the top-K BM25 candidates are
// ever embedded, batched into a small, fixed number of API calls.
func TestApplyHybridSearch_BoundedRequestCount_NotWholeCorpus(t *testing.T) {
	resetEmbedCache(t)
	var voyageReqs, cohereReqs int64
	voyage := fakeVoyageServer(t, &voyageReqs, func(text, inputType string) []float32 {
		return unitVec(0, 4)
	})
	defer voyage.Close()
	cohere := fakeCohereServer(t, &cohereReqs, func(text string) float64 { return 0.5 })
	defer cohere.Close()

	withEnv(t, "VOYAGE_AI_API_KEY", "test-voyage-key")
	withEnv(t, "COHERE_API_KEY", "test-cohere-key")
	withEndpoint(t, &voyageAPIURL, voyage.URL)
	withEndpoint(t, &cohereAPIURL, cohere.URL)

	in := mkCandidates(500) // far larger than any bounded top-K window
	out := applyHybridSearch("anything", in)

	if len(out) != len(in) {
		t.Fatalf("length changed: got %d want %d", len(out), len(in))
	}
	// K+1: one batched document-embedding call plus one query-embedding call.
	if voyageReqs > int64(hybridTopK+1) {
		t.Fatalf("voyage request count %d looks like whole-corpus embedding (candidates=%d, topK=%d)",
			voyageReqs, len(in), hybridTopK)
	}
	if voyageReqs > 3 {
		t.Fatalf("expected O(1) voyage requests regardless of corpus size, got %d", voyageReqs)
	}
	if cohereReqs > 1 {
		t.Fatalf("expected a single batched Cohere rerank call, got %d", cohereReqs)
	}
}

func countNonEmptyLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
