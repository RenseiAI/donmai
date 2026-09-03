package worktree_test

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

// The tests in this file pin the two coordination mechanisms that stand between
// concurrent session launches and the Git failure
// "cannot lock ref 'refs/remotes/origin/<ref>': is at X but expected Y":
//
//   - the single-flight base fetch, keyed by (canonical parent, ref); and
//   - the process-wide parent lock held across every parent-mutating git call.
//
// Each test names the production code it pins. Removing that code must turn the
// test RED — that, not the test's existence, is what makes the behaviour
// covered.

// --- helpers -----------------------------------------------------------------

// realGitRunner executes git for real. Tests wrap it to observe overlap.
func realGitRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	//nolint:gosec // test fixture runs the git binary selected by PATH.
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// cloneAt clones bare into a fresh directory named leaf and returns its path.
func cloneAt(t *testing.T, bare, leaf string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), leaf)
	//nolint:gosec // test fixture uses the git binary selected by PATH.
	cmd := exec.Command("git", "clone", "--branch", "main", bare, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone %s: %v\n%s", leaf, err, out)
	}
	return dst
}

// gitIn runs git in dir and fails the test on error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	//nolint:gosec // test fixture uses the git binary selected by PATH.
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// sameDir reports whether two path spellings denote one directory.
func sameDir(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Clean(r)
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}

// overlapCounter observes the maximum number of simultaneously-executing
// operations. A correctly locked critical section never exceeds one.
type overlapCounter struct {
	current atomic.Int64
	max     atomic.Int64
}

func (o *overlapCounter) enter() func() {
	cur := o.current.Add(1)
	for {
		prev := o.max.Load()
		if cur <= prev || o.max.CompareAndSwap(prev, cur) {
			break
		}
	}
	return func() { o.current.Add(-1) }
}

// --- B4: the single-flight base fetch ---------------------------------------

// TestBaseFetchCoalescesConcurrentSameRefProvisions pins invariant 1 of
// refreshBase: concurrent provisions of the same (canonical parent, ref) share
// exactly one `git fetch`. Delete the flight registration in refreshBase and
// every caller fetches for itself, which is the same-ref contention that fails
// real Git launches.
func TestBaseFetchCoalescesConcurrentSameRefProvisions(t *testing.T) {
	const sessions = 8
	dir, parent := t.TempDir(), filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int64
	var overlap overlapCounter
	release := make(chan struct{})
	runner := func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "fetch":
			fetches.Add(1)
			leave := overlap.enter()
			defer leave()
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "worktree":
			if err := os.MkdirAll(args[len(args)-2], 0o750); err != nil {
				return nil, err
			}
		case "rev-parse":
			return []byte("shared-tip\n"), nil
		}
		return nil, nil
	}
	m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	arrived := make(chan struct{}, sessions)
	errs := make(chan error, sessions)
	var wg sync.WaitGroup
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			arrived <- struct{}{}
			if _, provisionErr := m.Provision(context.Background(), worktree.ProvisionSpec{
				SessionID: fmt.Sprintf("coalesce-%d", i), Branch: fmt.Sprintf("session-%d", i),
				Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: parent, BaseRef: "origin/main",
			}); provisionErr != nil {
				errs <- provisionErr
			}
		}(i)
	}
	for range sessions {
		<-arrived
	}
	// Every caller is inside Provision and the elected fetch is blocked, so the
	// remaining callers can only join the in-flight fetch. The grace covers the
	// local filesystem work between entering Provision and reaching
	// refreshBase — a handful of syscalls — and is deliberately generous: a
	// late arrival would fetch for itself and fail this test spuriously.
	// maxConcurrentFetches below is the timing-independent half of the same
	// invariant, and stays 1 no matter when a caller arrives.
	time.Sleep(2 * time.Second)
	close(release)
	wg.Wait()
	close(errs)
	for provisionErr := range errs {
		t.Errorf("Provision: %v", provisionErr)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("git fetch invocations = %d for %d concurrent same-ref provisions, want 1", got, sessions)
	}
	if got := overlap.max.Load(); got != 1 {
		t.Fatalf("max concurrent fetches = %d, want 1", got)
	}
	if worktree.BaseFetchFlightRegistered(parent, "main") {
		t.Fatal("completed base fetch was left registered in the flight registry")
	}
}

