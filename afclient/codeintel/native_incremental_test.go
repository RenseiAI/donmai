package codeintel

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuildIndex_SecondBuildSkipsExtraction proves real incremental indexing:
// a second BuildIndex over an unchanged tree must NOT re-invoke the language
// extractors (the expensive parse step). The extractCount seam observes it.
//
// RED (against the pre-fix BuildIndex, which extracts every file on every call):
//
//	native_incremental_test.go: second build extracted 2 files; want 0 (unchanged tree)
func TestBuildIndex_SecondBuildSkipsExtraction(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)

	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("first BuildIndex: %v", err)
	}
	firstExtractions := nr.extractCount.Load()
	if firstExtractions == 0 {
		t.Fatal("first build extracted 0 files; expected the cold build to extract the whole tree")
	}

	// Second build over an unchanged tree.
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("second BuildIndex: %v", err)
	}
	secondExtractions := nr.extractCount.Load() - firstExtractions
	if secondExtractions != 0 {
		t.Errorf("second build extracted %d files; want 0 (unchanged tree)", secondExtractions)
	}
}

// TestBuildIndex_OnlyChangedFileReExtracted verifies that editing exactly one
// file re-extracts exactly that file (added/modified via MerkleDiff), not the
// whole tree, and that a newly-added file is picked up.
func TestBuildIndex_OnlyChangedFileReExtracted(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)

	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("first BuildIndex: %v", err)
	}
	base := nr.extractCount.Load()

	// Modify one existing file.
	writeFile(t, filepath.Join(dir, "greet.go"), `package greet

// GreetLoudly shouts a greeting.
func GreetLoudly(name string) string { return "HELLO " + name }
`)
	// Add a brand-new file.
	writeFile(t, filepath.Join(dir, "extra.go"), `package greet

// Extra is a new symbol.
func Extra() {}
`)

	idx, err := nr.BuildIndex(GetRepoMapOptions{})
	if err != nil {
		t.Fatalf("second BuildIndex: %v", err)
	}
	delta := nr.extractCount.Load() - base
	if delta != 2 {
		t.Errorf("re-extracted %d files after 1 edit + 1 add; want 2", delta)
	}
	if _, ok := idx.Files["extra.go"]; !ok {
		t.Error("newly-added extra.go missing from index")
	}
	// The modified file's new content must be reflected.
	found := false
	for _, s := range idx.Files["greet.go"].Symbols {
		if s.Name == "GreetLoudly" {
			found = true
		}
	}
	if !found {
		t.Error("modified greet.go did not pick up GreetLoudly symbol")
	}
}

// TestBuildIndex_DeletedFileDropped verifies a file removed from disk is
// dropped from the persisted index.
func TestBuildIndex_DeletedFileDropped(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)
	if _, err := nr.BuildIndex(GetRepoMapOptions{}); err != nil {
		t.Fatalf("first BuildIndex: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "greet.ts")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	idx, err := nr.BuildIndex(GetRepoMapOptions{})
	if err != nil {
		t.Fatalf("second BuildIndex: %v", err)
	}
	if _, ok := idx.Files["greet.ts"]; ok {
		t.Error("deleted greet.ts still present in index")
	}
	if _, ok := idx.Files["greet.go"]; !ok {
		t.Error("greet.go should remain in index")
	}
}

// TestBuildIndex_PopulatesV2Fields verifies the incremental build fills the
// schema-v2 content/import fields for freshly-extracted files.
func TestBuildIndex_PopulatesV2Fields(t *testing.T) {
	dir := setupTestRepo(t)
	nr := NewNativeRunner(dir)
	idx, err := nr.BuildIndex(GetRepoMapOptions{})
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	fi, ok := idx.Files["greet.go"]
	if !ok {
		t.Fatal("greet.go missing")
	}
	if fi.ContentHash == "" {
		t.Error("ContentHash not populated on fresh extraction")
	}
	if fi.SimHash == 0 {
		t.Error("SimHash not populated on fresh extraction")
	}
}
