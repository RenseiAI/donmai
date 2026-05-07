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

	"github.com/RenseiAI/agentfactory-tui/afclient"
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
	if err := os.MkdirAll(filepath.Join(dir, "tree"), 0o755); err != nil {
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
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatalf("mkdir parent: %v", err)
			}
			if err := os.Symlink(target, full); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
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
	if err := os.MkdirAll(filepath.Join(corrupt, "tree"), 0o755); err != nil {
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
	if err := os.MkdirAll(filepath.Join(dir, "tree"), 0o755); err != nil {
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

func TestWorkareaArchiveRegistry_Diff_ExcludesRenseiSubtree(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-x", tree: map[string]string{
		".rensei/state.json":  "private-a",
		".rensei/sub/log.txt": "private-a",
		"hello.txt":           "shared",
	}})
	writeFixtureArchive(t, root, fixtureArchive{id: "wa-y", tree: map[string]string{
		".rensei/state.json":  "private-b",
		".rensei/sub/log.txt": "private-b",
		"hello.txt":           "shared",
	}})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	res, err := reg.Diff("wa-x", "wa-y")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(res.Entries) != 0 {
		t.Errorf(".rensei subtree must be excluded; got entries: %+v", res.Entries)
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

// ── walkArchiveTree direct coverage ────────────────────────────────────────

func TestWalkArchiveTree_OrderingAndExclusion(t *testing.T) {
	tree := t.TempDir()
	for _, p := range []string{
		"z/zz.txt", "a/aa.txt", "m/mm.txt", ".rensei/private.txt", ".rensei/sub/x.txt",
	} {
		full := filepath.Join(tree, p)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
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
		if strings.HasPrefix(p, ".rensei") {
			t.Errorf("walk should exclude .rensei: %q", p)
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