// TestBaseFetchDoesNotSerializeDistinctRefs pins invariant 2 of refreshBase:
// distinct refs on one parent are deliberately allowed to fetch concurrently,
// because Git locks remote-tracking refs individually. Serializing the fetch
// per parent (for example by taking the parent lock around it) makes the
// barrier below unreachable and turns this test RED.
//
// The invariant was verified against real Git — 10 rounds of 6 concurrent
// distinct-ref fetches on one clone with every tip advancing, 0 failures in 60
// — and is pinned end-to-end by TestIntegrationConcurrentDistinctRefProvisions.
func TestBaseFetchDoesNotSerializeDistinctRefs(t *testing.T) {
	const sessions = 6
	dir, parent := t.TempDir(), filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	var fetches, inFlight atomic.Int64
	allConcurrent := make(chan struct{})
	var once sync.Once
	runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "fetch":
			fetches.Add(1)
			if inFlight.Add(1) == sessions {
				once.Do(func() { close(allConcurrent) })
			}
			// Released as soon as all six are in flight. The timeout only
			// applies when they never are — i.e. when the fetch has been
			// serialized per parent — and is long enough that a loaded machine
			// cannot fake that outcome.
			select {
			case <-allConcurrent:
			case <-time.After(3 * time.Second):
			}
			inFlight.Add(-1)
		case "worktree":
			if err := os.MkdirAll(args[len(args)-2], 0o750); err != nil {
				return nil, err
			}
		case "rev-parse":
			return []byte("distinct-tip\n"), nil
		}
		return nil, nil
	}
	m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, sessions)
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, provisionErr := m.Provision(context.Background(), worktree.ProvisionSpec{
				SessionID: fmt.Sprintf("distinct-%d", i), Branch: fmt.Sprintf("session-%d", i),
				Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: parent,
				BaseRef: fmt.Sprintf("origin/ref-%d", i),
			}); provisionErr != nil {
				errs <- provisionErr
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for provisionErr := range errs {
		t.Errorf("Provision: %v", provisionErr)
	}
	select {
	case <-allConcurrent:
	default:
		t.Fatalf("distinct-ref fetches never overlapped: %d fetches ran but the parent serialized them", fetches.Load())
	}
	if got := fetches.Load(); got != sessions {
		t.Fatalf("git fetch invocations = %d, want %d (one per distinct ref)", got, sessions)
	}
}

// --- B5: a leader's cancellation must not fail the waiters --------------------

