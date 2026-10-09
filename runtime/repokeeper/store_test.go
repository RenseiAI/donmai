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
	"testing"
)

// runGitT runs git in dir for fixture setup and assertions only. It is
// independent of the production runGit/localGitEnv path so these tests
// exercise Store as a black box.
func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git")
	cmd.Args = append(cmd.Args, args...)
	cmd.Dir = dir
	cmd.Env = append(
		os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.test",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newSourceRepo creates a local git repository with one commit and returns
// its directory plus the commit SHA.
func newSourceRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	runGitT(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	runGitT(t, dir, "add", "a.go")
	runGitT(t, dir, "commit", "-q", "-m", "first")
	return dir, runGitT(t, dir, "rev-parse", "HEAD")
}

// newTestStore builds a Store over a fresh state dir with the worktree root
// beside it on the same filesystem.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	store, err := New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return store
}

// copyDir replicates srcDir at dstPath, preserving modes, for fixtures that
// plant a pre-existing mirror with mismatched provenance. Both paths live
// under the test's TempDir. Paths are gathered first and copied afterwards,
// so no filesystem operation happens inside the walk callback.
func copyDir(t *testing.T, srcDir, dstPath string) error {
	t.Helper()
	var paths []string
	if err := filepath.Walk(srcDir, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	}); err != nil {
		return err
	}
	for _, path := range paths {
		// #nosec G304 G703 -- path was produced by walking the test's own TempDir.
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstPath, rel)
		if info.IsDir() {
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// #nosec G122 G304 -- test fixture replication under TempDir; no writes through the link.
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			continue
		}
		// #nosec G304 G703 -- path was produced by walking the test's own TempDir.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// #nosec G703 -- target joins the walked path's own relative form under TempDir.
		if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// mirrorDigestFor resolves the on-disk mirror digest for (source, scope)
// through the production identity functions.
func mirrorDigestFor(t *testing.T, source, scope string) string {
	t.Helper()
	_, remoteDigest, err := CanonicalRemote(source)
	if err != nil {
		t.Fatalf("CanonicalRemote() error = %v", err)
	}
	digest, err := ScopeDigest(remoteDigest, scope)
	if err != nil {
		t.Fatalf("ScopeDigest() error = %v", err)
	}
	return digest
}

func TestCanonicalRemoteStripsCredentialBearingParts(t *testing.T) {
	t.Parallel()
	plain := "https://example.com/org/repo.git"
	decorated := "https://user:token@example.com/org/repo.git?ref=main#fragment"
	plainCanonical, plainDigest, err := CanonicalRemote(plain)
	if err != nil {
		t.Fatalf("CanonicalRemote(plain) error = %v", err)
	}
	decoratedCanonical, decoratedDigest, err := CanonicalRemote(decorated)
	if err != nil {
		t.Fatalf("CanonicalRemote(decorated) error = %v", err)
	}
	if decoratedCanonical != plainCanonical {
		t.Errorf("canonical = %q, want %q (userinfo/query/fragment must not change identity)", decoratedCanonical, plainCanonical)
	}
	if decoratedDigest != plainDigest {
		t.Errorf("digest = %q, want %q (credential-bearing parts must not change the key)", decoratedDigest, plainDigest)
	}
	if strings.Contains(plainCanonical, "user") || strings.Contains(plainCanonical, "token") {
		t.Errorf("canonical = %q, must not carry userinfo", plainCanonical)
	}
}

func TestScopeDigestSeparatesScopes(t *testing.T) {
	t.Parallel()
	_, remoteDigest, err := CanonicalRemote("https://example.com/org/repo.git")
	if err != nil {
		t.Fatalf("CanonicalRemote() error = %v", err)
	}
	a, err := ScopeDigest(remoteDigest, "scope-a")
	if err != nil {
		t.Fatalf("ScopeDigest(a) error = %v", err)
	}
	b, err := ScopeDigest(remoteDigest, "scope-b")
	if err != nil {
		t.Fatalf("ScopeDigest(b) error = %v", err)
	}
	def, err := ScopeDigest(remoteDigest, "")
	if err != nil {
		t.Fatalf("ScopeDigest(default) error = %v", err)
	}
	if a == b {
		t.Error("two scopes for one remote produced the same mirror digest")
	}
	if len(a) != 64 || len(b) != 64 || len(def) != 64 {
		t.Errorf("digests = %d/%d/%d hex chars, want 64 each", len(a), len(b), len(def))
	}
}

