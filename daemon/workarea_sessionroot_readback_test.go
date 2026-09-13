package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// Slice two: session-root restore readback. A committed session-root restore
// re-enters the SAME owning workarea/session identity under its acquisition
// authority. List/show must surface that verified ready state through the
// unmodified registry handlers with no fake provider injection and no legacy
// sidecar flattening. All fixtures live in tempdirs.

type sessionRootReadbackFixtureBundle struct {
	registry       *WorkareaArchiveRegistry
	archiveRoot    string
	worktreeParent string
	sourceRoot     string
	acquisitionID  string
}

// sessionRootReadbackFixture builds a Ready, archive-bound session-root
// generation and archives it. The returned registry resolves its acquisition
// authority lazily from WorktreeParent only: no injected store, no fake
// live provider.
func sessionRootReadbackFixture(t *testing.T, selected string) *sessionRootReadbackFixtureBundle {
	t.Helper()
	archiveRoot := t.TempDir()
	worktreeParent := t.TempDir()
	sourceRoot := filepath.Join(worktreeParent, "session-root")
	declaration := workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: "https://example.test/web.git", Ref: "main"}, Name: "web", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: "https://example.test/docs.git", Ref: "main"}, Name: "docs", Role: workarea.RepositoryRoleContext, Authority: workarea.RepositoryReadOnly},
		},
		Select: &workarea.RepositoryFilter{Kind: workarea.RepositoryFilterNamed, Name: selected},
	}
	normalized, err := declaration.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	acquisitions, err := workarea.NewAcquisitionStore(worktreeParent, nil)
	if err != nil {
		t.Fatal(err)
	}
	acquisition, err := acquisitions.Begin("archive-session", "wa_session_readback", workarea.RootPath(sourceRoot), selected, "")
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
		"archive-session", "wa_session_readback", normalized,
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
		AcquisitionID: acquisition.Record.AcquisitionID, WorkareaID: "wa_session_readback", SessionID: "archive-session",
		WorkareaRoot: sourceRoot, SelectedPath: filepath.Join(sourceRoot, selected),
	}); err != nil {
		t.Fatal(err)
	}
	// The readback registry below opens its own authority handle and proves
	// discovery from durable state, not shared memory.
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, WorktreeParent: worktreeParent})
	return &sessionRootReadbackFixtureBundle{
		registry: registry, archiveRoot: archiveRoot,
		worktreeParent: worktreeParent, sourceRoot: sourceRoot,
		acquisitionID: acquisition.Record.AcquisitionID,
	}
}

