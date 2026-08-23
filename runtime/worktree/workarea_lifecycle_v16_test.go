package worktree_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

func TestNestedProvisionCrashBoundariesRecoverFromDurableAcquisition(t *testing.T) {
	for _, boundary := range []string{"root-created", "mid-clone", "pre-record"} {
		boundary := boundary
		t.Run(boundary, func(t *testing.T) {
			parent := t.TempDir()
			crash := errors.New("simulated process loss")
			manager, err := worktree.NewManager(worktree.Options{
				ParentDir: parent, CommandRunner: nestedCloneStub(),
				ProvisionHook: func(stage string, _ workarea.AcquisitionRecord) error {
					if stage == boundary {
						return crash
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Provision(context.Background(), nestedSpec("crash-"+boundary, nil)); !errors.Is(err, crash) {
				t.Fatalf("fault boundary error = %v", err)
			}
			recovered, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub()})
			if err != nil {
				t.Fatal(err)
			}
			path, err := recovered.Provision(context.Background(), nestedSpec("crash-"+boundary, nil))
			if err != nil {
				t.Fatalf("reprovision after recovery: %v", err)
			}
			layout, err := recovered.Layout("crash-" + boundary)
			if err != nil || filepath.Dir(path) != layout.Root.String() {
				t.Fatalf("recovered layout = %+v, path=%q, err=%v", layout, path, err)
			}
		})
	}
}

func TestLegacyFlatProvisionLeavesNegotiatedAuthoritiesDormant(t *testing.T) {
	parent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Provision(context.Background(), worktree.ProvisionSpec{
		SessionID: "legacy-dormant", RepoURL: "https://example.test/repo.git", Strategy: worktree.StrategyClone,
	}); err != nil {
		t.Fatal(err)
	}
	for _, negotiatedState := range []string{".workarea-acquisitions", ".workarea-seeds"} {
		if _, err := os.Stat(filepath.Join(parent, negotiatedState)); !os.IsNotExist(err) {
			t.Fatalf("legacy flat provision activated %s: %v", negotiatedState, err)
		}
	}
}

func TestSharedParticipantUsesParentRootAcrossOwnerReleaseAndRestart(t *testing.T) {
	parent := t.TempDir()
	owner, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	ownerSpec := nestedSpec("owner", nil)
	ownerPath, err := owner.Provision(context.Background(), ownerSpec)
	if err != nil {
		t.Fatal(err)
	}
	ownerLayout, err := owner.Layout("owner")
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := workarea.ReadDeclaration(ownerLayout.Root)
	if err != nil {
		t.Fatal(err)
	}
	child, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: "child"})
	if err != nil {
		t.Fatal(err)
	}
	childSpec := nestedSpec("child", nil)
	childSpec.Mode = worktree.ModeShared
	childSpec.ParentWorkareaID = declaration.WorkareaID
	childSpec.RepositoryFilter = &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "corpus"}
	childSpec.RepositoryDeclaration.Select = childSpec.RepositoryFilter
	childPath, err := child.Provision(context.Background(), childSpec)
	if err != nil {
		t.Fatal(err)
	}
	childLayout, err := child.Layout("child")
	if err != nil {
		t.Fatal(err)
	}
	if childLayout.Root != ownerLayout.Root || childPath == ownerPath || filepath.Base(childPath) != "corpus" {
		t.Fatalf("shared paths owner=%q child=%q ownerRoot=%q childRoot=%q", ownerPath, childPath, ownerLayout.Root, childLayout.Root)
	}
	if err := owner.Teardown(context.Background(), "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ownerLayout.Root.String()); err != nil {
		t.Fatalf("owner release removed participant root: %v", err)
	}
	restartedChild, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: "child"})
	if err != nil {
		t.Fatal(err)
	}
	restartedLayout, err := restartedChild.Layout("child")
	if err != nil || restartedLayout != childLayout {
		t.Fatalf("restarted participant layout = %+v, %v; want %+v", restartedLayout, err, childLayout)
	}
	if err := restartedChild.Teardown(context.Background(), "child"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ownerLayout.Root.String()); !os.IsNotExist(err) {
		t.Fatalf("last participant did not release whole root: %v", err)
	}
}

