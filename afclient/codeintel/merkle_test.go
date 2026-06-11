package codeintel

import (
	"testing"
)

// TestMerkleTree_EmptyTree returns stable empty root hash.
func TestMerkleTree_EmptyTree(_ *testing.T) {
	tree := NewMerkleTree()
	tree.computeHashes()
	// An empty tree should produce a deterministic (possibly empty) hash.
	h := tree.RootHash()
	_ = h // Just verify it doesn't panic.
}

// TestMerkleTree_SingleFile produces a deterministic root hash.
func TestMerkleTree_SingleFile(t *testing.T) {
	tree := MerkleTreeFromHashes(map[string]string{
		"src/main.go": "abc123",
	})
	h1 := tree.RootHash()
	// Same tree rebuilt from same input should have same hash.
	tree2 := MerkleTreeFromHashes(map[string]string{
		"src/main.go": "abc123",
	})
	if h1 != tree2.RootHash() {
		t.Errorf("same content should produce same root hash: %q != %q", h1, tree2.RootHash())
	}
}

// TestMerkleTree_FilesMethod returns all file hashes.
func TestMerkleTree_FilesMethod(t *testing.T) {
	input := map[string]string{
		"a.go":   "hash_a",
		"b/c.go": "hash_bc",
		"b/d.ts": "hash_bd",
	}
	tree := MerkleTreeFromHashes(input)
	files := tree.Files()
	for path, wantHash := range input {
		gotHash, ok := files[path]
		if !ok {
			t.Errorf("path %q not in tree.Files()", path)
			continue
		}
		if gotHash != wantHash {
			t.Errorf("path %q: hash %q, want %q", path, gotHash, wantHash)
		}
	}
}

// TestMerkleTree_RootHashChangesOnModification verifies that changing a file hash
// changes the root hash.
func TestMerkleTree_RootHashChangesOnModification(t *testing.T) {
	t1 := MerkleTreeFromHashes(map[string]string{
		"src/main.go": "original_hash",
	})
	t2 := MerkleTreeFromHashes(map[string]string{
		"src/main.go": "modified_hash",
	})
	if t1.RootHash() == t2.RootHash() {
		t.Error("different file hashes should produce different root hashes")
	}
}

// TestMerkleDiff_DetectsChange tests the incremental diff between two tree states.
func TestMerkleDiff_DetectsChange(t *testing.T) {
	oldTree := MerkleTreeFromHashes(map[string]string{
		"src/main.go":   "hash_main",
		"src/helper.go": "hash_helper",
	})
	newTree := MerkleTreeFromHashes(map[string]string{
		"src/main.go":   "hash_main_modified",
		"src/helper.go": "hash_helper",
		"src/new.go":    "hash_new",
	})

	cs := MerkleDiff(oldTree, newTree)

	if len(cs.Modified) != 1 || cs.Modified[0] != "src/main.go" {
		t.Errorf("expected 1 modified file (src/main.go), got %v", cs.Modified)
	}
	if len(cs.Added) != 1 || cs.Added[0] != "src/new.go" {
		t.Errorf("expected 1 added file (src/new.go), got %v", cs.Added)
	}
	if len(cs.Deleted) != 0 {
		t.Errorf("expected no deleted files, got %v", cs.Deleted)
	}
}

// TestMerkleDiff_DetectsDeletion detects deleted files.
func TestMerkleDiff_DetectsDeletion(t *testing.T) {
	oldTree := MerkleTreeFromHashes(map[string]string{
		"a.go": "hash_a",
		"b.go": "hash_b",
	})
	newTree := MerkleTreeFromHashes(map[string]string{
		"a.go": "hash_a",
	})

	cs := MerkleDiff(oldTree, newTree)
	if len(cs.Deleted) != 1 || cs.Deleted[0] != "b.go" {
		t.Errorf("expected b.go deleted, got %v", cs.Deleted)
	}
	if len(cs.Modified) != 0 {
		t.Errorf("expected no modified, got %v", cs.Modified)
	}
	if len(cs.Added) != 0 {
		t.Errorf("expected no added, got %v", cs.Added)
	}
}

// TestMerkleIdentical returns true for equal trees.
func TestMerkleIdentical(t *testing.T) {
	hashes := map[string]string{
		"x.go": "h1",
		"y.go": "h2",
	}
	t1 := MerkleTreeFromHashes(hashes)
	t2 := MerkleTreeFromHashes(hashes)
	if !MerkleIdentical(t1, t2) {
		t.Error("expected identical trees to be equal")
	}
}

// TestMerkleIdentical_NotEqual returns false for different trees.
func TestMerkleIdentical_NotEqual(t *testing.T) {
	t1 := MerkleTreeFromHashes(map[string]string{"x.go": "h1"})
	t2 := MerkleTreeFromHashes(map[string]string{"x.go": "h2"})
	if MerkleIdentical(t1, t2) {
		t.Error("expected non-identical trees to be different")
	}
}

// TestMerkleFromIndex builds a tree from an IndexFile.
func TestMerkleFromIndex(t *testing.T) {
	idx := IndexFile{
		Files: map[string]FileIndex{
			"pkg/main.go": {FilePath: "pkg/main.go", GitHash: "abc"},
			"pkg/util.go": {FilePath: "pkg/util.go", GitHash: "def"},
		},
	}
	tree := MerkleTreeFromIndex(idx)
	files := tree.Files()
	if files["pkg/main.go"] != "abc" {
		t.Errorf("pkg/main.go hash: got %q, want abc", files["pkg/main.go"])
	}
	if files["pkg/util.go"] != "def" {
		t.Errorf("pkg/util.go hash: got %q, want def", files["pkg/util.go"])
	}
}

// TestRecomputeRootHash verifies that building from a known file set produces a
// stable hash (determinism test).
func TestRecomputeRootHash(t *testing.T) {
	files := map[string]FileIndex{
		"a.go": {FilePath: "a.go", GitHash: "hash1"},
		"b.go": {FilePath: "b.go", GitHash: "hash2"},
	}
	h1 := RecomputeRootHash(files)
	h2 := RecomputeRootHash(files)
	if h1 != h2 {
		t.Errorf("RecomputeRootHash is not deterministic: %q != %q", h1, h2)
	}
	if h1 == "" {
		t.Error("expected non-empty root hash")
	}
}
