package repokeeper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/internal/gitexec"
)

// Hardening coverage: poisoned-mirror refusal, cross-process create races,
// lease semantics, staging cleanup, scope reservations, and root setup.
// Every test drives the production entry points against real local git
// repositories unless it names the internal helper it pins.

// plantBadConfigKey writes one out-of-allowlist key into an existing
// mirror's config through the test's own git (not the keeper path), so the
// next production Ensure must refuse the plant.
func plantBadConfigKey(t *testing.T, mirrorDir, key, value string) {
	t.Helper()
	runGitT(t, mirrorDir, "config", key, value)
}

// TestVerifyRefusesPlantedConfigKeys drives the production Ensure entry
// point against mirrors carrying each hostile config key from the threat
// model: a planted hook path, filesystem monitor, ssh command,
// uploadpack, auth header, credential helper, include directive, and URL
// rewrite must each fail closed with ErrOriginMismatch rather than serve.
func TestVerifyRefusesPlantedConfigKeys(t *testing.T) {
	t.Parallel()

	plants := []struct{ key, value string }{
		{"core.hooksPath", "/tmp/evil-hooks"},
		{"core.fsmonitor", "/tmp/evil-fsmonitor"},
		{"core.sshCommand", "ssh -oProxyCommand=/tmp/evil %h %p"},
		{"remote.origin.uploadpack", "/tmp/evil-uploadpack"},
		{"remote.origin.uploadpackAllowTipSHA1InWant", "true"},
		{"http.extraHeader", "AUTHORIZATION: Bearer evil"},
		{"credential.helper", "/tmp/evil-helper"},
		{"include.path", "/tmp/evil-include"},
		{"url.file:///tmp/evil.insteadOf", "https://example.com/org/repo.git"},
		{"core.bare", "false"},
	}
	for _, plant := range plants {
		t.Run(strings.ReplaceAll(plant.key, ".", "_"), func(t *testing.T) {
			t.Parallel()
			srcDir, _ := newSourceRepo(t)
			store := newTestStore(t)

			lease, err := store.Ensure(context.Background(), srcDir, "")
			if err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			dir := lease.Dir()
			lease.Release()
			plantBadConfigKey(t, dir, plant.key, plant.value)

			_, err = store.Ensure(context.Background(), srcDir, "")
			if !errors.Is(err, ErrOriginMismatch) {
				t.Fatalf("Ensure(planted %s) error = %v, want ErrOriginMismatch", plant.key, err)
			}
		})
	}
}

