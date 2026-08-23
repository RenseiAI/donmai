package workarea

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

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
