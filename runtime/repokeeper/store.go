package repokeeper

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/RenseiAI/donmai/internal/credentials"
	"github.com/RenseiAI/donmai/internal/gitexec"
)

// Store keeps one private bare git mirror per (canonical remote,
// credential scope) under <state-dir>/repo-keeper/mirrors/<digest>.
//
// The zero value is not usable: build one with New and keep using the same
// instance so per-mirror locks serialize concurrent callers.
//
// Mirrors are private to their scope: two scopes for one remote resolve to
// two directories, and the store never configures git alternates between
// them, so objects fetched under one scope are never reachable from
// another. Credentials never touch the mirror config: every remote
// operation authenticates through per-invocation environment only, and the
// identity sidecar and catalog are secret-free by construction.
type Store struct {
	stateDir     string
	worktreeRoot string
	logf         func(format string, args ...any)

	mu       sync.Mutex
	mirrorMu map[string]*sync.Mutex

	catalogMu sync.Mutex
}

// New builds a keeper Store rooted at stateDir, holding its mirrors under
// <state-dir>/repo-keeper/mirrors/. worktreeRoot names the worktree tree
// the mirrors will later serve: it must live on the same filesystem as
// stateDir, and New fails when the two do not share one. logf receives
// lifecycle lines that never carry remotes, scopes, or credentials; nil
// discards them.
func New(stateDir, worktreeRoot string, logf func(format string, args ...any)) (*Store, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("repo keeper: state dir is required")
	}
	if worktreeRoot == "" {
		return nil, fmt.Errorf("repo keeper: worktree root is required")
	}
	if !filepath.IsAbs(stateDir) {
		return nil, fmt.Errorf("repo keeper: state dir is not absolute")
	}
	if !filepath.IsAbs(worktreeRoot) {
		return nil, fmt.Errorf("repo keeper: worktree root is not absolute")
	}
	if err := checkSameFilesystem(stateDir, worktreeRoot); err != nil {
		return nil, err
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Store{stateDir: stateDir, worktreeRoot: worktreeRoot, logf: logf}, nil
}

// Ensure returns the on-disk bare mirror directory for (source, scope),
// creating it with a fresh clone when absent. An existing mirror has its
// recorded canonical remote re-verified against source's canonicalization
// and its recorded scope re-verified against scope on every call — either
// mismatch fails closed rather than serving the wrong remote's or another
// scope's content. The mirror directory and every parent the store creates
// use mode 0700.
//
// source is the clone URL; the directory key is its canonicalization, so a
// URL that differs only by embedded userinfo, query, or fragment addresses
// the same mirror. scope is the opaque credential-scope name; the empty
// scope is the host-wide default. Neither value is written to the log.
func (s *Store) Ensure(ctx context.Context, source, scope string) (string, error) {
	canonical, remoteDigest, err := CanonicalRemote(source)
	if err != nil {
		return "", err
	}
	digest, err := ScopeDigest(remoteDigest, scope)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.stateDir, keeperDirName, mirrorsDirName, digest)

	unlock := s.lockMirror(digest)
	defer unlock()

	if info, err := os.Lstat(dir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("repo keeper: mirror path exists and is not a real directory")
		}
		if err := s.verifyMirror(dir, digest, canonical, scope); err != nil {
			return "", err
		}
		return dir, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("repo keeper: stat mirror dir: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", fmt.Errorf("repo keeper: create mirror parent dir: %w", err)
	}
	// Clone into a temp sibling, then rename into place: a failed or
	// interrupted clone never leaves a half-written directory at the live
	// name for a later Ensure to mistake for a verified mirror.
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".clone-*.tmp")
	if err != nil {
		return "", fmt.Errorf("repo keeper: create mirror staging dir: %w", err)
	}
	staged := false
	defer func() {
		if !staged {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := chmodPath(tmp, 0o700); err != nil {
		return "", err
	}
	if _, err := runGit(ctx, localGitEnv(), "clone", "--mirror", "--", source, tmp); err != nil {
		return "", fmt.Errorf("repo keeper: clone mirror: %w", err)
	}
	if err := s.hardenMirror(tmp); err != nil {
		return "", err
	}
	if err := writeMirrorIdentity(tmp, mirrorIdentity{
		Version:         mirrorIdentityVersion,
		RemoteDigest:    remoteDigest,
		CanonicalRemote: canonical,
		Scope:           scope,
	}); err != nil {
		return "", err
	}
	if err := chmodPath(tmp, 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", fmt.Errorf("repo keeper: publish mirror: %w", err)
	}
	staged = true
	if err := fsyncDir(filepath.Dir(dir)); err != nil {
		return "", err
	}
	if err := s.verifyMirror(dir, digest, canonical, scope); err != nil {
		return "", err
	}
	if numBytes, err := dirBytes(dir); err == nil {
		_ = s.refreshCatalogEntry(digest, dirModTime(dir), numBytes)
	}
	return dir, nil
}

// verifyMirror re-verifies an existing mirror before every use: its
// identity sidecar must record exactly this canonical remote and scope,
// and its git origin must still equal the canonical remote. Any mismatch
// fails closed.
func (s *Store) verifyMirror(mirrorDir, digest, canonical, scope string) error {
	identity, err := readMirrorIdentity(mirrorDir)
	if err != nil {
		return err
	}
	if identity.CanonicalRemote != canonical {
		return fmt.Errorf("%w: mirror identity remote changed", ErrOriginMismatch)
	}
	if identity.Scope != scope {
		return fmt.Errorf("%w: mirror identity scope changed", ErrScopeMismatch)
	}
	bound, err := identity.dirDigest()
	if err != nil || bound != digest {
		return fmt.Errorf("%w: mirror identity digest does not match its directory", ErrOriginMismatch)
	}
	out, err := runGit(ctxBackground(), localGitEnv(), "--git-dir", mirrorDir, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("%w: read existing mirror origin", ErrOriginMismatch)
	}
	originCanonical, _, err := CanonicalRemote(strings.TrimSpace(string(out)))
	if err != nil || originCanonical != canonical {
		return fmt.Errorf("%w: existing mirror origin does not match requested remote", ErrOriginMismatch)
	}
	return nil
}

// hardenMirror enforces the two cross-mirror isolation invariants on a
// freshly cloned mirror: no alternates file (a clone must never borrow
// objects from another scope's mirror), and no leftover alternates
// reference in config. The origin URL is left exactly as cloned — it is
// the provenance verifyMirror re-checks on every use — and credentials
// never reach it because remote operations run with per-invocation
// environment only, never a persisted config entry.
func (s *Store) hardenMirror(mirrorDir string) error {
	alternates := filepath.Join(mirrorDir, "objects", "info", "alternates")
	if _, err := os.Lstat(alternates); err == nil {
		return fmt.Errorf("repo keeper: fresh mirror carries an alternates file")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("repo keeper: inspect mirror alternates: %w", err)
	}
	return nil
}

// List returns the secret-free catalog: one row per recorded mirror with
// its digest, last fetch, revoked flag, and bytes. It never contains a
// remote URL, scope credential, or any value a credential could be
// recovered from.
func (s *Store) List() ([]MirrorEntry, error) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	return readCatalog(s.catalogPath())
}

