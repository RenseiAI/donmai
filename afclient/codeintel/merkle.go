package codeintel

// merkle.go — Merkle-tree incremental index diffing (S3).
//
// Design matches the TS MerkleTree + ChangeDetector from
// donmai-libraries/packages/code-intelligence/src/indexing/merkle-tree.ts and
// donmai-libraries/packages/code-intelligence/src/indexing/change-detector.ts.
//
// The tree stores file-level and directory-level nodes. Each directory hash is
// computed as SHA1("tree <len>\0<sorted child hashes joined by \n>"), matching
// the TS GitHashProvider.hashDirectory method.
//
// This allows efficient diff: after re-indexing a subset of files, only the
// changed paths (and their ancestor directory nodes) need to be recomputed.
// A root-hash comparison quickly determines whether anything has changed.

import (
	"crypto/sha1" //nolint:gosec // sha1 is required for git compatibility
	"fmt"
	"sort"
	"strings"
)

// ── Tree node ─────────────────────────────────────────────────────────────────

// merkleNode is a node in the Merkle tree. Leaf nodes represent files; internal
// nodes represent directories.
type merkleNode struct {
	path        string
	hash        string
	isDirectory bool
	children    map[string]*merkleNode
}

func newDirNode(path string) *merkleNode {
	return &merkleNode{path: path, isDirectory: true, children: make(map[string]*merkleNode)}
}

// ── MerkleTree ────────────────────────────────────────────────────────────────

// MerkleTree builds and diffs a content-addressed tree over a file set.
type MerkleTree struct {
	root *merkleNode
}

// NewMerkleTree returns an empty MerkleTree.
func NewMerkleTree() *MerkleTree {
	return &MerkleTree{root: newDirNode("")}
}

// MerkleTreeFromHashes builds a MerkleTree from a path → gitHash map.
func MerkleTreeFromHashes(fileHashes map[string]string) *MerkleTree {
	t := NewMerkleTree()
	for path, hash := range fileHashes {
		t.addFileWithHash(path, hash)
	}
	t.computeHashes()
	return t
}

// MerkleTreeFromIndex builds a MerkleTree from an IndexFile's gitHash entries.
func MerkleTreeFromIndex(idx IndexFile) *MerkleTree {
	hashes := make(map[string]string, len(idx.Files))
	for path, fi := range idx.Files {
		hashes[path] = fi.GitHash
	}
	return MerkleTreeFromHashes(hashes)
}

// addFileWithHash inserts a file node at path with the given hash.
func (t *MerkleTree) addFileWithHash(path, hash string) {
	parts := splitPath(path)
	cur := t.root
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		if cur.children[part] == nil {
			dirPath := strings.Join(parts[:i+1], "/")
			cur.children[part] = newDirNode(dirPath)
		}
		cur = cur.children[part]
	}
	fileName := parts[len(parts)-1]
	cur.children[fileName] = &merkleNode{
		path:        path,
		hash:        hash,
		isDirectory: false,
		children:    nil,
	}
}

// computeHashes recomputes all directory hashes bottom-up.
func (t *MerkleTree) computeHashes() {
	computeNodeHash(t.root)
}

// computeNodeHash recursively computes the hash for a node and its subtree.
func computeNodeHash(node *merkleNode) string {
	if !node.isDirectory {
		return node.hash
	}
	childHashes := make([]string, 0, len(node.children))
	// Sort children by name for determinism.
	names := make([]string, 0, len(node.children))
	for name := range node.children {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child := node.children[name]
		h := computeNodeHash(child)
		childHashes = append(childHashes, name+":"+h)
	}
	node.hash = hashDirectory(childHashes)
	return node.hash
}

// hashDirectory computes the git tree-like hash for a sorted list of child
// entries. Matches the TS GitHashProvider.hashDirectory method:
//
//	sha1("tree <len>\0<sorted-child-hashes-joined-by-\n>")
func hashDirectory(childHashes []string) string {
	sorted := make([]string, len(childHashes))
	copy(sorted, childHashes)
	sort.Strings(sorted)
	combined := strings.Join(sorted, "\n")
	header := fmt.Sprintf("tree %d\x00", len(combined))
	h := sha1.New() //nolint:gosec // sha1 required for git compat
	_, _ = h.Write([]byte(header))
	_, _ = h.Write([]byte(combined))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// RootHash returns the root hash of the tree.
func (t *MerkleTree) RootHash() string {
	return t.root.hash
}

// Files returns a map of file path → file hash for all leaf nodes.
func (t *MerkleTree) Files() map[string]string {
	out := make(map[string]string)
	collectFiles(t.root, out)
	return out
}

func collectFiles(node *merkleNode, out map[string]string) {
	if !node.isDirectory {
		out[node.path] = node.hash
		return
	}
	for _, child := range node.children {
		collectFiles(child, out)
	}
}

// ── ChangeSet + diff ──────────────────────────────────────────────────────────

// ChangeSet holds the files that differ between two Merkle trees.
type ChangeSet struct {
	Added    []string
	Modified []string
	Deleted  []string
}

// MerkleDiff compares old and new trees, returning the file-level ChangeSet.
// Matches the TS ChangeDetector.detect method.
func MerkleDiff(oldTree, newTree *MerkleTree) ChangeSet {
	oldFiles := oldTree.Files()
	newFiles := newTree.Files()

	var added, modified, deleted []string

	for path, newHash := range newFiles {
		oldHash, ok := oldFiles[path]
		if !ok {
			added = append(added, path)
		} else if oldHash != newHash {
			modified = append(modified, path)
		}
	}
	for path := range oldFiles {
		if _, ok := newFiles[path]; !ok {
			deleted = append(deleted, path)
		}
	}

	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(deleted)
	return ChangeSet{Added: added, Modified: modified, Deleted: deleted}
}

// MerkleIdentical returns true when both trees share the same root hash.
func MerkleIdentical(oldTree, newTree *MerkleTree) bool {
	return oldTree.RootHash() == newTree.RootHash()
}

// ── RecomputeRootHash ─────────────────────────────────────────────────────────

// RecomputeRootHash builds a Merkle tree from the current index and returns the
// new root hash. This replaces the simpler XOR-fold approach used in S0/S1 with
// the proper tree-based hash that matches the TS implementation.
//
// After calling this, update idx.RootHash with the returned value before saving.
func RecomputeRootHash(files map[string]FileIndex) string {
	hashes := make(map[string]string, len(files))
	for path, fi := range files {
		hashes[path] = fi.GitHash
	}
	tree := MerkleTreeFromHashes(hashes)
	return tree.RootHash()
}

// ── helpers ───────────────────────────────────────────────────────────────────

// splitPath splits a path on "/" and filters empty segments.
func splitPath(path string) []string {
	raw := strings.Split(path, "/")
	out := raw[:0]
	for _, p := range raw {
		if p != "" {
			out = append(out, p)
		}
	}
	// Also handle Windows paths (backslash)
	if len(out) == 1 && strings.Contains(out[0], "\\") {
		out = strings.Split(out[0], "\\")
	}
	return out
}
