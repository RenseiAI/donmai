package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/runtime/workarea"
)

func accountingTestSpawner(root, ownerPath, participantPath string) *WorkerSpawner {
	return &WorkerSpawner{
		sessions: map[string]*spawnedSession{
			"owner": {
				handle: SessionHandle{SessionID: "owner", WorkareaRoot: root, WorktreePath: ownerPath},
				spec:   SessionSpec{SessionID: "owner", Repository: "owner"},
			},
			"participant": {
				handle: SessionHandle{SessionID: "participant", WorkareaRoot: root, WorktreePath: participantPath},
				spec:   SessionSpec{SessionID: "participant", Repository: "participant"},
			},
		},
	}
}

func TestActiveWorkareasStrictFailsClosedOnCorruptNestedDeclaration(t *testing.T) {
	root := t.TempDir()
	ownerPath := filepath.Join(root, "owner")
	participantPath := filepath.Join(root, "participant")
	for _, path := range []string{ownerPath, participantPath, filepath.Join(root, ".workarea")} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".workarea", "declaration.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	spawner := accountingTestSpawner(root, ownerPath, participantPath)
	if _, err := spawner.ActiveWorkareasStrict(); err == nil {
		t.Fatal("strict active accounting omitted a corrupt nested declaration")
	}
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: t.TempDir(), ActiveProvider: spawner})
	if _, _, err := registry.List(); err == nil {
		t.Fatal("workarea registry converted accounting failure into a partial list")
	}
}

func TestActiveWorkareasStrictChargesSharedRootOnce(t *testing.T) {
	root := t.TempDir()
	ownerPath := filepath.Join(root, "owner")
	participantPath := filepath.Join(root, "participant")
	for path, body := range map[string]string{
		filepath.Join(ownerPath, "file"):       "owner",
		filepath.Join(participantPath, "file"): "participant",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	declaration, err := (workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "owner", Ref: "main"}, Name: "owner", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: "participant", Ref: "main"}, Name: "participant", Role: workarea.RepositoryRoleSecondary, Authority: workarea.RepositoryMutable},
		},
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	record := workarea.NewDeclarationRecord("owner", "wa_shared_accounting", declaration, nil)
	if err := workarea.WriteDeclaration(t.Context(), workarea.RootPath(root), record); err != nil {
		t.Fatal(err)
	}
	spawner := accountingTestSpawner(root, ownerPath, participantPath)
	rows, err := spawner.ActiveWorkareasStrict()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "wa_shared_accounting" || rows[0].SizeBytes <= 0 {
		t.Fatalf("shared accounting rows = %+v", rows)
	}
}