// TestVerifyRefusesPlantedAlternates drives the production Ensure entry
// point against a mirror whose objects/info/alternates points at another
// scope's mirror: the plant must fail closed rather than make foreign
// commits reachable.
func TestVerifyRefusesPlantedAlternates(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	leaseA, err := store.Ensure(context.Background(), srcDir, "scope-a")
	if err != nil {
		t.Fatalf("Ensure(scope-a) error = %v", err)
	}
	dirA := leaseA.Dir()
	leaseA.Release()
	leaseB, err := store.Ensure(context.Background(), srcDir, "scope-b")
	if err != nil {
		t.Fatalf("Ensure(scope-b) error = %v", err)
	}
	dirB := leaseB.Dir()
	leaseB.Release()

	// Point scope-b's mirror at scope-a's objects through an alternates
	// file: the next Ensure for scope-b must refuse it.
	if err := os.MkdirAll(filepath.Join(dirB, "objects", "info"), 0o700); err != nil {
		t.Fatalf("create objects/info: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "objects", "info", "alternates"), []byte(filepath.Join(dirA, "objects")+"\n"), 0o600); err != nil {
		t.Fatalf("plant alternates: %v", err)
	}
	if _, err := store.Ensure(context.Background(), srcDir, "scope-b"); !errors.Is(err, ErrOriginMismatch) {
		t.Fatalf("Ensure(planted alternates) error = %v, want ErrOriginMismatch", err)
	}
}

// TestVerifyRefusesSymlinkedControlFiles drives the production Ensure
// entry point against a mirror whose config file is a symlink steering
// reads outside the mirror: the plant must fail closed.
func TestVerifyRefusesSymlinkedControlFiles(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	lease, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	dir := lease.Dir()
	lease.Release()

	outside := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(outside, []byte("[core]\n\tbare = true\n"), 0o600); err != nil {
		t.Fatalf("write outside config: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "config")); err != nil {
		t.Fatalf("remove mirror config: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "config")); err != nil {
		t.Fatalf("plant config symlink: %v", err)
	}
	if _, err := store.Ensure(context.Background(), srcDir, ""); !errors.Is(err, ErrOriginMismatch) {
		t.Fatalf("Ensure(symlinked config) error = %v, want ErrOriginMismatch", err)
	}
}

// TestVerifyRefusesExecutableHooks drives the production Ensure entry
// point against a mirror carrying an executable hook: keeper git runs
// with hooks disabled, but the plant must still fail closed so a
// non-hermetic consumer can never run it.
func TestVerifyRefusesExecutableHooks(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	lease, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	dir := lease.Dir()
	lease.Release()

	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o700); err != nil {
		t.Fatalf("create hooks dir: %v", err)
	}
	//nolint:gosec // G306: the fixture hook must be executable to prove it is refused.
	if err := os.WriteFile(filepath.Join(dir, "hooks", "reference-transaction"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("plant hook: %v", err)
	}
	if _, err := store.Ensure(context.Background(), srcDir, ""); !errors.Is(err, ErrOriginMismatch) {
		t.Fatalf("Ensure(planted hook) error = %v, want ErrOriginMismatch", err)
	}
}

// TestEnsureRefusesUnsupportedTransport drives the production Ensure entry
// point with a remote-helper source: the transport allowlist must refuse
// it before anything is published.
func TestEnsureRefusesUnsupportedTransport(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	if _, err := store.Ensure(context.Background(), "ext::sh -c echo", ""); err == nil {
		t.Fatal("Ensure(ext transport) error = nil, want a refusal")
	}
	if _, err := store.Ensure(context.Background(), "git://example.com/org/repo.git", ""); err == nil {
		t.Fatal("Ensure(git transport) error = nil, want a refusal")
	}
	if _, err := store.Ensure(context.Background(), "http://example.com/org/repo.git", ""); err == nil {
		t.Fatal("Ensure(http transport) error = nil, want a refusal")
	}
	leftovers, err := os.ReadDir(filepath.Join(store.stateDir, keeperDirName, mirrorsDirName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read mirrors dir: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("refused transports left %d entries behind, want none", len(leftovers))
	}
}

// TestTwoStoresRaceCreateServeOneMirror drives production Ensure for one
// (source, scope) concurrently from two Store instances over one state
// dir — the cross-process shape with no shared memory. Both must serve
// the same directory: the flock serializes the creators and the loser
// re-verifies the winner. Each side releases its lease immediately: a
// lease held across a concurrent Ensure would exclude the other's
// maintenance lock by design, so holding one here would deadlock the
// test, not the store.
func TestTwoStoresRaceCreateServeOneMirror(t *testing.T) {
	t.Parallel()
	srcDir, sha := newSourceRepo(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	first, err := New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("New(first) error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("New(second) error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	var wg sync.WaitGroup
	var dir1, dir2, digest1, digest2 string
	var err1, err2 error
	wg.Add(2)
	go func() {
		defer wg.Done()
		var lease *Lease
		var err error
		lease, err = first.Ensure(context.Background(), srcDir, "scope-a")
		if err == nil {
			dir1, digest1 = lease.Dir(), lease.Digest()
			lease.Release()
		} else {
			err1 = err
		}
	}()
	go func() {
		defer wg.Done()
		var lease *Lease
		var err error
		lease, err = second.Ensure(context.Background(), srcDir, "scope-a")
		if err == nil {
			dir2, digest2 = lease.Dir(), lease.Digest()
			lease.Release()
		} else {
			err2 = err
		}
	}()
	wg.Wait()
	if err1 != nil {
		t.Fatalf("first Ensure error = %v", err1)
	}
	if err2 != nil {
		t.Fatalf("second Ensure error = %v", err2)
	}
	if dir1 != dir2 {
		t.Fatalf("two stores serve %q and %q, want one directory", dir1, dir2)
	}
	if digest1 != digest2 {
		t.Errorf("two stores report %q and %q, want one digest", digest1, digest2)
	}
	if got := runGitT(t, dir1, "cat-file", "-e", sha+"^{commit}"); got != "" {
		t.Errorf("cat-file output = %q, want empty on success", got)
	}
}

// TestPublishRaceLoserReverifiesWinner pins the rename-loser branch
// directly: with a valid winner already published, a second store's
// cloneAndPublish for the same directory must report the race loss, and
// the second store's Ensure must then serve the winner's directory.
func TestPublishRaceLoserReverifiesWinner(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	first, err := New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("New(first) error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("New(second) error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	winner, err := first.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("first Ensure error = %v", err)
	}
	defer winner.Release()
	canonical, _, err := CanonicalRemote(srcDir)
	if err != nil {
		t.Fatalf("CanonicalRemote() error = %v", err)
	}
	digest, err := ScopeDigest(mustRemoteDigest(canonical), "")
	if err != nil {
		t.Fatalf("ScopeDigest() error = %v", err)
	}
	dir := filepath.Join(stateDir, keeperDirName, mirrorsDirName, digest)
	if err := second.cloneAndPublish(context.Background(), dir, canonical, "", nil); !errors.Is(err, errMirrorRaceLost) {
		t.Fatalf("cloneAndPublish over a published winner error = %v, want the race loss", err)
	}
	loser, err := second.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("losing Ensure error = %v", err)
	}
	defer loser.Release()
	if loser.Dir() != winner.Dir() {
		t.Fatalf("losing Ensure serves %q, want the winner %q", loser.Dir(), winner.Dir())
	}
}

// TestLeaseHoldsSharedLockAcrossRemove pins the lease contract: Remove
// waits for an outstanding lease rather than deleting under it, and
// proceeds once the lease releases.
func TestLeaseHoldsSharedLockAcrossRemove(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	lease, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if lease.VerifiedAt().IsZero() {
		t.Error("lease VerifiedAt is zero, want the verification time")
	}
	digest := lease.Digest()

	removed := make(chan error, 1)
	go func() {
		removed <- store.Remove(digest)
	}()
	select {
	case err := <-removed:
		t.Fatalf("Remove proceeded under a held lease: err = %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	lease.Release()
	select {
	case err := <-removed:
		if err != nil {
			t.Fatalf("Remove after release error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Remove did not proceed after the lease released")
	}
	if _, err := os.Lstat(filepath.Join(store.stateDir, keeperDirName, mirrorsDirName, digest)); !os.IsNotExist(err) {
		t.Errorf("removed mirror still on disk: err = %v", err)
	}
}

// TestLeaseReleaseIsIdempotentAndNilSafe pins the lease edge contract:
// double release and nil leases never fail.
func TestLeaseReleaseIsIdempotentAndNilSafe(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	lease, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	lease.Release()
	lease.Release()
	var nilLease *Lease
	nilLease.Release()
	if got := nilLease.Dir(); got != "" {
		t.Errorf("nil lease Dir = %q, want empty", got)
	}
	if got := nilLease.Digest(); got != "" {
		t.Errorf("nil lease Digest = %q, want empty", got)
	}
	if got := nilLease.VerifiedAt(); !got.IsZero() {
		t.Errorf("nil lease VerifiedAt = %v, want zero", got)
	}
	if got := nilLease.String(); got == "" {
		t.Error("nil lease String is empty, want a placeholder")
	}
	if got := lease.String(); got != lease.Digest() {
		t.Errorf("lease String = %q, want the digest %q", got, lease.Digest())
	}
}

// TestScopeDigestRefusals pins the scope reservations: the literal name
// "default" collides with the empty (host-wide) scope and is refused, and
// an empty remote digest is rejected so callers cannot address the bare
// namespace.
func TestScopeDigestRefusals(t *testing.T) {
	t.Parallel()
	_, remoteDigest, err := CanonicalRemote("https://example.com/org/repo.git")
	if err != nil {
		t.Fatalf("CanonicalRemote() error = %v", err)
	}
	if _, err := ScopeDigest(remoteDigest, "default"); err == nil {
		t.Error(`ScopeDigest("default") error = nil, want a refusal`)
	}
	if _, err := ScopeDigest("", "scope-a"); err == nil {
		t.Error("ScopeDigest(empty digest) error = nil, want a refusal")
	}
	if _, err := MirrorDigest("https://example.com/org/repo.git", "default"); err == nil {
		t.Error(`MirrorDigest("default" scope) error = nil, want a refusal`)
	}
	if _, err := newTestStore(t).Ensure(context.Background(), "https://example.com/org/repo.git", "default"); err == nil {
		t.Error(`Ensure("default" scope) error = nil, want a refusal`)
	}
}

// TestEnsureRealDirRefusals pins the root setup: a symlinked state dir,
// a state dir that is a regular file, and a relative state dir are all
// refused at New.
func TestEnsureRealDirRefusals(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("create target: %v", err)
	}
	linked := filepath.Join(root, "linked")
	if err := os.Symlink(target, linked); err != nil {
		t.Fatalf("symlink state dir: %v", err)
	}
	worktrees := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(worktrees, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	if _, err := New(linked, worktrees, nil); err == nil {
		t.Error("New(symlinked state dir) error = nil, want a refusal")
	}
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	if _, err := New(regular, worktrees, nil); err == nil {
		t.Error("New(file state dir) error = nil, want a refusal")
	}
	if _, err := New("relative/state", worktrees, nil); err == nil {
		t.Error("New(relative state dir) error = nil, want a refusal")
	}
}

// TestVerifyRefusesSymlinkedMirrorPaths drives the production Ensure entry
// point against mirrors carrying a symlinked objects fanout (or a symlinked
// ref) pointing at another scope's mirror: the plant must fail closed with
// ErrOriginMismatch rather than make foreign commits reachable or confuse
// refs. The top-level control check alone misses these deeper plants.
func TestVerifyRefusesSymlinkedMirrorPaths(t *testing.T) {
	t.Parallel()
	plants := []struct {
		name  string
		plant func(t *testing.T, victim, donor, sha string)
	}{
		{
			name: "objects fanout",
			plant: func(t *testing.T, victim, donor, sha string) {
				t.Helper()
				fanout := sha[:2]
				if err := os.RemoveAll(filepath.Join(victim, "objects", fanout)); err != nil {
					t.Fatalf("clear victim fanout: %v", err)
				}
				if err := os.Symlink(filepath.Join(donor, "objects", fanout), filepath.Join(victim, "objects", fanout)); err != nil {
					t.Fatalf("plant fanout symlink: %v", err)
				}
			},
		},
		{
			name: "packed-refs",
			plant: func(t *testing.T, victim, donor, _ string) {
				t.Helper()
				target := filepath.Join(victim, "packed-refs")
				if err := os.Remove(target); err != nil {
					t.Fatalf("clear victim packed-refs: %v", err)
				}
				if err := os.Symlink(filepath.Join(donor, "packed-refs"), target); err != nil {
					t.Fatalf("plant packed-refs symlink: %v", err)
				}
			},
		},
	}
	for _, tt := range plants {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srcDir, sha := newSourceRepo(t)
			store := newTestStore(t)

			leaseA, err := store.Ensure(context.Background(), srcDir, "scope-a")
			if err != nil {
				t.Fatalf("Ensure(scope-a) error = %v", err)
			}
			dirA := leaseA.Dir()
			leaseA.Release()
			leaseB, err := store.Ensure(context.Background(), srcDir, "scope-b")
			if err != nil {
				t.Fatalf("Ensure(scope-b) error = %v", err)
			}
			dirB := leaseB.Dir()
			leaseB.Release()

			tt.plant(t, dirB, dirA, sha)
			if _, err := store.Ensure(context.Background(), srcDir, "scope-b"); !errors.Is(err, ErrOriginMismatch) {
				t.Fatalf("Ensure(planted %s symlink) error = %v, want ErrOriginMismatch", tt.name, err)
			}
		})
	}
}

// TestSweepReapsStaleStaging pins crash-left cleanup: a backdated temp
// clone dir is reaped by the next Ensure while a fresh one is left alone.
func TestSweepReapsStaleStaging(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	mirrors := filepath.Join(store.stateDir, keeperDirName, mirrorsDirName)
	if err := os.MkdirAll(mirrors, 0o700); err != nil {
		t.Fatalf("create mirrors dir: %v", err)
	}
	stale := filepath.Join(mirrors, ".clone-stale.tmp")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("create stale staging: %v", err)
	}
	// Backdate past the stale threshold with the test's own handle.
	past := time.Now().Add(-2 * stagingStaleAfter)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatalf("backdate stale staging: %v", err)
	}
	fresh := filepath.Join(mirrors, ".clone-fresh.tmp")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatalf("create fresh staging: %v", err)
	}

	lease, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	lease.Release()
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("stale staging still on disk: err = %v", err)
	}
	if _, err := os.Lstat(fresh); err != nil {
		t.Errorf("fresh staging reaped: %v", err)
	}
}

// TestSweepReapsStaleCatalogStaging pins the keeper-root half of crash-left
// cleanup: a backdated catalog temp beside the published catalog is reaped
// by the next Ensure, while a fresh one is left alone. Catalog temps were
// previously swept only at New, so a long-lived store accumulated them.
func TestSweepReapsStaleCatalogStaging(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	keeperRoot := filepath.Join(store.stateDir, keeperDirName)
	if err := os.MkdirAll(keeperRoot, 0o700); err != nil {
		t.Fatalf("create keeper root: %v", err)
	}
	stale := filepath.Join(keeperRoot, ".catalog-stale.tmp")
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatalf("create stale catalog temp: %v", err)
	}
	past := time.Now().Add(-2 * stagingStaleAfter)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatalf("backdate stale catalog temp: %v", err)
	}
	fresh := filepath.Join(keeperRoot, ".catalog-fresh.tmp")
	if err := os.WriteFile(fresh, []byte("{}"), 0o600); err != nil {
		t.Fatalf("create fresh catalog temp: %v", err)
	}

	lease, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	lease.Release()
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("stale catalog temp still on disk: err = %v", err)
	}
	if _, err := os.Lstat(fresh); err != nil {
		t.Errorf("fresh catalog temp reaped: %v", err)
	}
}

