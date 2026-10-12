package repokeeper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/internal/credentials"
	"github.com/RenseiAI/donmai/internal/gitexec"
)

// Store keeps one private bare git mirror per (canonical remote,
// credential scope) under <state-dir>/repo-keeper/mirrors/<digest>.
//
// The zero value is not usable: build one with New and keep using the same
// instance so per-mirror locks serialize concurrent callers. Close it when
// done to release the root handle; outstanding leases stay valid until
// their own Release.
//
// Mirrors are private to their scope: two scopes for one remote resolve to
// two directories, and the store never configures git alternates between
// them, so objects fetched under one scope are never reachable from
// another. Credentials never touch the mirror config or git's argv: after
// a fresh clone the persisted origin is reset to the canonical remote
// (which carries no userinfo on non-ssh transports), every git invocation
// runs under the hermetic keeperGitEnv (hooks disabled, no system or
// global config, transport allowlist), and the identity sidecar and
// catalog are secret-free by construction.
//
// Every path the store creates or inspects below the state dir goes
// through the os.Root handle opened in New, so a symlinked mirrors parent
// planted inside keeper storage cannot steer the store outside the state
// dir. The roots themselves are pinned by ensureRealDir: a symlinked
// state dir is refused at New. Git subprocesses necessarily receive plain
// paths; a within-root symlink parent is tolerated there (planting one
// needs write access to keeper storage), but verifyMirror re-checks the
// live directory's type, provenance, and config on every use, so a
// redirected mirror fails closed rather than serving foreign content.

// GitAuth resolves authorization for one remote git operation. It mirrors
// the daemon and worktree seams: authHeader is injected as an in-memory
// http.extraHeader scoped to the remote, suppressHelper disables configured
// OS credential helpers for the invocation, and an error aborts the
// operation. Returning ("", false, nil) preserves the ambient behavior.
type GitAuth func(ctx context.Context, repoURL string) (authHeader string, suppressHelper bool, err error)

// Lease is one verified mirror checkout: the directory to use, the digest
// that addresses it, and the verification time. It holds the mirror's
// shared (reader) maintenance lock from verification until Release, so no
// fetch, eviction, or removal by another process in the fleet can move the
// directory while the holder works. Release is idempotent; methods are
// nil-safe (a nil lease reports "" and releases to a no-op) so degraded
// paths stay short.
//
// VerifiedAt is the freshness anchor: the holder verified provenance at
// that time, and a future fetch slice will re-fetch under the exclusive
// lock once VerifiedAt is older than the fetch interval instead of
// serving stale content indefinitely.
type Lease struct {
	dir        string
	digest     string
	verifiedAt time.Time
	lock       *os.File
	release    sync.Once
}

// Dir returns the on-disk bare mirror directory. It is "" on a nil lease.
func (l *Lease) Dir() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// String returns the lease's mirror digest for secret-free logging. It
// never carries a remote URL, scope, or directory path.
func (l *Lease) String() string {
	if l == nil {
		return "<nil>"
	}
	return l.digest
}

// Digest returns the scope digest addressing the mirror. It is "" on a nil
// lease.
func (l *Lease) Digest() string {
	if l == nil {
		return ""
	}
	return l.digest
}

// VerifiedAt returns the time the mirror's provenance was last verified
// under the held lock. It is zero on a nil lease.
func (l *Lease) VerifiedAt() time.Time {
	if l == nil {
		return time.Time{}
	}
	return l.verifiedAt
}

// Release drops the shared maintenance lock. It is idempotent and nil-safe.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.release.Do(func() {
		_ = unlockAndClose(l.lock)
	})
}

