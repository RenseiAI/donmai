package worktree_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// dehydrateBareRemote builds a single-commit fixture with a lockfile: a
// bare remote with a published main branch carrying tracked source and a
// lockfile the kit store selects on.
func dehydrateBareRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	dehydrateGit(t, "", "init", "--bare", "--quiet", remote)
	dehydrateGit(t, "", "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("published bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dehydrateGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "add", "tracked.txt", "pnpm-lock.yaml")
	dehydrateGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "initial")
	dehydrateGit(t, seed, "remote", "add", "origin", remote)
	dehydrateGit(t, seed, "push", "--quiet", "-u", "origin", "main")
	dehydrateGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote
}

func dehydrateGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", args...) //nolint:gosec // fixed test binary and temp-dir arguments
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// dehydrateNodeKit is the minimal kit-declared dependency store the pass
// classifies against: the pnpm manager selected by its lockfile, with
// node_modules as the regenerable installed tree.
func dehydrateNodeKit() kit.DependencyStoreView {
	return kit.DependencyStoreView{
		Manager:   "pnpm",
		Ecosystem: "node",
		Lockfiles: []string{"pnpm-lock.yaml"},
		Store: kit.DependencyStoreLayout{
			Sharing:   kit.DependencyStoreSharingContentAddressed,
			Integrity: kit.DependencyStoreIntegrityOnUse,
		},
		Commands: map[string]string{
			kit.DependencyStoreCommandFetch:   "pnpm fetch",
			kit.DependencyStoreCommandInstall: "pnpm install --offline --frozen-lockfile",
		},
		Snapshot: kit.DependencyStoreSnapshot{
			Installed: []string{"node_modules"},
		},
	}
}

// dehydrateStores projects kit views into the engine's minimal store
// input, mirroring the daemon's projection of validated entries.
func dehydrateStores(stores []kit.DependencyStoreView) []worktree.DehydrateStore {
	out := make([]worktree.DehydrateStore, 0, len(stores))
	for _, store := range stores {
		out = append(out, worktree.DehydrateStore{
			Manager:   store.Manager,
			Lockfiles: append([]string(nil), store.Lockfiles...),
			Installed: append([]string(nil), store.Snapshot.Installed...),
		})
	}
	return out
}

// dehydrateSessionID is the abandoned session owning the fixture root.
const dehydrateSessionID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