// TestUnpublishFailedMirrorRemovesDirectory pins the first-verification
// cleanup mechanics: a published mirror removed through the helper is
// gone from disk with its catalog row dropped.
func TestUnpublishFailedMirrorRemovesDirectory(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	lease, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	dir, digest := lease.Dir(), lease.Digest()
	lease.Release()

	store.unpublishFailedMirror(digest, dir)
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("unpublished mirror still on disk: err = %v", err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, entry := range entries {
		if entry.Digest == digest {
			t.Errorf("unpublished digest %q still listed", digest)
		}
	}
}

// TestKeeperGitEnvIsHermetic pins the git environment contract without
// running git: no GIT_* passes through except the keeper's own pins, no
// system or global config is read, hooks are disabled, and only the
// file/https/ssh transports are allowed.
func TestKeeperGitEnvIsHermetic(t *testing.T) {
	// Sets process environment, so this does not run in parallel.
	t.Setenv("GIT_DIR", "/tmp/evil-dir")
	t.Setenv("GIT_SSH_COMMAND", "/tmp/evil-ssh")
	t.Setenv("GIT_CONFIG_GLOBAL", "/tmp/evil-global")
	t.Setenv("GIT_CONFIG_PARAMETERS", "protocol.allow=always")

	env := keeperGitEnv(gitexec.Auth{})
	joined := strings.Join(env, "\n")
	for _, banned := range []string{"/tmp/evil-dir", "/tmp/evil-ssh", "/tmp/evil-global", "protocol.allow=always"} {
		if strings.Contains(joined, banned) {
			t.Errorf("keeper git env carries %q", banned)
		}
	}
	for _, want := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_ALLOW_PROTOCOL=file:https:ssh",
		"GIT_TERMINAL_PROMPT=0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("keeper git env lacks %q", want)
		}
	}
	keys := map[string]string{}
	vals := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if idx, ok := strings.CutPrefix(key, "GIT_CONFIG_KEY_"); ok {
			keys[idx] = value
		} else if idx, ok := strings.CutPrefix(key, "GIT_CONFIG_VALUE_"); ok {
			vals[idx] = value
		}
	}
	values := map[string]string{}
	for idx, k := range keys {
		values[k] = vals[idx]
	}
	for _, want := range [][2]string{
		{"core.hooksPath", os.DevNull},
		{"protocol.allow", "never"},
		{"protocol.file.allow", "always"},
		{"protocol.https.allow", "always"},
		{"protocol.ssh.allow", "always"},
	} {
		if values[want[0]] != want[1] {
			t.Errorf("keeper git config %q = %q, want %q", want[0], values[want[0]], want[1])
		}
	}
}

