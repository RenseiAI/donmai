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
	"time"
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

func TestCanonicalRemoteIdentityMatrix(t *testing.T) {
	t.Parallel()
	// Query and fragment never change identity; a credential-bearing
	// non-ssh URL is refused (its secret would otherwise reach git's
	// argv and the persisted origin).
	plain := "https://example.com/org/repo.git"
	plainCanonical, plainDigest, err := CanonicalRemote(plain)
	if err != nil {
		t.Fatalf("CanonicalRemote(plain) error = %v", err)
	}
	decoratedCanonical, decoratedDigest, err := CanonicalRemote(plain + "?ref=main#fragment")
	if err != nil {
		t.Fatalf("CanonicalRemote(decorated) error = %v", err)
	}
	if decoratedCanonical != plainCanonical {
		t.Errorf("canonical = %q, want %q (query/fragment must not change identity)", decoratedCanonical, plainCanonical)
	}
	if decoratedDigest != plainDigest {
		t.Errorf("digest = %q, want %q (query/fragment must not change the key)", decoratedDigest, plainDigest)
	}
	for _, source := range []string{
		"https://user:token@example.com/org/repo.git?ref=main#fragment",
		"https://user@example.com/org/repo.git",
		"file://user:secret@localhost/srv/repo.git",
	} {
		if _, _, err := CanonicalRemote(source); err == nil {
			t.Errorf("CanonicalRemote(%q) error = nil, want a refusal", source)
		}
	}
	// ssh keeps its login user: the clone must connect as the recorded
	// user, and distinct users address distinct mirrors. A password in
	// ssh userinfo is dropped — it is never ssh transport material —
	// without changing the mirror the user alone addresses.
	sshCanonical, sshDigest, err := CanonicalRemote("ssh://git@example.com/org/repo.git")
	if err != nil {
		t.Fatalf("CanonicalRemote(ssh) error = %v", err)
	}
	if sshCanonical != "ssh://git@example.com/org/repo.git" {
		t.Errorf("canonical = %q, want the login-user-preserving form", sshCanonical)
	}
	pwCanonical, pwDigest, err := CanonicalRemote("ssh://git:s3cret@example.com/org/repo.git")
	if err != nil {
		t.Fatalf("CanonicalRemote(ssh password) error = %v", err)
	}
	if pwCanonical != sshCanonical || pwDigest != sshDigest {
		t.Errorf("ssh password changed identity: canonical %q/%q digests %q/%q", pwCanonical, sshCanonical, pwDigest, sshDigest)
	}
	if strings.Contains(pwCanonical, "s3cret") {
		t.Errorf("canonical remote = %q, must not carry the password", pwCanonical)
	}
	otherDigest, err := MirrorDigest("ssh://other@example.com/org/repo.git", "scope-a")
	if err != nil {
		t.Fatalf("MirrorDigest(ssh other user) error = %v", err)
	}
	thisDigest, err := MirrorDigest("ssh://git@example.com/org/repo.git", "scope-a")
	if err != nil {
		t.Fatalf("MirrorDigest(ssh) error = %v", err)
	}
	wantDigest, err := ScopeDigest(sshDigest, "scope-a")
	if err != nil {
		t.Fatalf("ScopeDigest() error = %v", err)
	}
	if thisDigest != wantDigest {
		t.Errorf("MirrorDigest = %q, want the scope-joined digest %q", thisDigest, wantDigest)
	}
	if otherDigest == thisDigest {
		t.Error("two ssh login users share one mirror digest, want one mirror each")
	}
	// Unsupported transports are refused before git starts.
	for _, source := range []string{"git://example.com/org/repo.git", "ext::sh -c echo"} {
		if _, _, err := CanonicalRemote(source); err == nil {
			t.Errorf("CanonicalRemote(%q) error = nil, want a refusal", source)
		}
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

	dirALease, err := store.Ensure(context.Background(), srcDir, "scope-a")
	if err != nil {
		t.Fatalf("Ensure(scope-a) error = %v", err)
	}
	dirA := dirALease.Dir()
	dirALease.Release()
	dirBLease, err := store.Ensure(context.Background(), srcDir, "scope-b")
	if err != nil {
		t.Fatalf("Ensure(scope-b) error = %v", err)
	}
	dirB := dirBLease.Dir()
	dirBLease.Release()
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

	dirBLease, err := store.Ensure(context.Background(), srcB, "")
	if err != nil {
		t.Fatalf("Ensure(srcB) error = %v", err)
	}
	dirB := dirBLease.Dir()
	dirBLease.Release()
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

	dirLease, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	dir := dirLease.Dir()
	dirLease.Release()
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
	dirLease, err := store2.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure(local) error = %v", err)
	}
	dir := dirLease.Dir()
	dirLease.Release()
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

// TestEnsureCredentialBearingSourceRefused drives the production Ensure
// entry point with a credential-bearing file source: it must be refused
// before anything is written — the credential would otherwise reach git's
// argv (visible via ps) and the persisted origin — and the refusal must
// neither echo the credential nor leave it behind in anything the store
// wrote. The userinfo-free form of the same source still serves.
func TestEnsureCredentialBearingSourceRefused(t *testing.T) {
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

	if _, err := store.Ensure(context.Background(), source, "scope-a"); err == nil {
		t.Fatal("Ensure(credential-bearing file source) error = nil, want a refusal")
	} else if strings.Contains(err.Error(), secret) {
		t.Errorf("refusal error = %q, must not echo the credential", err.Error())
	}

	// Anything the store may have written, plus the captured logs: the
	// walk only reads regular files the store itself wrote under TempDir.
	var sweep strings.Builder
	_ = filepath.Walk(stateDir, func(sweepPath string, info os.FileInfo, err error) error { //nolint:gosec // G122: secret sweep over the test's own TempDir; no writes.
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
	logsMu.Lock()
	for _, line := range logged {
		sweep.WriteString(line)
		sweep.WriteByte('\n')
	}
	logsMu.Unlock()
	if strings.Contains(sweep.String(), secret) {
		t.Error("credential reached mirror state, catalog, or logs")
	}
	if entries, err := store.List(); err != nil {
		t.Fatalf("List() error = %v", err)
	} else if len(entries) != 0 {
		t.Errorf("catalog after refusal has %d rows, want none", len(entries))
	}

	// The userinfo-free form of the same source still serves through the
	// production path.
	plain := "file://localhost" + srcDir
	lease, err := store.Ensure(context.Background(), plain, "scope-a")
	if err != nil {
		t.Fatalf("Ensure(userinfo-free source) error = %v", err)
	}
	dir := lease.Dir()
	lease.Release()
	if got := runGitT(t, dir, "cat-file", "-e", sha+"^{commit}"); got != "" {
		t.Errorf("cat-file output = %q, want empty on success", got)
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
		lease1, err1 := store.Ensure(context.Background(), srcDir, "scope-a")
		if err1 == nil {
			dir1 = lease1.Dir()
			lease1.Release()
		}
	}()
	go func() {
		defer wg.Done()
		lease2, err2 := store.Ensure(context.Background(), srcDir, "scope-a")
		if err2 == nil {
			dir2 = lease2.Dir()
			lease2.Release()
		}
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

// newNamedSourceRepo creates a one-commit repository named name under
// parent whose commit subject is subject, so a test can tell which
// repository a mirror actually holds.
func newNamedSourceRepo(t *testing.T, parent, name, subject string) {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create fixture repo dir: %v", err)
	}
	runGitT(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte(subject), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	runGitT(t, dir, "add", "f")
	runGitT(t, dir, "commit", "-q", "-m", subject)
}

// TestEnsureClonesTheRemoteItsIdentityRecords drives the production entry
// point with sources whose query or fragment git reads as part of a file://
// path. The identity drops both, so the clone must too: otherwise the
// mirror recorded for one remote holds another repository's content, and
// every later Ensure for the genuine remote is served it.
func TestEnsureClonesTheRemoteItsIdentityRecords(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	newNamedSourceRepo(t, parent, "repo", "genuine")
	newNamedSourceRepo(t, parent, "repo?x", "query-named")
	newNamedSourceRepo(t, parent, "repo#y", "fragment-named")
	genuine := "file://" + parent + "/repo"

	tests := []struct {
		name    string
		crafted string
	}{
		{name: "query", crafted: genuine + "?x"},
		{name: "fragment", crafted: genuine + "#y"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			if _, err := store.Ensure(context.Background(), tt.crafted, "scope-a"); err != nil {
				t.Fatalf("Ensure(crafted) error = %v", err)
			}
			dirLease, err := store.Ensure(context.Background(), genuine, "scope-a")
			dir := dirLease.Dir()
			dirLease.Release()
			if err != nil {
				t.Fatalf("Ensure(genuine) error = %v", err)
			}
			if got := runGitT(t, dir, "log", "-1", "--format=%s", "main"); got != "genuine" {
				t.Errorf("mirror recorded for the genuine remote holds %q, want %q", got, "genuine")
			}
		})
	}
}

// TestEnsureRefusesCredentialBearingOpaqueRemote drives the production
// entry point with scp-like and other opaque remotes carrying
// password-shaped userinfo ("user:password@..."). Opaque forms hash
// verbatim, so the token would persist verbatim into the sidecar and the
// mirror config: the remote must be refused with ErrOpaqueCredential
// before anything is written. A bare "user@host:path" has no password
// separator and is still served by identity (it fails later at clone time
// only because the fixture host is unreachable — never at the refusal).
// A password in a hierarchical URL (ssh://user:password@host/...) is not
// opaque: canonicalization strips it, so it is pinned clean below rather
// than refused.
func TestEnsureRefusesCredentialBearingOpaqueRemote(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-token-ABC123"
	refused := []string{
		"deploy:" + secret + "@forge.example:org/repo.git",
		"user:" + secret + "@forge.example:org/repo.git",
	}
	for _, source := range refused {
		store := newTestStore(t)
		_, err := store.Ensure(context.Background(), source, "scope-a")
		if !errors.Is(err, ErrOpaqueCredential) {
			t.Errorf("Ensure(%q) error = %v, want ErrOpaqueCredential", source, err)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("refusal error = %q, must not echo the source", err.Error())
		}
		var leaked []string
		_ = filepath.Walk(store.stateDir, func(sweepPath string, info os.FileInfo, err error) error { //nolint:gosec // G122: secret sweep over the test's own TempDir; no writes.
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			data, err := os.ReadFile(sweepPath) //nolint:gosec // G304: files enumerated from the test's own TempDir walk.
			if err == nil && strings.Contains(string(data), secret) {
				leaked = append(leaked, sweepPath)
			}
			return nil
		})
		if len(leaked) > 0 {
			t.Errorf("credential persisted under the state dir: %v", leaked)
		}
	}
	// Hierarchical ssh password forms keep their login user but drop the
	// password: the user addresses its own mirror, while the secret never
	// reaches git's argv or the persisted origin. A userinfo-free ssh
	// remote still addresses a different mirror than a login-user one.
	plain := "ssh://forge.example/org/repo.git"
	usered := "ssh://deploy@forge.example/org/repo.git"
	credentialed := "ssh://deploy:" + secret + "@forge.example/org/repo.git"
	plainCanonical, plainDigest, err := CanonicalRemote(plain)
	if err != nil {
		t.Fatalf("CanonicalRemote(plain) error = %v", err)
	}
	userCanonical, userDigest, err := CanonicalRemote(usered)
	if err != nil {
		t.Fatalf("CanonicalRemote(user) error = %v", err)
	}
	credCanonical, credDigest, err := CanonicalRemote(credentialed)
	if err != nil {
		t.Fatalf("CanonicalRemote(credentialed) error = %v", err)
	}
	if credCanonical != userCanonical || credDigest != userDigest {
		t.Errorf("ssh password changed identity: canonical %q/%q digests %q/%q", credCanonical, userCanonical, credDigest, userDigest)
	}
	if strings.Contains(credCanonical, secret) {
		t.Errorf("canonical remote = %q, must not carry the password", credCanonical)
	}
	if userCanonical == plainCanonical || userDigest == plainDigest {
		t.Errorf("ssh login user merged with the userinfo-free remote: canonical %q/%q digests %q/%q", userCanonical, plainCanonical, userDigest, plainDigest)
	}
}

// TestResolveSourceRefusesCredentialBearingNonSSH proves the production
// clone argv never carries a credential: resolveSource — the single
// function Ensure clones from — refuses any non-ssh source carrying
// userinfo before anything is written, so no secret reaches git's argv
// (visible via ps) or the persisted origin. The clone fetches exactly the
// canonical remote: per-invocation HTTP auth travels via gitexec.Auth, and
// ssh keeps its login user in the canonical remote itself.
func TestResolveSourceRefusesCredentialBearingNonSSH(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-token-ABC123"
	refused := []string{
		"https://user:" + secret + "@example.com/org/repo.git",
		"http://user:" + secret + "@example.com/org/repo.git",
		"https://user@example.com/org/repo.git",
		"file://user:" + secret + "@localhost/srv/repo.git",
	}
	for _, source := range refused {
		if _, _, err := resolveSource(source); err == nil {
			t.Errorf("resolveSource(%q) error = nil, want a refusal", source)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("resolveSource(%q) refusal echoes the credential", source)
		}
	}
	// ssh keeps its login user: the clone must connect as the recorded
	// user, so the canonical remote preserves it.
	canonical, _, err := resolveSource("ssh://git@forge.example/org/repo.git")
	if err != nil {
		t.Fatalf("resolveSource(ssh) error = %v", err)
	}
	if canonical != "ssh://git@forge.example/org/repo.git" {
		t.Errorf("ssh canonical = %q, want the login-user-preserving form", canonical)
	}
}

// TestTrimURLCredentials pins the clone-argv trim: http(s) userinfo is
// stripped, while ssh/scp forms, local paths, and file:// URLs pass
// through untouched — their userinfo is part of what those transports
// read, not http auth material.
func TestTrimURLCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		in        string
		want      string
		wantStrip bool
	}{
		{
			name:      "https with userinfo is stripped",
			in:        "https://user:s3cret@example.com/org/repo.git",
			want:      "https://example.com/org/repo.git",
			wantStrip: true,
		},
		{
			name:      "https without userinfo unchanged",
			in:        "https://example.com/org/repo.git",
			want:      "https://example.com/org/repo.git",
			wantStrip: false,
		},
		{
			name:      "http with userinfo is stripped",
			in:        "http://user:s3cret@example.com/org/repo.git",
			want:      "http://example.com/org/repo.git",
			wantStrip: true,
		},
		{
			name:      "scp-like remote untouched",
			in:        "deploy:s3cret@forge.example:org/repo.git",
			want:      "deploy:s3cret@forge.example:org/repo.git",
			wantStrip: false,
		},
		{
			name:      "ssh scheme with userinfo untouched",
			in:        "ssh://deploy:s3cret@forge.example/org/repo.git",
			want:      "ssh://deploy:s3cret@forge.example/org/repo.git",
			wantStrip: false,
		},
		{
			name:      "local path untouched",
			in:        "/srv/repos/repo.git",
			want:      "/srv/repos/repo.git",
			wantStrip: false,
		},
		{
			name:      "file URL with userinfo untouched",
			in:        "file://user:s3cret@localhost/srv/repo.git",
			want:      "file://user:s3cret@localhost/srv/repo.git",
			wantStrip: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, stripped := TrimURLCredentials(tt.in)
			if got != tt.want {
				t.Errorf("TrimURLCredentials(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if stripped != tt.wantStrip {
				t.Errorf("TrimURLCredentials(%q) stripped = %v, want %v", tt.in, stripped, tt.wantStrip)
			}
		})
	}
}

// TestEnsureBareUserOpaqueRemoteIsNotCredentialBearing pins the
// refusal's narrowness: an scp-like remote with a bare user and no
// password separator is still accepted by identity — it shares the
// verbatim digest rather than failing with ErrOpaqueCredential, and only
// fails later when the unreachable fixture host cannot be cloned.
func TestEnsureBareUserOpaqueRemoteIsNotCredentialBearing(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	_, err := store.Ensure(context.Background(), "deploy@forge.example:org/repo.git", "scope-a")
	if err == nil {
		t.Fatal("Ensure(unreachable opaque remote) error = nil, want a clone failure")
	}
	if errors.Is(err, ErrOpaqueCredential) {
		t.Fatalf("Ensure(bare-user opaque remote) error = %v, want a clone failure, not the opaque-credential refusal", err)
	}
}

// TestNewSweepsCrashLeftoverStaging is the simulated crash: a
// credentialed clone staging sibling (.clone-*.tmp with a planted
// credential in its git config) is left behind between clone and publish,
// and the next keeper start (New over the same state dir) must sweep it
// so no credential bytes remain on disk.
func TestNewSweepsCrashLeftoverStaging(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-token-ABC123"
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	// First start creates the store; the crash is simulated by planting a
	// credentialed staging sibling the way a kill between clone and
	// publish would leave it.
	first, err := New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_ = first
	mirrors := filepath.Join(stateDir, keeperDirName, mirrorsDirName)
	if err := os.MkdirAll(mirrors, 0o700); err != nil {
		t.Fatalf("create mirrors dir: %v", err)
	}
	staging, err := os.MkdirTemp(mirrors, ".clone-*.tmp")
	if err != nil {
		t.Fatalf("plant crash staging: %v", err)
	}
	// A real interrupted clone leaves a git config recording the
	// credentialed URL; plant the secret the same way.
	if err := os.WriteFile(filepath.Join(staging, "config"), []byte("[remote \"origin\"]\n\turl = https://user:"+secret+"@example.com/org/repo.git\n"), 0o600); err != nil {
		t.Fatalf("plant crash credential: %v", err)
	}
	evict, err := os.MkdirTemp(mirrors, ".evict-*.tmp")
	if err != nil {
		t.Fatalf("plant evict staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(evict, "marker"), []byte(secret), 0o600); err != nil {
		t.Fatalf("plant evict marker: %v", err)
	}
	// A live mirror beside the staging must survive the sweep.
	srcDir, _ := newSourceRepo(t)
	liveLease, err := first.Ensure(context.Background(), srcDir, "")
	live := liveLease.Dir()
	liveLease.Release()
	if err != nil {
		t.Fatalf("Ensure(live) error = %v", err)
	}

	// The next start sweeps the crash leftovers.
	if _, err := New(stateDir, worktreeRoot, nil); err != nil {
		t.Fatalf("New(after crash) error = %v", err)
	}
	entries, err := os.ReadDir(mirrors)
	if err != nil {
		t.Fatalf("read mirrors dir: %v", err)
	}
	for _, entry := range entries {
		if staleStagingPrefix(entry.Name()) {
			t.Errorf("stale staging %q survived the restart sweep", entry.Name())
		}
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live mirror did not survive the sweep: %v", err)
	}
	var leaked []string
	_ = filepath.Walk(stateDir, func(sweepPath string, info os.FileInfo, err error) error { //nolint:gosec // G122: secret sweep over the test's own TempDir; no writes.
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(sweepPath) //nolint:gosec // G304: files enumerated from the test's own TempDir walk.
		if err == nil && strings.Contains(string(data), secret) {
			leaked = append(leaked, sweepPath)
		}
		return nil
	})
	if len(leaked) > 0 {
		t.Errorf("crash credential survived the restart sweep: %v", leaked)
	}
}

// TestEnsureRefusesUnparseableURLSource drives the production entry point
// with a URL-shaped source that url.Parse rejects but git still clones. Its
// verbatim form keeps the userinfo, so it must be refused before anything
// is written rather than persisted as the canonical remote.
func TestEnsureRefusesUnparseableURLSource(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	newNamedSourceRepo(t, parent, "repo%zz", "percent-named")
	const secret = "s3cr3t-token-ABC123"
	source := "file://user:" + secret + "@localhost" + parent + "/repo%zz"
	store := newTestStore(t)

	_, err := store.Ensure(context.Background(), source, "")
	if err == nil {
		t.Fatal("Ensure(unparseable URL source) error = nil, want a refusal")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("refusal error = %q, must not echo the source", err.Error())
	}
	var leaked []string
	_ = filepath.Walk(store.stateDir, func(sweepPath string, info os.FileInfo, err error) error { //nolint:gosec // G122: secret sweep over the test's own TempDir; no writes.
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(sweepPath) //nolint:gosec // G304: files enumerated from the test's own TempDir walk.
		if err == nil && strings.Contains(string(data), secret) {
			leaked = append(leaked, sweepPath)
		}
		return nil
	})
	if len(leaked) > 0 {
		t.Errorf("credential persisted under the state dir: %v", leaked)
	}
}

// TestEnsureIgnoresAmbientGitConfig drives the production entry point under
// a global git config of the kind a developer host carries. A template
// directory must not seed hooks into the mirror: keeper git runs
// hermetically (no system or global config, empty template), so ambient
// settings never reach the clone. This case sets process environment, so
// it does not run in parallel.
func TestEnsureIgnoresAmbientGitConfig(t *testing.T) {
	srcDir, sha := newSourceRepo(t)
	templateDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(templateDir, "hooks"), 0o700); err != nil {
		t.Fatalf("create template hooks dir: %v", err)
	}
	//nolint:gosec // G306: the fixture hook must be executable to prove it is not copied.
	if err := os.WriteFile(filepath.Join(templateDir, "hooks", "reference-transaction"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write template hook: %v", err)
	}
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[init]\n\ttemplateDir = "+templateDir+"\n"), 0o600); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	store := newTestStore(t)

	lease, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	dir := lease.Dir()
	lease.Release()
	lease2, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("second Ensure() error = %v", err)
	}
	lease2.Release()
	if got := runGitT(t, dir, "cat-file", "-e", sha+"^{commit}"); got != "" {
		t.Errorf("cat-file output = %q, want empty on success", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "hooks", "reference-transaction")); !os.IsNotExist(err) {
		t.Errorf("mirror carries the template hook (err = %v)", err)
	}
}

// TestEnsureIgnoresAmbientInsteadOfRewrite pins the hermetic half of the
// ambient-config rule: a global url.insteadOf rewrite that would redirect
// the clone at another repository is ignored — keeper git reads no system
// or global config — so cloning the literal (unreachable) source fails
// and publishes nothing. Without the hermetic environment the rewrite
// would steer the clone while the identity still recorded the requested
// remote.
func TestEnsureIgnoresAmbientInsteadOfRewrite(t *testing.T) {
	srcDir, _ := newSourceRepo(t)
	const rewritten = "https://forge.invalid/org/repo.git"
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalConfig, []byte("[url \"file://"+srcDir+"\"]\n\tinsteadOf = "+rewritten+"\n"), 0o600); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	store := newTestStore(t)

	if _, err := store.Ensure(context.Background(), rewritten, ""); err == nil {
		t.Fatal("Ensure(rewritten remote) error = nil, want a clone failure against the literal source")
	}
	leftovers, err := os.ReadDir(filepath.Join(store.stateDir, keeperDirName, mirrorsDirName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read mirrors dir: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("refused clone left %d entries behind, want none", len(leftovers))
	}
}

// TestEnsureMirrorLocalInsteadOfFailsClosed pins the other half of the
// ambient-config rule: a rewrite planted in the mirror's own config
// re-points its live origin, and must still fail closed.
func TestEnsureMirrorLocalInsteadOfFailsClosed(t *testing.T) {
	t.Parallel()
	srcA, _ := newSourceRepo(t)
	srcB, _ := newSourceRepo(t)
	store := newTestStore(t)

	dirBLease, err := store.Ensure(context.Background(), srcB, "")
	if err != nil {
		t.Fatalf("Ensure(srcB) error = %v", err)
	}
	dirB := dirBLease.Dir()
	dirBLease.Release()
	runGitT(t, dirB, "config", "url."+srcA+".insteadOf", srcB)

	if _, err := store.Ensure(context.Background(), srcB, ""); !errors.Is(err, ErrOriginMismatch) {
		t.Fatalf("Ensure(srcB with a mirror-local rewrite) error = %v, want ErrOriginMismatch", err)
	}
}

// TestMirrorDigestJoinsCanonicalAndScope proves the daemon layer's choke
// point names exactly the directory Ensure serves: one (source, scope)
// pair resolves through MirrorDigest to the digest Ensure published.
func TestMirrorDigestJoinsCanonicalAndScope(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	dirLease, err := store.Ensure(context.Background(), srcDir, "scope-a")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	dir := dirLease.Dir()
	dirLease.Release()
	digest, err := MirrorDigest(srcDir, "scope-a")
	if err != nil {
		t.Fatalf("MirrorDigest() error = %v", err)
	}
	if filepath.Base(dir) != digest {
		t.Errorf("Ensure dir = %q, want digest %q", filepath.Base(dir), digest)
	}
	if _, err := MirrorDigest("file://user:secret@localhost/x%zz", ""); err == nil {
		t.Error("MirrorDigest(unparseable URL source) error = nil, want a refusal")
	}
	if _, err := MirrorDigest("   ", ""); err == nil {
		t.Error("MirrorDigest(empty source) error = nil, want a refusal")
	}
}

// TestRecordFetchReplacesRowState proves RecordFetch advances last-fetch,
// replaces bytes and revoked state, and advances the use marker so a
// freshly fetched mirror is not the next eviction victim.
func TestRecordFetchReplacesRowState(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	if _, err := store.Ensure(context.Background(), srcDir, ""); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	digest := mirrorDigestFor(t, srcDir, "")
	at := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	if err := store.RecordFetch(digest, at, 42, true); err != nil {
		t.Fatalf("RecordFetch() error = %v", err)
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
		if !entry.LastFetch.Equal(at) {
			t.Errorf("LastFetch = %v, want %v", entry.LastFetch, at)
		}
		if !entry.LastUse.Equal(at) {
			t.Errorf("LastUse = %v, want %v", entry.LastUse, at)
		}
		if entry.Bytes != 42 {
			t.Errorf("Bytes = %d, want 42", entry.Bytes)
		}
		if !entry.Revoked {
			t.Error("Revoked = false, want true")
		}
	}
	if !found {
		t.Fatalf("no catalog row for digest %q", digest)
	}
	// A second fetch replaces the revoked flag without losing the row.
	if err := store.RecordFetch(digest, at.Add(time.Hour), 7, false); err != nil {
		t.Fatalf("RecordFetch() error = %v", err)
	}
	entries, err = store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, entry := range entries {
		if entry.Digest != digest {
			continue
		}
		if entry.Revoked {
			t.Error("Revoked still true after a clearing fetch")
		}
		if entry.Bytes != 7 {
			t.Errorf("Bytes = %d, want 7", entry.Bytes)
		}
	}
}

// TestRemoveDeletesDirectoryAndDropsRow proves Remove deletes the mirror
// directory outside the per-mirror lock and drops its catalog row, and
// that removing a missing mirror still converges the row away.
func TestRemoveDeletesDirectoryAndDropsRow(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	dirLease, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	dir := dirLease.Dir()
	digest := mirrorDigestFor(t, srcDir, "")
	dirLease.Release()
	if err := store.Remove(digest); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("removed mirror still on disk: err = %v", err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, entry := range entries {
		if entry.Digest == digest {
			t.Errorf("removed digest %q still listed", digest)
		}
	}
	leftovers, err := os.ReadDir(filepath.Join(store.stateDir, keeperDirName, mirrorsDirName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read mirrors dir: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("Remove left %d entries behind, want none", len(leftovers))
	}
	// Removing again converges instead of failing: the row is already
	// gone and the directory is already gone.
	if err := store.Remove(digest); err != nil {
		t.Errorf("Remove(missing) error = %v, want nil", err)
	}
	if err := store.Remove("short"); err == nil {
		t.Error("Remove(path-shaped digest) error = nil, want a refusal")
	}
}

// TestEnsureWithAuthFailureAbortsClone proves a failing auth resolver
// aborts a fresh clone before git starts and publishes nothing.
func TestEnsureWithAuthFailureAbortsClone(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)
	boom := errors.New("auth unavailable")

	_, err := store.EnsureWithAuth(context.Background(), srcDir, "", func(context.Context, string) (string, bool, error) {
		return "", false, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("EnsureWithAuth(auth error) error = %v, want the resolver error", err)
	}
	leftovers, err := os.ReadDir(filepath.Join(store.stateDir, keeperDirName, mirrorsDirName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read mirrors dir: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("aborted clone left %d entries behind, want none", len(leftovers))
	}
}

// TestEnsureWithAuthSuccessClonesLocalSource proves a resolving auth
// callback still clones through the production path: the header travels
// in-memory (gitexec scopes it), and the published mirror verifies.
func TestEnsureWithAuthSuccessClonesLocalSource(t *testing.T) {
	t.Parallel()
	srcDir, sha := newSourceRepo(t)
	store := newTestStore(t)

	lease, err := store.EnsureWithAuth(context.Background(), srcDir, "", func(context.Context, string) (string, bool, error) {
		return "Authorization: Bearer token", true, nil
	})
	dir := ""
	if err == nil {
		dir = lease.Dir()
		lease.Release()
	}
	if err != nil {
		t.Fatalf("EnsureWithAuth() error = %v", err)
	}
	if got := runGitT(t, dir, "cat-file", "-e", sha+"^{commit}"); got != "" {
		t.Errorf("cat-file output = %q, want empty on success", got)
	}
	// The resolver runs only for a fresh clone: serving the existing
	// mirror with a failing resolver still succeeds without the network.
	lease2, err := store.EnsureWithAuth(context.Background(), srcDir, "", func(context.Context, string) (string, bool, error) {
		return "", false, errors.New("must not run for an existing mirror")
	})
	dir2 := ""
	if err == nil {
		dir2 = lease2.Dir()
		lease2.Release()
	}
	if err != nil {
		t.Fatalf("EnsureWithAuth(existing) error = %v", err)
	}
	if dir2 != dir {
		t.Errorf("existing mirror dir = %q, want %q", dir2, dir)
	}
}

// TestTouchUseAdvancesMarker proves TouchUse advances one row's LastUse
// marker without changing its fetch time, bytes, or revoked flag.
func TestTouchUseAdvancesMarker(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	if _, err := store.Ensure(context.Background(), srcDir, ""); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	digest := mirrorDigestFor(t, srcDir, "")
	fetchAt := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	if err := store.RecordFetch(digest, fetchAt, 42, true); err != nil {
		t.Fatalf("RecordFetch() error = %v", err)
	}
	useAt := fetchAt.Add(2 * time.Hour)
	if err := store.TouchUse(digest, useAt); err != nil {
		t.Fatalf("TouchUse() error = %v", err)
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
		if !entry.LastUse.Equal(useAt) {
			t.Errorf("LastUse = %v, want %v", entry.LastUse, useAt)
		}
		if !entry.LastFetch.Equal(fetchAt) {
			t.Errorf("LastFetch = %v, want %v (TouchUse must not move it)", entry.LastFetch, fetchAt)
		}
		if entry.Bytes != 42 {
			t.Errorf("Bytes = %d, want 42 (TouchUse must not change it)", entry.Bytes)
		}
		if !entry.Revoked {
			t.Error("Revoked = false, want true (TouchUse must not clear it)")
		}
	}
	if !found {
		t.Fatalf("no catalog row for digest %q", digest)
	}
}
