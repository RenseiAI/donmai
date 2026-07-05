package codeintel

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// benchRepo creates a temp repo with enough files that the walk+extract cost is
// observable in the cold-vs-warm benchmark.
func benchRepo(tb testing.TB) string {
	tb.Helper()
	dir := tb.TempDir()
	for i := 0; i < 60; i++ {
		src := fmt.Sprintf(`package pkg%d

import "fmt"

// Handler%d handles requests.
type Handler%d struct{ ID int }

// Serve%d serves a request.
func (h *Handler%d) Serve%d() string { return fmt.Sprintf("id=%%d", h.ID) }

// Helper%d is a free function.
func Helper%d(x int) int { return x * %d }
`, i, i, i, i, i, i, i, i, i+1)
		p := filepath.Join(dir, fmt.Sprintf("file%02d.go", i))
		if err := os.WriteFile(p, []byte(src), 0o640); err != nil { //nolint:gosec // G306 test fixture
			tb.Fatalf("write %s: %v", p, err)
		}
	}
	return dir
}

// TestWarmPath_SecondQuerySkipsWalk proves the in-process warm cache: a second
// index-consuming query on the same NativeRunner serves results WITHOUT a second
// full-tree walk (the cache is trusted until Refresh/Invalidate).
//
// RED (before the warm cache, when every query calls BuildIndex->discoverFiles):
//
//	native_warm_test.go: second query triggered 1 extra walk(s); want 0 (warm cache)
func TestWarmPath_SecondQuerySkipsWalk(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)

	if _, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Greet"}); err != nil {
		t.Fatalf("first query: %v", err)
	}
	walksAfterFirst := nr.walkCount.Load()
	if walksAfterFirst == 0 {
		t.Fatal("first query performed 0 walks; expected the cold build to walk once")
	}

	if _, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Greet"}); err != nil {
		t.Fatalf("second query: %v", err)
	}
	extraWalks := nr.walkCount.Load() - walksAfterFirst
	if extraWalks != 0 {
		t.Errorf("second query triggered %d extra walk(s); want 0 (warm cache)", extraWalks)
	}

	// Cross-tool warmth: a different query kind also reuses the cache.
	if _, err := nr.SearchCodeNative(SearchCodeOptions{Query: "Greet"}); err != nil {
		t.Fatalf("cross-tool query: %v", err)
	}
	if nr.walkCount.Load() != walksAfterFirst {
		t.Errorf("cross-tool query re-walked; walkCount %d, want %d", nr.walkCount.Load(), walksAfterFirst)
	}
}

// TestWarmPath_RefreshAndInvalidate verifies the explicit staleness contract:
// Refresh() eagerly rebuilds (re-walks); Invalidate() drops the cache so the
// next query rebuilds lazily.
func TestWarmPath_RefreshAndInvalidate(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)

	if _, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Greet"}); err != nil {
		t.Fatalf("first query: %v", err)
	}
	base := nr.walkCount.Load()

	if err := nr.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if nr.walkCount.Load() != base+1 {
		t.Errorf("Refresh should re-walk once; walkCount %d, want %d", nr.walkCount.Load(), base+1)
	}

	// After Refresh, still warm: query does not re-walk.
	if _, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Greet"}); err != nil {
		t.Fatalf("query after refresh: %v", err)
	}
	if nr.walkCount.Load() != base+1 {
		t.Errorf("query after Refresh re-walked; walkCount %d, want %d", nr.walkCount.Load(), base+1)
	}

	// Invalidate drops the cache; the next query rebuilds (re-walks).
	nr.Invalidate()
	if _, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Greet"}); err != nil {
		t.Fatalf("query after invalidate: %v", err)
	}
	if nr.walkCount.Load() != base+2 {
		t.Errorf("query after Invalidate should re-walk; walkCount %d, want %d", nr.walkCount.Load(), base+2)
	}
}

// TestWarmPath_ConcurrentQueriesSafe exercises the RWMutex under -race: many
// goroutines run queries and Refresh concurrently against one runner.
func TestWarmPath_ConcurrentQueriesSafe(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)
	if _, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Greet"}); err != nil {
		t.Fatalf("warm-up: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				_, _ = nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Greet"})
			case 1:
				_, _ = nr.SearchCodeNative(SearchCodeOptions{Query: "Greet"})
			case 2:
				_, _ = nr.GetRepoMapNative(GetRepoMapOptions{})
			case 3:
				_ = nr.Refresh()
			}
		}(i)
	}
	wg.Wait()
}

// BenchmarkColdQuery rebuilds the index from disk on every call (cache dropped
// each iteration) — the pre-warm-cache cost profile.
func BenchmarkColdQuery(b *testing.B) {
	dir := benchRepo(b)
	nr := NewNativeRunner(dir)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nr.Invalidate() // force a full rebuild (walk + selective extract) each call
		if _, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Handler"}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWarmQuery reuses the in-process warm cache — the Wave-2 MCP-server
// steady-state cost profile (no walk, no re-hash, no disk).
func BenchmarkWarmQuery(b *testing.B) {
	dir := benchRepo(b)
	nr := NewNativeRunner(dir)
	if _, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Handler"}); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Handler"}); err != nil {
			b.Fatal(err)
		}
	}
}
