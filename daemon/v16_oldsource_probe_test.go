package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// V16 old-source behavior probe: archive a Ready session-root generation,
// then read it back through the EXISTING (old) registry APIs. Old source
// has no same-identity readback, so ListV1 must omit the ready row and
// GetV1 must resolve the archived record — this test FAILS (RED) on old
// source at the list assertion and PASSES (GREEN) once repaired. It uses
// only pre-existing fixtures/APIs, so it compiles against old source.
func TestV16OldSource_SessionRootRestoreListsSameIdentity(t *testing.T) {
	archiveRoot := t.TempDir()
	worktreeParent := t.TempDir()
	sourceRoot := filepath.Join(worktreeParent, "session-root")
	declaration := workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "https://example.test/web.git", Ref: "main"}, Name: "web", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: "https://example.test/docs.git", Ref: "main"}, Name: "docs", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
		},
		Select: &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "docs"},
	}
	normalized, err := declaration.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	acquisitions, err := workarea.NewAcquisitionStore(worktreeParent, nil)
	if err != nil {
		t.Fatal(err)
	}
	acquisition, err := acquisitions.Begin("archive-session", "wa_session_v16", workarea.RootPath(sourceRoot), "docs", "")
	if err != nil {
		t.Fatal(err)
	}
	for leaf, body := range map[string]string{"web/source.txt": "mutable", "docs/context.txt": "readonly"} {
		path := filepath.Join(acquisition.StagingRoot.String(), leaf)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record := workarea.NewDeclarationRecord(
		"archive-session", "wa_session_v16", normalized,
		map[string]string{"web": "aaa", "docs": "bbb"}, acquisition.Record.AcquisitionID,
	)
	if err := workarea.WriteDeclaration(t.Context(), acquisition.StagingRoot, record); err != nil {
		t.Fatal(err)
	}
	if _, err := acquisitions.Commit(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	prodRegistry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, AcquisitionStore: acquisitions})
	if err := prodRegistry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		AcquisitionID: acquisition.Record.AcquisitionID, WorkareaID: "wa_session_v16", SessionID: "archive-session",
		WorkareaRoot: sourceRoot, SelectedPath: filepath.Join(sourceRoot, "docs"),
	}); err != nil {
		t.Fatal(err)
	}
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, WorktreeParent: worktreeParent})

	active, _, err := registry.ListV1()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for i := range active {
		if active[i].ID == "wa_session_v16" {
			found = true
			if active[i].Kind != afclient.WorkareaKindActive || active[i].Status != afclient.WorkareaStatusReady {
				t.Fatalf("list entry kind/status = %q/%q, want active/ready", active[i].Kind, active[i].Status)
			}
			if active[i].SessionID != "archive-session" {
				t.Fatalf("list entry session = %q, want archive-session", active[i].SessionID)
			}
		}
	}
	if !found {
		t.Fatalf("RED: ListV1 omits ready session-root id %q (active=%d)", "wa_session_v16", len(active))
	}
	got, err := registry.GetV1("wa_session_v16")
	if err != nil {
		t.Fatalf("RED: GetV1: %v", err)
	}
	if got.ID != "wa_session_v16" || got.Kind != afclient.WorkareaKindActive {
		t.Fatalf("RED: show = %+v, want active wa_session_v16", got)
	}
}