// TestBaseFetchLeaderCancellationDoesNotFailWaiters pins the detached context
// in refreshBase: the shared fetch must not inherit the cancellation of
// whichever caller happened to elect it. Run the fetch on the leader's context
// instead — context.WithoutCancel removed — and the waiter, whose own context
// is never touched, fails with the leader's context error.
func TestBaseFetchLeaderCancellationDoesNotFailWaiters(t *testing.T) {
	dir, parent := t.TempDir(), filepath.Join(t.TempDir(), "parent")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int64
	release := make(chan struct{})
	runner := func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		switch args[2] {
		case "fetch":
			fetches.Add(1)
			select {
			case <-release:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "worktree":
			if err := os.MkdirAll(args[len(args)-2], 0o750); err != nil {
				return nil, err
			}
		case "rev-parse":
			return []byte("leader-tip\n"), nil
		}
		return nil, nil
	}
	m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	var wg sync.WaitGroup
	var leaderErr, waiterErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, leaderErr = m.Provision(leaderCtx, worktree.ProvisionSpec{
			SessionID: "leader", Branch: "leader", Strategy: worktree.StrategyWorktreeAdd,
			ParentRepoPath: parent, BaseRef: "origin/main",
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !worktree.BaseFetchFlightRegistered(parent, "main") {
		if time.Now().After(deadline) {
			t.Fatal("leader never registered a base fetch flight")
		}
		time.Sleep(time.Millisecond)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, waiterErr = m.Provision(context.Background(), worktree.ProvisionSpec{
			SessionID: "waiter", Branch: "waiter", Strategy: worktree.StrategyWorktreeAdd,
			ParentRepoPath: parent, BaseRef: "origin/main",
		})
	}()
	// Let the waiter join the in-flight fetch, then abandon the leader.
	time.Sleep(500 * time.Millisecond)
	cancelLeader()
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if waiterErr != nil {
		t.Fatalf("waiter with a healthy context failed because the leader cancelled: %v", waiterErr)
	}
	if leaderErr == nil || !errors.Is(leaderErr, context.Canceled) {
		t.Fatalf("leader error = %v, want its own context cancellation", leaderErr)
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("git fetch invocations = %d, want 1 shared fetch", got)
	}
}

// --- B2: the parent lock registry -------------------------------------------

// TestParentLockSerializesAddTeardownCleanup pins all three parent lock sites
// at once: `git worktree add` (provisionOnceWithReference), `git worktree
// remove` on teardown (teardownResult), and the conflict cleanup
// (cleanupConflict). The runner wraps real Git, counts overlapping
// parent-mutating invocations, and holds each one open long enough that any
// unlocked pair would be observed. Remove any one of the three
// acquireParentWorktreeLock calls and the observed maximum exceeds one.
func TestParentLockSerializesAddTeardownCleanup(t *testing.T) {
	requireGit(t)
	const sessions = 4
	bare := initBareRepo(t)
	parent := cloneAt(t, bare, "parent")

	var overlap overlapCounter
	var conflictInjected atomic.Bool
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) < 4 || args[0] != "-C" || args[2] != "worktree" || !sameDir(args[1], parent) {
			return realGitRunner(ctx, name, args...)
		}
		leave := overlap.enter()
		defer leave()
		time.Sleep(15 * time.Millisecond)
		if args[3] == "add" && conflictInjected.CompareAndSwap(false, true) {
			// Drive one provision down the retriable conflict path so that
			// cleanupConflict's lock site is exercised too.
			return []byte("fatal: destination is already checked out"), errors.New("worktree conflict")
		}
		return realGitRunner(ctx, name, args...)
	}
	m, err := worktree.NewManager(worktree.Options{
		ParentDir: t.TempDir(), CommandRunner: runner, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2*sessions)
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := fmt.Sprintf("locked-%d", i)
			if _, provisionErr := m.Provision(context.Background(), worktree.ProvisionSpec{
				SessionID: session, Branch: fmt.Sprintf("session-%d", i),
				Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: parent, BaseRef: "origin/main",
			}); provisionErr != nil {
				errs <- fmt.Errorf("provision %s: %w", session, provisionErr)
				return
			}
			if teardownErr := m.Teardown(context.Background(), session); teardownErr != nil {
				errs <- fmt.Errorf("teardown %s: %w", session, teardownErr)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for opErr := range errs {
		t.Errorf("%v", opErr)
	}
	if !conflictInjected.Load() {
		t.Fatal("conflict cleanup path was never exercised")
	}
	if got := overlap.max.Load(); got != 1 {
		t.Fatalf("max concurrent parent-mutating git invocations = %d, want 1", got)
	}
}

// TestParentLockKeyCanonical pins the canonical lock key: an absolute, a
// relative, and a symlinked spelling of one parent must collapse onto a single
// lock. Make canonicalParentPath return its argument unchanged and the
// spellings acquire different locks, so their git invocations overlap.
func TestParentLockKeyCanonical(t *testing.T) {
	requireGit(t)
	bare := initBareRepo(t)
	parent := cloneAt(t, bare, "parent")
	link := filepath.Join(t.TempDir(), "parent-link")
	if err := os.Symlink(parent, link); err != nil {
		t.Fatalf("symlink parent: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, parent)
	if err != nil {
		t.Fatal(err)
	}
	spellings := []string{parent, link, relative}

	var overlap overlapCounter
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) < 4 || args[0] != "-C" || args[2] != "worktree" || !sameDir(args[1], parent) {
			return realGitRunner(ctx, name, args...)
		}
		leave := overlap.enter()
		defer leave()
		time.Sleep(15 * time.Millisecond)
		return realGitRunner(ctx, name, args...)
	}
	m, err := worktree.NewManager(worktree.Options{
		ParentDir: t.TempDir(), CommandRunner: runner, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(spellings))
	for i, spelling := range spellings {
		wg.Add(1)
		go func(i int, spelling string) {
			defer wg.Done()
			if _, provisionErr := m.Provision(context.Background(), worktree.ProvisionSpec{
				SessionID: fmt.Sprintf("spelling-%d", i), Branch: fmt.Sprintf("spelling-%d", i),
				Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: spelling, BaseRef: "origin/main",
			}); provisionErr != nil {
				errs <- provisionErr
			}
		}(i, spelling)
	}
	wg.Wait()
	close(errs)
	for provisionErr := range errs {
		t.Errorf("Provision: %v", provisionErr)
	}
	if got := overlap.max.Load(); got != 1 {
		t.Fatalf("max concurrent parent-mutating git invocations across %d spellings of one parent = %d, want 1",
			len(spellings), got)
	}
}

// TestLockRegistryPrunes pins the reference counting in
// acquireParentWorktreeLock: the registry must not accumulate an entry per
// parent it has ever seen. Delete the refs==0 prune and the entry survives
// every release.
func TestLockRegistryPrunes(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "pruned-parent")
	for range 8 {
		release := worktree.AcquireParentLock(parent)
		if !worktree.ParentLockRegistered(parent) {
			t.Fatal("parent lock is not registered while held")
		}
		release()
		if worktree.ParentLockRegistered(parent) {
			t.Fatalf("parent lock registry retained an entry for %q after release", parent)
		}
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worktree.AcquireParentLock(parent)()
		}()
	}
	wg.Wait()
	if worktree.ParentLockRegistered(parent) {
		t.Fatalf("parent lock registry retained an entry for %q after concurrent release", parent)
	}
}

// --- receipt resolution failure ----------------------------------------------

// TestRevParseFailureTearsDownAndPropagates pins the teardown-on-verification-
// failure path. Delete the teardownResult call and the workarea survives a
// provision that returned an error; degrade the joined cleanup error back to %v
// and the cleanup failure stops being retrievable from the error chain.
func TestRevParseFailureTearsDownAndPropagates(t *testing.T) {
	errRemoveFailed := errors.New("worktree remove refused")

	newManager := func(t *testing.T, removeErr error) (*worktree.Manager, string, string) {
		t.Helper()
		dir, parent := t.TempDir(), filepath.Join(t.TempDir(), "parent")
		if err := os.MkdirAll(parent, 0o750); err != nil {
			t.Fatal(err)
		}
		runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
			switch args[2] {
			case "fetch":
				return nil, nil
			case "worktree":
				if args[3] == "remove" {
					return []byte("refused"), removeErr
				}
				if err := os.MkdirAll(args[len(args)-2], 0o750); err != nil {
					return nil, err
				}
			case "rev-parse":
				return []byte("fatal: not a git repository"), errors.New("rev-parse failed")
			}
			return nil, nil
		}
		m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner, RetryDelay: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		return m, dir, parent
	}

	t.Run("tears the workarea down", func(t *testing.T) {
		m, dir, parent := newManager(t, nil)
		_, err := m.Provision(context.Background(), worktree.ProvisionSpec{
			SessionID: "orphan", Branch: "main", Strategy: worktree.StrategyWorktreeAdd,
			ParentRepoPath: parent, BaseRef: "origin/main",
		})
		if !errors.Is(err, worktree.ErrBaseFetch) {
			t.Fatalf("error = %v, want typed base-fetch failure", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "orphan")); !os.IsNotExist(statErr) {
			t.Fatalf("workarea survived a failed receipt resolution: stat = %v", statErr)
		}
	})

	t.Run("propagates a failed teardown", func(t *testing.T) {
		m, _, parent := newManager(t, errRemoveFailed)
		_, err := m.Provision(context.Background(), worktree.ProvisionSpec{
			SessionID: "orphan", Branch: "main", Strategy: worktree.StrategyWorktreeAdd,
			ParentRepoPath: parent, BaseRef: "origin/main",
		})
		if !errors.Is(err, worktree.ErrBaseFetch) {
			t.Fatalf("error = %v, want typed base-fetch failure", err)
		}
		if !errors.Is(err, errRemoveFailed) {
			t.Fatalf("error = %v, want the cleanup failure retrievable from the chain", err)
		}
	})
}

