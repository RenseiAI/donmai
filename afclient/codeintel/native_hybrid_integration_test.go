package codeintel

import (
	"sync/atomic"
	"testing"
)

// TestSearchCodeNative_RoutesThroughHybridWhenKeySet proves the integrator's
// wiring: SearchCodeNative must call applyHybridSearch, so with VOYAGE_AI_API_KEY
// set and the Voyage endpoint redirected to a fake, at least one embedding
// request is issued end-to-end through the native search path.
//
// RED (before the applyHybridSearch call was wired into SearchCodeNative):
//
//	native_hybrid_integration_test.go: hybrid path not reached: voyage requests = 0; want >= 1
func TestSearchCodeNative_RoutesThroughHybridWhenKeySet(t *testing.T) {
	resetEmbedCache(t)

	var voyageReqs int64
	voyage := fakeVoyageServer(t, &voyageReqs, func(_, _ string) []float32 {
		return unitVec(0, 256)
	})
	t.Cleanup(voyage.Close)

	withEnv(t, "VOYAGE_AI_API_KEY", "test-voyage-key")
	withEnv(t, "COHERE_API_KEY", "")
	withEndpoint(t, &voyageAPIURL, voyage.URL)

	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)

	if _, err := nr.SearchCodeNative(SearchCodeOptions{Query: "Greet"}); err != nil {
		t.Fatalf("SearchCodeNative: %v", err)
	}

	if got := atomic.LoadInt64(&voyageReqs); got < 1 {
		t.Fatalf("hybrid path not reached: voyage requests = %d; want >= 1", got)
	}
}

// TestSearchCodeNative_KeyAbsentUnaffected proves the passthrough guarantee at
// the native boundary: with no VOYAGE_AI_API_KEY, SearchCodeNative behaves
// exactly as the pure-BM25 path (non-empty results, zero network).
func TestSearchCodeNative_KeyAbsentUnaffected(t *testing.T) {
	resetEmbedCache(t)
	withEnv(t, "VOYAGE_AI_API_KEY", "")
	withEnv(t, "COHERE_API_KEY", "")
	withEndpoint(t, &voyageAPIURL, "http://127.0.0.1:1/unreachable")

	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)

	out, err := nr.SearchCodeNative(SearchCodeOptions{Query: "Greet"})
	if err != nil {
		t.Fatalf("SearchCodeNative: %v", err)
	}
	results, ok := out.([]map[string]any)
	if !ok {
		t.Fatalf("unexpected result type %T", out)
	}
	if len(results) == 0 {
		t.Fatal("expected non-empty BM25 results for a matching query")
	}
}