// TestSessionRootReadback_ListsSameIdentityAfterArchive is the RED control
// for slice two: a Ready, archive-bound session-root generation must appear
// in ListV1 as an active/ready row under its SAME workarea/session identity,
// resolved through unmodified handlers with no injected provider.
func TestSessionRootReadback_ListsSameIdentityAfterArchive(t *testing.T) {
	fix := sessionRootReadbackFixture(t, "docs")

	active, _, err := fix.registry.ListV1()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *afclient.WorkareaSummaryV1
	for i := range active {
		if active[i].ID == "wa_session_readback" {
			found = &active[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("ListV1 omits ready session-root id %q (active=%d)", "wa_session_readback", len(active))
	}
	if found.Kind != afclient.WorkareaKindActive || found.Status != afclient.WorkareaStatusReady {
		t.Errorf("list entry kind/status = %q/%q, want active/ready", found.Kind, found.Status)
	}
	if found.SessionID != "archive-session" {
		t.Errorf("list entry session = %q, want archive-session", found.SessionID)
	}
	if found.WorkareaRoot != fix.sourceRoot {
		t.Errorf("list entry root = %q, want committed final root %q", found.WorkareaRoot, fix.sourceRoot)
	}
}

// TestSessionRootReadback_ShowResolvesSameIdentity is the show control: a
// verified ready session-root row resolves as the same-identity active
// entry, while the on-disk archive record still reads as archived through
// ArchiveV1.
func TestSessionRootReadback_ShowResolvesSameIdentity(t *testing.T) {
	fix := sessionRootReadbackFixture(t, "docs")

	archived, err := fix.registry.ArchiveV1("wa_session_readback")
	if err != nil {
		t.Fatalf("ArchiveV1: %v", err)
	}
	if archived.Kind != afclient.WorkareaKindArchived || archived.Status != afclient.WorkareaStatusArchived {
		t.Errorf("archive record = %q/%q, want archived/archived", archived.Kind, archived.Status)
	}

	got, err := fix.registry.GetV1("wa_session_readback")
	if err != nil {
		t.Fatalf("GetV1: %v", err)
	}
	if got.ID != "wa_session_readback" || got.Kind != afclient.WorkareaKindActive || got.Status != afclient.WorkareaStatusReady {
		t.Errorf("show identity = %+v, want active/ready wa_session_readback", got)
	}
	if got.SessionID != "archive-session" {
		t.Errorf("show session = %q, want archive-session", got.SessionID)
	}
	if got.WorkareaRoot != fix.sourceRoot || got.Path == "" {
		t.Errorf("show path/root mismatch: path=%q root=%q", got.Path, got.WorkareaRoot)
	}
	if got.ArchiveLocation != filepath.Join(fix.archiveRoot, "wa_session_readback") {
		t.Errorf("show lineage = %q", got.ArchiveLocation)
	}

	// On-disk archives still resolve first: the archive row itself reads as
	// archived from its own manifest.
	manifest, err := os.ReadFile(filepath.Join(fix.archiveRoot, "wa_session_readback", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !containsBytes(manifest, []byte("donmai.workarea-archive.v1")) {
		t.Errorf("archive manifest lost its session-root schema: %s", manifest)
	}
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// TestSessionRootReadback_RecreatedRegistryRediscovers proves durability: a
// recreated registry against the same roots rediscovers the same identity
// exactly once with no in-memory state, no fake provider, and no tree change.
func TestSessionRootReadback_RecreatedRegistryRediscovers(t *testing.T) {
	fix := sessionRootReadbackFixture(t, "docs")
	before := snapshotWorktreeParent(t, fix.worktreeParent)

	reg2 := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: fix.archiveRoot, WorktreeParent: fix.worktreeParent})
	active, _, err := reg2.ListV1()
	if err != nil {
		t.Fatalf("recreated list: %v", err)
	}
	seen := 0
	for i := range active {
		if active[i].ID == "wa_session_readback" {
			seen++
			if active[i].WorkareaRoot != fix.sourceRoot || active[i].SessionID != "archive-session" {
				t.Errorf("recreated entry drifted: %+v", active[i])
			}
		}
	}
	if seen != 1 {
		t.Errorf("recreated ListV1 shows id %d times, want exactly 1", seen)
	}
	got, err := reg2.GetV1("wa_session_readback")
	if err != nil {
		t.Fatalf("recreated GetV1: %v", err)
	}
	if got.ID != "wa_session_readback" || got.WorkareaRoot != fix.sourceRoot {
		t.Errorf("recreated show drifted: %+v", got)
	}
	assertWorktreeParentUnchanged(t, fix.worktreeParent, before)
}

// TestSessionRootReadback_LegacyOnlyHostCreatesNoStore proves the reader
// never activates negotiated metadata: a legacy-flat-only host lists only
// the legacy restore row and gains no acquisition store directory.
func TestSessionRootReadback_LegacyOnlyHostCreatesNoStore(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id: "wa-legacy-only", tree: map[string]string{"hello.txt": "world"},
	})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	restored, _, err := reg.RestoreV1("wa-legacy-only", afclient.WorkareaRestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	legacyParent := t.TempDir()
	reg2 := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root, WorktreeParent: legacyParent})
	active, _, err := reg2.ListV1()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := false
	for _, entry := range active {
		if entry.ID == restored.ID {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("legacy restore row %q missing from list", restored.ID)
	}
	if _, statErr := os.Lstat(filepath.Join(legacyParent, ".workarea-acquisitions")); !os.IsNotExist(statErr) {
		t.Fatalf("readback created store on legacy-only host: %v", statErr)
	}
}

// TestSessionRootReadback_ProviderWinsWithoutDuplicating locks pool
// accounting for archive-bound rows: a live provider entry for the same ID
// is served once (provider wins), and a live session for another ID
// coexists with the readback entry.
func TestSessionRootReadback_ProviderWinsWithoutDuplicating(t *testing.T) {
	fix := sessionRootReadbackFixture(t, "docs")
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{
		Root: fix.archiveRoot, WorktreeParent: fix.worktreeParent,
		ActiveProvider: &fakeActiveProvider{members: []afclient.WorkareaSummary{{
			ID: "wa_session_readback", Kind: afclient.WorkareaKindActive, Status: afclient.WorkareaStatusReady,
		}, {
			ID: "sess-live-other", Kind: afclient.WorkareaKindActive, Status: afclient.WorkareaStatusReady,
		}}},
	})
	active, _, err := reg.ListV1()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	count := 0
	other := false
	for _, entry := range active {
		if entry.ID == "wa_session_readback" {
			count++
		}
		if entry.ID == "sess-live-other" {
			other = true
		}
	}
	if count != 1 {
		t.Errorf("session-root id listed %d times, want exactly 1 (provider wins, no double-count)", count)
	}
	if !other {
		t.Errorf("live provider entry sess-live-other missing from list")
	}
}

