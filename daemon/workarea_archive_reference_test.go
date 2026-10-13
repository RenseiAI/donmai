package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

func commitFlatArchiveForReference(t *testing.T, workareaID, sessionID string) (*WorkareaArchiveRegistry, WorkareaRootArchiveSpec) {
	t.Helper()
	archiveRoot := t.TempDir()
	flat := filepath.Join(t.TempDir(), "legacy")
	if err := os.MkdirAll(filepath.Join(flat, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flat, "file"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot})
	spec := WorkareaRootArchiveSpec{
		WorkareaID: workareaID, SessionID: sessionID, WorkareaRoot: flat, SelectedPath: flat,
		TerminalResultID: "tr_11111111111111111111111111111111",
	}
	return registry, spec
}

func TestCommitArchiveRootReturnsReferenceOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	registry, spec := commitFlatArchiveForReference(t, "wa_11111111111111111111111111111111", "11111111-1111-4111-8111-111111111111")
	reference, err := registry.CommitArchiveRoot(t.Context(), spec)
	if err != nil {
		t.Fatalf("CommitArchiveRoot: %v", err)
	}
	if reference == nil {
		t.Fatal("committed archive returned no recovery reference")
	}
	if err := reference.Validate(); err != nil {
		t.Fatalf("reference invalid: %v", err)
	}
	if reference.ArchiveID != "wa_11111111111111111111111111111111" || reference.SessionID != "11111111-1111-4111-8111-111111111111" ||
		reference.WorkareaID != "wa_11111111111111111111111111111111" || reference.TerminalResultID != "tr_11111111111111111111111111111111" {
		t.Fatalf("reference identities = %+v", reference)
	}
	if reference.ArchiveDigest == "" {
		t.Fatal("reference carries no digest")
	}
	manifest, err := registry.readManifest("wa_11111111111111111111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.TreeDigest != reference.ArchiveDigest {
		t.Fatalf("reference digest %q != committed manifest %q", reference.ArchiveDigest, manifest.TreeDigest)
	}
	// Exact duplicate delivery is idempotent and reproduces the same reference.
	again, err := registry.CommitArchiveRoot(t.Context(), spec)
	if err != nil {
		t.Fatalf("duplicate commit: %v", err)
	}
	if again == nil || !again.IdentityEqual(*reference) {
		t.Fatalf("duplicate reference = %+v, want %+v", again, reference)
	}
}

func TestCommitArchiveRootWithoutTerminalResultCommitsSilently(t *testing.T) {
	t.Parallel()
	registry, spec := commitFlatArchiveForReference(t, "wa_22222222222222222222222222222222", "11111111-1111-4111-8111-111111111111")
	spec.TerminalResultID = ""
	reference, err := registry.CommitArchiveRoot(t.Context(), spec)
	if err != nil {
		t.Fatalf("CommitArchiveRoot: %v", err)
	}
	if reference != nil {
		t.Fatalf("legacy commit must not invent a reference: %+v", reference)
	}
	if _, err := registry.readManifest("wa_22222222222222222222222222222222"); err != nil {
		t.Fatalf("archive did not commit: %v", err)
	}
}

func TestCommitArchiveRootRefusesConflictingIdentity(t *testing.T) {
	t.Parallel()
	registry, spec := commitFlatArchiveForReference(t, "wa_33333333333333333333333333333333", "11111111-1111-4111-8111-111111111111")
	if _, err := registry.CommitArchiveRoot(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	conflicting := spec
	conflicting.SessionID = "22222222-2222-4222-8222-222222222222"
	if _, err := registry.CommitArchiveRoot(t.Context(), conflicting); err == nil {
		t.Fatal("conflicting session identity accepted")
	}
}

func TestArchiveInspectProjectsReference(t *testing.T) {
	t.Parallel()
	registry, spec := commitFlatArchiveForReference(t, "wa_44444444444444444444444444444444", "11111111-1111-4111-8111-111111111111")
	if _, err := registry.CommitArchiveRoot(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	wa, err := registry.GetV1("wa_44444444444444444444444444444444")
	if err != nil {
		t.Fatal(err)
	}
	ref := wa.ArchiveReference
	if ref == nil {
		t.Fatal("inspect projection carries no archive reference")
	}
	if ref.SchemaVersion != afclient.ArchiveReferenceSchemaV1 || ref.ArchiveID != "wa_44444444444444444444444444444444" ||
		ref.SessionID != "11111111-1111-4111-8111-111111111111" || ref.WorkareaID != "wa_44444444444444444444444444444444" || ref.ArchiveDigest == "" {
		t.Fatalf("inspect reference = %+v", ref)
	}
	manifest, err := registry.readManifest("wa_44444444444444444444444444444444")
	if err != nil {
		t.Fatal(err)
	}
	if ref.ArchiveDigest != manifest.TreeDigest {
		t.Fatalf("inspect digest %q != manifest %q", ref.ArchiveDigest, manifest.TreeDigest)
	}
	if _, err := workarea.CanonicalBytes(struct {
		SchemaVersion string `json:"schemaVersion"`
	}{SchemaVersion: ref.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
}
