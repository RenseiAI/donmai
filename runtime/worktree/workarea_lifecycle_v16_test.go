package worktree_test

import (
	"context"
	"errors"
	"os"
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
		if slices.Contains(invocation, "--reference-if-able") && slices.Contains(invocation, "--dissociate") {
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
