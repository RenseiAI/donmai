package codeintel

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestGetRepoMap_HubOutranksLeaf proves get-repo-map ranks by real PageRank over
// the import graph, not by symbol count. hub.ts has ONE symbol but is imported
// by five files; leaf.ts has TEN symbols but is imported by nobody. Under the
// old exported*2+symbolCount heuristic the leaf wins; under PageRank the hub —
// the structurally important file — must win.
//
// RED (against the old computeFileRank heuristic):
//
//	native_pagerank_test.go: hub rank 3.00 did NOT outrank leaf rank 30.00 (heuristic favours symbol count)
func TestGetRepoMap_HubOutranksLeaf(t *testing.T) {
	dir := t.TempDir()

	// Hub: a single exported symbol, imported by everyone.
	writeFile(t, filepath.Join(dir, "hub.ts"), `export function hubFn(): void {}
`)

	// Leaf: many exported symbols, imports nothing, imported by nobody.
	var leaf string
	for i := 0; i < 10; i++ {
		leaf += fmt.Sprintf("export function leafFn%d(): number { return %d }\n", i, i)
	}
	writeFile(t, filepath.Join(dir, "leaf.ts"), leaf)

	// Five importers that each depend on the hub.
	for i := 0; i < 5; i++ {
		src := fmt.Sprintf("import { hubFn } from './hub'\nexport function importer%d(): void { hubFn() }\n", i)
		writeFile(t, filepath.Join(dir, fmt.Sprintf("importer%d.ts", i)), src)
	}

	nr := NewNativeRunner(dir)
	out, err := nr.GetRepoMapNative(GetRepoMapOptions{})
	if err != nil {
		t.Fatalf("GetRepoMapNative: %v", err)
	}
	entries := repoMapEntries(t, out)

	var hubRank, leafRank float64
	foundHub, foundLeaf := false, false
	for _, e := range entries {
		switch e.FilePath {
		case "hub.ts":
			hubRank, foundHub = e.Rank, true
		case "leaf.ts":
			leafRank, foundLeaf = e.Rank, true
		}
	}
	if !foundHub || !foundLeaf {
		t.Fatalf("missing hub or leaf in entries (hub=%v leaf=%v): %+v", foundHub, foundLeaf, entries)
	}
	if hubRank <= leafRank {
		t.Errorf("hub rank %.4f did NOT outrank leaf rank %.4f (PageRank must favour the imported hub)", hubRank, leafRank)
	}
}

// TestGetRepoMap_FilePatternsFilterOutput verifies FilePatterns acts as an
// OUTPUT filter (which files appear in the map) while ranking still uses the
// whole-repo import graph.
func TestGetRepoMap_FilePatternsFilterOutput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hub.ts"), "export function hubFn(): void {}\n")
	for i := 0; i < 3; i++ {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("importer%d.ts", i)),
			fmt.Sprintf("import { hubFn } from './hub'\nexport function importer%d(): void {}\n", i))
	}

	nr := NewNativeRunner(dir)
	out, err := nr.GetRepoMapNative(GetRepoMapOptions{FilePatterns: []string{"hub.ts"}})
	if err != nil {
		t.Fatalf("GetRepoMapNative: %v", err)
	}
	entries := repoMapEntries(t, out)
	if len(entries) != 1 || entries[0].FilePath != "hub.ts" {
		t.Fatalf("FilePatterns should limit output to hub.ts; got %+v", entries)
	}
	// The hub earned rank from importers that were filtered OUT of the output —
	// proving the graph covered the whole repo, not just the pattern match.
	if entries[0].Rank <= 0.15/4.0 {
		t.Errorf("hub rank %.5f suggests the import graph was scoped to the pattern; want whole-repo graph", entries[0].Rank)
	}
}

func repoMapEntries(t *testing.T, out any) []RepoMapEntry {
	t.Helper()
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result type %T; want map", out)
	}
	entries, ok := m["entries"].([]RepoMapEntry)
	if !ok {
		t.Fatalf("entries type %T; want []RepoMapEntry", m["entries"])
	}
	return entries
}