// Store keeps one private bare git mirror per (canonical remote,
// credential scope) under <state-dir>/repo-keeper/mirrors/<digest>.
//
// The zero value is not usable: build one with New and keep using the same
// instance so per-mirror locks serialize concurrent callers.
type Store struct {
	stateDir     string
	worktreeRoot string
	root         *os.Root
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

// Close releases the store's root handle. Outstanding leases stay valid
// until their own Release; calling Ensure after Close fails.
func (s *Store) Close() error {
	if s == nil || s.root == nil {
		return nil
	}
	return s.root.Close()
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
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return nil, fmt.Errorf("repo keeper: open state root: %w", err)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Store{stateDir: stateDir, worktreeRoot: worktreeRoot, root: root, logf: logf}, nil
}

// Ensure returns a lease on the on-disk bare mirror for (source, scope),
// creating it with a fresh clone when absent. An existing mirror has its
// recorded canonical remote re-verified against source's canonicalization
// and its recorded scope re-verified against scope on every call — either
// mismatch fails closed rather than serving the wrong remote's or another
// scope's content. The mirror directory and every parent the store creates
// use mode 0700.
//
// The lease holds the mirror's shared maintenance lock until Release, so
// the verify-then-use window is closed for as long as the holder works.
// Callers that only need the directory must still Release.
//
// source is the clone URL; the directory key is its canonicalization. A
// URL that differs only by query or fragment addresses the same mirror,
// and the clone fetches that canonical remote — never a query or fragment
// the identity drops. A non-ssh source carrying userinfo is refused before
// anything is written: the credential would otherwise reach git's argv.
// scope is the opaque credential-scope name; the empty scope is the
// host-wide default, and the literal name "default" is refused so an
// explicit scope can never merge with it. Neither value is written to the
// log.
func (s *Store) Ensure(ctx context.Context, source, scope string) (*Lease, error) {
	return s.EnsureWithAuth(ctx, source, scope, nil)
}

// EnsureWithAuth is Ensure with a per-invocation authorization resolver
// for the clone: the resolver runs only when a fresh clone is needed (an
// existing mirror is served from its verified state without touching the
// network), and its header travels to git as an in-memory extraHeader
// scoped to the clone URL — never via argv, never into the mirror config.
// A resolver error aborts the clone. Neither the source nor the header is
// written to the log or echoed in an error.
func (s *Store) EnsureWithAuth(ctx context.Context, source, scope string, auth GitAuth) (*Lease, error) {
	canonical, remoteDigest, err := resolveSource(source)
	if err != nil {
		return nil, err
	}
	digest, err := ScopeDigest(remoteDigest, scope)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(s.stateDir, keeperDirName, mirrorsDirName, digest)

	unlock := s.lockMirror(digest)
	defer unlock()

	s.sweepStaleStaging()

	if info, err := s.root.Lstat(mirrorsRelPath(digest)); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("repo keeper: mirror path exists and is not a real directory")
		}
		return s.openVerifiedLease(ctx, dir, digest, canonical, scope)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("repo keeper: stat mirror dir: %w", err)
	}

	// The mirror is absent: take the exclusive (maintenance) side of the
	// cross-process lock before creating anything, so two Store instances
	// on one state dir serialize instead of racing the publish rename.
	excl, err := s.openMirrorLock(digest)
	if err != nil {
		return nil, err
	}
	if err := lockExclusiveCtx(fdOf(excl), ctxDoneOf(ctx)); err != nil {
		_ = excl.Close()
		return nil, err
	}
	// Another process may have published while this one waited on the
	// lock: re-stat under the exclusive lock and serve the winner when
	// the directory is already there.
	if info, err := s.root.Lstat(mirrorsRelPath(digest)); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		_ = unlockAndClose(excl)
		return s.openVerifiedLease(ctx, dir, digest, canonical, scope)
	}
	// Another process may have published while this one waited on the
	// lock: re-stat under the exclusive lock and serve the winner when
	// the directory is already there. The exclusive lock is dropped
	// before taking the shared one: flock conflicts across file
	// descriptions even within one process.
	publishErr := s.cloneAndPublish(ctx, dir, digest, canonical, scope, auth)
	_ = unlockAndClose(excl)
	if publishErr != nil {
		if errors.Is(publishErr, errMirrorRaceLost) {
			return s.openVerifiedLease(ctx, dir, digest, canonical, scope)
		}
		return nil, publishErr
	}
	// The freshly published mirror is verified under a shared lock like
	// any existing one; a verification failure removes it again so a
	// mirror that fails its first verification never stays on disk for a
	// later call to trip over.
	lease, err := s.openVerifiedLease(ctx, dir, digest, canonical, scope)
	if err != nil {
		s.unpublishFailedMirror(digest, dir)
		return nil, err
	}
	return lease, nil
}

