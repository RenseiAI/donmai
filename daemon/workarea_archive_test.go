package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// fixtureArchive lays out a minimal archive directory under root/<id>/
// with a manifest sidecar and an optional tree contents map. Each tree
// entry's value is either a regular file body (string) or a "symlink:<target>"
// directive. Directories are inferred from intermediate path segments.
type fixtureArchive struct {
	id       string
	manifest archiveManifest
	tree     map[string]string
}

func writeFixtureArchive(t *testing.T, root string, fa fixtureArchive) {
	t.Helper()
	dir := filepath.Join(root, fa.id)
	if err := os.MkdirAll(filepath.Join(dir, "tree"), 0o750); err != nil {
		t.Fatalf("mkdir tree: %v", err)
	}
	if fa.manifest.ID == "" {
		fa.manifest.ID = fa.id
	}
	if fa.manifest.CreatedAt == "" {
		fa.manifest.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}
	manifestData, err := json.MarshalIndent(&fa.manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestData, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	for relPath, content := range fa.tree {
		full := filepath.Join(dir, "tree", relPath)
		if strings.HasPrefix(content, "symlink:") {
			target := strings.TrimPrefix(content, "symlink:")
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				t.Fatalf("mkdir parent: %v", err)
			}
			if err := os.Symlink(target, full); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir parent: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
}

func TestWorkareaArchiveRegistry_List_EmptyRoot(t *testing.T) {
	root := t.TempDir() + "/missing"
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	active, archived, err := reg.List()
	if err != nil {
		t.Fatalf("expected no error on missing root: %v", err)
	}
	if len(archived) != 0 {
		t.Errorf("expected zero archives, got %d", len(archived))
	}
	if len(active) != 0 {
		t.Errorf("expected zero active, got %d", len(active))
	}
}

func TestWorkareaArchiveRegistry_List_DeterministicOrder(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"zeta-1", "alpha-1", "mike-1"} {
		writeFixtureArchive(t, root, fixtureArchive{id: id, manifest: archiveManifest{
			SessionID: "sess-" + id,
		}})
	}
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	_, archived, err := reg.List()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(archived) != 3 {
		t.Fatalf("expected 3 archives, got %d", len(archived))
	}
	want := []string{"alpha-1", "mike-1", "zeta-1"}
	for i, w := range want {
		if archived[i].ID != w {
			t.Errorf("entry %d: want %q, got %q", i, w, archived[i].ID)
		}
		if archived[i].Kind != afclient.WorkareaKindArchived {
			t.Errorf("entry %d: kind want archived, got %q", i, archived[i].Kind)
		}
	}
}

func TestWorkareaArchiveRegistry_List_SkipsCorruptedManifestSilentlyAsRow(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "good", manifest: archiveManifest{}})

	// Drop a directory with invalid manifest JSON.
	corrupt := filepath.Join(root, "broken")
	if err := os.MkdirAll(filepath.Join(corrupt, "tree"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, "manifest.json"), []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	_, archived, err := reg.List()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(archived) != 2 {
		t.Fatalf("want 2 entries (corrupted included), got %d", len(archived))
	}
	// "broken" must surface with corruption-disposition.
	var brokenRow afclient.WorkareaSummary
	for _, row := range archived {
		if row.ID == "broken" {
			brokenRow = row
		}
	}
	if !strings.Contains(brokenRow.Disposition, "corrupted") {
		t.Errorf("broken row should have corrupted disposition, got %q", brokenRow.Disposition)
	}
}

func TestWorkareaArchiveRegistry_List_IncludesActiveProvider(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-archive-1", manifest: archiveManifest{}})
	provider := &fakeActiveProvider{
		members: []afclient.WorkareaSummary{{
			ID:     "wa-active-1",
			Kind:   afclient.WorkareaKindActive,
			Status: afclient.WorkareaStatusReady,
		}},
	}
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root, ActiveProvider: provider})
	active, archived, err := reg.List()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(active) != 1 || active[0].ID != "wa-active-1" {
		t.Errorf("expected one active member, got %+v", active)
	}
	if len(archived) != 1 || archived[0].ID != "wa-archive-1" {
		t.Errorf("expected one archived, got %+v", archived)
	}
}