func TestCacheSeedMaterializesDistinctSessionGenerations(t *testing.T) {
	const firstSession = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const secondSession = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	parent := t.TempDir()
	var cloneInvocations [][]string
	runner := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "clone" {
			cloneInvocations = append(cloneInvocations, append([]string(nil), args...))
			return nil, os.MkdirAll(filepath.Join(args[len(args)-1], ".git"), 0o750)
		}
		if name == "git" && len(args) >= 2 && args[len(args)-2] == "rev-parse" {
			return []byte("deadbeef\n"), nil
		}
		return nil, nil
	}
	first, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: runner, RestoreSessionID: firstSession})
	if err != nil {
		t.Fatal(err)
	}
	firstSpec := nestedSpec(firstSession, nil)
	firstSpec.CacheSeedID = "shared-seed"
	if _, err := first.Provision(context.Background(), firstSpec); err != nil {
		t.Fatal(err)
	}
	firstLayout, _ := first.Layout(firstSession)
	firstDeclaration, err := workarea.ReadDeclaration(firstLayout.Root)
	if err != nil {
		t.Fatal(err)
	}
	firstGeneration, err := first.AcquisitionRecord(firstDeclaration.WorkareaID)
	if err != nil {
		t.Fatal(err)
	}
	firstLease, err := first.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: firstSession, TerminalResultID: "tr_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "return-to-pool",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := first.ClaimTerminalLeaseExecution(context.Background(), workarea.ExecutionClaimSpec{
		LeaseID: firstLease.LeaseID, SessionID: firstLease.SessionID, TerminalResultID: firstLease.TerminalResultID,
		WorkareaID: firstLease.WorkareaID, InvocationID: "11111111-1111-4111-8111-111111111111", ClaimID: "22222222-2222-4222-8222-222222222222",
	})
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	if _, err := first.AcknowledgeTerminalResult(context.Background(), workarea.TerminalResultAcknowledgement{
		SchemaVersion: workarea.TerminalLeaseAcknowledgementSchemaV1, Acknowledged: true,
		LeaseID: firstLease.LeaseID, SessionID: firstLease.SessionID, TerminalResultID: firstLease.TerminalResultID,
		WorkareaID: firstLease.WorkareaID, InvocationID: claim.Claim.InvocationID, ClaimID: claim.Claim.ClaimID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ReapExpiredTerminalLeases(context.Background(), 1, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(firstLayout.Root.String()); !os.IsNotExist(err) {
		t.Fatalf("return-to-pool retained first session root: %v", err)
	}

	second, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: runner, RestoreSessionID: secondSession})
	if err != nil {
		t.Fatal(err)
	}
	secondSpec := nestedSpec(secondSession, nil)
	secondSpec.CacheSeedID = "shared-seed"
	if _, err := second.Provision(context.Background(), secondSpec); err != nil {
		t.Fatal(err)
	}
	secondLayout, _ := second.Layout(secondSession)
	secondDeclaration, err := workarea.ReadDeclaration(secondLayout.Root)
	if err != nil {
		t.Fatal(err)
	}
	secondGeneration, err := second.AcquisitionRecord(secondDeclaration.WorkareaID)
	if err != nil {
		t.Fatal(err)
	}
	secondLease, err := second.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: secondSession, TerminalResultID: "tr_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "destroy",
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstLayout.Root == secondLayout.Root || firstDeclaration.WorkareaID == secondDeclaration.WorkareaID ||
		firstDeclaration.AcquisitionID == secondDeclaration.AcquisitionID || firstLease.LeaseID == secondLease.LeaseID ||
		firstGeneration.AccountingID == secondGeneration.AccountingID || firstGeneration.ObservationCursorID == secondGeneration.ObservationCursorID {
		t.Fatalf("cache seed reused session generation identity:\nfirst=%+v\nsecond=%+v", firstGeneration, secondGeneration)
	}
	if firstDeclaration.SessionID == secondDeclaration.SessionID {
		t.Fatal("cache seed reused declaration session identity")
	}
	seed, err := second.CacheSeedRecord("shared-seed")
	if err != nil || seed.PhysicalBytes <= 0 {
		t.Fatalf("separate cache seed accounting = %+v, %v", seed, err)
	}
	if _, err := os.Stat(filepath.Join(parent, ".workarea-seeds", "shared-seed")); err != nil {
		t.Fatalf("session return removed reusable seed: %v", err)
	}
	if _, err := os.Stat(secondLayout.Root.String()); err != nil {
		t.Fatalf("first generation return affected second root: %v", err)
	}
	referencedSeed := false
	for _, invocation := range cloneInvocations {
		if slices.Contains(invocation, "--reference") && slices.Contains(invocation, "--dissociate") {
			referencedSeed = true
			break
		}
	}
	if !referencedSeed {
		t.Fatalf("generation clones did not actually materialize from the seed: %#v", cloneInvocations)
	}
}