// openVerifiedLease takes the shared side of the cross-process lock,
// re-verifies the mirror under it, and returns the held lease. Any
// verification failure releases the lock and fails closed.
func (s *Store) openVerifiedLease(ctx context.Context, dir, digest, canonical, scope string) (*Lease, error) {
	lock, err := s.openMirrorLock(digest)
	if err != nil {
		return nil, err
	}
	if err := lockSharedCtx(fdOf(lock), ctxDoneOf(ctx)); err != nil {
		_ = lock.Close()
		return nil, err
	}
	lease := &Lease{dir: dir, digest: digest, lock: lock}
	if err := s.verifyMirror(dir, digest, canonical, scope); err != nil {
		lease.Release()
		return nil, err
	}
	lease.verifiedAt = time.Now()
	if numBytes, err := dirBytes(dir); err == nil {
		_ = s.refreshCatalogEntry(digest, dirModTime(dir), numBytes)
	}
	return lease, nil
}

// cloneAndPublish clones the canonical remote into a temp sibling and
// renames it into place. The caller holds the in-process per-mirror mutex
// and the exclusive cross-process lock. A failed or interrupted clone
// never leaves a half-written directory at the live name for a later
// Ensure to mistake for a verified mirror: staging is removed on every
// failure path, and crash-left staging from a killed process is swept by
// sweepStaging on the next Ensure.
//
// When the publish rename loses to another process's winner (rename
// reports "file exists"), the staging is discarded and errMirrorRaceLost
// is returned; the caller drops its exclusive lock and re-verifies the
// winner under a shared lock instead of failing.
func (s *Store) cloneAndPublish(ctx context.Context, dir, digest, canonical, scope string, auth GitAuth) error {
	if err := s.root.MkdirAll(filepath.Join(keeperDirName, mirrorsDirName), 0o700); err != nil {
		return fmt.Errorf("repo keeper: create mirror parent dir: %w", err)
	}
	// Clone into a temp sibling, then rename into place: a failed or
	// interrupted clone never leaves a half-written directory at the live
	// name for a later Ensure to mistake for a verified mirror.
	tmp, err := os.MkdirTemp(filepath.Join(s.stateDir, keeperDirName, mirrorsDirName), ".clone-*.tmp")
	if err != nil {
		return fmt.Errorf("repo keeper: create mirror staging dir: %w", err)
	}
	staged := false
	defer func() {
		if !staged {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := chmodPath(tmp, 0o700); err != nil {
		return err
	}
	// cloneURL fetches exactly the repository the identity records: the
	// canonical remote carries no query or fragment the identity drops,
	// and a credential-bearing non-ssh source was already refused by
	// resolveSource, so no credential reaches git's argv.
	cloneEnv, err := s.cloneEnv(ctx, canonical, auth)
	if err != nil {
		return err
	}
	if _, err := runGit(ctx, cloneEnv, "clone", "--mirror", "--", canonical, tmp); err != nil {
		return fmt.Errorf("repo keeper: clone mirror: %w", err)
	}
	if err := s.hardenMirror(tmp, canonical); err != nil {
		return err
	}
	if err := writeMirrorIdentity(tmp, mirrorIdentity{
		Version:         mirrorIdentityVersion,
		RemoteDigest:    mustRemoteDigest(canonical),
		CanonicalRemote: canonical,
		Scope:           scope,
	}); err != nil {
		return err
	}
	if err := chmodPath(tmp, 0o700); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if os.IsExist(err) {
			// Lost the create race: another process published first.
			// Discard the staging and report the loss; the caller
				// drops its exclusive lock (a shared lock on the same
				// file would deadlock against it) and re-verifies
				// the winner under a shared lock instead of failing
				// closed on a mirror that is, by construction,
				// exactly what was requested.
				staged = true
				_ = os.RemoveAll(tmp)
				return errMirrorRaceLost
			}
		return fmt.Errorf("repo keeper: publish mirror: %w", err)
	}
	staged = true
	if err := fsyncDir(filepath.Dir(dir)); err != nil {
		return err
	}
	return nil
}

// unpublishFailedMirror removes a freshly published mirror that failed its
// first verification. The caller holds the in-process per-mirror mutex but
// no flock: this takes the exclusive cross-process lock so a concurrent
// lease holder's git reads fail against a renamed-away directory (fail
// closed) rather than a half-removed one. Best-effort: the original
// verification error is what the caller reports.
func (s *Store) unpublishFailedMirror(digest, dir string) {
	excl, err := s.openMirrorLock(digest)
	if err != nil {
		return
	}
	defer func() { _ = unlockAndClose(excl) }()
	if err := lockExclusiveCtx(fdOf(excl), ctxDoneOf(context.Background())); err != nil {
		return
	}
	parent := filepath.Dir(dir)
	staging, err := os.MkdirTemp(parent, ".evict-*.tmp")
	if err != nil {
		return
	}
	if err := os.Remove(staging); err != nil {
		return
	}
	if err := os.Rename(dir, staging); err != nil {
		return
	}
	_ = fsyncDir(parent)
	_ = os.RemoveAll(staging)
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	entries, err := readCatalog(s.catalogPath())
	if err != nil {
		return
	}
	kept := entries[:0]
	for _, entry := range entries {
		if entry.Digest != digest {
			kept = append(kept, entry)
		}
	}
	_ = s.writeCatalogLocked(kept)
}

// errMirrorRaceLost is an internal signal: the publish rename lost to
// another process's winner. The caller drops its exclusive lock and
// re-verifies the winner under a shared lock; it never escapes Ensure.
var errMirrorRaceLost = errors.New("repo keeper: lost the mirror publish race")

// mustRemoteDigest recomputes the remote digest for a canonical remote
// the store just resolved. It cannot fail: canonical already parsed
// cleanly, so a failure returns the empty digest and the directory
// binding check fails closed downstream.
func mustRemoteDigest(canonical string) string {
	_, digest, err := CanonicalRemote(canonical)
	if err != nil {
		return ""
	}
	return digest
}

// verifyMirror re-verifies an existing mirror before every use: its
// identity sidecar must record exactly this canonical remote and scope,
// the directory must hold no alternates file and no symlinked control
// files, its git config must carry only keys a fresh hermetic clone
// writes, and its git origin must still equal the canonical remote. Any
// mismatch fails closed.
//
// Every git read runs under keeperGitEnv: hooks disabled, no system or
// global config, transport allowlist. A planted core.hooksPath,
// core.fsmonitor, core.sshCommand, remote.origin.uploadpack, or
// url.*.insteadOf in the mirror's own config is therefore either
// neutralized for the read or refused by the config allowlist — and a
// later fetch runs under the same environment, so the refused settings
// can never take effect there either.
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
	// A planted alternates file would make another scope's commits
	// reachable from this mirror; symlinked control files would steer
	// git's reads outside the mirror. Neither survives a hermetic clone,
	// so either fails closed.
	alternates := filepath.Join(mirrorDir, "objects", "info", "alternates")
	if _, err := os.Lstat(alternates); err == nil {
		return fmt.Errorf("%w: mirror carries an alternates file", ErrOriginMismatch)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("repo keeper: inspect mirror alternates: %w", err)
	}
	for _, control := range []string{"config", "HEAD", "objects", "refs"} {
		if info, err := os.Lstat(filepath.Join(mirrorDir, control)); err != nil {
			return fmt.Errorf("%w: mirror control file is missing", ErrOriginMismatch)
		} else if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: mirror control file is not a real file", ErrOriginMismatch)
		}
	}
	if err := verifyMirrorConfig(mirrorDir, canonical); err != nil {
		return err
	}
	out, err := runGit(ctxBackground(), keeperGitEnv(gitexec.Auth{}), "--git-dir", mirrorDir, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("%w: read existing mirror origin", ErrOriginMismatch)
	}
	originCanonical, _, err := CanonicalRemote(strings.TrimSpace(string(out)))
	if err != nil || originCanonical != canonical {
		return fmt.Errorf("%w: existing mirror origin does not match requested remote", ErrOriginMismatch)
	}
	return nil
}