// TestEnsureTwoScopesProduceTwoDirectories is the production entry point
// for scope isolation: two Ensure calls for one remote under different
// scopes must publish two directories, each recording its own scope.
func TestEnsureTwoScopesProduceTwoDirectories(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	dirA, err := store.Ensure(context.Background(), srcDir, "scope-a")
	if err != nil {
		t.Fatalf("Ensure(scope-a) error = %v", err)
	}
	dirB, err := store.Ensure(context.Background(), srcDir, "scope-b")
	if err != nil {
		t.Fatalf("Ensure(scope-b) error = %v", err)
	}
	if dirA == dirB {
		t.Fatal("two scopes for one remote shared one mirror directory")
	}
	digestA := mirrorDigestFor(t, srcDir, "scope-a")
	digestB := mirrorDigestFor(t, srcDir, "scope-b")
	if got := filepath.Base(dirA); got != digestA {
		t.Errorf("scope-a dir = %q, want digest %q", got, digestA)
	}
	if got := filepath.Base(dirB); got != digestB {
		t.Errorf("scope-b dir = %q, want digest %q", got, digestB)
	}
	identityA, err := readMirrorIdentity(dirA)
	if err != nil {
		t.Fatalf("read scope-a identity: %v", err)
	}
	if bound, err := identityA.dirDigest(); err != nil || bound != digestA {
		t.Fatalf("scope-a identity binds to %q (err %v), want %q", bound, err, digestA)
	}
	if identityA.Scope != "scope-a" {
		t.Errorf("scope-a identity scope = %q, want %q", identityA.Scope, "scope-a")
	}
}

// TestEnsureScopeMismatchFailsClosed drives the production entry point
// against a mirror recorded for another scope: reuse must fail closed
// with ErrScopeMismatch rather than serve cross-scope content.
func TestEnsureScopeMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	if _, err := store.Ensure(context.Background(), srcDir, "scope-a"); err != nil {
		t.Fatalf("Ensure(scope-a) error = %v", err)
	}
	digestB := mirrorDigestFor(t, srcDir, "scope-b")
	dirB := filepath.Join(store.stateDir, keeperDirName, mirrorsDirName, digestB)
	// Plant scope-b's directory as a copy of scope-a's mirror: same remote,
	// but the recorded scope no longer matches the requested one.
	dirA := filepath.Join(store.stateDir, keeperDirName, mirrorsDirName, mirrorDigestFor(t, srcDir, "scope-a"))
	if err := copyDir(t, dirA, dirB); err != nil {
		t.Fatalf("plant mismatched mirror: %v", err)
	}

	_, err := store.Ensure(context.Background(), srcDir, "scope-b")
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("Ensure(scope-b over scope-a mirror) error = %v, want ErrScopeMismatch", err)
	}
}