func TestNestedTerminalArchiveCapturesRootBeforeAuthorizedDisposal(t *testing.T) {
	const sessionID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	parent := t.TempDir()
	archived := false
	manager, err := worktree.NewManager(worktree.Options{
		ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: sessionID,
		ArchiveRoot: func(_ context.Context, spec worktree.ArchiveRootSpec) error {
			for _, path := range []string{
				filepath.Join(spec.WorkareaRoot, ".workarea", "declaration.json"),
				filepath.Join(spec.WorkareaRoot, "web", ".git"),
				filepath.Join(spec.WorkareaRoot, "corpus", ".git"),
			} {
				if _, err := os.Stat(path); err != nil {
					return err
				}
			}
			if filepath.Clean(spec.SelectedPath) != filepath.Join(spec.WorkareaRoot, "web") || spec.AcquisitionID == "" {
				return errors.New("archive did not receive the complete root identity")
			}
			archived = true
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Provision(context.Background(), nestedSpec(sessionID, nil)); err != nil {
		t.Fatal(err)
	}
	layout, _ := manager.Layout(sessionID)
	lease, err := manager.AcquireTerminalLease(context.Background(), workarea.AcquireSpec{
		SessionID: sessionID, TerminalResultID: "tr_cccccccccccccccccccccccccccccccc",
		Policy: workarea.DefaultLeasePolicy(), ReleaseRequested: true, ReleaseDisposition: "archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := manager.ClaimTerminalLeaseExecution(context.Background(), workarea.ExecutionClaimSpec{
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, InvocationID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", ClaimID: "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AcknowledgeTerminalResult(context.Background(), workarea.TerminalResultAcknowledgement{
		SchemaVersion: workarea.TerminalLeaseAcknowledgementSchemaV1, Acknowledged: true,
		LeaseID: lease.LeaseID, SessionID: lease.SessionID, TerminalResultID: lease.TerminalResultID,
		WorkareaID: lease.WorkareaID, InvocationID: claim.Claim.InvocationID, ClaimID: claim.Claim.ClaimID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ReapExpiredTerminalLeases(context.Background(), 1, time.Second); err != nil {
		t.Fatal(err)
	}
	if !archived {
		t.Fatal("whole-root archive provider was not called")
	}
	if _, err := os.Stat(layout.Root.String()); !os.IsNotExist(err) {
		t.Fatalf("archived source root survived authorized disposal: %v", err)
	}
}

func TestCacheSeedGenerationsAreReallyIndependentOfSeedObjects(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(t.TempDir(), "source")
	git := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...) //nolint:gosec // fixed git binary with test-owned fixture arguments
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.test", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.test")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, output)
		}
	}
	git("init", "-b", "main", source)
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("independent"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("-C", source, "add", "tracked.txt")
	git("-C", source, "commit", "-m", "seed")
	declaration := workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{{
			Source: workarea.RepositorySource{Repository: source, Ref: "main"}, Name: "source",
			Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
		}},
	}
	capabilities := workarea.ExecutorWorkareaCapabilities{MultiRepositoryWorkareaProtocols: []workarea.Protocol{workarea.ProtocolSessionRootV1}}
	manager, err := worktree.NewManager(worktree.Options{ParentDir: parent})
	if err != nil {
		t.Fatal(err)
	}
	provision := func(sessionID string) string {
		t.Helper()
		path, err := manager.Provision(t.Context(), worktree.ProvisionSpec{
			SessionID: sessionID, RepoURL: source, SourceRef: "main", Strategy: worktree.StrategyClone,
			RepositoryDeclaration: &declaration, ExecutorCapabilities: capabilities, CacheSeedID: "real-seed",
		})
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	first := provision("real-seed-a")
	second := provision("real-seed-b")
	for _, generation := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(generation, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
			t.Fatalf("generation retained seed alternates %q: %v", generation, err)
		}
	}
	seedPath := filepath.Join(parent, ".workarea-seeds", "real-seed")
	if err := os.Rename(seedPath, seedPath+"-offline"); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []string{first, second} {
		command := exec.Command("git", "-C", generation, "fsck", "--full") //nolint:gosec // fixed git binary and test-owned generation path
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("independent generation fsck %q: %v (%s)", generation, err, output)
		}
		body, err := os.ReadFile(filepath.Join(generation, "tracked.txt"))
		if err != nil || string(body) != "independent" {
			t.Fatalf("independent generation content %q: %q, %v", generation, body, err)
		}
	}
}

func TestRootTransactionSerializesSharedJoinLeaseAndTeardown(t *testing.T) {
	t.Run("join-versus-lease", func(t *testing.T) {
		const ownerID = "11111111-aaaa-4aaa-8aaa-111111111111"
		const childID = "22222222-bbbb-4bbb-8bbb-222222222222"
		parent := t.TempDir()
		leaseEntered := make(chan struct{})
		allowLease := make(chan struct{})
		owner, err := worktree.NewManager(worktree.Options{
			ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: ownerID,
			LifecycleHook: func(stage string) {
				if stage == "lease-before-commit" {
					close(leaseEntered)
					<-allowLease
				}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ownerSpec := nestedSpec(ownerID, nil)
		if _, err := owner.Provision(t.Context(), ownerSpec); err != nil {
			t.Fatal(err)
		}
		layout, _ := owner.Layout(ownerID)
		declaration, _ := workarea.ReadDeclaration(layout.Root)
		child, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: childID})
		if err != nil {
			t.Fatal(err)
		}
		childSpec := nestedSpec(childID, nil)
		childSpec.Mode, childSpec.ParentWorkareaID = worktree.ModeShared, declaration.WorkareaID
		leaseErr := make(chan error, 1)
		joinErr := make(chan error, 1)
		go func() {
			_, err := owner.AcquireTerminalLease(t.Context(), workarea.AcquireSpec{
				SessionID: ownerID, TerminalResultID: "tr_11111111111111111111111111111111", Policy: workarea.DefaultLeasePolicy(),
			})
			leaseErr <- err
		}()
		<-leaseEntered
		go func() {
			_, err := child.Provision(t.Context(), childSpec)
			joinErr <- err
		}()
		select {
		case err := <-joinErr:
			t.Fatalf("join crossed an uncommitted root lease: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(allowLease)
		leaseResult, joinResult := <-leaseErr, <-joinErr
		if leaseResult != nil || joinResult == nil {
			t.Fatalf("join/lease atomic outcomes lease=%v join=%v", leaseResult, joinResult)
		}
	})

	t.Run("teardown-versus-lease", func(t *testing.T) {
		const ownerID = "33333333-cccc-4ccc-8ccc-333333333333"
		parent := t.TempDir()
		first, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: ownerID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := first.Provision(t.Context(), nestedSpec(ownerID, nil)); err != nil {
			t.Fatal(err)
		}
		layout, _ := first.Layout(ownerID)
		second, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub(), RestoreSessionID: ownerID})
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		leaseErr := make(chan error, 1)
		teardownErr := make(chan error, 1)
		go func() {
			<-start
			_, err := second.AcquireTerminalLease(t.Context(), workarea.AcquireSpec{
				SessionID: ownerID, TerminalResultID: "tr_33333333333333333333333333333333", Policy: workarea.DefaultLeasePolicy(),
			})
			leaseErr <- err
		}()
		go func() {
			<-start
			teardownErr <- first.Teardown(t.Context(), ownerID)
		}()
		close(start)
		leaseResult, teardownResult := <-leaseErr, <-teardownErr
		if teardownResult != nil {
			t.Fatal(teardownResult)
		}
		if leaseResult == nil {
			if _, err := os.Stat(layout.Root.String()); err != nil {
				t.Fatalf("committed lease crossed teardown removal: %v", err)
			}
		} else if _, err := os.Stat(layout.Root.String()); !os.IsNotExist(err) {
			t.Fatalf("failed lease left teardown root ambiguous: %v", err)
		}
	})
}

func TestSharedJoinRequiresExactSourceRefSparsePathsAndFilter(t *testing.T) {
	parent := t.TempDir()
	owner, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub()})
	if err != nil {
		t.Fatal(err)
	}
	ownerSpec := nestedSpec("exact-owner", nil)
	ownerSpec.RepositoryDeclaration.Repositories[1].Source.Paths = []string{"docs"}
	if _, err := owner.Provision(t.Context(), ownerSpec); err != nil {
		t.Fatal(err)
	}
	layout, _ := owner.Layout("exact-owner")
	durable, _ := workarea.ReadDeclaration(layout.Root)
	tests := []struct {
		name   string
		mutate func(*worktree.ProvisionSpec)
	}{
		{"source", func(spec *worktree.ProvisionSpec) {
			spec.RepositoryDeclaration.Repositories[1].Source.Repository = "https://example.test/other.git"
		}},
		{"ref", func(spec *worktree.ProvisionSpec) { spec.RepositoryDeclaration.Repositories[1].Source.Ref = "other" }},
		{"sparse-paths", func(spec *worktree.ProvisionSpec) {
			spec.RepositoryDeclaration.Repositories[1].Source.Paths = []string{"other"}
		}},
		{"filter", func(spec *worktree.ProvisionSpec) {
			spec.RepositoryFilter = &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "corpus"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, err := worktree.NewManager(worktree.Options{ParentDir: parent, CommandRunner: nestedCloneStub()})
			if err != nil {
				t.Fatal(err)
			}
			spec := nestedSpec("invalid-"+test.name, nil)
			spec.RepositoryDeclaration.Repositories[1].Source.Paths = []string{"docs"}
			spec.Mode, spec.ParentWorkareaID = worktree.ModeShared, durable.WorkareaID
			test.mutate(&spec)
			if _, err := manager.Provision(t.Context(), spec); err == nil {
				t.Fatalf("shared join accepted mismatched %s", test.name)
			}
		})
	}
}
