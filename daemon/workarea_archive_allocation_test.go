package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// Physical allocation is filesystem accounting, not archive identity. These
// tests change a committed archive's allocated blocks without changing a
// byte, the way delayed allocation and speculative preallocation do on Linux
// filesystems, and require commit, retry and restore to accept the archive.
// The tree digest still refuses an archive whose bytes changed.

const archiveAllocationGrowth = 1 << 20

func requireArchiveAllocationGrowth(t *testing.T) {
	t.Helper()
	if !archiveAllocationGrowthSupported {
		t.Skip("this platform has no allocate-past-EOF primitive for the drift fixture")
	}
}

type allocationCommitCase struct {
	name             string
	workareaID       string
	terminalResultID string
}

var allocationCommitCases = []allocationCommitCase{
	{name: "with terminal result", workareaID: "wa_55555555555555555555555555555555", terminalResultID: "tr_55555555555555555555555555555555"},
	{name: "without terminal result", workareaID: "wa_66666666666666666666666666666666"},
}

// markerWorkareaSpec builds the flat terminal workarea a released lease
// archives: one root holding one small marker file.
func markerWorkareaSpec(t *testing.T, c allocationCommitCase) WorkareaRootArchiveSpec {
	t.Helper()
	root := filepath.Join(t.TempDir(), "worktrees", "55555555-5555-4555-8555-555555555555")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "marker.txt"), []byte("terminal workarea marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return WorkareaRootArchiveSpec{
		WorkareaID: c.workareaID, SessionID: "55555555-5555-4555-8555-555555555555",
		WorkareaRoot: root, SelectedPath: root, TerminalResultID: c.terminalResultID,
	}
}

func archivedMarker(archiveRoot, workareaID string) string {
	return filepath.Join(archiveRoot, workareaID, "tree", "marker.txt")
}

// requireAllocationDrift proves the fixture really moved the archive's
// physical allocation away from the size the manifest recorded at commit.
func requireAllocationDrift(t *testing.T, registry *WorkareaArchiveRegistry, workareaID string) {
	t.Helper()
	manifest, err := registry.readManifest(workareaID)
	if err != nil {
		t.Fatalf("read committed manifest: %v", err)
	}
	physical, err := workarea.PhysicalUsage(workarea.RootPath(registry.treeDir(workareaID)))
	if err != nil {
		t.Fatalf("account committed tree: %v", err)
	}
	if physical <= manifest.SizeBytes {
		t.Fatalf("fixture did not grow allocation: physical %d, manifest %d", physical, manifest.SizeBytes)
	}
}

func requireCommitReference(t *testing.T, c allocationCommitCase, reference *workarea.TerminalArchiveReference) {
	t.Helper()
	if c.terminalResultID == "" {
		if reference != nil {
			t.Fatalf("commit without a terminal result invented a reference: %+v", reference)
		}
		return
	}
	if reference == nil || reference.TerminalResultID != c.terminalResultID || reference.ArchiveID != c.workareaID {
		t.Fatalf("committed reference = %+v", reference)
	}
}

func TestCommitArchiveRootAcceptsAllocationChangeAfterPublish(t *testing.T) {
	t.Parallel()
	requireArchiveAllocationGrowth(t)
	for _, c := range allocationCommitCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			archiveRoot := t.TempDir()
			registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, ArchiveHook: func(stage string) error {
				if stage == "after-publish-archive" {
					return growFileAllocation(archivedMarker(archiveRoot, c.workareaID), archiveAllocationGrowth)
				}
				return nil
			}})
			spec := markerWorkareaSpec(t, c)
			reference, err := registry.CommitArchiveRoot(t.Context(), spec)
			if err != nil {
				t.Fatalf("commit with unchanged bytes and changed allocation: %v", err)
			}
			requireCommitReference(t, c, reference)
			requireAllocationDrift(t, registry, c.workareaID)
		})
	}
}

