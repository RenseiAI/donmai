package workarea

import (
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// RootPath is the absolute session-owned directory. Cleanup, lease, archive,
// accounting, cache generation, and restart adoption bind this path.
type RootPath string

// RepositoryPath is the absolute selected repository leaf and harness CWD.
type RepositoryPath string

func (p RootPath) String() string       { return string(p) }
func (p RepositoryPath) String() string { return string(p) }

// Layout keeps the two path meanings distinct, including the retained legacy
// case where they are equal.
type Layout struct {
	Root       RootPath
	Repository RepositoryPath
}

// IsNested reports whether this is the session-root-v1 shape.
func (l Layout) IsNested() bool {
	return filepath.Clean(l.Root.String()) != filepath.Clean(l.Repository.String())
}

// RepositoryPathFor returns a validated leaf under the root.
func (l Layout) RepositoryPathFor(leaf string) (RepositoryPath, error) {
	if err := ValidateRepositoryLeaf(leaf); err != nil {
		return "", err
	}
	path := filepath.Join(l.Root.String(), leaf)
	if !pathWithin(l.Root.String(), path) {
		return "", repositoryError(ReasonRepositoryLeafInvalid, RuleLeafSafe, leaf, "repository path escapes workarea root")
	}
	return RepositoryPath(path), nil
}

// NewLayout builds <worktree-root>/<encoded-session-id>/<repository-leaf>.
// Session identity bytes use a bounded, injective base32 encoding so a retained
// flat <worktree-root>/<session-id> may coexist during migration.
func NewLayout(parentDir, sessionID, repositoryLeaf string) (Layout, error) {
	root, err := nestedRootPath(parentDir, sessionID)
	if err != nil {
		return Layout{}, err
	}
	layout := Layout{Root: root}
	repository, err := layout.RepositoryPathFor(repositoryLeaf)
	if err != nil {
		return Layout{}, err
	}
	layout.Repository = repository
	return layout, nil
}

// FlatLayout describes the retained legacy checkout in place.
func FlatLayout(parentDir, sessionID string) (Layout, error) {
	root, err := legacyRootPath(parentDir, sessionID)
	if err != nil {
		return Layout{}, err
	}
	return Layout{Root: root, Repository: RepositoryPath(root)}, nil
}

// DiscoverLayout discovers only declared nested roots or retained flat git
// workareas. It never infers ownership from a directory listing.
func DiscoverLayout(parentDir, sessionID, prospectiveLeaf string) (Layout, bool, error) {
	flat, err := FlatLayout(parentDir, sessionID)
	if err != nil {
		return Layout{}, false, err
	}
	prospective := Layout{}
	if prospectiveLeaf != "" {
		prospective, err = NewLayout(parentDir, sessionID, prospectiveLeaf)
		if err != nil {
			return Layout{}, false, err
		}
	} else {
		root, rootErr := nestedRootPath(parentDir, sessionID)
		if rootErr != nil {
			return Layout{}, false, rootErr
		}
		prospective.Root = root
	}
	if record, readErr := ReadDeclaration(prospective.Root); readErr == nil {
		rootHandle, openErr := openDeclarationRoot(prospective.Root)
		if openErr != nil {
			return Layout{}, false, openErr
		}
		defer func() { _ = rootHandle.Close() }()
		for _, repository := range record.Repositories {
			if repository.Name != record.SelectedRepository {
				continue
			}
			path, pathErr := prospective.RepositoryPathFor(repository.Leaf)
			if pathErr != nil {
				return Layout{}, false, pathErr
			}
			info, statErr := rootHandle.Lstat(repository.Leaf)
			if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return Layout{}, false, repositoryError(ReasonDeclarationRecordInvalid, RuleDeclarationRecordSecretFree, repository.Name, "declared selected repository is absent")
			}
			return Layout{Root: prospective.Root, Repository: path}, true, nil
		}
		return Layout{}, false, repositoryError(ReasonDeclarationRecordInvalid, RuleFilterDeclared, record.SelectedRepository, "selected repository is not declared")
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return Layout{}, false, readErr
	}
	if isGitRepositoryAt(parentDir, sessionID) {
		return flat, true, nil
	}
	if prospectiveLeaf == "" {
		return flat, false, nil
	}
	return prospective, false, nil
}

func nestedRootPath(parentDir, sessionID string) (RootPath, error) {
	if !utf8.ValidString(sessionID) || sessionID == "" || len(sessionID) > 128 {
		return "", fmt.Errorf("runtime/workarea: invalid session identity for root encoding")
	}
	encoded := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(sessionID)))
	return joinedRootPath(parentDir, "wa-"+encoded)
}

func legacyRootPath(parentDir, sessionID string) (RootPath, error) {
	if strings.TrimSpace(parentDir) == "" {
		return "", errors.New("runtime/workarea: parent directory required")
	}
	if err := validateSessionLeaf(sessionID); err != nil {
		return "", err
	}
	return joinedRootPath(parentDir, sessionID)
}

func joinedRootPath(parentDir, leaf string) (RootPath, error) {
	parent, err := filepath.Abs(parentDir)
	if err != nil {
		return "", fmt.Errorf("runtime/workarea: resolve parent directory: %w", err)
	}
	root := filepath.Join(parent, leaf)
	if !pathWithin(parent, root) {
		return "", errors.New("runtime/workarea: session root escapes parent directory")
	}
	return RootPath(root), nil
}

func validateSessionLeaf(sessionID string) error {
	if sessionID == "" || sessionID == "." || sessionID == ".." || strings.HasPrefix(sessionID, ".") || strings.ContainsAny(sessionID, `/\`) || len(sessionID) > 128 {
		return fmt.Errorf("runtime/workarea: unsafe session identity leaf %q", sessionID)
	}
	return nil
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isGitRepositoryAt(parentDir, sessionLeaf string) bool {
	parent, err := filepath.Abs(parentDir)
	if err != nil {
		return false
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return false
	}
	defer func() { _ = parentRoot.Close() }()
	leafInfo, err := parentRoot.Lstat(sessionLeaf)
	if err != nil || !leafInfo.IsDir() || leafInfo.Mode()&os.ModeSymlink != 0 {
		return false
	}
	flatRoot, err := parentRoot.OpenRoot(sessionLeaf)
	if err != nil {
		return false
	}
	defer func() { _ = flatRoot.Close() }()
	openedInfo, err := flatRoot.Stat(".")
	if err != nil || !os.SameFile(leafInfo, openedInfo) {
		return false
	}
	gitInfo, err := flatRoot.Lstat(".git")
	return err == nil && gitInfo.Mode()&os.ModeSymlink == 0
}