func TestWorkareaArchiveRegistry_Get_HappyPath(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id: "wa-1",
		manifest: archiveManifest{
			SessionID:    "sess-abc",
			Repository:   "github.com/acme/repo",
			Ref:          "main",
			ProviderID:   "local-pool",
			Toolchain:    map[string]string{"node": "20"},
			Capabilities: []string{"shared"},
		},
		tree: map[string]string{"hello.txt": "world"},
	})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	wa, err := reg.Get("wa-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if wa.ID != "wa-1" {
		t.Errorf("id: got %q", wa.ID)
	}
	if wa.Kind != afclient.WorkareaKindArchived {
		t.Errorf("kind: got %q", wa.Kind)
	}
	if wa.SessionID != "sess-abc" {
		t.Errorf("sessionId: got %q", wa.SessionID)
	}
	if wa.Toolchain["node"] != "20" {
		t.Errorf("toolchain not propagated: %+v", wa.Toolchain)
	}
}

// TestWorkareaArchiveRegistry_RejectsPathTraversalIDs pins that an archive id
// carrying a path separator or a traversal component cannot read, diff, or
// restore outside the archive root. Archive ids reach the registry straight
// from the local control-API request path, and net/http only redirects a
// LITERAL ".." segment — a percent-encoded one ("%2e%2e") decodes to ".." and
// arrives intact — so the guard must live at the registry, not the router.
func TestWorkareaArchiveRegistry_RejectsPathTraversalIDs(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workareas")

	// A legitimate archive inside the root.
	writeFixtureArchive(t, root, fixtureArchive{
		id:       "wa-ok",
		manifest: archiveManifest{SessionID: "sess-inside"},
		tree:     map[string]string{"a.txt": "hi"},
	})

	// A "secret" manifest ONE LEVEL ABOVE the root, reachable only by escaping:
	// archiveDir("..") == parent, so an unguarded readManifest("..") reads it.
	secret, err := json.Marshal(archiveManifest{ID: "..", SessionID: "sess-ESCAPED"})
	if err != nil {
		t.Fatalf("marshal secret manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "manifest.json"), secret, 0o600); err != nil {
		t.Fatalf("plant parent manifest: %v", err)
	}

	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})

	// A normal id still resolves.
	if wa, err := reg.Get("wa-ok"); err != nil || wa.SessionID != "sess-inside" {
		t.Fatalf("valid id must resolve: wa=%+v err=%v", wa, err)
	}

	// Every separator / traversal / non-single-component id is rejected as
	// not-found by each externally reachable read path.
	malicious := []string{"", ".", "..", "../..", "wa-ok/../..", "/etc", `..\..`, "sub/child"}
	for _, id := range malicious {
		if _, err := reg.Get(id); !errors.Is(err, ErrArchiveNotFound) {
			t.Errorf("Get(%q): want ErrArchiveNotFound, got %v", id, err)
		}
		if _, _, err := reg.Restore(id, afclient.WorkareaRestoreRequest{}); !errors.Is(err, ErrArchiveNotFound) {
			t.Errorf("Restore(%q): want ErrArchiveNotFound, got %v", id, err)
		}
		if _, err := reg.Diff(id, "wa-ok"); !errors.Is(err, ErrArchiveNotFound) {
			t.Errorf("Diff(%q, ok): want ErrArchiveNotFound, got %v", id, err)
		}
		if _, err := reg.CountDiff("wa-ok", id); !errors.Is(err, ErrArchiveNotFound) {
			t.Errorf("CountDiff(ok, %q): want ErrArchiveNotFound, got %v", id, err)
		}
		if _, err := reg.DiffStream(id, "wa-ok", func(afclient.WorkareaDiffEntry) error { return nil }); !errors.Is(err, ErrArchiveNotFound) {
			t.Errorf("DiffStream(%q, ok): want ErrArchiveNotFound, got %v", id, err)
		}
	}

	// Concrete escape proof: ".." must never surface the parent (sess-ESCAPED)
	// manifest. Without the registry guard, Get("..") reads it and returns nil.
	if wa, err := reg.Get(".."); err == nil {
		t.Fatalf("Get(\"..\") escaped the root and read %q", wa.SessionID)
	}
}

func TestWorkareaArchiveRegistry_Get_NotFound(t *testing.T) {
	root := t.TempDir()
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	_, err := reg.Get("nonesuch")
	if !errors.Is(err, ErrArchiveNotFound) {
		t.Errorf("expected ErrArchiveNotFound, got %v", err)
	}
}

