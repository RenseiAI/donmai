package workarea

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const acquisitionProcessHelperEnv = "DONMAI_TEST_ACQUISITION_PROCESS_HELPER"

func waitForAcquisitionTestFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", filepath.Base(path))
}

func waitForAcquisitionTestRootFile(root *os.Root, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := root.Lstat(name); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", name)
}

func TestAcquisitionStoreProcessHelper(t *testing.T) {
	if os.Getenv(acquisitionProcessHelperEnv) != "1" {
		return
	}
	parent := os.Getenv("DONMAI_TEST_ACQUISITION_PARENT")
	suffix := os.Getenv("DONMAI_TEST_ACQUISITION_SUFFIX")
	if !filepath.IsAbs(parent) || (suffix != "a" && suffix != "b") {
		t.Fatal("invalid acquisition process-helper identity")
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parentRoot.Close() }()
	store, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := parentRoot.WriteFile("ready-"+suffix, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForAcquisitionTestRootFile(parentRoot, "process-start", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	acquisition, beginErr := store.Begin("same-process-session", "wa_process_"+suffix, RootPath(filepath.Join(parent, "process-root-"+suffix)), "repo", "")
	outcome := "failure"
	if beginErr == nil {
		outcome = "success"
	}
	if err := parentRoot.WriteFile("result-"+suffix, []byte(outcome), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForAcquisitionTestRootFile(parentRoot, "process-release", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if acquisition != nil {
		_ = store.Abandon(acquisition.Record.AcquisitionID)
	}
}

func TestAcquisitionStoreEnforcesSessionUniquenessAcrossProcesses(t *testing.T) {
	parent := t.TempDir()
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatal(err)
	}
	start := filepath.Join(parent, "process-start")
	release := filepath.Join(parent, "process-release")
	type child struct {
		command *exec.Cmd
		ready   string
		result  string
		output  bytes.Buffer
	}
	children := make([]*child, 0, 2)
	t.Cleanup(func() {
		_ = os.WriteFile(release, []byte("release"), 0o600)
		for _, current := range children {
			if current.command.Process != nil && current.command.ProcessState == nil {
				_ = current.command.Process.Kill()
			}
		}
	})
	for _, suffix := range []string{"a", "b"} {
		current := &child{
			ready:  filepath.Join(parent, "ready-"+suffix),
			result: filepath.Join(parent, "result-"+suffix),
		}
		current.command = exec.Command(os.Args[0], "-test.run=^TestAcquisitionStoreProcessHelper$") //nolint:gosec // current test binary with fixed helper selector
		current.command.Env = append(os.Environ(),
			acquisitionProcessHelperEnv+"=1",
			"DONMAI_TEST_ACQUISITION_PARENT="+parent,
			"DONMAI_TEST_ACQUISITION_SUFFIX="+suffix,
		)
		current.command.Stdout, current.command.Stderr = &current.output, &current.output
		if err := current.command.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, current)
	}
	for _, current := range children {
		if err := waitForAcquisitionTestFile(current.ready, 10*time.Second); err != nil {
			t.Fatalf("helper not ready: %v (%s)", err, current.output.String())
		}
	}
	if err := os.WriteFile(start, []byte("start"), 0o600); err != nil {
		t.Fatal(err)
	}
	successes, failures := 0, 0
	for _, current := range children {
		if err := waitForAcquisitionTestFile(current.result, 10*time.Second); err != nil {
			t.Fatalf("helper produced no result: %v (%s)", err, current.output.String())
		}
		body, err := os.ReadFile(current.result)
		if err != nil {
			t.Fatal(err)
		}
		switch string(body) {
		case "success":
			successes++
		case "failure":
			failures++
		default:
			t.Fatalf("unknown helper outcome %q", body)
		}
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, current := range children {
		if err := current.command.Wait(); err != nil {
			t.Fatalf("helper failed: %v (%s)", err, current.output.String())
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("cross-process same-session outcomes successes=%d failures=%d", successes, failures)
	}
}

func acquisitionTestDeclaration(t *testing.T) NormalizedDeclaration {
	t.Helper()
	normalized, err := (RepositoryDeclarationV1{
		Protocol: ProtocolSessionRootV1,
		Repositories: []DeclaredRepositoryV1{{
			Source: RepositorySource{Repository: "https://example.test/repo.git", Ref: "main"},
			Name:   "repo", Role: RepositoryRolePrimary, Authority: RepositoryMutable,
		}},
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}

func TestOpenExistingAcquisitionStoreKeepsMissingLegacyParentDormant(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-worktree-parent")
	store, found, err := OpenExistingAcquisitionStore(missing, nil)
	if err != nil || found || store != nil {
		t.Fatalf("missing legacy parent = (%v, %v, %v), want (nil, false, nil)", store, found, err)
	}
}

func TestAcquisitionStoreEnforcesCrossProcessSessionUniqueness(t *testing.T) {
	parent := t.TempDir()
	first, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		acquisition *Acquisition
		err         error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for index, store := range []*AcquisitionStore{first, second} {
		suffix, store := []string{"a", "b"}[index], store
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			acquisition, err := store.Begin("same-session", "wa_unique_"+suffix, RootPath(filepath.Join(parent, "root-"+suffix)), "repo", "")
			outcomes <- outcome{acquisition, err}
		}()
	}
	close(start)
	wait.Wait()
	close(outcomes)
	successes, failures := 0, 0
	for result := range outcomes {
		if result.err == nil {
			successes++
			if err := first.Abandon(result.acquisition.Record.AcquisitionID); err != nil {
				_ = second.Abandon(result.acquisition.Record.AcquisitionID)
			}
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("cross-process same-session outcomes successes=%d failures=%d", successes, failures)
	}
}

func TestAcquisitionStoreRefusesOwnerAsParticipantAndStoreSwap(t *testing.T) {
	parent := t.TempDir()
	store, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	acquisition, err := store.Begin("owner", "wa_owner", RootPath(filepath.Join(parent, "owner-root")), "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	committed := commitAcquisitionForTest(t, store, acquisition)
	if _, err := store.JoinShared(committed.WorkareaID, committed.SessionID, "repo"); err == nil {
		t.Fatal("root owner joined again as a participant")
	}
	second, err := store.Begin("other-owner", "wa_other_owner", RootPath(filepath.Join(parent, "other-owner-root")), "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	secondCommitted := commitAcquisitionForTest(t, store, second)
	peer, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.JoinShared(committed.WorkareaID, "one-child-session", "repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.JoinShared(secondCommitted.WorkareaID, "one-child-session", "repo"); err == nil {
		t.Fatal("shared session joined two durable roots across store handles")
	}
	storePath := filepath.Join(parent, acquisitionStoreDirName)
	if err := os.Rename(storePath, storePath+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(storePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordForWorkareaID(committed.WorkareaID); err == nil {
		t.Fatal("pinned acquisition store accepted a replacement directory")
	}
	if _, _, err := OpenExistingAcquisitionStore(parent, nil); err == nil {
		t.Fatal("replacement acquisition store escaped the durable parent anchor after restart")
	}
}

func TestAcquisitionStoreRecoversCrashBeforeFirstRecord(t *testing.T) {
	parent := t.TempDir()
	store, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	const claim = ".claim-wac_crash_before_record"
	if err := store.root.Mkdir(claim, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAcquisitionStore(parent, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.root.Lstat(claim); !os.IsNotExist(err) {
		t.Fatalf("unpublished empty claim poisoned recovery: %v", err)
	}
}

func commitAcquisitionForTest(t *testing.T, store *AcquisitionStore, acquisition *Acquisition) AcquisitionRecord {
	t.Helper()
	declaration := acquisitionTestDeclaration(t)
	if err := os.Mkdir(filepath.Join(acquisition.StagingRoot.String(), "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	record := NewDeclarationRecord(
		acquisition.Record.SessionID, acquisition.Record.WorkareaID, declaration,
		map[string]string{"repo": "deadbeef"}, acquisition.Record.AcquisitionID,
	)
	if err := WriteDeclaration(context.Background(), acquisition.StagingRoot, record); err != nil {
		t.Fatal(err)
	}
	committed, err := store.Commit(acquisition.Record.AcquisitionID)
	if err != nil {
		t.Fatal(err)
	}
	return committed
}

func TestAcquisitionRecoveryDoesNotReclaimLiveProvisioningRoot(t *testing.T) {
	parent := t.TempDir()
	first, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	final := RootPath(filepath.Join(parent, "wa-live"))
	acquisition, err := first.Begin("session-live", "wa_live", final, "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(acquisition.StagingRoot.String(), "mid-clone")
	if err := os.WriteFile(marker, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("concurrent recovery reclaimed a live provisioner: %v", err)
	}
	if record, err := second.RecordForWorkareaID("wa_live"); err != nil || record.State != AcquisitionProvisioning {
		t.Fatalf("live record = %+v, %v", record, err)
	}
	if err := first.Abandon(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	third, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(acquisition.StagingRoot.String()); !os.IsNotExist(err) {
		t.Fatalf("dead provisioner's proved staging root survived recovery: %v", err)
	}
	recovered, err := third.readRecord(acquisition.Record.AcquisitionID)
	if err != nil || recovered.State != AcquisitionAborted {
		t.Fatalf("recovered record = %+v, %v", recovered, err)
	}
}

func TestAcquisitionRecoveryNeverDeletesUnprovenFinalRoot(t *testing.T) {
	parent := t.TempDir()
	store, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	final := RootPath(filepath.Join(parent, "wa-unproven"))
	acquisition, err := store.Begin("session-unproven", "wa_unproven", final, "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Abandon(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(final.String(), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(final.String(), "operator-data")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	recoveredStore, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := recoveredStore.readRecord(acquisition.Record.AcquisitionID)
	if err != nil || record.State != AcquisitionQuarantined {
		t.Fatalf("unproven final classification = %+v, %v", record, err)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "preserve" {
		t.Fatalf("unproven final root was deleted or changed: %q, %v", body, err)
	}
}

func TestAcquisitionRecoveryAdoptsAtomicPublishBeforeReadyRecord(t *testing.T) {
	parent := t.TempDir()
	store, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	final := RootPath(filepath.Join(parent, "wa-published"))
	acquisition, err := store.Begin("session-published", "wa_published", final, "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	declaration := acquisitionTestDeclaration(t)
	if err := os.Mkdir(filepath.Join(acquisition.StagingRoot.String(), "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteDeclaration(context.Background(), acquisition.StagingRoot, NewDeclarationRecord(
		"session-published", "wa_published", declaration, nil, acquisition.Record.AcquisitionID,
	)); err != nil {
		t.Fatal(err)
	}
	stagingInfo, err := os.Lstat(acquisition.StagingRoot.String())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := fileIdentity(stagingInfo)
	if err != nil {
		t.Fatal(err)
	}
	record := acquisition.Record
	record.RootIdentity = identity
	if err := store.writeRecord(record); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(acquisition.StagingRoot.String(), final.String()); err != nil {
		t.Fatal(err)
	}
	if err := store.Abandon(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	recoveredRecord, err := recovered.RecordForWorkareaID("wa_published")
	if err != nil || recoveredRecord.State != AcquisitionReady || recoveredRecord.RootIdentity != identity {
		t.Fatalf("atomic publish recovery = %+v, %v", recoveredRecord, err)
	}
	if _, err := os.Stat(filepath.Join(final.String(), "repo")); err != nil {
		t.Fatalf("proved published root was not retained: %v", err)
	}
}

func TestAcquisitionRecoveryFinishesProvedPrivateDisposal(t *testing.T) {
	parent := t.TempDir()
	store, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	final := RootPath(filepath.Join(parent, "wa-release"))
	acquisition, err := store.Begin("session-release", "wa_release", final, "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	committed := commitAcquisitionForTest(t, store, acquisition)
	private := filepath.Join(parent, acquisitionStoreDirName, committed.AcquisitionID, acquisitionReleasedRootName)
	if err := os.Rename(final.String(), private); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := recovered.readRecord(committed.AcquisitionID)
	if err != nil || record.State != AcquisitionReleased {
		t.Fatalf("private disposal recovery = %+v, %v", record, err)
	}
	if _, err := os.Stat(private); !os.IsNotExist(err) {
		t.Fatalf("proved private disposal root survived: %v", err)
	}
}

func releasedAcquisitionForRestore(t *testing.T, parent string) (*AcquisitionStore, AcquisitionRecord) {
	t.Helper()
	store, err := NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	acquisition, err := store.Begin("restore-session", "wa_restore", RootPath(filepath.Join(parent, "wa-restore")), "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	committed := commitAcquisitionForTest(t, store, acquisition)
	if err := store.BindArchive(committed.AcquisitionID, committed.WorkareaID, committed.SessionID, "archive", "sha256:digest"); err != nil {
		t.Fatal(err)
	}
	if err := store.RemovePublishedRoot(committed.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	released, err := store.RecordForAcquisitionID(committed.AcquisitionID)
	if err != nil {
		t.Fatal(err)
	}
	return store, released
}

func populateRestoreStage(t *testing.T, restore RestoreAcquisition) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(restore.StagingRoot.String(), "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	declaration := acquisitionTestDeclaration(t)
	if err := WriteDeclaration(t.Context(), restore.StagingRoot, NewDeclarationRecord(
		restore.Record.SessionID, restore.Record.WorkareaID, declaration, nil, restore.Record.AcquisitionID,
	)); err != nil {
		t.Fatal(err)
	}
}

func TestAcquisitionRestoreCrashRecoveryAndNoReplace(t *testing.T) {
	for _, stage := range []string{"pre-copy", "mid-copy"} {
		t.Run(stage, func(t *testing.T) {
			parent := t.TempDir()
			store, released := releasedAcquisitionForRestore(t, parent)
			restore, err := store.BeginRestore(released.AcquisitionID, released.WorkareaID, released.SessionID, "archive", "sha256:digest")
			if err != nil {
				t.Fatal(err)
			}
			if stage == "mid-copy" {
				if err := os.WriteFile(filepath.Join(restore.StagingRoot.String(), "partial"), []byte("partial"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Abandon(released.AcquisitionID); err != nil {
				t.Fatal(err)
			}
			recovered, err := NewAcquisitionStore(parent, nil)
			if err != nil {
				t.Fatal(err)
			}
			record, err := recovered.RecordForAcquisitionID(released.AcquisitionID)
			if err != nil || record.State != AcquisitionReleased {
				t.Fatalf("%s restore recovery = %+v, %v", stage, record, err)
			}
			if _, err := os.Stat(restore.StagingRoot.String()); !os.IsNotExist(err) {
				t.Fatalf("%s restore stage survived: %v", stage, err)
			}
		})
	}

	t.Run("post-publish-pre-ready", func(t *testing.T) {
		parent := t.TempDir()
		store, released := releasedAcquisitionForRestore(t, parent)
		restore, err := store.BeginRestore(released.AcquisitionID, released.WorkareaID, released.SessionID, "archive", "sha256:digest")
		if err != nil {
			t.Fatal(err)
		}
		populateRestoreStage(t, restore)
		stageInfo, err := os.Lstat(restore.StagingRoot.String())
		if err != nil {
			t.Fatal(err)
		}
		identity, err := fileIdentity(stageInfo)
		if err != nil {
			t.Fatal(err)
		}
		record := restore.Record
		record.RootIdentity = identity
		if err := store.writeRecord(record); err != nil {
			t.Fatal(err)
		}
		if err := renameNoReplace(restore.StagingRoot.String(), record.FinalRoot); err != nil {
			t.Fatal(err)
		}
		if err := store.Abandon(released.AcquisitionID); err != nil {
			t.Fatal(err)
		}
		recovered, err := NewAcquisitionStore(parent, nil)
		if err != nil {
			t.Fatal(err)
		}
		recoveredRecord, err := recovered.RecordForAcquisitionID(record.AcquisitionID)
		if err != nil || recoveredRecord.State != AcquisitionReady || recoveredRecord.RootIdentity != identity {
			t.Fatalf("post-publish restore recovery = %+v, %v", recoveredRecord, err)
		}
	})

	t.Run("live-mid-copy", func(t *testing.T) {
		parent := t.TempDir()
		store, released := releasedAcquisitionForRestore(t, parent)
		restore, err := store.BeginRestore(released.AcquisitionID, released.WorkareaID, released.SessionID, "archive", "sha256:digest")
		if err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(restore.StagingRoot.String(), "live-copy")
		if err := os.WriteFile(marker, []byte("live"), 0o600); err != nil {
			t.Fatal(err)
		}
		concurrent, err := NewAcquisitionStore(parent, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("concurrent recovery reclaimed a live restore stage: %v", err)
		}
		record, err := concurrent.RecordForAcquisitionID(released.AcquisitionID)
		if err != nil || record.State != AcquisitionRestoring {
			t.Fatalf("live restore record = %+v, %v", record, err)
		}
		if err := store.Abandon(released.AcquisitionID); err != nil {
			t.Fatal(err)
		}
		recovered, err := NewAcquisitionStore(parent, nil)
		if err != nil {
			t.Fatal(err)
		}
		recoveredRecord, err := recovered.RecordForAcquisitionID(released.AcquisitionID)
		if err != nil || recoveredRecord.State != AcquisitionReleased {
			t.Fatalf("abandoned live restore recovery = %+v, %v", recoveredRecord, err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("abandoned restore stage survived recovery: %v", err)
		}
	})

	t.Run("no-replace", func(t *testing.T) {
		parent := t.TempDir()
		store, released := releasedAcquisitionForRestore(t, parent)
		restore, err := store.BeginRestore(released.AcquisitionID, released.WorkareaID, released.SessionID, "archive", "sha256:digest")
		if err != nil {
			t.Fatal(err)
		}
		populateRestoreStage(t, restore)
		if err := os.Mkdir(released.FinalRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		unprovedIdentity, err := os.Stat(released.FinalRoot)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.CommitRestore(released.AcquisitionID); !errors.Is(err, ErrAcquisitionRootOccupied) {
			t.Fatalf("restore collision = %v", err)
		}
		currentIdentity, err := os.Stat(released.FinalRoot)
		if err != nil || !os.SameFile(unprovedIdentity, currentIdentity) {
			t.Fatalf("restore replaced unproved destination identity: %v", err)
		}
		if err := store.AbortRestore(released.AcquisitionID); err != nil {
			t.Fatal(err)
		}
		quarantined, err := store.RecordForAcquisitionID(released.AcquisitionID)
		if err != nil || quarantined.State != AcquisitionQuarantined {
			t.Fatalf("colliding restore classification = %+v, %v", quarantined, err)
		}
	})
}
