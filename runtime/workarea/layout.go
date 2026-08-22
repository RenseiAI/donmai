package workarea

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Layout models the session-owned workarea namespace.
//
// A session owns a workspace that CONTAINS repositories; a session is not
// synonymous with one git repository. Two paths follow from that and they are
// deliberately distinct types so no call site can silently overload one for the
// other:
//
//	<worktree-root>/<session-id>/                  → RootPath
//	<worktree-root>/<session-id>/<repo-leaf>/      → RepositoryPath (agent CWD)
//	<worktree-root>/<session-id>/<other-repo>/     → sibling context repositories
//
// Session cleanup, terminal leases, archives and disk accounting own RootPath
// atomically. Completion, branch, landing and every mutable git authority stay
// scoped to RepositoryPath.
//
// The pre-nesting ("flat") layout made the repository clone itself the session
// directory, which turned every secondary/context repository into a global peer
// under <worktree-root>/ shared by unrelated sessions. Retained flat workareas
// remain discoverable — see [DiscoverLayout] — so a workarea provisioned by an
// older binary is still recoverable after upgrade.
type Layout struct {
	// Root is the session-owned workarea root.
	Root RootPath
	// Repository is the selected repository worktree inside Root. It is the
	// agent CWD and the only path that carries mutable git authority.
	Repository RepositoryPath
}

// RootPath is an absolute session-owned workarea root.
type RootPath string

// RepositoryPath is an absolute repository worktree path inside a [RootPath].
type RepositoryPath string

// String returns the underlying path.
func (p RootPath) String() string { return string(p) }

// String returns the underlying path.
func (p RepositoryPath) String() string { return string(p) }

// ErrUnsafeRepositoryLeaf is returned when a repository leaf name would escape
// or clobber the session root.
var ErrUnsafeRepositoryLeaf = errors.New("runtime/workarea: unsafe repository leaf name")

// reservedLeafNames are directory names the session root reserves for its own
// metadata. A repository leaf may never take one.
var reservedLeafNames = map[string]bool{
	".agent":           true,
	".donmai":          true,
	".terminal-leases": true,
}

// NewLayout builds the nested session-owned layout
// <parentDir>/<sessionID>/<repoLeaf>. An empty or unsafe repoLeaf is an error;
// callers that genuinely want the retained flat shape ask for [FlatLayout].
func NewLayout(parentDir, sessionID, repoLeaf string) (Layout, error) {
	root, err := rootFor(parentDir, sessionID)
	if err != nil {
		return Layout{}, err
	}
	if !SafeRepositoryLeaf(repoLeaf) {
		return Layout{}, fmt.Errorf("%w: %q", ErrUnsafeRepositoryLeaf, repoLeaf)
	}
	return Layout{
		Root:       root,
		Repository: RepositoryPath(filepath.Join(root.String(), repoLeaf)),
	}, nil
}

// FlatLayout builds the retained pre-nesting layout, where the repository clone
// IS the session directory. Root and Repository are the same path; teardown of
// the root therefore still removes exactly the repository, as it always did.
func FlatLayout(parentDir, sessionID string) (Layout, error) {
	root, err := rootFor(parentDir, sessionID)
	if err != nil {
		return Layout{}, err
	}
	return Layout{Root: root, Repository: RepositoryPath(root)}, nil
}

func rootFor(parentDir, sessionID string) (RootPath, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", errors.New("runtime/workarea: session id required")
	}
	if !safePathLeaf(sessionID) {
		return "", fmt.Errorf("%w: session leaf %q", ErrUnsafeRepositoryLeaf, sessionID)
	}
	abs, err := filepath.Abs(filepath.Join(parentDir, sessionID))
	if err != nil {
		return "", fmt.Errorf("runtime/workarea: resolve workarea root: %w", err)
	}
	return RootPath(abs), nil
}

// IsNested reports whether the repository worktree lives beneath the session
// root rather than being the session root itself.
func (l Layout) IsNested() bool {
	return filepath.Clean(l.Root.String()) != filepath.Clean(l.Repository.String())
}

// SiblingPath returns the per-session path for a context/secondary repository
// leaf inside this layout's root. The leaf is validated; a leaf that collides
// with the selected repository is rejected so a context clone can never
// overwrite the mutable work repository.
func (l Layout) SiblingPath(leaf string) (string, error) {
	if !SafeRepositoryLeaf(leaf) {
		return "", fmt.Errorf("%w: %q", ErrUnsafeRepositoryLeaf, leaf)
	}
	target := filepath.Join(l.Root.String(), leaf)
	if filepath.Clean(target) == filepath.Clean(l.Repository.String()) {
		return "", fmt.Errorf("%w: %q collides with the selected repository", ErrUnsafeRepositoryLeaf, leaf)
	}
	return target, nil
}