func TestWorkareaArchiveRegistry_Get_CorruptedManifest(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "wa-bad")
	if err := os.MkdirAll(filepath.Join(dir, "tree"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	_, err := reg.Get("wa-bad")
	if !errors.Is(err, ErrArchiveCorrupted) {
		t.Errorf("expected ErrArchiveCorrupted, got %v", err)
	}
}

// ── Diff coverage ─────────────────────────────────────────────────────────

func TestWorkareaArchiveRegistry_Diff_AddedRemovedModified(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id:       "wa-a",
		manifest: archiveManifest{SessionID: "a"},
		tree: map[string]string{
			"shared/keep.txt":      "same",
			"shared/changed.txt":   "old",
			"shared/removed.txt":   "gone",
			"sub/dir/sub-file.txt": "alpha",
		},
	})
	writeFixtureArchive(t, root, fixtureArchive{
		id:       "wa-b",
		manifest: archiveManifest{SessionID: "b"},
		tree: map[string]string{
			"shared/keep.txt":      "same",
			"shared/changed.txt":   "new",
			"shared/added.txt":     "hello",
			"sub/dir/sub-file.txt": "alpha",
		},
	})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	res, err := reg.Diff("wa-a", "wa-b")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	gotByPath := map[string]afclient.WorkareaDiffEntry{}
	for _, e := range res.Entries {
		gotByPath[e.Path] = e
	}

	if e, ok := gotByPath["shared/added.txt"]; !ok {
		t.Error("expected added.txt")
	} else if e.Status != afclient.WorkareaDiffStatusAdded {
		t.Errorf("added.txt status: got %q", e.Status)
	}
	if e, ok := gotByPath["shared/removed.txt"]; !ok {
		t.Error("expected removed.txt")
	} else if e.Status != afclient.WorkareaDiffStatusRemoved {
		t.Errorf("removed.txt status: got %q", e.Status)
	}
	if e, ok := gotByPath["shared/changed.txt"]; !ok {
		t.Error("expected changed.txt")
	} else if e.Status != afclient.WorkareaDiffStatusModified {
		t.Errorf("changed.txt status: got %q", e.Status)
	} else if e.HashA == e.HashB || e.HashA == "" || e.HashB == "" {
		t.Errorf("changed.txt hashes should differ and be non-empty: A=%q B=%q", e.HashA, e.HashB)
	}
	if _, ok := gotByPath["shared/keep.txt"]; ok {
		t.Errorf("identical file should NOT appear in diff")
	}
	if _, ok := gotByPath["sub/dir/sub-file.txt"]; ok {
		t.Errorf("identical nested file should NOT appear in diff")
	}

	// Summary aggregates must match.
	if res.Summary.Added != 1 || res.Summary.Removed != 1 || res.Summary.Modified != 1 {
		t.Errorf("summary mismatch: %+v", res.Summary)
	}
	if res.Summary.Total != 3 {
		t.Errorf("summary total mismatch: %d", res.Summary.Total)
	}
}

func TestWorkareaArchiveRegistry_Diff_SymlinkComparison(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks unreliable on Windows CI")
	}
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "sym-a", tree: map[string]string{
		"link": "symlink:/etc/hosts",
	}})
	writeFixtureArchive(t, root, fixtureArchive{id: "sym-b", tree: map[string]string{
		"link": "symlink:/etc/passwd",
	}})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	res, err := reg.Diff("sym-a", "sym-b")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(res.Entries))
	}
	if res.Entries[0].Status != afclient.WorkareaDiffStatusModified {
		t.Errorf("symlink target diff should be modified, got %q", res.Entries[0].Status)
	}
}

func TestWorkareaArchiveRegistry_Diff_BinaryFile(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "bin-a", tree: map[string]string{
		"bin.dat": "\x00\x01\x02\x03\x04",
	}})
	writeFixtureArchive(t, root, fixtureArchive{id: "bin-b", tree: map[string]string{
		"bin.dat": "\x00\x01\x02\x03\x05",
	}})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	res, err := reg.Diff("bin-a", "bin-b")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Status != afclient.WorkareaDiffStatusModified {
		t.Errorf("binary diff entry mismatch: %+v", res.Entries)
	}
	if res.Entries[0].HashA == res.Entries[0].HashB {
		t.Errorf("binary diff: hashes must differ")
	}
}

