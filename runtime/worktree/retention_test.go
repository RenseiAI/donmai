package worktree_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func retentionBareRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	retentionGit(t, "", "init", "--bare", "--quiet", remote)
	retentionGit(t, "", "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("published bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	retentionGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "add", "tracked.txt")
	retentionGit(t, seed, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "initial")
	retentionGit(t, seed, "remote", "add", "origin", remote)
	retentionGit(t, seed, "push", "--quiet", "-u", "origin", "main")
	// Point the bare remote's HEAD at the published branch before any
	// clone: without it a clone checks out nothing and the retained root
	// holds no commits.
	retentionGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote
}

func retentionGit(t *testing.T, dir string, args ...string) {
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

// retentionExpiryManager builds the expiry manager over a parent whose
// roots were provisioned by an earlier manager: the earlier manager is
// dropped without teardown, so its directories are retained in the exact
// sense the pass handles — directories left behind after their session
// ended. Teardown cannot produce this fixture: it removes the directory
// for unleased sessions.
func retentionExpiryManager(t *testing.T, parent string, now func() time.Time) *worktree.Manager {
	t.Helper()
	manager, err := worktree.NewManager(worktree.Options{
		ParentDir: parent,
		Now:       now,
		ArchiveRoot: func(_ context.Context, spec worktree.ArchiveRootSpec) error {
			// The retention archive path captures the root into a
			// scratch archive marker: the test asserts archive-vs-
			// destroy routing, not the daemon's archive format.
			marker := filepath.Join(parent, "archive-markers", spec.WorkareaID)
			if err := os.MkdirAll(filepath.Dir(marker), 0o750); err != nil {
				return err
			}
			return os.WriteFile(marker, []byte(spec.WorkareaRoot), 0o600)
		},
		RetentionArchiveRoot: filepath.Join(parent, "archives"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func retentionProvisionAbandoned(t *testing.T, parent, remote, sessionID string, now func() time.Time, mutate func(path string)) string {
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
	// Drop the owner without teardown: the directory stays, the session
	// is gone. The expiry manager below adopts no live session.
	return path
}

func retentionAgePolicy() worktree.RetentionPolicy {
	return worktree.RetentionPolicy{MaxAgeDays: 14, ArchiveMaxAgeDays: -1}
}

// retentionAgeRoot backdates a retained root so the age rule fires: the
// root directory and every top-level entry get the old timestamp. Only
// the root directory would leave fresh checkout entries behind, and the
// pass orders by the youngest top-level entry.
func retentionAgeRoot(t *testing.T, path string, now time.Time) {
	t.Helper()
	old := now.Add(-15 * 24 * time.Hour)
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Chtimes(filepath.Join(path, entry.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// A clean root at the published HEAD holds no unpushed work, so age expiry
// destroys it.
func TestRetentionExpiresCleanRootByDestroying(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := retentionBareRemote(t)
	parent := t.TempDir()
	now := time.Now()
	path := retentionProvisionAbandoned(t, parent, remote, "11111111-1111-4111-8111-111111111111", func() time.Time { return now }, nil)
	manager := retentionExpiryManager(t, parent, func() time.Time { return now })
	retentionAgeRoot(t, path, now)
	report, err := manager.ExpireRetainedRoots(context.Background(), retentionAgePolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 1 {
		t.Fatalf("receipts = %+v, want one destroy receipt", report.Receipts)
	}
	receipt := report.Receipts[0]
	if receipt.Disposition != worktree.RetentionDestroyed || receipt.Reason != worktree.RetentionReasonAge {
		t.Fatalf("receipt = %+v, want destroyed by age", receipt)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired root still exists: %v", err)
	}
}

// An uncommitted change to a tracked file is unpushed work: the root is
// archived, never destroyed.
func TestRetentionArchivesRootWithUncommittedChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := retentionBareRemote(t)
	parent := t.TempDir()
	now := time.Now()
	path := retentionProvisionAbandoned(t, parent, remote, "22222222-2222-4222-8222-222222222222", func() time.Time { return now }, func(path string) {
		if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("dirty bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	manager := retentionExpiryManager(t, parent, func() time.Time { return now })
	retentionAgeRoot(t, path, now)
	report, err := manager.ExpireRetainedRoots(context.Background(), retentionAgePolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 1 {
		t.Fatalf("receipts = %+v, want one archive receipt", report.Receipts)
	}
	if report.Receipts[0].Disposition != worktree.RetentionArchived {
		t.Fatalf("receipt = %+v, want archived, never destroyed", report.Receipts[0])
	}
	entries, err := os.ReadDir(filepath.Join(parent, "archive-markers"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("archive markers = %+v err = %v, want one archive capture", entries, err)
	}
}

// An untracked file git does not ignore is unpushed work: archive, never
// destroy.
func TestRetentionArchivesRootWithUntrackedFile(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := retentionBareRemote(t)
	parent := t.TempDir()
	now := time.Now()
	path := retentionProvisionAbandoned(t, parent, remote, "33333333-3333-4333-8333-333333333333", func() time.Time { return now }, func(path string) {
		if err := os.WriteFile(filepath.Join(path, "notes.txt"), []byte("untracked bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	manager := retentionExpiryManager(t, parent, func() time.Time { return now })
	retentionAgeRoot(t, path, now)
	report, err := manager.ExpireRetainedRoots(context.Background(), retentionAgePolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 1 || report.Receipts[0].Disposition != worktree.RetentionArchived {
		t.Fatalf("receipts = %+v, want one archive receipt", report.Receipts)
	}
}

// A local commit unreachable from remote-tracking refs is unpushed work:
// archive, never destroy.
func TestRetentionArchivesRootWithUnpushedCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := retentionBareRemote(t)
	parent := t.TempDir()
	now := time.Now()
	path := retentionProvisionAbandoned(t, parent, remote, "44444444-4444-4444-8444-444444444444", func() time.Time { return now }, func(path string) {
		if err := os.WriteFile(filepath.Join(path, "tracked.txt"), []byte("local commit bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		retentionGit(t, path, "add", "tracked.txt")
		retentionGit(t, path, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "local")
	})
	manager := retentionExpiryManager(t, parent, func() time.Time { return now })
	retentionAgeRoot(t, path, now)
	report, err := manager.ExpireRetainedRoots(context.Background(), retentionAgePolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 1 || report.Receipts[0].Disposition != worktree.RetentionArchived {
		t.Fatalf("receipts = %+v, want one archive receipt", report.Receipts)
	}
}

// A leased root is untouched under pressure: the age rule fires, but the
// liveness guard keeps the root.
func TestRetentionLeavesLeasedRootUntouched(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := retentionBareRemote(t)
	parent := t.TempDir()
	now := time.Now()
	owner, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "55555555-5555-4555-8555-555555555555"
	path, err := owner.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: sessionID, RepoURL: remote, Strategy: worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: sessionID, TerminalResultID: "tr_55555555555555555555555555555555",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "archive",
	}); err != nil {
		t.Fatal(err)
	}
	retentionAgeRoot(t, path, now)
	manager := retentionExpiryManager(t, parent, func() time.Time { return now })
	report, err := manager.ExpireRetainedRoots(context.Background(), retentionAgePolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 0 {
		t.Fatalf("receipts = %+v, want none: leased roots are untouched", report.Receipts)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("leased root expired: %v", err)
	}
}

// A root the probe cannot read is kept: without a checkout there is no age
// signal the pass trusts, and destroying on an unreadable directory could
// delete work the probe could not see.
func TestRetentionKeepsUnprovableRoot(t *testing.T) {
	parent := t.TempDir()
	now := time.Now()
	manager := retentionExpiryManager(t, parent, func() time.Time { return now })
	root := filepath.Join(parent, "unprovable-root")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	retentionAgeRoot(t, root, now)
	report, err := manager.ExpireRetainedRoots(context.Background(), retentionAgePolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 0 {
		t.Fatalf("receipts = %+v, want none: unprovable roots are kept", report.Receipts)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("unprovable root expired: %v", err)
	}
}

// A quarantined root is untouched under pressure: the acquisition
// authority proved no cleanup is authorized, so the pass must not dispose
// of it. The fixture abandons a generation mid-provision and plants a
// clean clone at its final root: recovery quarantines the record (the
// unproven directory fails identity verification), and the quarantined
// root stays even though no lease retains it and the checkout is clean.
func TestRetentionLeavesQuarantinedRootUntouched(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := retentionBareRemote(t)
	parent := t.TempDir()
	now := time.Now()
	owner, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	acquisitions, err := owner.AcquisitionStoreForTest()
	if err != nil {
		t.Fatal(err)
	}
	final := workarea.RootPath(filepath.Join(parent, "wa-quarantined"))
	acquisition, err := acquisitions.Begin(
		"99999999-9999-4999-8999-999999999999", "wa_quarantined", final, "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate process loss mid-provision: release the owner lock
	// without committing, then plant an unproven checkout at the final
	// root. The expiry manager is a fresh process: its acquisition
	// store runs recovery on open, which quarantines the generation —
	// the directory proves no authorized identity.
	if err := acquisitions.Abandon(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	retentionGit(t, "", "clone", "--quiet", remote, final.String())
	fresh, _, err := workarea.OpenExistingAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == nil {
		t.Fatal("existing acquisition store not found after abandon")
	}
	quarantined, err := fresh.RecordForAcquisitionID(acquisition.Record.AcquisitionID)
	if err != nil || quarantined.State != workarea.AcquisitionQuarantined {
		t.Fatalf("recovery classification = %+v, %v, want quarantined", quarantined, err)
	}
	retentionAgeRoot(t, final.String(), now)
	manager := retentionExpiryManager(t, parent, func() time.Time { return now })
	report, err := manager.ExpireRetainedRoots(context.Background(), retentionAgePolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 0 {
		t.Fatalf("receipts = %+v, want none: quarantined roots are untouched", report.Receipts)
	}
	if _, err := os.Stat(final.String()); err != nil {
		t.Fatalf("quarantined root expired: %v", err)
	}
}

// A parked root is untouched under pressure: a release-pending terminal
// lease still owns the exact path (its disposition is "return this root
// to its session" until the reaper runs), so the liveness guard keeps it.
// The fixture holds the lease at release-pending by requesting release
// without reaping: RequestReleasePath marks release-requested while the
// path stays retained.
func TestRetentionLeavesParkedRootUntouched(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := retentionBareRemote(t)
	parent := t.TempDir()
	now := time.Now()
	owner, err := worktree.NewManager(worktree.Options{ParentDir: parent, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "88888888-8888-4888-8888-888888888888"
	path, err := owner.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: sessionID, RepoURL: remote, Strategy: worktree.StrategyClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: sessionID, TerminalResultID: "tr_88888888888888888888888888888888",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "archive",
	}); err != nil {
		t.Fatal(err)
	}
	retentionAgeRoot(t, path, now)
	manager := retentionExpiryManager(t, parent, func() time.Time { return now })
	report, err := manager.ExpireRetainedRoots(context.Background(), retentionAgePolicy(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) != 0 {
		t.Fatalf("receipts = %+v, want none: parked roots are untouched", report.Receipts)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("parked root expired: %v", err)
	}
}

// Archives past their maximum age drop with their own receipt; younger
// archives stay.
func TestRetentionDropsExpiredArchives(t *testing.T) {
	parent := t.TempDir()
	now := time.Now()
	archiveRoot := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{
		ParentDir: parent, Now: func() time.Time { return now },
		RetentionArchiveRoot: archiveRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeArchive := func(id string, created time.Time) {
		t.Helper()
		dir := filepath.Join(archiveRoot, id)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		body := `{"id":"` + id + `","createdAt":"` + created.UTC().Format(time.RFC3339Nano) + `"}`
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeArchive("old-archive", now.Add(-31*24*time.Hour))
	writeArchive("fresh-archive", now.Add(-time.Hour))
	// The exact-boundary archive (age == maxAge) stays: the drop fires
	// only past the maximum age, not on it.
	writeArchive("boundary-archive", now.Add(-30*24*time.Hour))
	var dropped []string
	dropper := func(_ context.Context, id, dir string) error {
		dropped = append(dropped, id)
		return os.RemoveAll(dir)
	}
	report, err := manager.ExpireRetainedRoots(context.Background(), worktree.RetentionPolicy{
		MaxAgeDays: -1, ArchiveMaxAgeDays: 30,
	}, dropper)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != "old-archive" {
		t.Fatalf("dropped = %v, want [old-archive]", dropped)
	}
	if len(report.Receipts) != 1 || report.Receipts[0].Disposition != worktree.RetentionArchiveDropped ||
		report.Receipts[0].Reason != worktree.RetentionReasonArchiveAge {
		t.Fatalf("receipts = %+v, want one archive-dropped receipt", report.Receipts)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "fresh-archive")); err != nil {
		t.Fatalf("fresh archive dropped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "boundary-archive")); err != nil {
		t.Fatalf("exact-boundary archive dropped: %v", err)
	}
}

// The drop scan resolves against the configured archive root: with
// divergent roots the pass drops from the configured directory, never
// the default beside the worktree parent.
func TestRetentionDropsFromConfiguredArchiveRoot(t *testing.T) {
	parent := t.TempDir()
	now := time.Now()
	configured := t.TempDir()
	decoy := filepath.Join(filepath.Dir(filepath.Clean(parent)), "workareas")
	manager, err := worktree.NewManager(worktree.Options{
		ParentDir: parent, Now: func() time.Time { return now },
		RetentionArchiveRoot: configured,
	})
	if err != nil {
		t.Fatal(err)
	}
	writeRetentionArchive := func(root, id string, created time.Time) {
		t.Helper()
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		body := `{"id":"` + id + `","createdAt":"` + created.UTC().Format(time.RFC3339Nano) + `"}`
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeRetentionArchive(configured, "configured-old", now.Add(-31*24*time.Hour))
	var dropped []string
	dropper := func(_ context.Context, id, dir string) error {
		dropped = append(dropped, id+"="+dir)
		return os.RemoveAll(dir)
	}
	report, err := manager.ExpireRetainedRoots(context.Background(), worktree.RetentionPolicy{
		MaxAgeDays: -1, ArchiveMaxAgeDays: 30,
	}, dropper)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != "configured-old="+filepath.Join(configured, "configured-old") {
		t.Fatalf("dropped = %v, want the configured-root archive with its directory", dropped)
	}
	if len(report.Receipts) != 1 || report.Receipts[0].Disposition != worktree.RetentionArchiveDropped {
		t.Fatalf("receipts = %+v, want one archive-dropped receipt", report.Receipts)
	}
	if _, err := os.Stat(decoy); !os.IsNotExist(err) {
		t.Fatalf("drop scan touched the default archive root %q: %v", decoy, err)
	}
}

// Disk pressure expires least recently used first: with a budget under
// the combined size, the older root goes and the newer stays.
func TestRetentionDiskPressureExpiresLeastRecentlyUsedFirst(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	remote := retentionBareRemote(t)
	parent := t.TempDir()
	now := time.Now()
	oldPath := retentionProvisionAbandoned(t, parent, remote, "66666666-6666-4666-8666-666666666666", func() time.Time { return now }, nil)
	newPath := retentionProvisionAbandoned(t, parent, remote, "77777777-7777-4777-8777-777777777777", func() time.Time { return now }, nil)
	older := now.Add(-2 * time.Hour)
	newer := now.Add(-time.Hour)
	if err := os.Chtimes(oldPath, older, older); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, newer, newer); err != nil {
		t.Fatal(err)
	}
	manager := retentionExpiryManager(t, parent, func() time.Time { return now })
	report, err := manager.ExpireRetainedRoots(context.Background(), worktree.RetentionPolicy{
		MaxAgeDays: -1, MaxDiskBytes: 1, ArchiveMaxAgeDays: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Receipts) == 0 {
		t.Fatalf("no receipts under pressure: %+v", report)
	}
	if report.Receipts[0].Reason != worktree.RetentionReasonDiskPressure {
		t.Fatalf("first receipt = %+v, want disk-pressure", report.Receipts[0])
	}
	if report.Receipts[0].Root != oldPath {
		t.Fatalf("first victim = %q, want oldest %q", report.Receipts[0].Root, oldPath)
	}
}
