package codeintel

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeFileMkdir writes content, creating any missing parent directories first.
func writeFileMkdir(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	writeFile(t, path, content)
}

// TestGetRepoMap_GoHubOutranksLeaf proves get-repo-map ranks a Go repo by real
// PageRank over the intra-repo import graph, not alphabetically. hub/hub.go is
// imported (by module path) by three services; leaf/leaf.go is imported by
// nobody. PageRank must rank the hub strictly above the leaf.
//
// RED (before Go/Py/Rust import resolution existed): every Go file tied at the
// dangling-node baseline 0.15/N, so hubRank == leafRank and the map was
// effectively alphabetical — the "PageRank importance ranking" claim was a
// no-op for Go repos.
func TestGetRepoMap_GoHubOutranksLeaf(t *testing.T) {
	dir := t.TempDir()

	writeFileMkdir(t, filepath.Join(dir, "go.mod"), "module example.com/gofix\n\ngo 1.25\n")

	writeFileMkdir(t, filepath.Join(dir, "hub", "hub.go"), `package hub

// Hub is the shared dependency.
func Hub() string { return "hub" }
`)

	// A leaf with MANY exported symbols but imported by nobody — under any
	// symbol-count heuristic it would win; under PageRank it must lose to the hub.
	var leaf string
	leaf += "package leaf\n\n"
	for i := 0; i < 10; i++ {
		leaf += fmt.Sprintf("func LeafFn%d() int { return %d }\n", i, i)
	}
	writeFileMkdir(t, filepath.Join(dir, "leaf", "leaf.go"), leaf)

	for i := 0; i < 3; i++ {
		src := fmt.Sprintf(`package svc%d

import "example.com/gofix/hub"

func Svc%d() string { return hub.Hub() }
`, i, i)
		writeFileMkdir(t, filepath.Join(dir, fmt.Sprintf("svc%d", i), fmt.Sprintf("svc%d.go", i)), src)
	}

	nr := NewNativeRunner(dir)
	out, err := nr.GetRepoMapNative(GetRepoMapOptions{MaxFiles: 100})
	if err != nil {
		t.Fatalf("GetRepoMapNative: %v", err)
	}
	entries := repoMapEntries(t, out)

	var hubRank, leafRank float64
	var foundHub, foundLeaf bool
	for _, e := range entries {
		switch e.FilePath {
		case "hub/hub.go":
			hubRank, foundHub = e.Rank, true
		case "leaf/leaf.go":
			leafRank, foundLeaf = e.Rank, true
		}
	}
	if !foundHub || !foundLeaf {
		t.Fatalf("missing hub or leaf (hub=%v leaf=%v): %+v", foundHub, foundLeaf, entries)
	}
	if hubRank <= leafRank {
		t.Errorf("Go hub rank %.6f did NOT outrank leaf rank %.6f — PageRank is a no-op on Go imports", hubRank, leafRank)
	}
}

// TestGetRepoMap_PythonHubOutranksLeaf is the Python analogue: hub.py is
// imported by three modules; leaf.py by nobody. RED before Python dotted-import
// resolution existed.
func TestGetRepoMap_PythonHubOutranksLeaf(t *testing.T) {
	dir := t.TempDir()

	writeFileMkdir(t, filepath.Join(dir, "pkg", "hub.py"), "def hub():\n    return 'hub'\n")

	var leaf string
	for i := 0; i < 10; i++ {
		leaf += fmt.Sprintf("def leaf_fn_%d():\n    return %d\n", i, i)
	}
	writeFileMkdir(t, filepath.Join(dir, "pkg", "leaf.py"), leaf)

	for i := 0; i < 3; i++ {
		src := fmt.Sprintf("from pkg.hub import hub\n\ndef svc_%d():\n    return hub()\n", i)
		writeFileMkdir(t, filepath.Join(dir, "pkg", fmt.Sprintf("svc_%d.py", i)), src)
	}

	nr := NewNativeRunner(dir)
	out, err := nr.GetRepoMapNative(GetRepoMapOptions{MaxFiles: 100})
	if err != nil {
		t.Fatalf("GetRepoMapNative: %v", err)
	}
	entries := repoMapEntries(t, out)

	var hubRank, leafRank float64
	var foundHub, foundLeaf bool
	for _, e := range entries {
		switch e.FilePath {
		case "pkg/hub.py":
			hubRank, foundHub = e.Rank, true
		case "pkg/leaf.py":
			leafRank, foundLeaf = e.Rank, true
		}
	}
	if !foundHub || !foundLeaf {
		t.Fatalf("missing hub or leaf (hub=%v leaf=%v): %+v", foundHub, foundLeaf, entries)
	}
	if hubRank <= leafRank {
		t.Errorf("Python hub rank %.6f did NOT outrank leaf rank %.6f — PageRank is a no-op on Python imports", hubRank, leafRank)
	}
}
