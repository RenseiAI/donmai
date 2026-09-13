package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

// Slice one: legacy-flat restore readback. A successful
// legacy-flat restore commits its tree plus a durable sidecar and returns a
// fresh active ID. List/show must resolve that committed state with no fake
// active-provider injection. All fixtures live in tempdirs; the source
// archive under test is never mutated.

// TestRestoreLegacy_ListAndShowDiscoverFreshID is the registry-level control:
// restore -> ListV1 contains the fresh ID -> GetV1 resolves it -> a recreated
// registry against the same root still resolves it without new state.
func TestRestoreLegacy_ListAndShowDiscoverFreshID(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id: "wa-readback",
		manifest: archiveManifest{
			SessionID:  "sess-original",
			Repository: "github.com/acme/repo",
			Ref:        "main",
		},
		tree: map[string]string{"hello.txt": "world"},
	})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	restored, _, err := reg.RestoreV1("wa-readback", afclient.WorkareaRestoreRequest{
		Reason: "investigation", IntoSessionID: "sess-target",
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	newID := restored.ID
	if newID == "wa-readback" || !strings.HasPrefix(newID, "wa-readback-restore-") {
		t.Fatalf("restore must mint a lineage-bearing fresh id, got %q", newID)
	}

	active, archived, err := reg.ListV1()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *afclient.WorkareaSummaryV1
	for i := range active {
		if active[i].ID == newID {
			found = &active[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("ListV1 omits restored id %q (active=%d archived=%d)", newID, len(active), len(archived))
	}
	if found.Kind != afclient.WorkareaKindActive || found.Status != afclient.WorkareaStatusReady {
		t.Errorf("list entry kind/status = %q/%q, want active/ready", found.Kind, found.Status)
	}
	if found.SessionID != "sess-target" {
		t.Errorf("list entry session = %q, want sess-target", found.SessionID)
	}
	if found.WorkareaRoot == "" || found.WorkareaRoot == reg.treeDir("wa-readback") {
		t.Errorf("list entry root must be the committed restored root, got %q", found.WorkareaRoot)
	}

	got, err := reg.GetV1(newID)
	if err != nil {
		t.Fatalf("GetV1(%q): %v", newID, err)
	}
	if got.ID != newID || got.Kind != afclient.WorkareaKindActive || got.Status != afclient.WorkareaStatusReady {
		t.Errorf("show identity = %+v, want active/ready %q", got, newID)
	}
	if got.SessionID != "sess-target" {
		t.Errorf("show session = %q, want sess-target", got.SessionID)
	}
	if got.WorkareaRoot != found.WorkareaRoot || got.Path == "" {
		t.Errorf("show path/root mismatch: path=%q root=%q", got.Path, got.WorkareaRoot)
	}
	if got.ArchiveLocation != reg.archiveDir("wa-readback") {
		t.Errorf("show lineage = %q, want archive dir", got.ArchiveLocation)
	}

	// Recreate the registry against the same durable records: discovery must
	// persist with no in-memory append and no duplicate entries.
	reg2 := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	active2, _, err := reg2.ListV1()
	if err != nil {
		t.Fatalf("recreated list: %v", err)
	}
	seen := 0
	for i := range active2 {
		if active2[i].ID == newID {
			seen++
			if active2[i].WorkareaRoot != found.WorkareaRoot || active2[i].SessionID != "sess-target" {
				t.Errorf("recreated entry drifted: %+v", active2[i])
			}
		}
	}
	if seen != 1 {
		t.Errorf("recreated ListV1 shows id %d times, want exactly 1", seen)
	}
	got2, err := reg2.GetV1(newID)
	if err != nil {
		t.Fatalf("recreated GetV1: %v", err)
	}
	if got2.ID != newID || got2.WorkareaRoot != found.WorkareaRoot {
		t.Errorf("recreated show drifted: %+v", got2)
	}
}

// TestRestoreLegacy_HTTPListShowWithoutFakeProvider is the end-to-end control:
// unmodified POST restore -> GET list -> GET show by the returned ID, with no
// post-restore provider injection.
func TestRestoreLegacy_HTTPListShowWithoutFakeProvider(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id:       "wa-http-readback",
		manifest: archiveManifest{SessionID: "sess-original"},
		tree:     map[string]string{"hello.txt": "world"},
	})
	hsrv := newServerForWorkareaTest(t, root, 0)

	resp, err := http.Post(hsrv.URL+"/api/daemon/workareas/wa-http-readback/restore",
		"application/json", strings.NewReader(`{"reason":"investigation"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("restore status: want 201, got %d", resp.StatusCode)
	}
	var result afclient.WorkareaRestoreV1Result
	decodeBody(t, resp, &result)
	newID := result.Workarea.ID

	listResp, err := http.Get(hsrv.URL + "/api/daemon/workareas")
	if err != nil {
		t.Fatal(err)
	}
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status: want 200, got %d", listResp.StatusCode)
	}
	var listed afclient.ListWorkareasV1Response
	decodeBody(t, listResp, &listed)
	listedOnce := false
	for _, entry := range listed.Active {
		if entry.ID == newID {
			if listedOnce {
				t.Fatalf("list duplicates restored id %q", newID)
			}
			listedOnce = true
			if entry.Kind != afclient.WorkareaKindActive || entry.Status != afclient.WorkareaStatusReady {
				t.Errorf("list entry = %q/%q, want active/ready", entry.Kind, entry.Status)
			}
		}
	}
	if !listedOnce {
		t.Fatalf("list omits restored id %q (active=%d archived=%d)", newID, len(listed.Active), len(listed.Archived))
	}

	showResp, err := http.Get(hsrv.URL + "/api/daemon/workareas/" + newID)
	if err != nil {
		t.Fatal(err)
	}
	if showResp.StatusCode != http.StatusOK {
		t.Fatalf("show status: want 200, got %d", showResp.StatusCode)
	}
	var env afclient.WorkareaV1Envelope
	decodeBody(t, showResp, &env)
	if env.Workarea.ID != newID || env.Workarea.Kind != afclient.WorkareaKindActive {
		t.Errorf("show = %+v, want active %q", env.Workarea, newID)
	}
}

// TestRestoreLegacy_UntrustedStateNeverManufacturesReadyEntry locks the
// fail-closed readback: corrupt sidecars, missing roots, symlink-escaped
// roots, and wrong identities must omit list entries and 404 on show.
func TestRestoreLegacy_UntrustedStateNeverManufacturesReadyEntry(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id: "wa-untrusted", tree: map[string]string{"hello.txt": "world"},
	})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	restored, _, err := reg.RestoreV1("wa-untrusted", afclient.WorkareaRestoreRequest{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	newID := restored.ID
	sidecarPath := filepath.Join(reg.restoredDir(), newID+".json")
	if filepath.Dir(sidecarPath) != filepath.Clean(reg.restoredDir()) {
		t.Fatalf("restored sidecar escaped restored dir: %q", sidecarPath)
	}
	//nolint:gosec // sidecarPath is pinned to the registry's own restored dir above.
	mustList := func() []afclient.WorkareaSummaryV1 {
		t.Helper()
		active, _, err := reg.ListV1()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		return active
	}
	mustOmit := func(stage string) {
		t.Helper()
		for _, entry := range mustList() {
			if entry.ID == newID {
				t.Fatalf("%s: list manufactures entry %+v", stage, entry)
			}
		}
		if _, err := reg.GetV1(newID); !errors.Is(err, ErrArchiveNotFound) {
			t.Fatalf("%s: GetV1 = %v, want ErrArchiveNotFound", stage, err)
		}
	}

	// Corrupt sidecar: discovery disappears, archive itself is untouched.
	original, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, []byte("{bad"), 0o600); err != nil { //nolint:gosec
		t.Fatal(err)
	}
	mustOmit("corrupt sidecar")
	if err := os.WriteFile(sidecarPath, original, 0o600); err != nil { //nolint:gosec
		t.Fatal(err)
	}

	// Missing restored root: no valid ready entry.
	dest := filepath.Join(reg.restoredDir(), newID)
	if err := os.RemoveAll(dest); err != nil {
		t.Fatal(err)
	}
	mustOmit("missing root")
	// Symlink-escaped root: must never resolve to outside content.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "evil.txt"), []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dest); err != nil {
		t.Fatal(err)
	}
	mustOmit("symlink-escaped root")
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}

	// Wrong identity: show by an unknown id stays NotFound even with valid
	// committed state present.
	if _, err := reg.GetV1(newID + "-ghost"); !errors.Is(err, ErrArchiveNotFound) {
		t.Fatalf("wrong identity GetV1 = %v, want ErrArchiveNotFound", err)
	}
	// And the genuine entry is back once state is intact.
	if _, err := reg.GetV1(newID); err != nil {
		t.Fatalf("intact GetV1: %v", err)
	}
}

// TestRestoreLegacy_LiveProviderWinsWithoutDuplicating locks pool accounting:
// a live provider entry for the same ID is served once (provider wins), and
// a live session for another ID coexists with the restored entry.
func TestRestoreLegacy_LiveProviderWinsWithoutDuplicating(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id: "wa-dedupe", tree: map[string]string{"hello.txt": "world"},
	})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	restored, _, err := reg.RestoreV1("wa-dedupe", afclient.WorkareaRestoreRequest{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	newID := restored.ID

	reg2 := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{
		Root: root,
		ActiveProvider: &fakeActiveProvider{members: []afclient.WorkareaSummary{{
			ID: newID, Kind: afclient.WorkareaKindActive, Status: afclient.WorkareaStatusReady,
		}, {
			ID: "sess-live-other", Kind: afclient.WorkareaKindActive, Status: afclient.WorkareaStatusReady,
		}}},
	})
	active, _, err := reg2.ListV1()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	count := 0
	other := false
	for _, entry := range active {
		if entry.ID == newID {
			count++
		}
		if entry.ID == "sess-live-other" {
			other = true
		}
	}
	if count != 1 {
		t.Errorf("restored id listed %d times, want exactly 1 (provider wins, no double-count)", count)
	}
	if !other {
		t.Errorf("live provider entry sess-live-other missing from list")
	}
}

// TestRestoreSessionRoot_ReadbackPreservesCanonicalIdentity locks the
// session-root contract: the restore re-enters the SAME owning
// workarea/session identity, mints no replacement, registers no fake live
// session, and never flattens into a legacy sidecar entry. The verified
// ready row is discoverable through ListV1 and GetV1 under that same
// identity; the on-disk archive record still reads as archived through
// ArchiveV1.
func TestRestoreSessionRoot_ReadbackPreservesCanonicalIdentity(t *testing.T) {
	registry, _ := sessionRootArchiveFixture(t, "docs")
	restored, _, err := registry.RestoreV1("wa_archive_metadata", afclient.WorkareaRestoreRequest{IntoSessionID: "archive-session"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.ID != "wa_archive_metadata" || restored.SessionID != "archive-session" {
		t.Fatalf("session-root restore must re-enter the same identity, got %+v", restored)
	}
	if restored.Kind != afclient.WorkareaKindActive || restored.Status != afclient.WorkareaStatusReady {
		t.Fatalf("session-root restore kind/status = %q/%q, want active/ready", restored.Kind, restored.Status)
	}

	// The verified ready row is discoverable under the same identity — no
	// legacy sidecar, no fake live session, no replacement id.
	active, _, err := registry.ListV1()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := 0
	for _, entry := range active {
		if entry.ID == "wa_archive_metadata" {
			seen++
			if entry.Kind != afclient.WorkareaKindActive || entry.Status != afclient.WorkareaStatusReady {
				t.Fatalf("list entry kind/status = %q/%q, want active/ready", entry.Kind, entry.Status)
			}
			if entry.SessionID != "archive-session" {
				t.Fatalf("list entry session = %q, want archive-session", entry.SessionID)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("list shows same-identity ready row %d times, want exactly 1", seen)
	}
	got, err := registry.GetV1("wa_archive_metadata")
	if err != nil {
		t.Fatalf("GetV1: %v", err)
	}
	if got.ID != "wa_archive_metadata" || got.Kind != afclient.WorkareaKindActive || got.Status != afclient.WorkareaStatusReady || got.SessionID != "archive-session" {
		t.Fatalf("show drifted from canonical identity: %+v", got)
	}
	// The canonical archived record still reads as archived through the
	// archive-only path.
	archived, err := registry.ArchiveV1("wa_archive_metadata")
	if err != nil {
		t.Fatalf("GetV1 archive: %v", err)
	}
	if archived.Kind != afclient.WorkareaKindArchived {
		t.Errorf("archive record kind = %q, want archived", archived.Kind)
	}
	var raw map[string]any
	manifestBody, err := os.ReadFile(filepath.Join(registry.Root(), "wa_archive_metadata", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifestBody, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["schemaVersion"] == workareaArchiveLegacyFlatV1 {
		t.Errorf("session-root manifest was flattened to legacy-flat")
	}
}

// TestRestoreLegacy_UnreadableSidecarSkipsEntryKeepsValidRows is the list
// availability control: one I/O-unreadable per-entry sidecar must not abort
// ListV1 — the valid archive row and the valid restore row still list,
// and the bad id stays NotFound on exact show.
func TestRestoreLegacy_UnreadableSidecarSkipsEntryKeepsValidRows(t *testing.T) {
	root := t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{
		id: "wa-list-avail", tree: map[string]string{"hello.txt": "world"},
	})
	reg := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	restored, _, err := reg.RestoreV1("wa-list-avail", afclient.WorkareaRestoreRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Plant an I/O-invalid per-entry sidecar: a directory at the .json path
	// makes ReadFile fail for every user (no chmod/root ambiguity).
	badPath := filepath.Join(reg.restoredDir(), "wa-list-avail-restore-zzz.json")
	if err := os.MkdirAll(badPath, 0o700); err != nil {
		t.Fatal(err)
	}

	active, archived, err := reg.ListV1()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	seenRestore := false
	for _, entry := range active {
		if entry.ID == restored.ID {
			seenRestore = true
		}
	}
	if !seenRestore {
		t.Fatalf("valid restore row %q hidden by bad sidecar (active=%d)", restored.ID, len(active))
	}
	seenArchive := false
	for _, entry := range archived {
		if entry.ID == "wa-list-avail" {
			seenArchive = true
		}
	}
	if !seenArchive {
		t.Fatalf("valid archive row hidden by bad sidecar (archived=%d)", len(archived))
	}
	if _, err := reg.GetV1("wa-list-avail-restore-zzz"); !errors.Is(err, ErrArchiveNotFound) {
		t.Fatalf("bad sidecar GetV1 = %v, want ErrArchiveNotFound", err)
	}
}
