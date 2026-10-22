package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	// The committed manifest retains the terminal result binding, and the
	// inspect projection serves it: the five-identity contract is realized
	// on the readback surface, not just the commit return.
	if ref.TerminalResultID != "tr_11111111111111111111111111111111" {
		t.Fatalf("inspect reference terminal result = %q, want the committed binding", ref.TerminalResultID)
	}
	manifest, err := registry.readManifest("wa_44444444444444444444444444444444")
	if err != nil {
		t.Fatal(err)
	}
	if ref.ArchiveDigest != manifest.TreeDigest {
		t.Fatalf("inspect digest %q != manifest %q", ref.ArchiveDigest, manifest.TreeDigest)
	}
	if manifest.TerminalResultID != ref.TerminalResultID {
		t.Fatalf("manifest binding %q != served %q", manifest.TerminalResultID, ref.TerminalResultID)
	}
	if _, err := workarea.CanonicalBytes(struct {
		SchemaVersion string `json:"schemaVersion"`
	}{SchemaVersion: ref.SchemaVersion}); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveInspectRefusesConflictingTerminalResult(t *testing.T) {
	t.Parallel()
	registry, spec := commitFlatArchiveForReference(t, "wa_55555555555555555555555555555555", "11111111-1111-4111-8111-111111111111")
	if _, err := registry.CommitArchiveRoot(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	// A different terminal result for the same committed bytes is refused:
	// the archive is immutable and its binding does not move.
	conflicting := spec
	conflicting.TerminalResultID = "tr_22222222222222222222222222222222"
	if _, err := registry.CommitArchiveRoot(t.Context(), conflicting); err == nil {
		t.Fatal("conflicting terminal result accepted")
	}
}

func TestWithArchiveReferenceTerminalResultAttachesBinding(t *testing.T) {
	t.Parallel()
	base := &afclient.ArchiveReference{
		SchemaVersion: afclient.ArchiveReferenceSchemaV1, ArchiveID: "wa_66666666666666666666666666666666",
		ArchiveDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SessionID:     "11111111-1111-4111-8111-111111111111", WorkareaID: "wa_66666666666666666666666666666666",
		CommittedAt: "2026-07-18T12:30:00.000Z",
	}
	if base.TerminalResultID != "" {
		t.Fatalf("fixture carries a binding: %+v", base)
	}
	attached := WithArchiveReferenceTerminalResult(base, "tr_11111111111111111111111111111111")
	if attached == nil || attached.TerminalResultID != "tr_11111111111111111111111111111111" {
		t.Fatalf("attached = %+v", attached)
	}
	if base.TerminalResultID != "" {
		t.Fatal("helper mutated its input")
	}
	// A conflicting binding is refused: the existing reference is returned
	// unchanged.
	if kept := WithArchiveReferenceTerminalResult(attached, "tr_22222222222222222222222222222222"); kept != attached {
		t.Fatalf("conflicting attach replaced the reference: %+v", kept)
	}
	if got := WithArchiveReferenceTerminalResult(nil, "tr_11111111111111111111111111111111"); got != nil {
		t.Fatalf("nil input = %+v", got)
	}
}

// TestCommitArchiveRootAcceptsPreContractCommitOnRetry pins the crash-retry
// across an upgrade: an older daemon committed the archive (its manifest has
// no terminal result field) and died before the lease release finished. The
// upgraded daemon retries the release with the lease's terminal result. The
// committed bytes are unchanged, so the retry is idempotent and yields the
// reference for the committed digest instead of an identity collision that
// would leave the lease stuck in release.
func TestCommitArchiveRootAcceptsPreContractCommitOnRetry(t *testing.T) {
	t.Parallel()
	const workareaID = "wa_77777777777777777777777777777777"
	registry, spec := commitFlatArchiveForReference(t, workareaID, "11111111-1111-4111-8111-111111111111")
	legacy := spec
	legacy.TerminalResultID = ""
	if err := registry.ArchiveRoot(t.Context(), legacy); err != nil {
		t.Fatalf("pre-contract commit: %v", err)
	}
	manifestPath := filepath.Join(registry.Root(), workareaID, "manifest.json")
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "terminalResultId") {
		t.Fatalf("fixture manifest is not pre-contract: %s", before)
	}
	reference, err := registry.CommitArchiveRoot(t.Context(), spec)
	if err != nil {
		t.Fatalf("retry over a pre-contract commit: %v", err)
	}
	if reference == nil {
		t.Fatal("retry returned no recovery reference")
	}
	if err := reference.Validate(); err != nil {
		t.Fatalf("reference invalid: %v", err)
	}
	manifest, err := registry.readManifest(workareaID)
	if err != nil {
		t.Fatal(err)
	}
	if reference.ArchiveID != workareaID || reference.WorkareaID != workareaID || reference.SessionID != spec.SessionID ||
		reference.TerminalResultID != spec.TerminalResultID || reference.ArchiveDigest != manifest.TreeDigest {
		t.Fatalf("reference = %+v, manifest digest %q", reference, manifest.TreeDigest)
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("retry rewrote the immutable pre-contract manifest")
	}
	// A conflicting identity is still refused over a pre-contract commit.
	conflicting := spec
	conflicting.SessionID = "22222222-2222-4222-8222-222222222222"
	if _, err := registry.CommitArchiveRoot(t.Context(), conflicting); !errors.Is(err, ErrArchiveExists) {
		t.Fatalf("conflicting session over a pre-contract commit: err = %v, want ErrArchiveExists", err)
	}
}