// MarkRevoked flags one mirror's catalog row as revoked (or clears the
// flag). Revocation only annotates the listing — later consumers decide
// what a revoked mirror may still serve — but the flag itself is sticky
// across catalog refreshes.
func (s *Store) MarkRevoked(digest string, revoked bool) error {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	entries, err := readCatalog(s.catalogPath())
	if err != nil {
		return err
	}
	found := false
	for i := range entries {
		if entries[i].Digest != digest {
			continue
		}
		entries[i].Revoked = revoked
		found = true
	}
	if !found {
		entries = append(entries, MirrorEntry{Digest: digest, Revoked: revoked})
	}
	return writeCatalog(s.catalogPath(), entries)
}

// lockMirror returns the unlock func serializing Ensure for one mirror
// digest. Distinct mirrors proceed in parallel; concurrent callers for the
// same mirror are serialized end-to-end (clone, verify, harden) against
// the one directory they share.
func (s *Store) lockMirror(digest string) func() {
	s.mu.Lock()
	if s.mirrorMu == nil {
		s.mirrorMu = make(map[string]*sync.Mutex)
	}
	mu, ok := s.mirrorMu[digest]
	if !ok {
		mu = &sync.Mutex{}
		s.mirrorMu[digest] = mu
	}
	s.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (s *Store) catalogPath() string {
	return filepath.Join(s.stateDir, keeperDirName, catalogFileName)
}

// ctxBackground detaches mirror re-verification reads from the caller's
// cancellation: verifyMirror runs only local git reads that must complete
// for the fail-closed check to mean anything.
func ctxBackground() context.Context {
	return context.Background()
}

// localGitEnv returns the environment for git invocations that must never
// carry a credential: mirror creation reads the ambient clone source only,
// and provenance reads never touch a remote. The daemon's own auth surface
// is filtered out of the child environment.
func localGitEnv() []string {
	base := credentials.Filter(os.Environ())
	return gitexec.HardenedEnv(base, true, gitexec.Auth{})
}

// runGit runs git with an explicit argv (never a shell) and an explicitly
// constructed environment. Args and combined output are deliberately
// excluded from the error: args may include a clone source and git's own
// diagnostics may echo it back, and either may carry a credential.
//
//nolint:gosec // G204: name is the hard-coded "git" binary; args are built
func runGit(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("repo keeper: git command failed: %w", err)
	}
	return out, nil
}

// chmodPath applies an owner-only mode to a path the store created.
func chmodPath(path string, mode os.FileMode) error {
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("repo keeper: mode path: %w", err)
	}
	return nil
}