func TestWorkareaArchiveRegistry_Diff_ExcludesDonmaiSubtree(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-x", tree: map[string]string{
		".donmai/state.json":  "private-a",
		".donmai/sub/log.txt": "private-a",
		"hello.txt":           "shared",
	}})
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-y", tree: map[string]string{
		".donmai/state.json":  "private-b",
		".donmai/sub/log.txt": "private-b",
		"hello.txt":           "shared",
	}})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	res, err := reg.Diff("wa-x", "wa-y")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Errorf(".donmai subtree must be excluded; got entries: %+v", res.Entries)
	}
}

func TestWorkareaArchiveRegistry_Diff_DeterministicOrder(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < 50; i++ {
		files[fmt.Sprintf("f-%03d.txt", i)] = fmt.Sprintf("content-%d", i)
	}
	writeFixtureArchive(t, root, fixtureArchive{id: "det-a", tree: files})
	files2 := map[string]string{}
	for k, v := range files {
		files2[k] = v + "-changed"
	}
	writeFixtureArchive(t, root, fixtureArchive{id: "det-b", tree: files2})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	res1, _ := reg.Diff("det-a", "det-b")
	res2, _ := reg.Diff("det-a", "det-b")
	if len(res1.Entries) != 50 {
		t.Fatalf("expected 50 modified entries, got %d", len(res1.Entries))
	}
	for i := range res1.Entries {
		if res1.Entries[i].Path != res2.Entries[i].Path {
			t.Errorf("non-deterministic order at %d: %q vs %q", i, res1.Entries[i].Path, res2.Entries[i].Path)
		}
	}
	// Verify sorted order
	for i := 1; i < len(res1.Entries); i++ {
		if res1.Entries[i-1].Path >= res1.Entries[i].Path {
			t.Errorf("entries not sorted at index %d: %q >= %q",
				i, res1.Entries[i-1].Path, res1.Entries[i].Path)
		}
	}
}

func TestWorkareaArchiveRegistry_Diff_EmptyOnSelf(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "self", tree: map[string]string{"a.txt": "x"}})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	res, err := reg.Diff("self", "self")
	if err != nil {
		t.Fatalf("self diff: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Errorf("self diff should be empty, got %+v", res.Entries)
	}
}

func TestWorkareaArchiveRegistry_Diff_MissingArchive(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "real"})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	if _, err := reg.Diff("real", "ghost"); !errors.Is(err, ErrArchiveNotFound) {
		t.Errorf("expected ErrArchiveNotFound for missing B, got %v", err)
	}
	if _, err := reg.Diff("ghost", "real"); !errors.Is(err, ErrArchiveNotFound) {
		t.Errorf("expected ErrArchiveNotFound for missing A, got %v", err)
	}
}

func TestWorkareaArchiveRegistry_DiffStream_EmitsEntriesOnce(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "stream-a", tree: map[string]string{
		"a.txt": "1",
		"b.txt": "2",
	}})
	writeFixtureArchive(t, root, fixtureArchive{id: "stream-b", tree: map[string]string{
		"a.txt": "1-different",
		"c.txt": "3",
	}})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})

	var got []afclient.WorkareaDiffEntry
	emit := func(e afclient.WorkareaDiffEntry) error {
		got = append(got, e)
		return nil
	}
	summary, err := reg.DiffStream("stream-a", "stream-b", emit)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 emitted entries, got %d: %+v", len(got), got)
	}
	if summary.Total != 3 {
		t.Errorf("summary total mismatch: %d", summary.Total)
	}
	// Order is path-sorted.
	wantOrder := []string{"a.txt", "b.txt", "c.txt"}
	for i, w := range wantOrder {
		if got[i].Path != w {
			t.Errorf("entry %d: want %q, got %q", i, w, got[i].Path)
		}
	}
}

func TestWorkareaArchiveRegistry_CountDiff(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "cnt-a", tree: map[string]string{"x": "1"}})
	writeFixtureArchive(t, root, fixtureArchive{id: "cnt-b", tree: map[string]string{"x": "2", "y": "3"}})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	count, err := reg.CountDiff("cnt-a", "cnt-b")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 { // x modified + y added
		t.Errorf("count: want 2, got %d", count)
	}
}

// ── Restore coverage ──────────────────────────────────────────────────────

