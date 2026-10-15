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
// another. Credentials never touch the mirror config: after a fresh clone
// the persisted origin is reset to the canonical remote (which carries no
// userinfo), every remote operation runs with ambient auth only, and the
// identity sidecar and catalog are secret-free by construction.

// GitAuth resolves authorization for one remote git operation. It mirrors
// the daemon and worktree seams: authHeader is injected as an in-memory
// http.extraHeader scoped to the remote, suppressHelper disables configured
// OS credential helpers for the invocation, and an error aborts the
// operation. Returning ("", false, nil) preserves the ambient behavior.
type GitAuth func(ctx context.Context, repoURL string) (authHeader string, suppressHelper bool, err error)

// Store keeps one private bare git mirror per (canonical remote,
// credential scope) under <state-dir>/repo-keeper/mirrors/<digest>.
//
// The zero value is not usable: build one with New and keep using the same
// instance so per-mirror locks serialize concurrent callers.
type Store struct {
	stateDir     string
	worktreeRoot string
	logf         func(format string, args ...any)

	mu       sync.Mutex
	mirrorMu map[string]*sync.Mutex

	catalogMu sync.Mutex
}

// StateDir returns the absolute state directory the store was built with.
// The daemon layer derives the mirror path for a digest from it when it
// needs filesystem operations the catalog alone cannot serve (budget
// eviction); the store remains the only writer of mirrors and catalog.
func (s *Store) StateDir() string {
	return s.stateDir
}