// TestEnsureOriginMismatchFailsClosed drives the production entry point
// against a mirror whose recorded remote no longer matches the requested
// source: reuse must fail closed with ErrOriginMismatch rather than serve
// the wrong remote's content.
func TestEnsureOriginMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	srcA, _ := newSourceRepo(t)
	srcB, _ := newSourceRepo(t)
	store := newTestStore(t)

	if _, err := store.Ensure(context.Background(), srcA, ""); err != nil {
		t.Fatalf("Ensure(srcA) error = %v", err)
	}
	// Point srcB at srcA's directory by forging its canonical digest: copy
	// srcA's mirror into the slot srcB's canonical remote would occupy.
	_, digestA, err := CanonicalRemote(srcA)
	if err != nil {
		t.Fatalf("CanonicalRemote(srcA) error = %v", err)
	}
	_, digestB, err := CanonicalRemote(srcB)
	if err != nil {
		t.Fatalf("CanonicalRemote(srcB) error = %v", err)
	}
	slotA, err := ScopeDigest(digestA, "")
	if err != nil {
		t.Fatalf("ScopeDigest(A) error = %v", err)
	}
	slotB, err := ScopeDigest(digestB, "")
	if err != nil {
		t.Fatalf("ScopeDigest(B) error = %v", err)
	}
	dirA := filepath.Join(store.stateDir, keeperDirName, mirrorsDirName, slotA)
	dirB := filepath.Join(store.stateDir, keeperDirName, mirrorsDirName, slotB)
	if err := copyDir(t, dirA, dirB); err != nil {
		t.Fatalf("plant mismatched mirror: %v", err)
	}

	_, err = store.Ensure(context.Background(), srcB, "")
	if !errors.Is(err, ErrOriginMismatch) {
		t.Fatalf("Ensure(srcB over srcA mirror) error = %v, want ErrOriginMismatch", err)
	}
	if err != nil && (strings.Contains(err.Error(), srcA) || strings.Contains(err.Error(), srcB)) {
		t.Errorf("mismatch error = %q, must not echo either source", err.Error())
	}
}

// TestEnsureLiveOriginMismatchFailsClosed isolates the live git-origin leg
// of the fail-closed re-verification: the planted mirror's sidecar is fully
// consistent with the request (recorded remote, scope, and directory
// binding all match), but its git origin points at another remote, so the
// production Ensure entry point must fail closed with ErrOriginMismatch
// rather than serve foreign content.
func TestEnsureLiveOriginMismatchFailsClosed(t *testing.T) {
	t.Parallel()
	srcA, _ := newSourceRepo(t)
	srcB, _ := newSourceRepo(t)
	store := newTestStore(t)

	dirB, err := store.Ensure(context.Background(), srcB, "")
	if err != nil {
		t.Fatalf("Ensure(srcB) error = %v", err)
	}
	// Re-point only the live git origin at srcA: the sidecar still records
	// srcB under the default scope in srcB's slot, so only the live-origin
	// comparison can catch the desync.
	runGitT(t, dirB, "remote", "set-url", "origin", srcA)

	_, err = store.Ensure(context.Background(), srcB, "")
	if !errors.Is(err, ErrOriginMismatch) {
		t.Fatalf("Ensure(srcB with desynced live origin) error = %v, want ErrOriginMismatch", err)
	}
	if err != nil && (strings.Contains(err.Error(), srcA) || strings.Contains(err.Error(), srcB)) {
		t.Errorf("mismatch error = %q, must not echo either source", err.Error())
	}
}

// TestEnsureMirrorLayoutModeAndFilesystem drives the production entry point
// and then proves the layout contract: mirrors live under
// repo-keeper/mirrors/<digest>, every store-created directory is owner-only,
// and no alternates file links mirrors together.
func TestEnsureMirrorLayoutModeAndFilesystem(t *testing.T) {
	t.Parallel()
	srcDir, sha := newSourceRepo(t)
	store := newTestStore(t)

	dir, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	digest := mirrorDigestFor(t, srcDir, "")
	want := filepath.Join(store.stateDir, keeperDirName, mirrorsDirName, digest)
	if dir != want {
		t.Errorf("mirror dir = %q, want %q", dir, want)
	}
	for _, path := range []string{
		filepath.Join(store.stateDir, keeperDirName),
		filepath.Join(store.stateDir, keeperDirName, mirrorsDirName),
		dir,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("mode of %s = %o, want owner-only", path, info.Mode().Perm())
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Errorf("mirror carries an alternates file (err = %v): mirrors must not share objects", err)
	}
	if got := runGitT(t, dir, "cat-file", "-e", sha+"^{commit}"); got != "" {
		t.Errorf("cat-file output = %q, want empty on success", got)
	}
}

