package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServerNewRejectsPhysicalRepoPathEscapesBeforeWarmup(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "worktree")
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "external.go"), []byte("package outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("escape", filepath.Join(root, "escape-chain")); err != nil {
		t.Fatal(err)
	}

	for _, repoPath := range []string{"escape", "escape-chain"} {
		t.Run(repoPath, func(t *testing.T) {
			if _, err := New(Config{Root: root, RepoPath: repoPath}); err == nil || !strings.Contains(err.Error(), "outside") {
				t.Fatalf("New root=%q repoPath=%q error=%v, want physical escape refusal", root, repoPath, err)
			}
			for _, location := range []string{root, outside} {
				if _, err := os.Lstat(filepath.Join(location, ".donmai")); !os.IsNotExist(err) {
					t.Fatalf("New invalid repoPath warmed %q: %v", location, err)
				}
			}
		})
	}
}
