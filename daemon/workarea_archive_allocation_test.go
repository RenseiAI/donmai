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
// The tree digest still refuses an archive whose bytes, modes, names or link
// structure changed. They live apart from the general archive suites because
// the allocation fixture needs per-platform helpers
// (archive_allocation_*_test.go).

const archiveAllocationGrowth = 1 << 20

var errSimulatedCrash = errors.New("simulated crash after publish")

// requireArchiveAllocationGrowth skips when the filesystem under the test's
// temporary directory cannot allocate past EOF, or does not report such an
// allocation in physical accounting; the drift fixture proves nothing there.
func requireArchiveAllocationGrowth(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "probe")
	if err := os.WriteFile(path, []byte("probe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := workarea.PhysicalUsage(workarea.RootPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := growFileAllocation(path, archiveAllocationGrowth); errors.Is(err, errors.ErrUnsupported) {
		t.Skipf("filesystem cannot allocate past EOF: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	after, err := workarea.PhysicalUsage(workarea.RootPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Skipf("filesystem does not account allocation past EOF (%d -> %d bytes)", before, after)
	}
}

// archivePayload is the archived content both fixture layouts carry: a file
// to tamper with, two identical files that are separate inodes, and a
// symlink whose target has the same length as "twin-a.txt".
var archivePayload = map[string]string{
	"marker.txt": "terminal workarea marker\n",
	"twin-a.txt": "twin\n",
	"twin-b.txt": "twin\n",
	"link":       "->marker.txt",
}

type archiveFixtureCase struct {
	name string
	// sessionRoot selects a declared, acquisition-backed session root (the
	// shape nested releases archive) over a legacy flat workarea.
	sessionRoot      bool
	workareaID       string
	sessionID        string
	terminalResultID string
}

var archiveFixtureCases = []archiveFixtureCase{
	{name: "flat with terminal result", workareaID: "wa_55555555555555555555555555555555", sessionID: "55555555-5555-4555-8555-555555555555", terminalResultID: "tr_55555555555555555555555555555555"},
	{name: "flat without terminal result", workareaID: "wa_66666666666666666666666666666666", sessionID: "66666666-6666-4666-8666-666666666666"},
	{name: "session root with terminal result", sessionRoot: true, workareaID: "wa_77777777777777777777777777777777", sessionID: "77777777-7777-4777-8777-777777777777", terminalResultID: "tr_77777777777777777777777777777777"},
	{name: "session root without terminal result", sessionRoot: true, workareaID: "wa_88888888888888888888888888888888", sessionID: "88888888-8888-4888-8888-888888888888"},
}

type archiveFixture struct {
	registry *WorkareaArchiveRegistry
	spec     WorkareaRootArchiveSpec
	// payload is the archived directory holding archivePayload: the tree
	// itself for a flat workarea, the web leaf for a session root.
	payload string
}

// newArchiveFixture prepares the source workarea for c and a registry whose
// after-publish seam runs afterPublish (nil for none) on the archived payload.
func newArchiveFixture(t *testing.T, c archiveFixtureCase, afterPublish func(payload string) error) archiveFixture {
	t.Helper()
	archiveRoot := t.TempDir()
	var acquisitions *workarea.AcquisitionStore
	var spec WorkareaRootArchiveSpec
	payloadLeaf := ""
	if c.sessionRoot {
		files := map[string]string{"docs/context.txt": "readonly"}
		for name, body := range archivePayload {
			files["web/"+name] = body
		}
		acquisitions, spec, _ = sessionRootSourceFixture(t, c.workareaID, c.sessionID, "docs", files)
		payloadLeaf = "web"
	} else {
		root := filepath.Join(t.TempDir(), "worktrees", c.sessionID)
		writeFixtureFiles(t, root, archivePayload)
		spec = WorkareaRootArchiveSpec{WorkareaID: c.workareaID, SessionID: c.sessionID, WorkareaRoot: root, SelectedPath: root}
	}
	spec.TerminalResultID = c.terminalResultID
	payload := filepath.Join(archiveRoot, c.workareaID, "tree", payloadLeaf)
	options := WorkareaArchiveOptions{Root: archiveRoot, AcquisitionStore: acquisitions}
	if afterPublish != nil {
		options.ArchiveHook = func(stage string) error {
			if stage == "after-publish-archive" {
				return afterPublish(payload)
			}
			return nil
		}
	}
	return archiveFixture{registry: NewWorkareaArchiveRegistry(options), spec: spec, payload: payload}
}

func crashAfterPublish(string) error { return errSimulatedCrash }

// commitThenCrash commits f's archive through a seam that fails right after
// the archive is published: the first attempt dies before a session root's
// digest is bound, and the published archive stays for the retry.
func commitThenCrash(t *testing.T, f archiveFixture) {
	t.Helper()
	if _, err := f.registry.CommitArchiveRoot(t.Context(), f.spec); !errors.Is(err, errSimulatedCrash) {
		t.Fatalf("first attempt = %v, want the simulated crash", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.registry.treeDir(f.spec.WorkareaID)), "manifest.json")); err != nil {
		t.Fatalf("archive not published before the simulated crash: %v", err)
	}
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

func requireCommitReference(t *testing.T, c archiveFixtureCase, reference *workarea.TerminalArchiveReference) {
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
	for _, c := range archiveFixtureCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newArchiveFixture(t, c, func(payload string) error {
				return growFileAllocation(filepath.Join(payload, "marker.txt"), archiveAllocationGrowth)
			})
			reference, err := f.registry.CommitArchiveRoot(t.Context(), f.spec)
			if err != nil {
				t.Fatalf("commit with unchanged bytes and changed allocation: %v", err)
			}
			requireCommitReference(t, c, reference)
			requireAllocationDrift(t, f.registry, c.workareaID)
		})
	}
}

