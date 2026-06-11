package codeintel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestNativeRunner_GetRepoMap verifies that GetRepoMapNative discovers files,
// extracts symbols, and returns a non-empty JSON response for a temporary git
// repo containing both a .go and a .ts file.
func TestNativeRunner_GetRepoMap(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)
	out, err := nr.GetRepoMapNative(GetRepoMapOptions{})
	if err != nil {
		t.Fatalf("GetRepoMapNative: %v", err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map result, got %T", out)
	}
	entriesRaw, ok := m["entries"]
	if !ok {
		t.Fatal("result missing 'entries' key")
	}
	entries, ok := entriesRaw.([]RepoMapEntry)
	if !ok {
		t.Fatalf("entries type: got %T", entriesRaw)
	}
	if len(entries) == 0 {
		t.Error("expected at least one repo-map entry")
	}
	// At least one entry should contain a symbol.
	totalSymbols := 0
	for _, e := range entries {
		totalSymbols += len(e.Symbols)
	}
	if totalSymbols == 0 {
		t.Error("expected at least one symbol across all entries")
	}
}

// TestNativeRunner_SearchSymbols verifies that SearchSymbolsNative finds
// symbols matching a query from the native index.
func TestNativeRunner_SearchSymbols(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)

	// Searching for "Greet" should find GreetUser and Greeter.
	out, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Greet"})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	results, ok := out.([]map[string]any)
	if !ok {
		t.Fatalf("expected []map result, got %T", out)
	}
	if len(results) == 0 {
		t.Error("expected at least one search result for 'Greet'")
	}
	// Verify result shape: each item must have 'symbol', 'score', 'matchType'.
	for i, r := range results {
		if _, ok := r["symbol"]; !ok {
			t.Errorf("result[%d] missing 'symbol'", i)
		}
		if _, ok := r["score"]; !ok {
			t.Errorf("result[%d] missing 'score'", i)
		}
		if _, ok := r["matchType"]; !ok {
			t.Errorf("result[%d] missing 'matchType'", i)
		}
	}
}

// TestNativeRunner_SearchSymbols_EmptyQuery verifies that an empty query
// returns an error.
func TestNativeRunner_SearchSymbols_EmptyQuery(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)
	_, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: ""})
	if err == nil {
		t.Error("expected error for empty query")
	}
}

// TestNativeRunner_IndexRoundTrip verifies that BuildIndex persists index.json
// and a subsequent load finds the same files.
func TestNativeRunner_IndexRoundTrip(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)

	idx, err := nr.BuildIndex(GetRepoMapOptions{})
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	if len(idx.Files) == 0 {
		t.Error("expected non-empty file index")
	}

	// Verify index.json was written.
	indexPath := filepath.Join(dir, ".donmai/code-index/index.json")
	data, err := os.ReadFile(indexPath) //nolint:gosec
	if err != nil {
		t.Fatalf("index.json not found: %v", err)
	}
	var loaded IndexFile
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("parse index.json: %v", err)
	}
	if len(loaded.Files) != len(idx.Files) {
		t.Errorf("files count: persisted %d, in-memory %d", len(loaded.Files), len(idx.Files))
	}
}

// TestNativeRunner_IncrementalIndex verifies that BuildIndex re-uses existing
// entries for files that have not changed (incremental indexing).
func TestNativeRunner_IncrementalIndex(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)

	// First build.
	idx1, err := nr.BuildIndex(GetRepoMapOptions{})
	if err != nil {
		t.Fatalf("first BuildIndex: %v", err)
	}
	// Record lastIndexed times.
	stamps := make(map[string]int64, len(idx1.Files))
	for path, fi := range idx1.Files {
		stamps[path] = fi.LastIndexed
	}

	// Second build — no file changes; all entries should be reused (same timestamp).
	// Use a NEW NativeRunner (fresh in-memory state) to simulate daemon restart.
	nr2 := NewNativeRunner(dir)
	idx2, err := nr2.BuildIndex(GetRepoMapOptions{})
	if err != nil {
		t.Fatalf("second BuildIndex: %v", err)
	}
	for path, fi := range idx2.Files {
		if oldTs, ok := stamps[path]; ok {
			if fi.LastIndexed != oldTs {
				t.Errorf("file %q was re-indexed on unchanged content (ts %d → %d)", path, oldTs, fi.LastIndexed)
			}
		}
	}
}

// ── helpers ────────────────────────────────────────────────────────────────────

// setupTestRepo creates a temporary directory with a small Go and TypeScript
// file to exercise both extractors.
func setupTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	goSrc := `package greet

// Greeter greets people.
type Greeter struct {
	Name string
}

// GreetUser returns a greeting.
func GreetUser(name string) string {
	return "Hello, " + name
}

// Greet returns a greeting from the Greeter.
func (g *Greeter) Greet() string {
	return "Hello, " + g.Name
}
`
	tsSrc := `/** GreetingService provides greetings. */
export class GreetingService {
  greet(name: string): string {
    return ` + "`" + `Hello, ${name}` + "`" + `
  }
}

export function greetUser(name: string): string {
  return ` + "`" + `Hi, ${name}` + "`" + `
}
`

	writeFile(t, filepath.Join(dir, "greet.go"), goSrc)
	writeFile(t, filepath.Join(dir, "greet.ts"), tsSrc)
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil { //nolint:gosec // G306 test file
		t.Fatalf("write %s: %v", path, err)
	}
}