// allowedMirrorConfigKeys is the allowlist for a mirror's git config: the
// keys a fresh `git clone --mirror` writes on its own, lowercased (git
// matches key names case-insensitively). Anything else — a planted hook
// path, filesystem monitor, ssh command, uploadpack, credential helper,
// auth header, include directive, or URL rewrite — fails closed. Values
// are checked only for remote.origin.url, which must equal the canonical
// remote; the remaining keys are inert provenance git maintains itself.
var allowedMirrorConfigKeys = map[string]bool{
	"core.repositoryformatversion": true,
	"core.filemode":                true,
	"core.bare":                    true,
	"core.ignorecase":              true,
	"core.precomposeunicode":       true,
	"core.logallrefupdates":        true,
	"remote.origin.fetch":          true,
	"remote.origin.tagopt":         true,
	"remote.origin.mirror":         true,
}

// verifyMirrorConfig refuses any mirror config key outside
// allowedMirrorConfigKeys. The read runs under keeperGitEnv, so only the
// mirror file's own pairs are listed: `--local` excludes the
// environment-injected pairs the keeper itself adds. Key names are
// lowercased before comparison because git treats them
// case-insensitively; neither keys nor values are echoed in the error.
func verifyMirrorConfig(mirrorDir, canonical string) error {
	out, err := runGit(ctxBackground(), keeperGitEnv(gitexec.Auth{}), "--git-dir", mirrorDir, "config", "--local", "-z", "--list")
	if err != nil {
		return fmt.Errorf("%w: read existing mirror config", ErrOriginMismatch)
	}
	// -z separates entries with NUL and names from values with LF, so a
	// hostile multi-line value cannot forge an extra entry.
	for _, entry := range strings.Split(string(out), "\x00") {
		if entry == "" {
			continue
		}
		name, value, _ := strings.Cut(entry, "\n")
		name = strings.ToLower(name)
		if name == "remote.origin.url" {
			if value != canonical {
				return fmt.Errorf("%w: existing mirror origin does not match requested remote", ErrOriginMismatch)
			}
			continue
		}
		if !allowedMirrorConfigKeys[name] {
			return fmt.Errorf("%w: existing mirror config carries an unexpected key", ErrOriginMismatch)
		}
	}
	return nil
}