func TestWorkareaArchiveRegistry_Restore_HappyPath(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id: "wa-restore",
		manifest: archiveManifest{
			SessionID:  "sess-from-archive",
			Repository: "github.com/acme/repo",
			Ref:        "main",
		},
		tree: map[string]string{
			"src/main.go": "package main",
			"go.sum":      "checksum",
		},
	})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	wa, retryAfter, err := reg.Restore("wa-restore", afclient.WorkareaRestoreRequest{Reason: "investigation"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if retryAfter != 0 {
		t.Errorf("retryAfter should be 0 on success, got %v", retryAfter)
	}
	if wa.ID == "wa-restore" {
		t.Errorf("restore must produce a NEW id distinct from archive id; got %q", wa.ID)
	}
	if !strings.HasPrefix(wa.ID, "wa-restore-restore-") {
		t.Errorf("expected lineage-bearing id, got %q", wa.ID)
	}
	if wa.Kind != afclient.WorkareaKindActive {
		t.Errorf("expected active kind, got %q", wa.Kind)
	}
	if wa.Status != afclient.WorkareaStatusReady {
		t.Errorf("expected ready status, got %q", wa.Status)
	}
	// Materialised tree should contain the expected files.
	if _, err := os.Stat(filepath.Join(wa.Path, "src/main.go")); err != nil {
		t.Errorf("restored tree missing src/main.go: %v", err)
	}
}

func TestWorkareaArchiveRegistry_Restore_PreservesIntoSessionId(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-x"})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	wa, _, err := reg.Restore("wa-x", afclient.WorkareaRestoreRequest{IntoSessionID: "sess-target"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if wa.SessionID != "sess-target" {
		t.Errorf("intoSessionId not preserved: %q", wa.SessionID)
	}
}

func TestWorkareaArchiveRegistry_Restore_ConflictOnDuplicateSession(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-c"})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	if _, _, err := reg.Restore("wa-c", afclient.WorkareaRestoreRequest{IntoSessionID: "sess-collide"}); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	_, _, err := reg.Restore("wa-c", afclient.WorkareaRestoreRequest{IntoSessionID: "sess-collide"})
	if !errors.Is(err, afclient.ErrConflict) {
		t.Errorf("expected ErrConflict on duplicate session, got %v", err)
	}
}

func TestWorkareaArchiveRegistry_Restore_PoolSaturation(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-busy"})
	guard := &fakePoolGuard{retryAfter: 30 * time.Second}
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root, PoolGuard: guard})
	_, retryAfter, err := reg.Restore("wa-busy", afclient.WorkareaRestoreRequest{})
	if !errors.Is(err, afclient.ErrUnavailable) {
		t.Errorf("expected ErrUnavailable, got %v", err)
	}
	if retryAfter != 30*time.Second {
		t.Errorf("expected 30s retry-after, got %v", retryAfter)
	}
}

func TestWorkareaArchiveRegistry_Restore_NotFound(t *testing.T) {
	root := t.TempDir()
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	if _, _, err := reg.Restore("ghost", afclient.WorkareaRestoreRequest{}); !errors.Is(err, ErrArchiveNotFound) {
		t.Errorf("expected ErrArchiveNotFound, got %v", err)
	}
}

func TestWorkareaArchiveRegistry_Restore_CorruptedArchive(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "wa-rotten")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	_, _, err := reg.Restore("wa-rotten", afclient.WorkareaRestoreRequest{})
	if !errors.Is(err, ErrArchiveCorrupted) {
		t.Errorf("expected ErrArchiveCorrupted, got %v", err)
	}
}

func TestWorkareaArchiveRegistry_RestorePreservesSymlinkWithoutFollowing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks unreliable on Windows CI")
	}
	root := t.TempDir()
	external := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFixtureArchive(t, root, fixtureArchive{
		id: "wa-symlink-restore", tree: map[string]string{"link": "symlink:" + external},
	})
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	restored, _, err := registry.Restore("wa-symlink-restore", afclient.WorkareaRestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(restored.Path, "link"))
	if err != nil || target != external {
		t.Fatalf("restored symlink target = %q, %v; want %q", target, err, external)
	}
	if body, err := os.ReadFile(external); err != nil || string(body) != "outside" {
		t.Fatalf("restore followed or changed external target: %q, %v", body, err)
	}
}

// ── Concurrency: restore + diff under load ────────────────────────────────

