package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// writeExcludeSource lays out a source tree mixing real content with
// regenerable dependency and build-output directories, at the top level
// and nested, plus lookalikes the filter must keep: a regular file
// carrying an excluded name and a symlink carrying one.
func writeExcludeSource(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"src/app.go":                       "package src\n",
		"src/app_test.go":                  "package src\n",
		"README.md":                        "# fixture\n",
		"node_modules/pkg/index.js":        "dependency output\n",
		".next/build/manifest.js":          "build output\n",
		"dist/bundle.js":                   "build output\n",
		"target/debug/binary":              "build output\n",
		"pkg/nested/node_modules/dep/x.js": "nested dependency output\n",
		"pkg/nested/dist/nested-bundle.js": "nested build output\n",
		"realdir/keep.txt":                 "kept\n",
		"pkg/target":                       "a regular file named target is kept\n",
	}
	for rel, body := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir parent: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
	// A symlink carrying an excluded name is not a real directory: the
	// capture keeps the link itself without following it.
	if err := os.Symlink("../realdir", filepath.Join(root, "pkg", "dist")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

// TestWorkareaArchiveRegistry_SkipsRegenerableDependencyDirs pins the slim
// capture: regenerable directories never reach the archived tree, the
// manifest records them, and the archived size covers the filtered tree.
func TestWorkareaArchiveRegistry_SkipsRegenerableDependencyDirs(t *testing.T) {
	root := t.TempDir()
	source := t.TempDir()
	writeExcludeSource(t, source)
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		WorkareaID: "wa-slim", SessionID: "slim-session", WorkareaRoot: source, SelectedPath: source,
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	for _, skipped := range []string{
		"node_modules", ".next", "dist", "target",
		"pkg/nested/node_modules", "pkg/nested/dist",
	} {
		if _, err := os.Lstat(filepath.Join(root, "wa-slim", "tree", skipped)); !os.IsNotExist(err) {
			t.Errorf("archived tree still holds %q (stat err %v)", skipped, err)
		}
	}
	for _, kept := range []string{
		"src/app.go", "src/app_test.go", "README.md",
		"pkg/target", "realdir/keep.txt", "pkg/dist", "pkg/nested",
	} {
		if _, err := os.Lstat(filepath.Join(root, "wa-slim", "tree", kept)); err != nil {
			t.Errorf("archived tree lost %q: %v", kept, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(root, "wa-slim", "tree", "pkg", "dist")); err != nil || target != "../realdir" {
		t.Errorf("archived symlink pkg/dist -> %q, err %v; want ../realdir kept as a link", target, err)
	}
	manifest, err := registry.readManifest("wa-slim")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	wantExcluded := []string{
		".next", "dist", "node_modules",
		"pkg/nested/dist", "pkg/nested/node_modules", "target",
	}
	if !slices.Equal(manifest.Excluded, wantExcluded) {
		t.Errorf("manifest excluded = %q; want %q", manifest.Excluded, wantExcluded)
	}
	archivedUsage, err := workarea.PhysicalUsage(workarea.RootPath(filepath.Join(root, "wa-slim", "tree")))
	if err != nil {
		t.Fatalf("archive usage: %v", err)
	}
	if manifest.SizeBytes != archivedUsage {
		t.Errorf("manifest size = %d; archived tree usage = %d", manifest.SizeBytes, archivedUsage)
	}
	sourceUsage, err := workarea.PhysicalUsage(workarea.RootPath(source))
	if err != nil {
		t.Fatalf("source usage: %v", err)
	}
	if manifest.SourceSizeBytes != sourceUsage {
		t.Errorf("manifest source size = %d; source usage = %d", manifest.SourceSizeBytes, sourceUsage)
	}
	if manifest.SizeBytes >= manifest.SourceSizeBytes {
		t.Errorf("slim archive size %d >= unfiltered source size %d", manifest.SizeBytes, manifest.SourceSizeBytes)
	}
	// The excluded list rides the inspect surface through the manifest.
	inspected, err := registry.GetV1("wa-slim")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	rawExcluded, ok := inspected.Manifest["excluded"]
	if !ok {
		t.Fatalf("inspect manifest has no excluded list: %v", inspected.Manifest)
	}
	var wireExcluded []string
	if body, err := json.Marshal(rawExcluded); err != nil || json.Unmarshal(body, &wireExcluded) != nil {
		t.Fatalf("inspect excluded unreadable: %v (%v)", rawExcluded, err)
	}
	if !slices.Equal(wireExcluded, wantExcluded) {
		t.Errorf("inspect excluded = %q; want %q", wireExcluded, wantExcluded)
	}
}

// TestWorkareaArchiveRegistry_DigestCoversFilteredTreeOnly pins that the
// tree digest is computed over the filtered tree: sources differing only
// inside regenerable directories share a digest, while a one-byte change
// to real content moves it.
func TestWorkareaArchiveRegistry_DigestCoversFilteredTreeOnly(t *testing.T) {
	archive := func(t *testing.T, id, depBody, appBody string) string {
		t.Helper()
		root := t.TempDir()
		source := t.TempDir()
		writeExcludeSource(t, source)
		if err := os.WriteFile(filepath.Join(source, "node_modules", "pkg", "index.js"), []byte(depBody), 0o600); err != nil {
			t.Fatalf("write dep: %v", err)
		}
		if err := os.WriteFile(filepath.Join(source, "target", "debug", "binary"), []byte(depBody), 0o600); err != nil {
			t.Fatalf("write build output: %v", err)
		}
		if err := os.WriteFile(filepath.Join(source, "src", "app.go"), []byte(appBody), 0o600); err != nil {
			t.Fatalf("write app: %v", err)
		}
		registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
		if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
			WorkareaID: id, SessionID: "digest-session", WorkareaRoot: source, SelectedPath: source,
		}); err != nil {
			t.Fatalf("archive: %v", err)
		}
		manifest, err := registry.readManifest(id)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		if manifest.TreeDigest == "" {
			t.Fatal("archive has no tree digest")
		}
		return manifest.TreeDigest
	}
	base := archive(t, "wa-digest-a", "dependency v1\n", "package src\n")
	movedDeps := archive(t, "wa-digest-b", "dependency v2, rebuilt\n", "package src\n")
	if base != movedDeps {
		t.Errorf("digests differ on regenerable-only change:\n%s\n%s", base, movedDeps)
	}
	movedSource := archive(t, "wa-digest-c", "dependency v1\n", "package src // edited\n")
	if base == movedSource {
		t.Errorf("digest did not move on real content change: %s", base)
	}
}

// TestWorkareaArchiveRegistry_RestoreFilteredArchiveRoundTrip pins that a
// slim archive restores: real content comes back and regenerable
// directories stay absent.
func TestWorkareaArchiveRegistry_RestoreFilteredArchiveRoundTrip(t *testing.T) {
	root := t.TempDir()
	source := t.TempDir()
	writeExcludeSource(t, source)
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		WorkareaID: "wa-slim-restore", SessionID: "slim-restore", WorkareaRoot: source, SelectedPath: source,
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	restored, _, err := registry.Restore("wa-slim-restore", afclient.WorkareaRestoreRequest{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(restored.Path, "src", "app.go")); err != nil || string(body) != "package src\n" {
		t.Errorf("restored app.go = %q, err %v", body, err)
	}
	for _, skipped := range []string{"node_modules", ".next", "target", "pkg/nested/node_modules"} {
		if _, err := os.Lstat(filepath.Join(restored.Path, skipped)); !os.IsNotExist(err) {
			t.Errorf("restored tree holds regenerable %q (stat err %v)", skipped, err)
		}
	}
}

// TestWorkareaArchiveRegistry_SessionRootArchiveSkipsRegenerableDirs pins
// the slim capture on the session-root path: the manifest records the
// skipped directories, and the strict restore (physical-size and digest
// reconciliation against the manifest) still accepts the filtered tree.
func TestWorkareaArchiveRegistry_SessionRootArchiveSkipsRegenerableDirs(t *testing.T) {
	archiveRoot := t.TempDir()
	worktreeParent := t.TempDir()
	sourceRoot := filepath.Join(worktreeParent, "session-root")
	declaration := workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "https://example.test/web.git", Ref: "main"}, Name: "web", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
		},
		Select: &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: "web"},
	}
	normalized, err := declaration.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	acquisitions, err := workarea.NewAcquisitionStore(worktreeParent, nil)
	if err != nil {
		t.Fatal(err)
	}
	acquisition, err := acquisitions.Begin("slim-session-root", "wa_slim_root", workarea.RootPath(sourceRoot), "web", "")
	if err != nil {
		t.Fatal(err)
	}
	for leaf, body := range map[string]string{
		"web/source.txt":                "mutable\n",
		"web/node_modules/dep/index.js": "dependency output\n",
		"web/target/debug/binary":       "build output\n",
	} {
		path := filepath.Join(acquisition.StagingRoot.String(), leaf)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record := workarea.NewDeclarationRecord(
		"slim-session-root", "wa_slim_root", normalized,
		map[string]string{"web": "aaa"}, acquisition.Record.AcquisitionID,
	)
	if err := workarea.WriteDeclaration(t.Context(), acquisition.StagingRoot, record); err != nil {
		t.Fatal(err)
	}
	if _, err := acquisitions.Commit(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, AcquisitionStore: acquisitions})
	if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		AcquisitionID: acquisition.Record.AcquisitionID, WorkareaID: "wa_slim_root", SessionID: "slim-session-root",
		WorkareaRoot: sourceRoot, SelectedPath: filepath.Join(sourceRoot, "web"),
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	manifest, err := registry.readManifest("wa_slim_root")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if want := []string{"web/node_modules", "web/target"}; !slices.Equal(manifest.Excluded, want) {
		t.Fatalf("manifest excluded = %q; want %q", manifest.Excluded, want)
	}
	if err := acquisitions.RemovePublishedRoot(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	restored, _, err := registry.RestoreV1("wa_slim_root", afclient.WorkareaRestoreRequest{IntoSessionID: "slim-session-root"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(restored.WorkareaRoot, "web", "source.txt")); err != nil || string(body) != "mutable\n" {
		t.Errorf("restored source.txt = %q, err %v", body, err)
	}
	for _, skipped := range []string{"web/node_modules", "web/target"} {
		if _, err := os.Lstat(filepath.Join(restored.WorkareaRoot, skipped)); !os.IsNotExist(err) {
			t.Errorf("restored tree holds regenerable %q (stat err %v)", skipped, err)
		}
	}
}