// hardenMirror enforces the cross-mirror isolation invariants on a freshly
// cloned mirror: no alternates file (a clone must never borrow objects
// from another scope's mirror), the persisted origin URL reset to the
// canonical remote (a clone records the exact source URL, and credentials
// must never reach the mirror config), and a config carrying only keys a
// hermetic clone writes. The canonical remote carries no query or
// fragment, and on non-ssh transports no userinfo, and is the provenance
// verifyMirror re-checks on every use, so the check is unaffected.
func (s *Store) hardenMirror(mirrorDir, canonical string) error {
	alternates := filepath.Join(mirrorDir, "objects", "info", "alternates")
	if _, err := os.Lstat(alternates); err == nil {
		return fmt.Errorf("repo keeper: fresh mirror carries an alternates file")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("repo keeper: inspect mirror alternates: %w", err)
	}
	if _, err := runGit(ctxBackground(), keeperGitEnv(gitexec.Auth{}), "--git-dir", mirrorDir, "remote", "set-url", "origin", canonical); err != nil {
		return fmt.Errorf("repo keeper: reset mirror origin to canonical remote: %w", err)
	}
	if err := verifyMirrorConfig(mirrorDir, canonical); err != nil {
		return fmt.Errorf("repo keeper: fresh mirror config refused: %w", err)
	}
	return nil
}

