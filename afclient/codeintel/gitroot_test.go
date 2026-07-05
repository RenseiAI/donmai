package codeintel

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindGitRoot_DirectoryForm verifies discovery of a git root marked by a
// `.git` directory (the normal, non-worktree case), when starting from a
// nested subdirectory several levels below the root.
func TestFindGitRoot_DirectoryForm(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}

	got, ok := FindGitRoot(sub)
	if !ok {
		t.Fatalf("FindGitRoot(%q) = ok=false, want true", sub)
	}
	wantRoot, _ := filepath.EvalSymlinks(root)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantRoot {
		t.Errorf("FindGitRoot(%q) = %q, want %q", sub, got, wantRoot)
	}
}

// TestFindGitRoot_FileForm verifies discovery when `.git` is a FILE rather
// than a directory, as used by `git worktree add` checkouts (the file
// contains "gitdir: <path>" pointing at the primary checkout's internal
// gitdir; the content itself doesn't matter for root discovery).
func TestFindGitRoot_FileForm(t *testing.T) {
	root := t.TempDir()
	gitFile := filepath.Join(root, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: /somewhere/else/.git/worktrees/w1\n"), 0o644); err != nil { //nolint:gosec // test fixture, world-readable is fine
		t.Fatal(err)
	}
	sub := filepath.Join(root, "nested")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}

	got, ok := FindGitRoot(sub)
	if !ok {
		t.Fatalf("FindGitRoot(%q) = ok=false, want true (worktree .git file form)", sub)
	}
	wantRoot, _ := filepath.EvalSymlinks(root)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantRoot {
		t.Errorf("FindGitRoot(%q) = %q, want %q", sub, got, wantRoot)
	}
}

// TestFindGitRoot_NoRepoFound verifies the no-git-root case returns ok=false
// rather than e.g. the filesystem root.
func TestFindGitRoot_NoRepoFound(t *testing.T) {
	// A fresh temp dir tree with no .git anywhere in its ancestry (t.TempDir()
	// roots live outside any repo checkout).
	dir := t.TempDir()
	sub := filepath.Join(dir, "x", "y")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}

	if _, ok := FindGitRoot(sub); ok {
		t.Errorf("FindGitRoot(%q) = ok=true, want false (no .git in ancestry)", sub)
	}
}

// TestFindGitRoot_StartsAtRootItself verifies the search includes the
// starting directory itself, not just its ancestors.
func TestFindGitRoot_StartsAtRootItself(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}

	got, ok := FindGitRoot(root)
	if !ok {
		t.Fatalf("FindGitRoot(%q) = ok=false, want true", root)
	}
	wantRoot, _ := filepath.EvalSymlinks(root)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantRoot {
		t.Errorf("FindGitRoot(%q) = %q, want %q", root, got, wantRoot)
	}
}