func TestCommitArchiveRootRetryAcceptsAllocationChangeSinceCommit(t *testing.T) {
	t.Parallel()
	requireArchiveAllocationGrowth(t)
	for _, c := range allocationCommitCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			archiveRoot := t.TempDir()
			registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot})
			spec := markerWorkareaSpec(t, c)
			first, err := registry.CommitArchiveRoot(t.Context(), spec)
			if err != nil {
				t.Fatalf("first commit: %v", err)
			}
			if err := growFileAllocation(archivedMarker(archiveRoot, c.workareaID), archiveAllocationGrowth); err != nil {
				t.Fatal(err)
			}
			requireAllocationDrift(t, registry, c.workareaID)
			// A release that committed and then failed later is retried; the
			// retry must find the same committed bytes, not a collision.
			again, err := registry.CommitArchiveRoot(t.Context(), spec)
			if err != nil {
				t.Fatalf("retry over the same committed bytes: %v", err)
			}
			requireCommitReference(t, c, again)
			if first != nil && !again.IdentityEqual(*first) {
				t.Fatalf("retry reference = %+v, want %+v", again, first)
			}
		})
	}
}

func TestRestoreSessionRootAcceptsAllocationChangeSinceCommit(t *testing.T) {
	t.Parallel()
	requireArchiveAllocationGrowth(t)
	registry, manifestPath := sessionRootArchiveFixture(t, "docs")
	if err := growFileAllocation(filepath.Join(filepath.Dir(manifestPath), "tree", "web", "source.txt"), archiveAllocationGrowth); err != nil {
		t.Fatal(err)
	}
	requireAllocationDrift(t, registry, "wa_archive_metadata")
	restored, _, err := registry.RestoreV1("wa_archive_metadata", afclient.WorkareaRestoreRequest{IntoSessionID: "archive-session"})
	if err != nil {
		t.Fatalf("restore unchanged bytes with changed allocation: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(restored.WorkareaRoot, "web", "source.txt"))
	if err != nil || string(body) != "mutable" {
		t.Fatalf("restored source = %q, %v", body, err)
	}
}

// The integrity guarantee the allocation tests must not loosen: an archive
// whose committed bytes changed is refused on re-read, on retry and on
// restore.

func TestCommitArchiveRootRefusesChangedBytesAfterPublish(t *testing.T) {
	t.Parallel()
	tampers := map[string]func(path string) error{
		"truncated": func(path string) error { return os.Truncate(path, 3) },
		"same-size rewrite": func(path string) error {
			return os.WriteFile(path, []byte("TERMINAL WORKAREA MARKER\n"), 0o600) //nolint:gosec // the test's own archive under t.TempDir()
		},
	}
	for _, c := range allocationCommitCases {
		for tamperName, tamper := range tampers {
			t.Run(c.name+"/"+tamperName, func(t *testing.T) {
				t.Parallel()
				archiveRoot := t.TempDir()
				registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, ArchiveHook: func(stage string) error {
					if stage == "after-publish-archive" {
						return tamper(archivedMarker(archiveRoot, c.workareaID))
					}
					return nil
				}})
				spec := markerWorkareaSpec(t, c)
				reference, err := registry.CommitArchiveRoot(t.Context(), spec)
				if !errors.Is(err, ErrArchiveCorrupted) {
					t.Fatalf("commit over changed bytes = (%+v, %v), want ErrArchiveCorrupted", reference, err)
				}
				if _, err := registry.CommitArchiveRoot(t.Context(), spec); !errors.Is(err, ErrArchiveExists) {
					t.Fatalf("retry over changed bytes = %v, want ErrArchiveExists", err)
				}
			})
		}
	}
}

func TestRestoreSessionRootRefusesTruncatedArchive(t *testing.T) {
	t.Parallel()
	registry, manifestPath := sessionRootArchiveFixture(t, "docs")
	if err := os.Truncate(filepath.Join(filepath.Dir(manifestPath), "tree", "web", "source.txt"), 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.RestoreV1("wa_archive_metadata", afclient.WorkareaRestoreRequest{IntoSessionID: "archive-session"}); !errors.Is(err, ErrArchiveCorrupted) {
		t.Fatalf("restore of truncated archive = %v, want ErrArchiveCorrupted", err)
	}
}
