package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// excludeTestGit runs git in dir, failing the test on error, and returns
// trimmed combined output. Commits use a fixed author/committer date so
// fixtures built from the same content hash the same: the archive digest
// test compares captures whose .git metadata must be byte-identical.
// The identity travels as -c flags (not process state) so the fixture
// commits on machines with no global git identity, such as CI runners.
func excludeTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if len(args) > 0 && args[0] == "commit" {
		args = append([]string{
			"-c", "user.email=test@example.com",
			"-c", "user.name=test",
			"-c", "commit.gpgsign=false",
		}, args...)
	}
	cmd := exec.Command("git", args...) //nolint:gosec // test fixture with caller-supplied args
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_DATE=2005-04-07T22:13:13+00:00",
		"GIT_COMMITTER_DATE=2005-04-07T22:13:13+00:00",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initExcludeTestRepo turns dir into a git checkout whose ignore rules
// mark every regenerable candidate in writeExcludeSource as ignored, so
// the slimming guard proves them regenerable. `target` is deliberately
// left unignored: the guard keeps it and the tests below pin that.
func initExcludeTestRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	excludeTestGit(t, dir, "init", "-q", "-b", "main")
	excludeTestGit(t, dir, "config", "user.email", "test@example.com")
	excludeTestGit(t, dir, "config", "user.name", "test")
	excludeTestGit(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_modules/\n.next/\ndist/\n"), 0o600); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}
	excludeTestGit(t, dir, "add", "-A")
	excludeTestGit(t, dir, "commit", "-qm", "fixture base")
}

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
	// The guard skips a candidate only when the checkout ignores it:
	// ignored candidates slim, the unignored `target` stays.
	initExcludeTestRepo(t, source)
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		WorkareaID: "wa-slim", SessionID: "slim-session", WorkareaRoot: source, SelectedPath: source,
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	for _, skipped := range []string{
		"node_modules", ".next", "dist",
		"pkg/nested/node_modules", "pkg/nested/dist",
	} {
		if _, err := os.Lstat(filepath.Join(root, "wa-slim", "tree", skipped)); !os.IsNotExist(err) {
			t.Errorf("archived tree still holds %q (stat err %v)", skipped, err)
		}
	}
	for _, kept := range []string{
		"src/app.go", "src/app_test.go", "README.md",
		"target/debug/binary", "pkg/target", "realdir/keep.txt", "pkg/dist", "pkg/nested",
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
		"pkg/nested/dist", "pkg/nested/node_modules",
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
// tree digest is computed over the filtered tree: captures of one checkout
// differing only inside the ignored `node_modules` share a digest, while
// a one-byte change to real content moves it. All three captures run
// against the SAME source directory with no new commits between them, so
// the checkout's own metadata is byte-identical and only the filtered
// content varies. Git-aware slimming itself is pinned by the
// dependency-dirs test above.
func TestWorkareaArchiveRegistry_DigestCoversFilteredTreeOnly(t *testing.T) {
	root := t.TempDir()
	source := t.TempDir()
	writeExcludeSource(t, source)
	initExcludeTestRepo(t, source)
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	archive := func(t *testing.T, id string) string {
		t.Helper()
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
	writeDep := func(t *testing.T, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(source, "node_modules", "pkg", "index.js"), []byte(body), 0o600); err != nil {
			t.Fatalf("write dep: %v", err)
		}
	}
	writeDep(t, "dependency v1\n")
	base := archive(t, "wa-digest-a")
	// The ignored dependency tree is rebuilt between captures; the
	// checkout itself (tracked files, index, refs) is untouched.
	writeDep(t, "dependency v2, rebuilt\n")
	movedDeps := archive(t, "wa-digest-b")
	if base != movedDeps {
		t.Errorf("digests differ on regenerable-only change:\n%s\n%s", base, movedDeps)
	}
	if err := os.WriteFile(filepath.Join(source, "src", "app.go"), []byte("package src // edited\n"), 0o600); err != nil {
		t.Fatalf("write app: %v", err)
	}
	movedSource := archive(t, "wa-digest-c")
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
	initExcludeTestRepo(t, source)
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
	for _, skipped := range []string{"node_modules", ".next", "pkg/nested/node_modules"} {
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
	// The committed leaf is a real checkout whose ignore rules mark
	// `node_modules` as regenerable; `target` stays unignored so the
	// guard keeps it even on the nested path. The identity travels as
	// -c flags so the fixture commits with no global git identity.
	excludeTestGit(t, sourceRoot, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(sourceRoot, ".gitignore"), []byte("node_modules/\n"), 0o600); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}
	excludeTestGit(t, sourceRoot, "add", "-A")
	excludeTestGit(t, sourceRoot, "commit", "-qm", "nested fixture base")
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
	if want := []string{"web/node_modules"}; !slices.Equal(manifest.Excluded, want) {
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
	for _, skipped := range []string{"web/node_modules"} {
		if _, err := os.Lstat(filepath.Join(restored.WorkareaRoot, skipped)); !os.IsNotExist(err) {
			t.Errorf("restored tree holds regenerable %q (stat err %v)", skipped, err)
		}
	}
	if body, err := os.ReadFile(filepath.Join(restored.WorkareaRoot, "web", "target", "debug", "binary")); err != nil || string(body) != "build output\n" {
		t.Errorf("restored unignored web/target = %q, err %v; want it kept", body, err)
	}
}

// writeGitProbeSource lays out a checkout holding the four cases the
// slimming guard must never drop: a tracked file inside a
// regenerable-named directory, an uncommitted new file in one, a
// committed build bundle, and an unpushed commit on a branch whose name
// collides with an excluded leaf.
func writeGitProbeSource(t *testing.T, dir string) (base string) {
	t.Helper()
	writeFile := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir parent: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
	writeFile("README.md", "# probe\n")
	writeFile("internal/target/model.go", "package target\n")
	writeFile("dist/index.js", "committed bundle\n")
	writeFile("node_modules/pkg/index.js", "installed dependency\n")
	// `sub/.git-tmp/dist/` is ignored, so the excluded-named
	// directory nested under it is an ignored, untracked candidate:
	// its leaf (not its ancestors) carries the excluded name, and
	// only the never-slim-git-metadata rule keeps it.
	writeFile("sub/.git-tmp/dist/payload.bin", "tool payload\n")
	// `.git/logs/` is ignored so the reflog directory for the `dist`
	// branch below is an ignored, untracked candidate: without the
	// never-slim-git-metadata rule the capture would drop it.
	writeFile(".gitignore", "node_modules/\n.git/logs/\nsub/.git-tmp/\n")
	excludeTestGit(t, dir, "init", "-q", "-b", "main")
	// The probe commits carry their identity as -c flags (via
	// excludeTestGit), so the fixture builds with no global git
	// identity, as on CI runners.
	excludeTestGit(t, dir, "add", "-A")
	excludeTestGit(t, dir, "commit", "-qm", "probe base")
	base = excludeTestGit(t, dir, "rev-parse", "HEAD")
	// An unpushed commit on a branch colliding with an excluded leaf.
	excludeTestGit(t, dir, "checkout", "-qb", "dist/hotfix")
	writeFile("hotfix.txt", "hotfix\n")
	excludeTestGit(t, dir, "add", "-A")
	excludeTestGit(t, dir, "commit", "-qm", "hotfix commit")
	// Uncommitted source inside a regenerable-named directory: written
	// AFTER the last commit so it stays untracked.
	writeFile("internal/target/new.go", "package target\n")
	return base
}

// TestWorkareaArchiveRegistry_KeepsTrackedSourceUnderExcludedNames pins the
// loss-prevention rule through the production ArchiveRoot entry point: a
// tracked file under `internal/target`, an uncommitted file under it, a
// committed `dist` bundle and the refs holding an unpushed commit on
// branch `dist/hotfix` all survive the capture, and the archived checkout
// still resolves HEAD with the unpushed commit reachable.
func TestWorkareaArchiveRegistry_KeepsTrackedSourceUnderExcludedNames(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	source := t.TempDir()
	base := writeGitProbeSource(t, source)
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		WorkareaID: "wa-probe", SessionID: "probe-session", WorkareaRoot: source, SelectedPath: source,
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	tree := filepath.Join(root, "wa-probe", "tree")
	for _, kept := range []string{
		"internal/target/model.go",
		"internal/target/new.go",
		"dist/index.js",
		"hotfix.txt",
		".git/refs/heads/dist/hotfix",
		// The reflog for the `dist` branch is ignored and untracked,
		// so it is exactly the candidate the name heuristic would
		// drop: git metadata is never slimmed.
		".git/logs/refs/heads/dist/hotfix",
		// The ignored, untracked excluded-named directory nested
		// under `sub/.git-tmp/`: only the never-slim-git-metadata
		// rule keeps it.
		"sub/.git-tmp/dist/payload.bin",
	} {
		if _, err := os.Lstat(filepath.Join(tree, kept)); err != nil {
			t.Errorf("archived tree lost %q: %v", kept, err)
		}
	}
	if got := excludeTestGit(t, tree, "rev-parse", "HEAD"); got != excludeTestGit(t, source, "rev-parse", "HEAD") {
		t.Errorf("archived HEAD = %q; want the source HEAD", got)
	}
	if got := excludeTestGit(t, tree, "branch", "--show-current"); got != "dist/hotfix" {
		t.Errorf("archived branch = %q; want dist/hotfix", got)
	}
	if status := excludeTestGit(t, tree, "status", "--porcelain=v1", "--untracked-files=all"); !strings.Contains(status, "internal/target/new.go") {
		t.Errorf("archived status = %q; want the uncommitted internal/target/new.go listed", status)
	}
	manifest, err := registry.readManifest("wa-probe")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	// Only the ignored, untracked `node_modules` is slimmed: the
	// git-metadata leaf and every tracked candidate stay out of the
	// excluded list. The exact-equality assertion below pins the
	// guard's keep (a narrowed guard would record
	// `sub/.git-tmp/dist` here); the kept-file assertions above pin
	// the copy walk's keep.
	if want := []string{"node_modules"}; !slices.Equal(manifest.Excluded, want) {
		t.Errorf("manifest excluded = %q; want %q", manifest.Excluded, want)
	}
	for _, dropped := range []string{"dist", "internal/target", ".git/refs/heads/dist"} {
		if slices.Contains(manifest.Excluded, dropped) {
			t.Errorf("manifest excluded = %q; want no %q — tracked source is never slimmed", manifest.Excluded, dropped)
		}
	}
	_ = base
}

// TestWorkareaArchiveRegistry_SkipsIgnoredUntrackedDependencyDirs pins the
// other side through the same entry point: an ignored, untracked
// `node_modules` IS slimmed and recorded, while the probe's tracked files
// around it survive.
func TestWorkareaArchiveRegistry_SkipsIgnoredUntrackedDependencyDirs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	source := t.TempDir()
	writeGitProbeSource(t, source)
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		WorkareaID: "wa-probe-slim", SessionID: "probe-session", WorkareaRoot: source, SelectedPath: source,
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "wa-probe-slim", "tree", "node_modules")); !os.IsNotExist(err) {
		t.Errorf("archived tree still holds ignored node_modules (stat err %v)", err)
	}
	manifest, err := registry.readManifest("wa-probe-slim")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !slices.Contains(manifest.Excluded, "node_modules") {
		t.Errorf("manifest excluded = %q; want node_modules recorded", manifest.Excluded)
	}
	for _, kept := range []string{"internal/target/model.go", "dist/index.js"} {
		if _, err := os.Lstat(filepath.Join(root, "wa-probe-slim", "tree", kept)); err != nil {
			t.Errorf("archived tree lost tracked %q: %v", kept, err)
		}
	}
}

// TestWorkareaArchiveRegistry_RestoreLegacyArchiveKeepsRegenerableDirs pins
// the restore path byte-for-byte: an archive committed before the
// exclusion existed — whose tree still holds a `node_modules` directory —
// restores with that directory intact. The capture may slim, but the
// restore never does.
func TestWorkareaArchiveRegistry_RestoreLegacyArchiveKeepsRegenerableDirs(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id:       "wa-legacy-deps",
		manifest: archiveManifest{SessionID: "legacy-session"},
		tree: map[string]string{
			"src/app.go":                "package src\n",
			"node_modules/pkg/index.js": "dependency output\n",
			"dist/bundle.js":            "build output\n",
		},
	})
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	restored, _, err := registry.Restore("wa-legacy-deps", afclient.WorkareaRestoreRequest{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, kept := range []string{"src/app.go", "node_modules/pkg/index.js", "dist/bundle.js"} {
		if body, err := os.ReadFile(filepath.Join(restored.Path, kept)); err != nil {
			t.Errorf("restored tree lost legacy %q: %v", kept, err)
		} else if body == nil {
			t.Errorf("restored tree holds empty legacy %q", kept)
		}
	}
}