// dehydratePlantInstalled commits the ignore rule a kit-installed leaf
// carries for its installed tree, then plants the installed tree itself.
// Classification requires the git-ignored signal; without the committed
// rule the leaf is not a dehydration candidate.
func dehydratePlantInstalled(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("node_modules/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dehydrateGit(t, path, "add", ".gitignore")
	dehydrateGit(t, path, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "ignore installed tree")
	if err := os.MkdirAll(filepath.Join(path, "node_modules", ".pnpm"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "node_modules", ".pnpm", "lock.yaml"), []byte("installed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// dehydrateProvisionAbandoned provisions a legacy flat workarea and drops
// the owner without teardown, so the directory is retained in the exact
// sense the dehydration pass handles. mutate runs inside the checkout
// before abandonment.
func dehydrateProvisionAbandoned(t *testing.T, parent, remote, sessionID string, now func() time.Time, mutate func(path string)) string {
	t.Helper()
	owner, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	path, err := owner.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: sessionID,
		RepoURL:   remote,
		Strategy:  worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(path)
	}
	return path
}

// dehydrateClassify runs one pass and requires exactly one classification.
func dehydrateClassify(t *testing.T, manager *worktree.Manager, now time.Time) {
	t.Helper()
	report, err := manager.DehydratePreservedRoots(context.Background(), worktree.DehydratePolicy{
		Stores: dehydrateStores([]kit.DependencyStoreView{dehydrateNodeKit()}),
		Now:    now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Classified != 1 {
		t.Fatalf("classified = %d, want 1", report.Classified)
	}
	if len(report.Receipts) != 0 {
		t.Fatalf("receipts = %+v, want none before the dehydration age", report.Receipts)
	}
}

// A preserved root dehydrates after the 24-hour age: the first pass writes
// the install record, the second pass (past the age) removes exactly the
// recorded installed paths, leaves tracked source alone, and writes a
// receipt naming the policy and the measured bytes.
func TestDehydratePreservedRootRemovesRecordedPaths(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := dehydrateBareRemote(t)
	parent := t.TempDir()
	now := time.Now()
	path := dehydrateProvisionAbandoned(t, parent, remote, dehydrateSessionID, func() time.Time { return now }, func(path string) {
		dehydratePlantInstalled(t, path)
	})
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	dehydrateClassify(t, manager, now)

	record, err := manager.DehydrateInstallRecord(path)
	if err != nil {
		t.Fatalf("install record missing after classify: %v", err)
	}
	if len(record.Paths) != 1 || record.Paths[0] != "node_modules" {
		t.Fatalf("install record = %+v, want [node_modules]", record)
	}

	later := now.Add(25 * time.Hour)
	report, err := manager.DehydratePreservedRoots(context.Background(), worktree.DehydratePolicy{
		Stores: dehydrateStores([]kit.DependencyStoreView{dehydrateNodeKit()}),
		Now:    later,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 1 {
		t.Fatalf("receipts = %+v, want one dehydrate receipt", report.Receipts)
	}
	receipt := report.Receipts[0]
	if receipt.Disposition != worktree.DehydrateDispositionDehydrated {
		t.Fatalf("receipt = %+v, want dehydrated", receipt)
	}
	if receipt.Reason != worktree.DehydrateReasonAge {
		t.Fatalf("receipt = %+v, want the age policy named", receipt)
	}
	if len(receipt.Paths) != 1 || receipt.Paths[0] != "node_modules" {
		t.Fatalf("receipt paths = %v, want [node_modules]: exactly the recorded paths", receipt.Paths)
	}
	if report.BytesReclaimed != receipt.BytesReclaimed {
		t.Fatalf("report bytes %d != receipt bytes %d: the change is measured per transition", report.BytesReclaimed, receipt.BytesReclaimed)
	}
	if _, err := os.Stat(filepath.Join(path, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("recorded installed tree survives: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "tracked.txt")); err != nil {
		t.Fatalf("tracked source removed by dehydration: %v", err)
	}
	stored, err := manager.DehydrateInstallRecord(path)
	if err != nil {
		t.Fatalf("install record missing after dehydrate: %v", err)
	}
	if stored.Dehydration == nil || stored.Dehydration.Reason != worktree.DehydrateReasonAge ||
		stored.Dehydration.Disposition != worktree.DehydrateDispositionDehydrated ||
		stored.Dehydration.BytesReclaimed != receipt.BytesReclaimed {
		t.Fatalf("stored receipt = %+v, want the age policy and the measured bytes", stored.Dehydration)
	}
}

// Parked, leased and quarantined roots are untouched under pressure: the
// age rule fires for every candidate, but liveness keeps the guarded
// roots. Reverting the lease/quarantine check (treating a leased path as
// a candidate) turns this RED by dehydrating the leased root.
func TestDehydrateLeavesGuardedRootsUntouched(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := dehydrateBareRemote(t)
	parent := t.TempDir()
	now := time.Now()

	// The leased root: provisioned, preserved via a terminal lease, and
	// carrying an installed tree. The lease retains the exact path.
	leasedOwner, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	const leasedSessionID = "55555555-5555-4555-8555-555555555555"
	leasedPath, err := leasedOwner.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: leasedSessionID, RepoURL: remote, Strategy: worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	dehydratePlantInstalled(t, leasedPath)
	if _, err := leasedOwner.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: leasedSessionID, TerminalResultID: "tr_55555555555555555555555555555555",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "archive",
	}); err != nil {
		t.Fatal(err)
	}

	// The parked root: a second session whose terminal lease requested
	// release (return-to-pool disposition) but was never reaped. The path
	// stays retained — its disposition is still "return this root to its
	// session" — so the pass must not race the reaper.
	parkedOwner, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	const parkedSessionID = "66666666-6666-4666-8666-666666666666"
	parkedPath, err := parkedOwner.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: parkedSessionID, RepoURL: remote, Strategy: worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	dehydratePlantInstalled(t, parkedPath)
	if _, err := parkedOwner.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: parkedSessionID, TerminalResultID: "tr_66666666666666666666666666666666",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "return-to-pool",
	}); err != nil {
		t.Fatal(err)
	}

	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	// Only the abandoned control root classifies: the leased and parked
	// roots are guarded before classification, so no record is written
	// for either.
	controlPath := dehydrateProvisionAbandoned(t, parent, remote, "77777777-7777-4777-8777-777777777777", func() time.Time { return now }, func(path string) {
		dehydratePlantInstalled(t, path)
	})
	if _, err := manager.DehydratePreservedRoots(context.Background(), worktree.DehydratePolicy{
		Stores: dehydrateStores([]kit.DependencyStoreView{dehydrateNodeKit()}),
		Now:    now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.DehydrateInstallRecord(controlPath); err != nil {
		t.Fatalf("control root missing install record: %v", err)
	}
	for _, guarded := range []string{leasedPath, parkedPath} {
		if _, err := manager.DehydrateInstallRecord(guarded); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("guarded root %s classified: no record must exist (err=%v)", guarded, err)
		}
	}
	later := now.Add(25 * time.Hour)
	report, err := manager.DehydratePreservedRoots(context.Background(), worktree.DehydratePolicy{
		Stores: dehydrateStores([]kit.DependencyStoreView{dehydrateNodeKit()}),
		Now:    later,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 1 || report.Receipts[0].Root != controlPath {
		t.Fatalf("receipts = %+v, want exactly the control root: leased and parked stay", report.Receipts)
	}
	for _, guarded := range []string{leasedPath, parkedPath} {
		if _, err := os.Stat(filepath.Join(guarded, "node_modules")); err != nil {
			t.Fatalf("guarded root %s dehydrated: %v", guarded, err)
		}
	}
}

// Tracked source under an installed name is never dehydrated: git
// refuses the ignore signal for a directory holding tracked files
// (check-ignore reports nothing for node_modules when keep.js is
// committed beneath it), so classification declines, no install record
// is written, and the dehydration age changes nothing. The classify-time
// tracked-source filter and the removal-time index recheck cover the same
// invariant for shapes no live git state can produce through this binary
// (a path both ignored and tracked): they are defense-in-depth, mirroring
// the archive capture's tracked-source rule, and stay pinned by the
// directory-level behavior here.
func TestDehydrateKeepsTrackedSourceUnderInstalledNames(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	dehydrateGit(t, "", "init", "--bare", "--quiet", remote)
	dehydrateGit(t, "", "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("published bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(seed, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "node_modules", "keep.js"), []byte("tracked source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, ".gitignore"), []byte("node_modules/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dehydrateGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "add", "-f", "tracked.txt", "pnpm-lock.yaml", "node_modules/keep.js", ".gitignore")
	dehydrateGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "initial")
	dehydrateGit(t, seed, "remote", "add", "origin", remote)
	dehydrateGit(t, seed, "push", "--quiet", "-u", "origin", "main")
	dehydrateGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	parent := t.TempDir()
	now := time.Now()
	path := dehydrateProvisionAbandoned(t, parent, remote, dehydrateSessionID, func() time.Time { return now }, nil)
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	report, err := manager.DehydratePreservedRoots(context.Background(), worktree.DehydratePolicy{
		Stores: dehydrateStores([]kit.DependencyStoreView{dehydrateNodeKit()}),
		Now:    now.Add(25 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 0 || report.Classified != 0 {
		t.Fatalf("report = %+v, want no receipts and no classifications: tracked source is never dehydrated", report)
	}
	if _, err := os.Stat(filepath.Join(path, "node_modules", "keep.js")); err != nil {
		t.Fatalf("tracked installed-named source removed: %v", err)
	}
	if _, err := manager.DehydrateInstallRecord(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tracked root classified: no install record must exist (err=%v)", err)
	}
}

// A root with no applicable store classifies nothing: without a lockfile
// the kit store does not select the leaf, so no install record is written
// and nothing is removed past the age.
func TestDehydrateWithoutLockfileClassifiesNothing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	dehydrateGit(t, "", "init", "--bare", "--quiet", remote)
	dehydrateGit(t, "", "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("published bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dehydrateGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "add", "tracked.txt")
	dehydrateGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "initial")
	dehydrateGit(t, seed, "remote", "add", "origin", remote)
	dehydrateGit(t, seed, "push", "--quiet", "-u", "origin", "main")
	dehydrateGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")

	parent := t.TempDir()
	now := time.Now()
	path := dehydrateProvisionAbandoned(t, parent, remote, dehydrateSessionID, func() time.Time { return now }, func(path string) {
		if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("node_modules/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		dehydrateGit(t, path, "add", ".gitignore")
		dehydrateGit(t, path, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "ignore installed tree")
		if err := os.MkdirAll(filepath.Join(path, "node_modules"), 0o750); err != nil {
			t.Fatal(err)
		}
	})
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	report, err := manager.DehydratePreservedRoots(context.Background(), worktree.DehydratePolicy{
		Stores: dehydrateStores([]kit.DependencyStoreView{dehydrateNodeKit()}),
		Now:    now.Add(25 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 0 || report.Classified != 0 {
		t.Fatalf("report = %+v, want no receipts and no classifications without a lockfile", report)
	}
	if _, err := os.Stat(filepath.Join(path, "node_modules")); err != nil {
		t.Fatalf("unselected installed tree removed: %v", err)
	}
}

// A root that is not a git checkout is skipped without an install record:
// without a checkout there is no ignore/track signal the pass trusts.
func TestDehydrateKeepsUnprovableRoot(t *testing.T) {
	parent := t.TempDir()
	now := time.Now()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "unprovable-root")
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}
	report, err := manager.DehydratePreservedRoots(context.Background(), worktree.DehydratePolicy{
		Stores: dehydrateStores([]kit.DependencyStoreView{dehydrateNodeKit()}),
		Now:    now.Add(25 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 0 || report.Classified != 0 {
		t.Fatalf("report = %+v, want no receipts and no classifications for an unprovable root", report)
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules")); err != nil {
		t.Fatalf("unprovable root touched: %v", err)
	}
}