// TestProvisionNormalizesBaseRefSpellings pins every accepted base-ref
// spelling onto the one ref that is fetched and the one start-point that is
// checked out. Drop any TrimPrefix in normalizeBaseRef and its row goes RED.
func TestProvisionNormalizesBaseRefSpellings(t *testing.T) {
	for _, test := range []struct {
		name string
		ref  string
	}{
		{name: "bare", ref: "main"},
		{name: "remote shorthand", ref: "origin/main"},
		{name: "local ref", ref: "refs/heads/main"},
		{name: "remote tracking ref", ref: "refs/remotes/origin/main"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, parent := t.TempDir(), filepath.Join(t.TempDir(), "parent")
			if err := os.MkdirAll(parent, 0o750); err != nil {
				t.Fatal(err)
			}
			var fetchRef, startPoint string
			runner := func(_ context.Context, _ string, args ...string) ([]byte, error) {
				switch args[2] {
				case "fetch":
					fetchRef = args[len(args)-1]
				case "worktree":
					startPoint = args[len(args)-1]
					if err := os.MkdirAll(args[len(args)-2], 0o750); err != nil {
						return nil, err
					}
				case "rev-parse":
					return []byte("normalized-tip\n"), nil
				}
				return nil, nil
			}
			m, err := worktree.NewManager(worktree.Options{ParentDir: dir, CommandRunner: runner, RetryDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Provision(context.Background(), worktree.ProvisionSpec{
				SessionID: "normalize", Branch: "session", Strategy: worktree.StrategyWorktreeAdd,
				ParentRepoPath: parent, BaseRef: test.ref,
			}); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			if fetchRef != "main" {
				t.Fatalf("fetched ref = %q, want %q", fetchRef, "main")
			}
			if startPoint != "origin/main" {
				t.Fatalf("worktree start-point = %q, want %q", startPoint, "origin/main")
			}
		})
	}
}

// --- real Git ----------------------------------------------------------------

// TestIntegrationConcurrentProvisionsAgainstStaleParent is the permanent
// regression test for the launch failure this change exists to fix: several
// sessions starting at once against a shared parent clone whose remote-tracking
// ref is behind. Without the single-flight fetch the concurrent same-ref
// fetches contend and Git fails all but one with
// "cannot lock ref 'refs/remotes/origin/main': is at X but expected Y".
func TestIntegrationConcurrentProvisionsAgainstStaleParent(t *testing.T) {
	requireGit(t)
	const sessions = 8
	bare := initBareRepo(t)
	parent := cloneAt(t, bare, "parent")
	advance := cloneAt(t, bare, "advance")

	gitIn(t, advance, "config", "user.email", "test@example.com")
	gitIn(t, advance, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(advance, "new.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, advance, "add", "new.txt")
	gitIn(t, advance, "commit", "-m", "advance origin past the parent clone")
	newTip := strings.TrimSpace(gitIn(t, advance, "rev-parse", "HEAD"))
	gitIn(t, advance, "push", "origin", "main")

	var fetches atomic.Int64
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 2 && args[2] == "fetch" {
			fetches.Add(1)
		}
		return realGitRunner(ctx, name, args...)
	}
	m, err := worktree.NewManager(worktree.Options{
		ParentDir: t.TempDir(), CommandRunner: runner, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, sessions)
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, provisionErr := m.Provision(context.Background(), worktree.ProvisionSpec{
				SessionID: fmt.Sprintf("stale-%d", i), Branch: fmt.Sprintf("session-%d", i),
				Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: parent, BaseRef: "origin/main",
			}); provisionErr != nil {
				errs <- fmt.Errorf("session %d: %w", i, provisionErr)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for provisionErr := range errs {
		t.Errorf("concurrent provision against a stale parent: %v", provisionErr)
	}
	// The assertion is the product invariant — every launch succeeds and lands
	// on the advanced tip — not the fetch count. Two same-ref fetches can only
	// run at once if coalescing is gone, and then they contend; how many
	// non-overlapping fetches ran is timing, not contract.
	if got := fetches.Load(); got < 1 {
		t.Fatalf("git fetch invocations = %d, want at least one base refresh", got)
	}
	for i := range sessions {
		result, resultErr := m.Result(fmt.Sprintf("stale-%d", i))
		if resultErr != nil {
			t.Fatalf("Result %d: %v", i, resultErr)
		}
		if result.BaseSHA != newTip {
			t.Fatalf("session %d based on %s, want the advanced tip %s", i, result.BaseSHA, newTip)
		}
	}
}

// TestIntegrationConcurrentDistinctRefProvisions pins the second half of the
// concurrency contract against real Git: distinct refs on one parent may be
// fetched concurrently. If a future Git ever contends across distinct
// remote-tracking refs, this test fails and the fetch must be serialized per
// parent instead.
func TestIntegrationConcurrentDistinctRefProvisions(t *testing.T) {
	requireGit(t)
	const sessions = 6
	bare := initBareRepo(t)
	parent := cloneAt(t, bare, "parent")
	advance := cloneAt(t, bare, "advance")
	gitIn(t, advance, "config", "user.email", "test@example.com")
	gitIn(t, advance, "config", "user.name", "test")

	// Publish the branches and let the parent see them, so the concurrent
	// fetches below are ref UPDATES — the contention shape, not first writes.
	for i := range sessions {
		gitIn(t, advance, "checkout", "-q", "-B", fmt.Sprintf("ref-%d", i), "main")
		if err := os.WriteFile(filepath.Join(advance, fmt.Sprintf("f%d.txt", i)), []byte("seed"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitIn(t, advance, "add", ".")
		gitIn(t, advance, "commit", "-m", fmt.Sprintf("seed ref-%d", i))
	}
	gitIn(t, advance, "push", "origin", "--all")
	gitIn(t, parent, "fetch", "origin")
	for i := range sessions {
		gitIn(t, advance, "checkout", "-q", fmt.Sprintf("ref-%d", i))
		if err := os.WriteFile(filepath.Join(advance, fmt.Sprintf("f%d.txt", i)), []byte("advanced"), 0o600); err != nil {
			t.Fatal(err)
		}
		gitIn(t, advance, "add", ".")
		gitIn(t, advance, "commit", "-m", fmt.Sprintf("advance ref-%d", i))
	}
	gitIn(t, advance, "push", "origin", "--all")

	var fetches atomic.Int64
	runner := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 2 && args[2] == "fetch" {
			fetches.Add(1)
		}
		return realGitRunner(ctx, name, args...)
	}
	m, err := worktree.NewManager(worktree.Options{
		ParentDir: t.TempDir(), CommandRunner: runner, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, sessions)
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, provisionErr := m.Provision(context.Background(), worktree.ProvisionSpec{
				SessionID: fmt.Sprintf("ref-session-%d", i), Branch: fmt.Sprintf("session-%d", i),
				Strategy: worktree.StrategyWorktreeAdd, ParentRepoPath: parent,
				BaseRef: fmt.Sprintf("origin/ref-%d", i),
			}); provisionErr != nil {
				errs <- fmt.Errorf("session %d: %w", i, provisionErr)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for provisionErr := range errs {
		t.Errorf("concurrent distinct-ref provision: %v", provisionErr)
	}
	if got := fetches.Load(); got != sessions {
		t.Fatalf("git fetch invocations = %d, want %d (one per distinct ref)", got, sessions)
	}
}