// RepositoryLeaf derives the canonical, collision-safe directory leaf for a
// repository URL: the URL path basename with any ".git" suffix stripped and
// every character outside [A-Za-z0-9._-] folded to "-".
//
// When the sanitized basename is empty or reserved, or when sanitizing changed
// the basename (so two distinct URLs could otherwise fold onto one leaf), a
// short digest of the full URL is appended. The result is stable for a given
// URL across processes and hosts, which is what makes restart adoption able to
// find the same leaf again.
func RepositoryLeaf(repoURL string) string {
	trimmed := strings.TrimSpace(repoURL)
	if trimmed == "" {
		return ""
	}
	base := path.Base(strings.TrimRight(filepath.ToSlash(trimmed), "/"))
	base = strings.TrimSuffix(base, ".git")
	sanitized := sanitizeLeaf(base)
	if sanitized == base && sanitized != "" && !reservedLeafNames[sanitized] {
		return sanitized
	}
	suffix := urlDigest(trimmed)
	if sanitized == "" {
		return "repo-" + suffix
	}
	return sanitized + "-" + suffix
}

func sanitizeLeaf(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "." || out == ".." {
		return ""
	}
	return out
}

func urlDigest(repoURL string) string {
	sum := sha256.Sum256([]byte(repoURL))
	return hex.EncodeToString(sum[:])[:8]
}

// SafeRepositoryLeaf rejects leaf names that would escape or clobber the
// session root: empty, dot dirs, anything carrying a path separator, and the
// names the session root reserves for its own metadata.
func SafeRepositoryLeaf(name string) bool {
	return safePathLeaf(name) && !reservedLeafNames[name]
}

// safePathLeaf is the structural half of the check: it rejects only names that
// could escape the directory they are joined to. The session-root leaf uses it
// directly — a session id is not competing for the session root's reserved
// metadata names, and the manager applies its own lease-state-directory check
// against the resolved root.
func safePathLeaf(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

// DiscoverLayout resolves the on-disk layout for a session that some earlier
// process provisioned, without requiring in-memory manager state.
//
// Resolution order:
//  1. the nested repository worktree <parentDir>/<sessionID>/<repoLeaf>, when it
//     exists on disk;
//  2. the retained flat workarea <parentDir>/<sessionID>, when that directory is
//     itself a git repository (the pre-nesting shape);
//  3. the single repository leaf under <parentDir>/<sessionID>, when exactly one
//     git repository sits directly beneath the root — this recovers a nested
//     workarea whose repository URL the caller no longer knows.
//
// The second return value reports whether the layout was found on disk. When it
// is false the returned Layout is still the nested layout the caller SHOULD
// provision (or the flat one when repoLeaf is empty), so callers can use it as
// the prospective path.
func DiscoverLayout(parentDir, sessionID, repoLeaf string) (Layout, bool) {
	flat, err := FlatLayout(parentDir, sessionID)
	if err != nil {
		return Layout{}, false
	}
	if repoLeaf != "" && SafeRepositoryLeaf(repoLeaf) {
		nested := Layout{
			Root:       flat.Root,
			Repository: RepositoryPath(filepath.Join(flat.Root.String(), repoLeaf)),
		}
		if isDir(nested.Repository.String()) {
			return nested, true
		}
	}
	if isGitRepo(flat.Root.String()) {
		return flat, true
	}
	if leaf, ok := soleRepositoryLeaf(flat.Root.String()); ok {
		return Layout{
			Root:       flat.Root,
			Repository: RepositoryPath(filepath.Join(flat.Root.String(), leaf)),
		}, true
	}
	if repoLeaf != "" && SafeRepositoryLeaf(repoLeaf) {
		return Layout{
			Root:       flat.Root,
			Repository: RepositoryPath(filepath.Join(flat.Root.String(), repoLeaf)),
		}, false
	}
	return flat, false
}

func soleRepositoryLeaf(root string) (string, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	found := ""
	for _, entry := range entries {
		if !entry.IsDir() || !SafeRepositoryLeaf(entry.Name()) {
			continue
		}
		if !isGitRepo(filepath.Join(root, entry.Name())) {
			continue
		}
		if found != "" {
			return "", false // ambiguous: more than one repository leaf
		}
		found = entry.Name()
	}
	return found, found != ""
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isGitRepo(p string) bool {
	_, err := os.Stat(filepath.Join(p, ".git"))
	return err == nil
}