// sweepStaleStaging removes crash-left temp clone and eviction dirs older
// than stagingStaleAfter. Only the store's own dot-prefixed staging
// patterns are candidates, and only under the mirrors directory. A staging
// dir owned by a live concurrent process is at most an hour old, so it is
// never touched; deleting a stale one cannot strand a live clone because
// the owner already failed when its directory vanished (fail closed).
// Best-effort: errors are logged, never returned.
func (s *Store) sweepStaleStaging() {
	s.sweepStaleStagingAfter(stagingStaleAfter)
}

// stagingStaleAfter is the age past which a crash-left staging dir is
// reaped by the next Ensure.
const stagingStaleAfter = time.Hour

func (s *Store) sweepStaleStagingAfter(olderThan time.Duration) {
	parent := filepath.Join(s.stateDir, keeperDirName, mirrorsDirName)
	names, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-olderThan)
	reaped := 0
	for _, name := range names {
		leaf := name.Name()
		staging := strings.HasPrefix(leaf, ".clone-") && strings.HasSuffix(leaf, ".tmp") ||
			strings.HasPrefix(leaf, ".evict-") && strings.HasSuffix(leaf, ".tmp")
		if !staging {
			continue
		}
		info, err := name.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := s.root.RemoveAll(mirrorsRelPath(leaf)); err != nil {
			s.logf("repo keeper: reap stale staging dir: %v", err)
			continue
		}
		reaped++
	}
	if reaped > 0 {
		s.logf("repo keeper: reaped %d stale staging dirs", reaped)
	}
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
	return s.writeCatalogLocked(entries)
}

// lockMirror returns the unlock func serializing Ensure for one mirror
// digest within this process. Distinct mirrors proceed in parallel;
// concurrent callers for the same mirror are serialized end-to-end
// (clone, verify, harden) against the one directory they share.
// Cross-process mutual exclusion comes from the flock side, taken inside.
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
// hermetic keeper baseline plus, when auth is non-nil, the resolver's
// header scoped to the clone URL. The resolver runs at most once per
// clone, after the store has committed to cloning; a resolver error aborts
// before git starts. The header travels via in-memory config (never argv),
// and the source URL is never logged or echoed in an error. The resolver's
// suppressHelper request is always honored as suppress: keeper git never
// consults a credential helper, so a resolver cannot re-enable one.
func (s *Store) cloneEnv(ctx context.Context, cloneURL string, auth GitAuth) ([]string, error) {
	if auth == nil {
		return keeperGitEnv(gitexec.Auth{}), nil
	}
	header, _, err := auth(ctx, cloneURL)
	if err != nil {
		return nil, fmt.Errorf("repo keeper: resolve git auth: %w", err)
	}
	if header != "" {
		if _, scoped := gitexec.ExtraHeaderConfigKey(cloneURL); !scoped {
			s.logf("repo keeper: git auth header dropped: its source is not an http(s) URL, so the header cannot be scoped to it")
		}
	}
	return keeperGitEnv(gitexec.Auth{Header: header, RemoteURL: cloneURL}), nil
}