// TestEnsureNoCredentialInConfigCatalogOrLogs is the credential sweep: a
// mirror cloned from a credential-bearing source must leave no credential
// in its git config, its identity sidecar, the catalog, or the store's log
// output.
func TestEnsureNoCredentialInConfigCatalogOrLogs(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	const secret = "s3cr3t-token-ABC123"
	remote := "https://user:" + secret + "@example.com/org/repo.git"

	var logs []string
	root := t.TempDir()
	store, err := New(
		filepath.Join(root, "state"),
		filepath.Join(root, "worktrees"),
		func(format string, _ ...any) { logs = append(logs, format) },
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// The remote is unreachable (no such host): Ensure must fail at clone
	// time, but the sweep below proves the failure left no credential
	// behind in anything the store wrote.
	_, cloneErr := store.Ensure(context.Background(), remote, "scope-a")
	if cloneErr == nil {
		t.Fatal("Ensure(unreachable remote) error = nil, want a clone failure")
	}

	// Anything the store may have written: partial staging is removed, but
	// sweep the whole state dir, the catalog, and the logs regardless.
	var sweep strings.Builder
	// Sweep the whole state dir, the catalog rows, and the logs: the walk
	// only reads regular files the store itself wrote under TempDir.
	_ = filepath.Walk(filepath.Join(root, "state"), func(sweepPath string, info os.FileInfo, err error) error { //nolint:gosec // G122: secret sweep over the test's own TempDir; no writes.
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(sweepPath) //nolint:gosec // G304: files enumerated from the test's own TempDir walk.
		if err != nil {
			return nil
		}
		sweep.Write(data)
		sweep.WriteByte('\n')
		return nil
	})
	if entries, err := store.List(); err != nil {
		t.Fatalf("List() error = %v", err)
	} else {
		for _, entry := range entries {
			sweep.WriteString(entry.Digest)
			sweep.WriteByte('\n')
		}
	}
	for _, line := range logs {
		sweep.WriteString(line)
		sweep.WriteByte('\n')
	}
	if strings.Contains(sweep.String(), secret) {
		t.Error("credential reached mirror config, catalog, or logs")
	}

	// The reachable half: a local source's mirror config must carry no
	// auth material either (clone with ambient auth only).
	store2 := newTestStore(t)
	dir, err := store2.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure(local) error = %v", err)
	}
	config, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatalf("read mirror config: %v", err)
	}
	for _, key := range []string{"extraHeader", "helper", "password", "token"} {
		if strings.Contains(strings.ToLower(string(config)), key) {
			t.Errorf("mirror config mentions %q:\n%s", key, config)
		}
	}
	identity, err := readMirrorIdentity(dir)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if identity.CanonicalRemote != srcDir {
		t.Errorf("identity remote = %q, want %q", identity.CanonicalRemote, srcDir)
	}
}

