package codeintel

import (
	"path/filepath"
	"testing"
)

// TestGetRepoMap_DoubleStarMatchesSubtree verifies the documented
// `--file-patterns "svc/**"` example matches files in NESTED subdirectories,
// not just direct children of svc/.
//
// RED (filepath.Match-based matcher): `svc/**` matched only svc/x.go, never
// svc/a/a.go, so the map came back with zero entries for a documented pattern.
func TestGetRepoMap_DoubleStarMatchesSubtree(t *testing.T) {
	dir := t.TempDir()
	writeFileMkdir(t, filepath.Join(dir, "svc", "a", "a.go"), "package a\nfunc A() {}\n")
	writeFileMkdir(t, filepath.Join(dir, "svc", "b", "b.go"), "package b\nfunc B() {}\n")
	writeFileMkdir(t, filepath.Join(dir, "svc", "top.go"), "package svc\nfunc Top() {}\n")
	writeFileMkdir(t, filepath.Join(dir, "other", "o.go"), "package other\nfunc O() {}\n")

	nr := NewNativeRunner(dir)
	out, err := nr.GetRepoMapNative(GetRepoMapOptions{FilePatterns: []string{"svc/**"}, MaxFiles: 50})
	if err != nil {
		t.Fatalf("GetRepoMapNative: %v", err)
	}
	entries := repoMapEntries(t, out)
	got := map[string]bool{}
	for _, e := range entries {
		got[e.FilePath] = true
	}
	for _, want := range []string{"svc/a/a.go", "svc/b/b.go", "svc/top.go"} {
		if !got[want] {
			t.Errorf("svc/** should match %s; entries=%v", want, keysOfBoolMap(got))
		}
	}
	if got["other/o.go"] {
		t.Errorf("svc/** must NOT match other/o.go; entries=%v", keysOfBoolMap(got))
	}
}

// TestGetRepoMap_EmptyMatchSerializesAsArray verifies that when no file matches,
// the "entries" value is a non-nil slice (serialises to JSON []), not nil (null),
// so array-typed consumers do not break.
func TestGetRepoMap_EmptyMatchSerializesAsArray(t *testing.T) {
	dir := t.TempDir()
	writeFileMkdir(t, filepath.Join(dir, "a.go"), "package a\nfunc A() {}\n")

	nr := NewNativeRunner(dir)
	out, err := nr.GetRepoMapNative(GetRepoMapOptions{FilePatterns: []string{"nomatch/**"}, MaxFiles: 50})
	if err != nil {
		t.Fatalf("GetRepoMapNative: %v", err)
	}
	m := out.(map[string]any)
	entries, ok := m["entries"].([]RepoMapEntry)
	if !ok {
		t.Fatalf("entries type %T; want []RepoMapEntry", m["entries"])
	}
	if entries == nil {
		t.Errorf("entries must be a non-nil slice (JSON []), got nil (JSON null)")
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

// TestSearchSymbols_DoubleStarMatchesSubtree verifies search-symbols
// --file-pattern "svc/**" reaches symbols in nested subdirectories.
func TestSearchSymbols_DoubleStarMatchesSubtree(t *testing.T) {
	dir := t.TempDir()
	writeFileMkdir(t, filepath.Join(dir, "svc", "deep", "handler.go"),
		"package deep\nfunc HandleRequest() {}\n")
	writeFileMkdir(t, filepath.Join(dir, "other", "x.go"),
		"package other\nfunc HandleRequest() {}\n")

	nr := NewNativeRunner(dir)
	out, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "HandleRequest", FilePattern: "svc/**"})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	results := out.([]map[string]any)
	if len(results) != 1 {
		t.Fatalf("svc/** should match exactly the nested svc symbol; got %d results", len(results))
	}
	sym := results[0]["symbol"].(map[string]any)
	if sym["filePath"] != "svc/deep/handler.go" {
		t.Errorf("got %s; want svc/deep/handler.go", sym["filePath"])
	}
}

func keysOfBoolMap(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