// TestVerifyRefusesRedirectFiles drives the production Ensure entry point
// against mirrors carrying a commondir or gitdir redirect, or an
// objects/info/http-alternates file: each would steer git's reads at
// another scope's mirror the way a planted alternates file does, so each
// must fail closed with ErrOriginMismatch rather than be served.
func TestVerifyRefusesRedirectFiles(t *testing.T) {
	t.Parallel()
	redirects := []string{
		"commondir",
		"gitdir",
		filepath.Join("objects", "info", "http-alternates"),
	}
	for _, redirect := range redirects {
		t.Run(strings.ReplaceAll(redirect, string(filepath.Separator), "_"), func(t *testing.T) {
			t.Parallel()
			srcDir, _ := newSourceRepo(t)
			store := newTestStore(t)

			leaseA, err := store.Ensure(context.Background(), srcDir, "scope-a")
			if err != nil {
				t.Fatalf("Ensure(scope-a) error = %v", err)
			}
			dirA := leaseA.Dir()
			leaseA.Release()
			leaseB, err := store.Ensure(context.Background(), srcDir, "scope-b")
			if err != nil {
				t.Fatalf("Ensure(scope-b) error = %v", err)
			}
			dirB := leaseB.Dir()
			leaseB.Release()

			target := dirA
			switch redirect {
			case "commondir", "gitdir":
				// A commondir or gitdir file carries the target directory path.
				if err := os.WriteFile(filepath.Join(dirB, redirect), []byte(target+"\n"), 0o600); err != nil {
					t.Fatalf("plant %s: %v", redirect, err)
				}
			default:
				if err := os.MkdirAll(filepath.Join(dirB, "objects", "info"), 0o700); err != nil {
					t.Fatalf("create objects/info: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dirB, redirect), []byte(filepath.Join(dirA, "objects")+"\n"), 0o600); err != nil {
					t.Fatalf("plant %s: %v", redirect, err)
				}
			}
			if _, err := store.Ensure(context.Background(), srcDir, "scope-b"); !errors.Is(err, ErrOriginMismatch) {
				t.Fatalf("Ensure(planted %s) error = %v, want ErrOriginMismatch", redirect, err)
			}
		})
	}
}

// TestVerifyRefusesPlantedQueryOrigin drives the production Ensure entry
// point against a mirror whose recorded origin carries a query string the
// identity drops: the exact-match guard must refuse it, because the
// canonicalizing get-url check alone strips the query and would serve a
// mirror that git resolves at a different repository.
func TestVerifyRefusesPlantedQueryOrigin(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	source := "file://" + srcDir
	store := newTestStore(t)

	lease, err := store.Ensure(context.Background(), source, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	dir := lease.Dir()
	lease.Release()

	canonical, _, err := CanonicalRemote(source)
	if err != nil {
		t.Fatalf("CanonicalRemote() error = %v", err)
	}
	plantBadConfigKey(t, dir, "remote.origin.url", canonical+"?redirect")
	if _, err := store.Ensure(context.Background(), source, ""); !errors.Is(err, ErrOriginMismatch) {
		t.Fatalf("Ensure(planted query origin) error = %v, want ErrOriginMismatch", err)
	}
}

// TestEnsureFirstVerificationFailureUnpublishes drives the production
// Ensure entry point with a mirror that fails its first verification: the
// plant lands between the publish rename and the verification read, and
// the failed mirror must be unpublished — gone from disk — rather than
// left for a later call to trip over. Deleting the unpublish call leaves
// the directory behind and this test goes RED.
func TestEnsureFirstVerificationFailureUnpublishes(t *testing.T) {
	// Sets a package-global hook: never parallel, so no concurrent fresh
	// Ensure in this package can observe the plant.
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	afterPublishHookForTest = func(dir string) {
		if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o700); err != nil {
			panic(err)
		}
		//nolint:gosec // G306: the fixture hook must be executable to fail verification.
		if err := os.WriteFile(filepath.Join(dir, "hooks", "post-checkout"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			panic(err)
		}
	}
	defer func() { afterPublishHookForTest = nil }()

	if _, err := store.Ensure(context.Background(), srcDir, ""); !errors.Is(err, ErrOriginMismatch) {
		t.Fatalf("Ensure(planted first verify) error = %v, want ErrOriginMismatch", err)
	}
	canonical, _, err := CanonicalRemote(srcDir)
	if err != nil {
		t.Fatalf("CanonicalRemote() error = %v", err)
	}
	digest, err := ScopeDigest(mustRemoteDigest(canonical), "")
	if err != nil {
		t.Fatalf("ScopeDigest() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(store.stateDir, keeperDirName, mirrorsDirName, digest)); !os.IsNotExist(err) {
		t.Errorf("failed first-verification mirror still on disk: err = %v", err)
	}
}

// TestNewSweepIsAgeGated pins the startup sweep's cross-process safety: a
// second Store starting while another process holds a live staging sibling
// must leave the fresh entry alone, while a crash-left stale entry is
// still reaped.
func TestNewSweepIsAgeGated(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	mirrors := filepath.Join(stateDir, keeperDirName, mirrorsDirName)
	if err := os.MkdirAll(mirrors, 0o700); err != nil {
		t.Fatalf("create mirrors dir: %v", err)
	}
	live := filepath.Join(mirrors, ".clone-live.tmp")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatalf("create live staging: %v", err)
	}
	stale := filepath.Join(mirrors, ".clone-stale.tmp")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatalf("create stale staging: %v", err)
	}
	past := time.Now().Add(-2 * stagingStaleAfter)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatalf("backdate stale staging: %v", err)
	}

	store, err := New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := os.Lstat(live); err != nil {
		t.Errorf("live staging reaped at New: %v", err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Errorf("stale staging still on disk after New: err = %v", err)
	}
}

// TestCatalogConcurrentStoresLoseNoUpdates pins the cross-process catalog
// contract: two Store instances over one state dir publishing different
// rows concurrently must both survive. With the flock covering only the
// write, both readers load the same document and one update is lost.
func TestCatalogConcurrentStoresLoseNoUpdates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	if err := os.MkdirAll(worktreeRoot, 0o700); err != nil {
		t.Fatalf("create worktree root: %v", err)
	}
	first, err := New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("New(first) error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := New(stateDir, worktreeRoot, nil)
	if err != nil {
		t.Fatalf("New(second) error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	const pairs = 10
	var wg sync.WaitGroup
	for i := 0; i < pairs; i++ {
		digestA := fmt.Sprintf("%064x", 2*i+1)
		digestB := fmt.Sprintf("%064x", 2*i+2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := first.RecordFetch(digestA, time.Now(), int64(i), false); err != nil {
				t.Errorf("first RecordFetch error = %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := second.RecordFetch(digestB, time.Now(), int64(i), false); err != nil {
				t.Errorf("second RecordFetch error = %v", err)
			}
		}()
	}
	wg.Wait()
	entries, err := first.List()
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(entries) != 2*pairs {
		t.Fatalf("catalog holds %d rows, want %d: concurrent updates were lost", len(entries), 2*pairs)
	}
}

// TestEnsureCloneErrorCarriesNoSource pins the error-hygiene contract: a
// failing clone's error must not echo the source URL (or its diagnostics),
// so a credential-bearing value can never leak through an error string.
func TestEnsureCloneErrorCarriesNoSource(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)

	const marker = "nonexistent-keeper-source"
	if _, err := store.Ensure(context.Background(), "file:///tmp/"+marker+"-repo.git", ""); err == nil {
		t.Fatal("Ensure(missing source) error = nil, want a clone failure")
	} else if strings.Contains(err.Error(), marker) {
		t.Errorf("clone error echoes the source: %v", err)
	}
}

// TestTryRemoveSkipsHeldLease pins the non-blocking removal contract:
// TryRemove reports removed=false while a lease is outstanding instead of
// waiting for it, and removes after the lease releases. Budget eviction
// relies on it so a leased mirror is skipped as a victim rather than
// stalling the keeper lock.
func TestTryRemoveSkipsHeldLease(t *testing.T) {
	t.Parallel()
	srcDir, _ := newSourceRepo(t)
	store := newTestStore(t)

	lease, err := store.Ensure(context.Background(), srcDir, "")
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	digest := lease.Digest()

	if removed, err := store.TryRemove(digest); err != nil {
		t.Fatalf("TryRemove(held) error = %v", err)
	} else if removed {
		t.Error("TryRemove(held lease) removed=true, want false while the lease is outstanding")
	}
	if _, err := os.Lstat(filepath.Join(store.stateDir, keeperDirName, mirrorsDirName, digest)); err != nil {
		t.Errorf("held mirror missing after skipped TryRemove: %v", err)
	}
	lease.Release()
	if removed, err := store.TryRemove(digest); err != nil {
		t.Fatalf("TryRemove(released) error = %v", err)
	} else if !removed {
		t.Error("TryRemove(released lease) removed=false, want true after the lease released")
	}
	if _, err := os.Lstat(filepath.Join(store.stateDir, keeperDirName, mirrorsDirName, digest)); !os.IsNotExist(err) {
		t.Errorf("removed mirror still on disk: err = %v", err)
	}
}