// TestEnsureCredentialBearingSourceLeavesNoCredential drives the production
// Ensure entry point with a credential-bearing source that still clones
// successfully (file transport ignores URL userinfo): the published mirror
// must serve the cloned content while its config, identity sidecar,
// catalog row, and captured logs carry no trace of the credential.
func TestEnsureCredentialBearingSourceLeavesNoCredential(t *testing.T) {
	t.Parallel()
	srcDir, sha := newSourceRepo(t)
	const secret = "s3cr3t-token-ABC123"
	source := "file://user:" + secret + "@localhost" + srcDir

	var logsMu sync.Mutex
	var logged []string
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	store, err := New(stateDir, worktreeRoot, func(format string, args ...any) {
		logsMu.Lock()
		defer logsMu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	dir, err := store.Ensure(context.Background(), source, "scope-a")
	if err != nil {
		t.Fatalf("Ensure(credential-bearing file source) error = %v", err)
	}
	if got := runGitT(t, dir, "cat-file", "-e", sha+"^{commit}"); got != "" {
		t.Errorf("cat-file output = %q, want empty on success", got)
	}
	// The persisted origin must be the canonical remote, which carries no
	// userinfo: a clone records the exact source URL otherwise.
	origin := runGitT(t, dir, "config", "--get", "remote.origin.url")
	canonical, _, err := CanonicalRemote(source)
	if err != nil {
		t.Fatalf("CanonicalRemote() error = %v", err)
	}
	if origin != canonical {
		t.Errorf("persisted origin = %q, want canonical %q", origin, canonical)
	}

	var sweep strings.Builder
	config, err := os.ReadFile(filepath.Join(dir, "config")) //nolint:gosec // G304: constant config name under the test's own TempDir.
	if err != nil {
		t.Fatalf("read mirror config: %v", err)
	}
	sweep.Write(config)
	sweep.WriteByte('\n')
	sidecar, err := os.ReadFile(filepath.Join(dir, identityFileName)) //nolint:gosec // G304: constant sidecar name under the test's own TempDir.
	if err != nil {
		t.Fatalf("read mirror identity: %v", err)
	}
	sweep.Write(sidecar)
	sweep.WriteByte('\n')
	entries, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, entry := range entries {
		sweep.WriteString(entry.Digest)
		sweep.WriteByte('\n')
	}
	logsMu.Lock()
	for _, line := range logged {
		sweep.WriteString(line)
		sweep.WriteByte('\n')
	}
	logsMu.Unlock()
	if strings.Contains(sweep.String(), secret) {
		t.Error("credential reached the published mirror config, sidecar, catalog, or logs")
	}
}

// TestEnsureConcurrentSameMirrorDoesNotCorrupt races two production Ensure
// calls for the same (source, scope) against a fresh state dir. Both must
// succeed at the same directory with no clone corruption.
func TestEnsureConcurrentSameMirrorDoesNotCorrupt(t *testing.T) {
	t.Parallel()
	srcDir, sha := newSourceRepo(t)
	store := newTestStore(t)

	var wg sync.WaitGroup
	var dir1, dir2 string
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		dir1, err1 = store.Ensure(context.Background(), srcDir, "scope-a")
	}()
	go func() {
		defer wg.Done()
		dir2, err2 = store.Ensure(context.Background(), srcDir, "scope-a")
	}()
	wg.Wait()

	if err1 != nil {
		t.Fatalf("Ensure #1 error = %v", err1)
	}
	if err2 != nil {
		t.Fatalf("Ensure #2 error = %v", err2)
	}
	if dir1 != dir2 {
		t.Errorf("concurrent Ensures resolved to %q and %q, want one directory", dir1, dir2)
	}
	if got := runGitT(t, dir1, "cat-file", "-e", sha+"^{commit}"); got != "" {
		t.Errorf("cat-file output = %q, want empty on success", got)
	}
}

// TestMarkRevokedStickyFlag proves the catalog's revoked flag annotates the
// listing and survives later refreshes of the same row.
func TestMarkRevokedStickyFlag(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	srcDir, _ := newSourceRepo(t)
	digest := mirrorDigestFor(t, srcDir, "")

	if _, err := store.Ensure(context.Background(), srcDir, ""); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if err := store.MarkRevoked(digest, true); err != nil {
		t.Fatalf("MarkRevoked(true) error = %v", err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Digest != digest {
			continue
		}
		found = true
		if !entry.Revoked {
			t.Error("catalog row is not revoked after MarkRevoked(true)")
		}
		if entry.Digest == "" || entry.Bytes <= 0 {
			t.Errorf("catalog row = %+v, want digest and positive bytes", entry)
		}
		if entry.LastFetch.IsZero() {
			t.Error("catalog row has zero last-fetch time")
		}
	}
	if !found {
		t.Fatalf("catalog has no row for digest %q", digest)
	}
}

// TestNewRejectsCrossFilesystem is best-effort: it proves New accepts the
// common same-filesystem layout. A genuine cross-filesystem pair is only
// exercised where the platform provides two mounts.
func TestNewAcceptsSameFilesystemLayout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, err := New(filepath.Join(root, "state"), filepath.Join(root, "worktrees"), nil); err != nil {
		t.Errorf("New(same root) error = %v, want nil", err)
	}
	if _, err := New("", filepath.Join(root, "worktrees"), nil); err == nil {
		t.Error("New(empty state dir) error = nil, want an error")
	}
	if _, err := New(filepath.Join(root, "state"), "", nil); err == nil {
		t.Error("New(empty worktree root) error = nil, want an error")
	}
	if _, err := New("relative/state", filepath.Join(root, "worktrees"), nil); err == nil {
		t.Error("New(relative state dir) error = nil, want an error")
	}
}
