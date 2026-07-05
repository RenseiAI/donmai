package codeintel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildIndex_SkipsSymlinkEscapingRoot proves the indexer does not follow a
// symlink whose target lives OUTSIDE the index root. A repo (e.g. an external
// PR) could plant a symlink with an indexed extension pointing at an arbitrary
// host file; following it would read, persist, and (with hybrid search) ship
// that out-of-repo content off the box.
//
// RED (before the fix): WalkDir treats a symlink-to-file as a regular file, so
// os.ReadFile followed it and the secret leaked into the index / repo map.
func TestBuildIndex_SkipsSymlinkEscapingRoot(t *testing.T) {
	// An "outside" directory that is NOT under the index root.
	outside := t.TempDir()
	secret := "SUPER_SECRET_TOKEN_do_not_leak"
	writeFile(t, filepath.Join(outside, "private.go"),
		"package private\n\n// "+secret+"\nfunc Leaked() {}\n")

	root := t.TempDir()
	// A legitimate in-repo file.
	writeFile(t, filepath.Join(root, "ok.go"), "package ok\nfunc OK() {}\n")
	// The malicious symlink: an indexed extension, target outside the root.
	link := filepath.Join(root, "leak.go")
	if err := os.Symlink(filepath.Join(outside, "private.go"), link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}

	nr := NewNativeRunner(root)
	idx, err := nr.BuildIndex(GetRepoMapOptions{})
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	if _, ok := idx.Files["leak.go"]; ok {
		t.Errorf("symlink leak.go (target outside root) must NOT be indexed")
	}
	if _, ok := idx.Files["ok.go"]; !ok {
		t.Errorf("legitimate ok.go should still be indexed; files=%v", keysOfIndex(idx))
	}

	// Belt-and-braces: the secret must appear nowhere in the persisted index.
	data, rerr := os.ReadFile(filepath.Join(root, ".donmai", "code-index", "index.json"))
	if rerr == nil && strings.Contains(string(data), secret) {
		t.Errorf("out-of-repo secret leaked into persisted index.json")
	}

	// And it must not surface through any query.
	out, err := nr.SearchSymbolsNative(SearchSymbolsOptions{Query: "Leaked"})
	if err != nil {
		t.Fatalf("SearchSymbolsNative: %v", err)
	}
	if results := out.([]map[string]any); len(results) != 0 {
		t.Errorf("out-of-repo symbol 'Leaked' leaked through search-symbols: %v", results)
	}
}

func keysOfIndex(idx IndexFile) []string {
	out := make([]string, 0, len(idx.Files))
	for k := range idx.Files {
		out = append(out, k)
	}
	return out
}