func TestCommitArchiveRootRetryAcceptsAllocationChangeSinceCommit(t *testing.T) {
	t.Parallel()
	requireArchiveAllocationGrowth(t)
	for _, c := range archiveFixtureCases {
		for _, crashed := range []bool{false, true} {
			name := c.name + "/after a committed attempt"
			if crashed {
				name = c.name + "/after a crash before bind"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var first *workarea.TerminalArchiveReference
				var f archiveFixture
				if crashed {
					f = newArchiveFixture(t, c, crashAfterPublish)
					commitThenCrash(t, f)
				} else {
					f = newArchiveFixture(t, c, nil)
					var err error
					if first, err = f.registry.CommitArchiveRoot(t.Context(), f.spec); err != nil {
						t.Fatalf("first commit: %v", err)
					}
				}
				if err := growFileAllocation(filepath.Join(f.payload, "marker.txt"), archiveAllocationGrowth); err != nil {
					t.Fatal(err)
				}
				requireAllocationDrift(t, f.registry, c.workareaID)
				// A release that published and then failed is retried; the
				// retry must find the same committed bytes, not a collision.
				again, err := f.registry.CommitArchiveRoot(t.Context(), f.spec)
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
}

// archiveTampers change committed bytes or metadata in the archived payload.
// Each one differs from the committed tree in exactly the digest field it
// names, so dropping that field from the digest lets the tamper through.
var archiveTampers = map[string]func(payload string) error{
	"truncated (size, hash)": func(payload string) error {
		return os.Truncate(filepath.Join(payload, "marker.txt"), 3)
	},
	"same-size rewrite (hash)": func(payload string) error {
		return os.WriteFile(filepath.Join(payload, "marker.txt"), []byte("TERMINAL WORKAREA MARKER\n"), 0o600) //nolint:gosec // the test's own archive under t.TempDir()
	},
	"mode change (mode)": func(payload string) error {
		return os.Chmod(filepath.Join(payload, "marker.txt"), 0o400)
	},
	"renamed entry (path)": func(payload string) error {
		return os.Rename(filepath.Join(payload, "twin-b.txt"), filepath.Join(payload, "twin-c.txt"))
	},
	"added entry (path)": func(payload string) error {
		return os.WriteFile(filepath.Join(payload, "extra.txt"), []byte("extra\n"), 0o600) //nolint:gosec // the test's own archive under t.TempDir()
	},
	"removed entry (path)": func(payload string) error {
		return os.Remove(filepath.Join(payload, "twin-b.txt"))
	},
	"symlink retarget (symlink target)": func(payload string) error {
		link := filepath.Join(payload, "link")
		if err := os.Remove(link); err != nil {
			return err
		}
		return os.Symlink("twin-a.txt", link)
	},
	"identical file split into a hardlink (hardlink)": func(payload string) error {
		twin := filepath.Join(payload, "twin-b.txt")
		if err := os.Remove(twin); err != nil {
			return err
		}
		return os.Link(filepath.Join(payload, "twin-a.txt"), twin)
	},
}

// The integrity guarantee the allocation tests must not loosen: an archive
// whose committed bytes or metadata changed is refused on re-read and on
// every retry, including a session-root retry whose first attempt died
// before its digest was bound.
func TestCommitArchiveRootRefusesChangedArchive(t *testing.T) {
	t.Parallel()
	for _, c := range archiveFixtureCases {
		for tamperName, tamper := range archiveTampers {
			t.Run(c.name+"/after publish/"+tamperName, func(t *testing.T) {
				t.Parallel()
				f := newArchiveFixture(t, c, tamper)
				reference, err := f.registry.CommitArchiveRoot(t.Context(), f.spec)
				if !errors.Is(err, ErrArchiveCorrupted) {
					t.Fatalf("commit over a changed archive = (%+v, %v), want ErrArchiveCorrupted", reference, err)
				}
				if reference, err := f.registry.CommitArchiveRoot(t.Context(), f.spec); !errors.Is(err, ErrArchiveExists) {
					t.Fatalf("retry over a changed archive = (%+v, %v), want ErrArchiveExists", reference, err)
				}
			})
			t.Run(c.name+"/after a crash before bind/"+tamperName, func(t *testing.T) {
				t.Parallel()
				f := newArchiveFixture(t, c, crashAfterPublish)
				commitThenCrash(t, f)
				if err := tamper(f.payload); err != nil {
					t.Fatal(err)
				}
				if reference, err := f.registry.CommitArchiveRoot(t.Context(), f.spec); !errors.Is(err, ErrArchiveExists) {
					t.Fatalf("retry over a changed archive = (%+v, %v), want ErrArchiveExists", reference, err)
				}
			})
		}
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