func snapshotWorktreeParent(t *testing.T, parent string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	// Test-only content snapshot of a TempDir-owned tree; paths come from
	// WalkDir itself, not external input.
	err := filepath.WalkDir(parent, func(path string, entry os.DirEntry, walkErr error) error { //nolint:gosec
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(parent, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		out[rel] = info.Mode().String()
		if !info.IsDir() && info.Mode().IsRegular() {
			//nolint:gosec // test-only TempDir snapshot; path comes from WalkDir itself.
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[rel] += "\x00" + string(body)
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] += "\x00->" + target
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertWorktreeParentUnchanged(t *testing.T, parent string, before map[string]string) {
	t.Helper()
	after := snapshotWorktreeParent(t, parent)
	if len(after) != len(before) {
		t.Fatalf("durable tree changed: %d entries before, %d after", len(before), len(after))
	}
	for name, mode := range before {
		if after[name] != mode {
			t.Fatalf("durable entry %q changed", name)
		}
	}
}

// TestSessionRootReadback_RealRestoreThenListShow runs the full authorized
// extension loop through the unmodified RestoreV1 handler: release a Ready
// session-root generation, restore its archive (same identity re-entry),
// then prove list/show discovery with zero tree mutation across the reads.
func TestSessionRootReadback_RealRestoreThenListShow(t *testing.T) {
	fix := sessionRootReadbackFixture(t, "docs")

	// Release the committed root through its authority handle so the
	// archive is restorable, then close it — the registry under test
	// resolves everything from durable state.
	authority, found, err := workarea.OpenExistingAcquisitionStore(fix.worktreeParent, nil)
	if err != nil || !found {
		t.Fatalf("open authority = (%v, %v, %v)", authority, found, err)
	}
	committed, err := authority.RecordForAcquisitionID(fix.acquisitionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.RemovePublishedRoot(committed.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	before := snapshotWorktreeParent(t, fix.worktreeParent)

	restored, _, err := fix.registry.RestoreV1("wa_session_readback", afclient.WorkareaRestoreRequest{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.ID != "wa_session_readback" || restored.SessionID != "archive-session" {
		t.Fatalf("restore must re-enter the same identity, got %+v", restored)
	}
	if restored.Kind != afclient.WorkareaKindActive || restored.Status != afclient.WorkareaStatusReady {
		t.Fatalf("restore kind/status = %q/%q, want active/ready", restored.Kind, restored.Status)
	}

	active, _, err := fix.registry.ListV1()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := 0
	for i := range active {
		if active[i].ID == "wa_session_readback" {
			seen++
			if active[i].SessionID != "archive-session" || active[i].WorkareaRoot != fix.sourceRoot {
				t.Errorf("list entry drifted: %+v", active[i])
			}
		}
	}
	if seen != 1 {
		t.Errorf("ListV1 shows restored id %d times, want exactly 1", seen)
	}
	got, err := fix.registry.GetV1("wa_session_readback")
	if err != nil {
		t.Fatalf("GetV1: %v", err)
	}
	if got.ID != "wa_session_readback" || got.Kind != afclient.WorkareaKindActive || got.WorkareaRoot != fix.sourceRoot {
		t.Errorf("show drifted: %+v", got)
	}
	// The reads above must not mutate durable state beyond the restore
	// itself: snapshot after restore, re-read, compare.
	afterRestore := snapshotWorktreeParent(t, fix.worktreeParent)
	if _, _, err := fix.registry.ListV1(); err != nil {
		t.Fatal(err)
	}
	if _, err := fix.registry.GetV1("wa_session_readback"); err != nil {
		t.Fatal(err)
	}
	assertWorktreeParentUnchanged(t, fix.worktreeParent, afterRestore)
	_ = before
}
