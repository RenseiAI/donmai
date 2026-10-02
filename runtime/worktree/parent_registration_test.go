package worktree_test

// These controls use real Git in test-owned temporary repositories. They model
// the null HEAD interval during linked-worktree registration without removing
// or pruning any worktree registration.
import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/worktree"
)

func ownedRegistrationFixture(t *testing.T, f branchGitFixture, id string) string {
	t.Helper()
	admin := filepath.Join(f.parent, ".git", "worktrees", id)
	checkout := filepath.Join(f.root, id)
	for _, dir := range []string{admin, checkout} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Model Git 2.39.5 add_worktree's temporary null HEAD interval; the
	// gitdir/commondir links refer only to this test's owned directories.
	files := map[string]string{
		filepath.Join(admin, "gitdir"):    filepath.Join(checkout, ".git") + "\n",
		filepath.Join(admin, "commondir"): "../..\n",
		filepath.Join(admin, "HEAD"):      strings.Repeat("0", len(f.baseSHA)) + "\n",
		filepath.Join(checkout, ".git"):   "gitdir: " + admin + "\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(admin, "HEAD")
}

func advanceOwnedRegistrationRemote(t *testing.T, f branchGitFixture) string {
	t.Helper()
	seed := filepath.Join(f.root, "seed")
	if err := os.WriteFile(filepath.Join(seed, "source.txt"), []byte("advanced owned base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(t, "-C", seed, "commit", "-am", "advance owned registration witness")
	tip := f.git(t, "-C", seed, "rev-parse", "HEAD")
	f.git(t, "-C", seed, "push", "origin", "release/next")
	return tip
}

func TestOwnedGitNullHEADConnectivityWitness(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("required real Git is unavailable")
	}
	f := newBranchGitFixture(t)
	tip := advanceOwnedRegistrationRemote(t, f)
	head := ownedRegistrationFixture(t, f, "owned-null-head")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	out, err := f.run(ctx, "git", "-C", f.parent, "fetch", "origin", "--", "release/next")
	if err == nil || !strings.Contains(string(out), "bad object worktrees/owned-null-head/HEAD") {
		t.Fatalf("owned null-HEAD witness did not reproduce actual connectivity failure: err=%v output=%s", err, out)
	}
	t.Logf("owned null-HEAD connectivity witness: %s", strings.TrimSpace(string(out)))
	// Repair only the file this probe created; no deletion/prune is used.
	if err := os.WriteFile(head, []byte(f.baseSHA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.git(t, "-C", f.parent, "fetch", "origin", "--", "release/next")
	if got := f.git(t, "-C", f.parent, "rev-parse", "origin/release/next"); got != tip {
		t.Fatalf("valid owned HEAD did not restore base fetch: got=%s want=%s", got, tip)
	}
}

func TestBaseFetchExcludesOwnedRegistrationWindow(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("required real Git is unavailable")
	}
	f := newBranchGitFixture(t)
	tip := advanceOwnedRegistrationRemote(t, f)
	unlock := worktree.AcquireParentLock(f.parent)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(unlock) }
	t.Cleanup(release)
	head := ownedRegistrationFixture(t, f, "owned-held-registration")

	fetchEntered := make(chan struct{})
	var entryOnce sync.Once
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 2 && args[0] == "-C" && args[2] == "fetch" {
			entryOnce.Do(func() { close(fetchEntered) })
		}
		return f.run(ctx, name, args...)
	}
	m, err := worktree.NewManager(worktree.Options{ParentDir: filepath.Join(f.root, "workareas"), CommandRunner: runner})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() {
		_, err := m.Provision(ctx, worktree.ProvisionSpec{
			SessionID: "owned-reader", Branch: "work/owned-reader", BaseRef: "origin/release/next",
			Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: f.parent,
		})
		result <- err
	}()

	// Establish that the production fetch flight was created while the
	// writer gate is held. A fast unlocked fetch can finish before the
	// registry poll, so its runner-entry channel is a durable witness too.
	overlap := false
waitFlight:
	for !worktree.BaseFetchFlightRegistered(f.parent, "release/next") {
		select {
		case <-fetchEntered:
			overlap = true
			break waitFlight
		case <-ctx.Done():
			t.Fatal("owned reader never reached its base-fetch flight")
		case <-time.After(time.Millisecond):
		}
	}
	if !overlap {
		select {
		case <-fetchEntered:
			overlap = true
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("owned writer exclusion observation timed out")
		}
	}
	if overlap {
		// Keep the owned null HEAD in place until the real fetch finishes,
		// proving the actual Git failure rather than just overlap counters.
		var fetchErr error
		select {
		case fetchErr = <-result:
		case <-ctx.Done():
			fetchErr = fmt.Errorf("reader timed out after entering held writer")
		}
		if err := os.WriteFile(head, []byte(f.baseSHA+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		release()
		if fetchErr == nil || !strings.Contains(fetchErr.Error(), "bad object worktrees/owned-held-registration/HEAD") {
			t.Fatalf("fetch crossed parent writer but exact null-HEAD cause was not observed: %v", fetchErr)
		}
		t.Fatalf("CAUSE: base fetch entered while existing parent registration writer was held; actual Git connectivity failed: %v", fetchErr)
	}
	// Candidate protection must wait, then execute the same real fetch
	// successfully once the private registration HEAD is valid.
	if err := os.WriteFile(head, []byte(f.baseSHA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case <-fetchEntered:
	case <-ctx.Done():
		t.Fatal("base fetch failed to resume after writer release")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("protected owned reader provision: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("protected owned reader did not finish")
	}
	provisioned, err := m.Result("owned-reader")
	if err != nil {
		t.Fatal(err)
	}
	if provisioned.BaseSHA != tip {
		t.Fatalf("protected owned reader base=%s want=%s", provisioned.BaseSHA, tip)
	}
}

func TestBaseFetchTimeoutWhileParentWriterHeld(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("required real Git is unavailable")
	}
	f := newBranchGitFixture(t)
	unlock := worktree.AcquireParentLock(f.parent)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(unlock) }
	t.Cleanup(release)
	var fetches atomic.Int32
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 2 && args[2] == "fetch" {
			fetches.Add(1)
		}
		return f.run(ctx, name, args...)
	}
	m, err := worktree.NewManager(worktree.Options{
		ParentDir: filepath.Join(f.root, "workareas"), CommandRunner: runner,
		BaseFetchTimeout: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	_, err = m.Provision(ctx, worktree.ProvisionSpec{
		SessionID: "owned-timeout", Branch: "work/owned-timeout", BaseRef: "origin/release/next",
		Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: f.parent,
	})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, worktree.ErrBaseFetch) {
		t.Fatalf("held parent writer did not consume the existing base-fetch timeout: %v", err)
	}
	if fetches.Load() != 0 || worktree.BaseFetchFlightRegistered(f.parent, "release/next") {
		t.Fatalf("timed-out fetch reached Git or retained its flight: fetches=%d", fetches.Load())
	}
	release()
	deadline := time.Now().Add(time.Second)
	for worktree.ParentLockRegistered(f.parent) {
		if time.Now().After(deadline) {
			t.Fatal("canceled parent reader retained the gate after writer release")
		}
		select {
		case <-ctx.Done():
			t.Fatal("canceled reader cleanup exceeded its bound")
		case <-time.After(time.Millisecond):
		}
	}
}