// keeperGitEnv is the hermetic environment for every git invocation the
// keeper runs — clone, verification reads, and origin maintenance. No
// GIT_* variable from the daemon's environment passes through (an
// operator's GIT_DIR, GIT_WORK_TREE, GIT_SSH_COMMAND, or GIT_CONFIG_*
// would otherwise steer or impersonate these invocations), and no system
// or global git config is read. On top of that baseline gitexec
// hardens the invocation: non-interactive, no credential helper, any
// inherited unscoped auth header reset, and the per-invocation header (if
// any) scoped to its remote.
//
// Two further pins, as environment (not -c flags or argv, so they cannot
// be seen via ps and compose with GIT_CONFIG_COUNT callers):
//
//   - core.hooksPath=/dev/null: hooks never run, even when a mirror's own
//     config plants core.hooksPath — the environment value wins for the
//     invocation, and the config allowlist refuses the plant for later.
//   - protocol.allow=never with per-transport allows for exactly
//     file/https/ssh: ext:: helpers, remote-helper protocols, and git://
//     never run, however the source was shaped. GIT_ALLOW_PROTOCOL carries
//     the same allowlist as a second, independent enforcement layer that
//     -c flags and GIT_CONFIG_PARAMETERS cannot override.
func keeperGitEnv(auth gitexec.Auth) []string {
	base := make([]string, 0, 32)
	for _, entry := range credentials.Filter(os.Environ()) {
		if name, _, ok := strings.Cut(entry, "="); ok && strings.HasPrefix(name, "GIT_") {
			continue
		}
		base = append(base, entry)
	}
	base = append(base,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_ALLOW_PROTOCOL=file:https:ssh",
	)
	// Environment-injected config pairs, numbered from zero: base carries
	// no GIT_CONFIG_COUNT (every GIT_* was scrubbed above), so
	// gitexec.HardenedEnv continues numbering after these.
	pairs := [][2]string{
		{"core.hooksPath", os.DevNull},
		{"protocol.allow", "never"},
		{"protocol.file.allow", "always"},
		{"protocol.https.allow", "always"},
		{"protocol.ssh.allow", "always"},
	}
	for i, pair := range pairs {
		base = append(base,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, pair[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, pair[1]),
		)
	}
	base = append(base, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(pairs)))
	return gitexec.HardenedEnv(base, true, auth)
}

func (s *Store) catalogPath() string {
	return filepath.Join(s.stateDir, keeperDirName, catalogFileName)
}

// writeCatalogLocked persists the catalog. The caller holds catalogMu for
// in-process serialization; the keeper-root flock serializes writers
// across processes sharing the state dir. Readers need neither: the
// rename-publish makes every read see the old or the new document, never
// a torn one.
func (s *Store) writeCatalogLocked(entries []MirrorEntry) error {
	if err := s.root.MkdirAll(keeperDirName, 0o700); err != nil {
		return fmt.Errorf("repo keeper: create keeper dir: %w", err)
	}
	lock, err := s.root.OpenFile(filepath.Join(keeperDirName, ".catalog.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("repo keeper: open catalog lock: %w", err)
	}
	fd, err := intFileDescriptor(lock.Fd())
	if err != nil {
		_ = lock.Close()
		return err
	}
	// The catalog critical section is short (one read-modify-write the
	// caller already did, one atomic publish here); block without a ctx.
	if err := syscallFlockExclusive(fd); err != nil {
		_ = lock.Close()
		return err
	}
	defer func() { _ = unlockAndClose(lock) }()
	return writeCatalog(s.catalogPath(), entries)
}

// ctxBackground detaches mirror re-verification reads from the caller's
// cancellation: verifyMirror runs only local git reads that must complete
// for the fail-closed check to mean anything.
func ctxBackground() context.Context {
	return context.Background()
}

// ctxDone carries only cancellation for the flock retry loops.
type ctxDone interface {
	Done() <-chan struct{}
	Err() error
}

func ctxDoneOf(ctx context.Context) ctxDone {
	if ctx == nil {
		return ctxBackground()
	}
	return ctx
}

func fdOf(lock *os.File) int {
	fd, err := intFileDescriptor(lock.Fd())
	if err != nil {
		return -1
	}
	return fd
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