// MirrorDir returns the on-disk mirror directory for a scope digest. It
// reports "" for a digest outside the store's shape so callers cannot be
// steered at an arbitrary path.
func (s *Store) MirrorDir(digest string) string {
	if len(digest) != 64 {
		return ""
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return filepath.Join(s.stateDir, keeperDirName, mirrorsDirName, digest)
}

// staleStagingPrefix names the temp-sibling prefixes the store clones,
// evictions, and catalog writes through: a crash (or kill) between the
// staging creation and the publish rename leaves the sibling — and any
// credential in its git config — on disk. New sweeps them at keeper start
// so no half-written staging outlives a restart.
func staleStagingPrefix(name string) bool {
	return strings.HasPrefix(name, ".clone-") ||
		strings.HasPrefix(name, ".evict-") ||
		strings.HasPrefix(name, ".catalog-")
}

// sweepStagingDirs removes crash-leftover staging siblings under the
// keeper root and the mirrors directory: the clone, eviction, and catalog
// temp prefixes the store publishes through. Only those prefixes are
// removed; live mirror directories (64-hex digests) and the published
// catalog are never touched. A leftover that cannot be removed is logged
// and skipped, never fatal: the next clone renames over a free name
// rather than reusing the leftover.
func (s *Store) sweepStagingDirs() {
	for _, dir := range []string{
		filepath.Join(s.stateDir, keeperDirName, mirrorsDirName),
		filepath.Join(s.stateDir, keeperDirName),
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !staleStagingPrefix(entry.Name()) {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if err := os.RemoveAll(path); err != nil {
				s.logf("repo keeper: stale staging dir survived sweep")
				continue
			}
			s.logf("repo keeper: swept stale staging dir")
		}
	}
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
	s := &Store{stateDir: stateDir, worktreeRoot: worktreeRoot, logf: logf}
	// A crash (or kill) between the staging creation and the publish
	// rename leaves the credentialed staging sibling on disk, so sweep
	// stale staging before serving anything from this root.
	s.sweepStagingDirs()
	return s, nil
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
// the same mirror. The clone fetches that canonical remote (plus only the
// source's userinfo), never a query or fragment the identity drops, and a
// URL-shaped source that does not parse is refused. scope is the opaque
// credential-scope name; the empty
// scope is the host-wide default. Neither value is written to the log.
func (s *Store) Ensure(ctx context.Context, source, scope string) (string, error) {
	return s.EnsureWithAuth(ctx, source, scope, nil)
}

// EnsureWithAuth is Ensure with a per-invocation authorization resolver
// for the clone: the resolver runs only when a fresh clone is needed (an
// existing mirror is served from its verified state without touching the
// network), and its header travels to git as an in-memory extraHeader
// scoped to the clone URL — never via argv, never into the mirror config.
// A resolver error aborts the clone. Neither the source nor the header is
// written to the log or echoed in an error.
func (s *Store) EnsureWithAuth(ctx context.Context, source, scope string, auth GitAuth) (string, error) {
	canonical, cloneURL, remoteDigest, err := resolveSource(source)
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
	// cloneURL fetches exactly the repository the identity records, and the
	// empty --template keeps the daemon user's template directory (which
	// may carry executable hooks) out of the mirror.
	cloneEnv, err := s.cloneEnv(ctx, cloneURL, auth)
	if err != nil {
		return "", err
	}
	if _, err := runGit(ctx, cloneEnv, "clone", "--mirror", "--template=", "--", cloneURL, tmp); err != nil {
		return "", fmt.Errorf("repo keeper: clone mirror: %w", err)
	}
	if err := s.hardenMirror(tmp, canonical); err != nil {
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
	out, err := runGit(ctxBackground(), mirrorOnlyGitEnv(), "--git-dir", mirrorDir, "remote", "get-url", "origin")
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
// reference in config. The persisted origin URL is reset to the canonical
// remote first: a clone records the exact source URL, which may carry an
// embedded credential, and credentials must never reach the mirror config.
// The canonical remote carries no userinfo, query, or fragment, and is the
// provenance verifyMirror re-checks on every use, so the check is
// unaffected.
func (s *Store) hardenMirror(mirrorDir, canonical string) error {
	alternates := filepath.Join(mirrorDir, "objects", "info", "alternates")
	if _, err := os.Lstat(alternates); err == nil {
		return fmt.Errorf("repo keeper: fresh mirror carries an alternates file")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("repo keeper: inspect mirror alternates: %w", err)
	}
	if _, err := runGit(ctxBackground(), localGitEnv(), "--git-dir", mirrorDir, "remote", "set-url", "origin", canonical); err != nil {
		return fmt.Errorf("repo keeper: reset mirror origin to canonical remote: %w", err)
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

// cloneEnv builds the environment for the fresh-clone git invocation: the
// ambient-auth baseline plus, when auth is non-nil, the resolver's header
// scoped to the clone URL. The resolver runs at most once per clone, after
// the store has committed to cloning; a resolver error aborts before git
// starts. The header travels via in-memory config (never argv), and the
// source URL is never logged or echoed in an error.
func (s *Store) cloneEnv(ctx context.Context, cloneURL string, auth GitAuth) ([]string, error) {
	if auth == nil {
		return localGitEnv(), nil
	}
	header, suppress, err := auth(ctx, cloneURL)
	if err != nil {
		return nil, fmt.Errorf("repo keeper: resolve git auth: %w", err)
	}
	if header != "" {
		if _, scoped := gitexec.ExtraHeaderConfigKey(cloneURL); !scoped {
			s.logf("repo keeper: git auth header dropped: its source is not an http(s) URL, so the header cannot be scoped to it")
		}
	}
	base := credentials.Filter(os.Environ())
	return gitexec.HardenedEnv(base, suppress, gitexec.Auth{Header: header, RemoteURL: cloneURL}), nil
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

// mirrorOnlyGitEnv is localGitEnv without the system and global git
// config, for reads that judge the mirror's own state. `remote get-url`
// expands url.<base>.insteadOf, so an operator's ambient rewrite (such as
// https to ssh) would otherwise make a freshly published mirror fail its
// own origin check. A rewrite planted in the mirror's config is still
// expanded, and still fails closed.
func mirrorOnlyGitEnv() []string {
	return append(localGitEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
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
