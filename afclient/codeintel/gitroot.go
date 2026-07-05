package codeintel

import (
	"os"
	"path/filepath"
)

// FindGitRoot walks upward from startDir looking for the enclosing git
// repository root, i.e. the nearest ancestor (including startDir itself) that
// contains a `.git` entry.
//
// Both forms of `.git` are accepted:
//   - a DIRECTORY, the normal case for a primary checkout.
//   - a FILE containing a `gitdir: <path>` pointer, the form used by
//     `git worktree add` checkouts. The file's contents are not parsed —
//     presence alone is enough to mark the enclosing directory as a repo
//     root for indexing-scope purposes.
//
// Returns the absolute path to the discovered root and true, or ("", false)
// if no `.git` entry is found before reaching the filesystem root.
func FindGitRoot(startDir string) (string, bool) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", false
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding `.git`.
			return "", false
		}
		dir = parent
	}
}
