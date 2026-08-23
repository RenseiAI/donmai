package workarea

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSeedStoreRecoversAndAccountsCrashLeftStage(t *testing.T) {
	parent := t.TempDir()
	store, err := NewSeedStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim := seedClaimRecord{
		SchemaVersion: seedClaimSchemaV1, ClaimID: "wseedclaim_fixture", SeedID: "seed-fixture",
		StageLeaf: ".seed-seed-fixture-crash", CreatedAt: time.Now().UTC(),
	}
	if err := store.writeStoreJSON(seedClaimFileName, claim); err != nil {
		t.Fatal(err)
	}
	if err := store.root.Mkdir(claim.StageLeaf, 0o700); err != nil {
		t.Fatal(err)
	}
	stageRoot, err := store.root.OpenRoot(claim.StageLeaf)
	if err != nil {
		t.Fatal(err)
	}
	if err := stageRoot.WriteFile("partial", []byte("crash-left"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = stageRoot.Close()
	recovered, err := NewSeedStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(parent, seedStoreDirName, claim.StageLeaf)); !os.IsNotExist(err) {
		t.Fatalf("crash-left stage survived recovery: %v", err)
	}
	recoveries, err := recovered.Recoveries()
	if err != nil || len(recoveries) != 1 || recoveries[0].ClaimID != claim.ClaimID || recoveries[0].PhysicalBytes <= 0 {
		t.Fatalf("seed recovery accounting = %+v, %v", recoveries, err)
	}
	if _, err := recovered.root.Lstat(seedClaimFileName); !os.IsNotExist(err) {
		t.Fatalf("durable claim survived completed recovery: %v", err)
	}
}