func TestWorkareaArchiveRegistry_ConcurrentRestoreAndDiff(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		writeFixtureArchive(t, root, fixtureArchive{
			id: fmt.Sprintf("wa-c-%d", i),
			tree: map[string]string{
				fmt.Sprintf("f-%d.txt", i): fmt.Sprintf("body-%d", i),
			},
		})
	}
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("wa-c-%d", i)
			if _, _, err := reg.Restore(id, afclient.WorkareaRestoreRequest{Reason: "concurrent"}); err != nil {
				t.Errorf("restore %s: %v", id, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			a := fmt.Sprintf("wa-c-%d", i)
			b := fmt.Sprintf("wa-c-%d", (i+1)%5)
			if _, err := reg.Diff(a, b); err != nil {
				t.Errorf("diff %s vs %s: %v", a, b, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestWorkareaArchiveRegistryArchivesAndRestoresWholeDeclaredRoot(t *testing.T) {
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
	acquisition, err := acquisitions.Begin("archive-session", "wa_archive_root", workarea.RootPath(sourceRoot), "docs", "")
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
	sparse := filepath.Join(acquisition.StagingRoot.String(), "web", "sparse.bin")
	sparseFile, err := os.OpenFile(sparse, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sparseFile.Seek(64<<20, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := sparseFile.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := sparseFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(acquisition.StagingRoot.String(), "web", "source.txt"), filepath.Join(acquisition.StagingRoot.String(), "docs", "source-hardlink.txt")); err != nil {
		t.Fatal(err)
	}
	record := workarea.NewDeclarationRecord(
		"archive-session", "wa_archive_root", normalized,
		map[string]string{"web": "aaa", "docs": "bbb"}, acquisition.Record.AcquisitionID,
	)
	if err := workarea.WriteDeclaration(t.Context(), acquisition.StagingRoot, record); err != nil {
		t.Fatal(err)
	}
	if _, err := acquisitions.Commit(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, AcquisitionStore: acquisitions})
	if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		AcquisitionID: acquisition.Record.AcquisitionID, WorkareaID: "wa_archive_root", SessionID: "archive-session",
		WorkareaRoot: sourceRoot, SelectedPath: filepath.Join(sourceRoot, "docs"),
	}); err != nil {
		t.Fatal(err)
	}
	for _, archivedPath := range []string{
		filepath.Join(archiveRoot, "wa_archive_root", "tree", ".workarea", "declaration.json"),
		filepath.Join(archiveRoot, "wa_archive_root", "tree", "web", "source.txt"),
		filepath.Join(archiveRoot, "wa_archive_root", "tree", "docs", "context.txt"),
	} {
		if _, err := os.Stat(archivedPath); err != nil {
			t.Fatalf("whole-root archive missing %q: %v", archivedPath, err)
		}
	}
	archivedTree := workarea.RootPath(filepath.Join(archiveRoot, "wa_archive_root", "tree"))
	archiveUsage, err := workarea.PhysicalUsage(archivedTree)
	if err != nil || archiveUsage >= 64<<20 {
		t.Fatalf("archive densified sparse allocation: usage=%d err=%v", archiveUsage, err)
	}
	archivedSource, _ := os.Stat(filepath.Join(archivedTree.String(), "web", "source.txt"))
	archivedHardlink, _ := os.Stat(filepath.Join(archivedTree.String(), "docs", "source-hardlink.txt"))
	if archivedSource == nil || archivedHardlink == nil || !os.SameFile(archivedSource, archivedHardlink) {
		t.Fatal("archive did not preserve hardlink identity")
	}
	archived, err := registry.GetV1("wa_archive_root")
	if err != nil {
		t.Fatal(err)
	}
	if archived.WorkareaRoot == "" || archived.Path != archived.RepositoryWorktreePath || filepath.Base(archived.Path) != "docs" || len(archived.Repositories) != 2 {
		t.Fatalf("archived root projection = %+v", archived)
	}
	if err := acquisitions.RemovePublishedRoot(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	liveManager, err := worktree.NewManager(worktree.Options{ParentDir: worktreeParent, RestoreSessionID: "archive-session"})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(archiveRoot, "wa_archive_root", "manifest.json")
	manifestBody, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest archiveManifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	originalDigest := manifest.TreeDigest
	manifest.OriginalRoot = filepath.Join(t.TempDir(), "manifest-controlled-root")
	manifest.TreeDigest = "sha256:mutated"
	mutatedBody, _ := json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, mutatedBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.Restore("wa_archive_root", afclient.WorkareaRestoreRequest{IntoSessionID: "archive-session"}); !errors.Is(err, ErrArchiveCorrupted) {
		t.Fatalf("mutable archive digest was trusted: %v", err)
	}
	manifest.TreeDigest = originalDigest
	validBody, _ := json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, validBody, 0o600); err != nil {
		t.Fatal(err)
	}
	restored, _, err := registry.RestoreV1("wa_archive_root", afclient.WorkareaRestoreRequest{IntoSessionID: "archive-session"})
	if err != nil {
		t.Fatal(err)
	}
	if restored.WorkareaRoot == "" || restored.Path != restored.RepositoryWorktreePath || filepath.Base(restored.Path) != "docs" || len(restored.Repositories) != 2 {
		t.Fatalf("restored root projection = %+v", restored)
	}
	if restored.WorkareaRoot != sourceRoot || restored.WorkareaRoot == manifest.OriginalRoot {
		t.Fatalf("restore trusted mutable OriginalRoot: restored=%q manifest=%q", restored.WorkareaRoot, manifest.OriginalRoot)
	}
	for _, restoredPath := range []string{
		filepath.Join(restored.WorkareaRoot, ".workarea", "declaration.json"),
		filepath.Join(restored.WorkareaRoot, "web", "source.txt"),
		filepath.Join(restored.WorkareaRoot, "docs", "context.txt"),
	} {
		if _, err := os.Stat(restoredPath); err != nil {
			t.Fatalf("whole-root restore missing %q: %v", restoredPath, err)
		}
	}
	restoredSource, _ := os.Stat(filepath.Join(restored.WorkareaRoot, "web", "source.txt"))
	restoredHardlink, _ := os.Stat(filepath.Join(restored.WorkareaRoot, "docs", "source-hardlink.txt"))
	if restoredSource == nil || restoredHardlink == nil || !os.SameFile(restoredSource, restoredHardlink) {
		t.Fatal("restore did not preserve hardlink identity")
	}
	restoredUsage, err := workarea.PhysicalUsage(workarea.RootPath(restored.WorkareaRoot))
	if err != nil || restoredUsage >= 64<<20 {
		t.Fatalf("restore densified sparse allocation: usage=%d err=%v", restoredUsage, err)
	}
	reentrySpec := worktree.ProvisionSpec{
		SessionID: "archive-session", RepoURL: "https://example.test/web.git", SourceRef: "main",
		Strategy: worktree.StrategyClone, RepositoryDeclaration: &declaration,
		ExecutorCapabilities: workarea.ExecutorWorkareaCapabilities{
			MultiRepositoryWorkareaProtocols: []workarea.Protocol{workarea.ProtocolSessionRootV1},
			RepositoryAuthorityEnforcement:   workarea.RepositoryAuthorityIsolatedReadOnlyV1,
		},
	}
	liveReenteredPath, err := liveManager.Provision(t.Context(), reentrySpec)
	if err != nil || liveReenteredPath != filepath.Join(sourceRoot, "docs") {
		t.Fatalf("live manager restored Provision re-entry = %q, %v", liveReenteredPath, err)
	}
	restartedManager, err := worktree.NewManager(worktree.Options{ParentDir: worktreeParent, RestoreSessionID: "archive-session"})
	if err != nil {
		t.Fatal(err)
	}
	restartedLayout, err := restartedManager.Layout("archive-session")
	if err != nil || restartedLayout.Root.String() != sourceRoot || filepath.Base(restartedLayout.Repository.String()) != "docs" {
		t.Fatalf("restored acquisition adoption = %+v, %v", restartedLayout, err)
	}
	reenteredPath, err := restartedManager.Provision(t.Context(), reentrySpec)
	if err != nil || reenteredPath != restartedLayout.Repository.String() {
		t.Fatalf("restored Provision re-entry = %q, %v", reenteredPath, err)
	}
}

func TestWorkareaArchiveRegistryPreservesLegacyFlatArchiveCompatibility(t *testing.T) {
	archiveRoot := t.TempDir()
	flat := filepath.Join(t.TempDir(), "legacy")
	if err := os.MkdirAll(filepath.Join(flat, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flat, "file"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot})
	spec := WorkareaRootArchiveSpec{WorkareaID: "wa_flat_archive", SessionID: "legacy-session", WorkareaRoot: flat, SelectedPath: flat}
	if err := registry.ArchiveRoot(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if err := registry.ArchiveRoot(t.Context(), spec); err != nil {
		t.Fatalf("idempotent legacy archive retry: %v", err)
	}
	restored, _, err := registry.RestoreV1("wa_flat_archive", afclient.WorkareaRestoreRequest{IntoSessionID: "legacy-restored"})
	if err != nil {
		t.Fatal(err)
	}
	if restored.WorkareaRoot == "" || restored.WorkareaRoot != restored.Path || restored.RepositoryWorktreePath != restored.Path || restored.SessionID != "legacy-restored" {
		t.Fatalf("legacy restored projection = %+v", restored)
	}
	if body, err := os.ReadFile(filepath.Join(restored.Path, "file")); err != nil || string(body) != "legacy" {
		t.Fatalf("legacy restored file = %q, %v", body, err)
	}
}

func TestWorkareaArchiveRegistryRefusesSourceRootSwapAfterAuthorization(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	acquisitions, err := workarea.NewAcquisitionStore(parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	declaration, err := (workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{{
			Source: workarea.RepositorySource{Repository: "repo", Ref: "main"}, Name: "repo",
			Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
		}},
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	acquisition, err := acquisitions.Begin("swap-session", "wa_swap", workarea.RootPath(root), "repo", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(acquisition.StagingRoot.String(), "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := workarea.WriteDeclaration(t.Context(), acquisition.StagingRoot, workarea.NewDeclarationRecord("swap-session", "wa_swap", declaration, nil, acquisition.Record.AcquisitionID)); err != nil {
		t.Fatal(err)
	}
	if _, err := acquisitions.Commit(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	moved := root + "-moved"
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{
		Root: t.TempDir(), AcquisitionStore: acquisitions,
		ArchiveHook: func(stage string) error {
			if stage != "after-authorize" {
				return nil
			}
			if err := os.Rename(root, moved); err != nil {
				return err
			}
			return os.Mkdir(root, 0o700)
		},
	})
	if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		AcquisitionID: acquisition.Record.AcquisitionID, WorkareaID: "wa_swap", SessionID: "swap-session",
		WorkareaRoot: root, SelectedPath: filepath.Join(root, "repo"),
	}); err == nil {
		t.Fatal("archive accepted a replacement root after authorization")
	}
	if _, err := os.Stat(filepath.Join(moved, ".workarea", "declaration.json")); err != nil {
		t.Fatalf("authorized original root was damaged: %v", err)
	}
}

// ── walkArchiveTree direct coverage ────────────────────────────────────────

func TestWalkArchiveTree_OrderingAndExclusion(t *testing.T) {
	tree := t.TempDir()
	for _, p := range []string{
		"z/zz.txt", "a/aa.txt", "m/mm.txt", ".donmai/private.txt", ".donmai/sub/x.txt",
	} {
		full := filepath.Join(tree, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o750)
		_ = os.WriteFile(full, []byte("hello"), 0o600)
	}
	entries, err := walkArchiveTree(tree)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
	}
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	for i, p := range paths {
		if p != sorted[i] {
			t.Errorf("path order mismatch at %d: %q vs %q", i, p, sorted[i])
		}
	}
	for _, p := range paths {
		if strings.HasPrefix(p, ".donmai") {
			t.Errorf("walk should exclude .donmai: %q", p)
		}
	}
}

// ── computeArchiveSize ─────────────────────────────────────────────────────

func TestComputeArchiveSize(t *testing.T) {
	tree := t.TempDir()
	_ = os.WriteFile(filepath.Join(tree, "a.txt"), []byte("01234"), 0o600) // 5 bytes
	_ = os.WriteFile(filepath.Join(tree, "b.txt"), []byte("xx"), 0o600)    // 2 bytes
	got, err := computeArchiveSize(tree)
	if err != nil {
		t.Fatalf("size: %v", err)
	}
	if got != 7 {
		t.Errorf("size: want 7, got %d", got)
	}
}

// ── Test doubles ───────────────────────────────────────────────────────────

type fakeActiveProvider struct {
	members []afclient.WorkareaSummary
}

func (f *fakeActiveProvider) ActiveWorkareas() []afclient.WorkareaSummary { return f.members }

type fakePoolGuard struct {
	retryAfter time.Duration
	err        error
}

func (f *fakePoolGuard) CheckCapacity() (time.Duration, error) {
	return f.retryAfter, f.err
}
