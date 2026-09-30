package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
)

type publicationFixture struct {
	writer              *WorkareaArchiveRegistry
	root, source, stage string
	spec                WorkareaRootArchiveSpec
	release             func()
	finished            chan struct{}
	writerErr           error
}

func newPublicationFixture(t *testing.T, phase string, abort bool, cleanupHooks ...func(string) error) *publicationFixture {
	t.Helper()
	f := &publicationFixture{root: t.TempDir(), source: t.TempDir(), finished: make(chan struct{})}
	for name, body := range map[string]string{"source.txt": "source generation", "nested/context.txt": "captured context"} {
		path := filepath.Join(f.source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.spec = WorkareaRootArchiveSpec{WorkareaID: "wa-publication", SessionID: "publication-session", WorkareaRoot: f.source, SelectedPath: f.source}
	ready, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.release = func() { once.Do(func() { close(resume) }) }
	f.writer = NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: f.root, ArchiveHook: func(stage string) error {
		if stage != phase {
			for _, hook := range cleanupHooks {
				if err := hook(stage); err != nil {
					return err
				}
			}
			return nil
		}
		close(ready)
		<-resume
		if abort {
			return fmt.Errorf("controlled publication abort")
		}
		return nil
	}})
	go func() { f.writerErr = f.writer.ArchiveRoot(t.Context(), f.spec); close(f.finished) }()
	t.Cleanup(func() {
		f.release()
		select {
		case <-f.finished:
		case <-time.After(10 * time.Second):
			t.Error("owned archive writer did not finish")
		}
	})
	select {
	case <-ready:
	case <-f.finished:
		t.Fatalf("actual writer did not reach %s: %v", phase, f.writerErr)
	case <-time.After(10 * time.Second):
		t.Fatalf("actual writer did not reach %s", phase)
	}
	entries, err := os.ReadDir(f.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".archive-"+f.spec.WorkareaID+"-") {
			if f.stage != "" {
				t.Fatal("multiple actual producer stages")
			}
			f.stage = entry.Name()
		}
	}
	if f.stage == "" {
		t.Fatal("actual producer stage missing")
	}
	return f
}

func (f *publicationFixture) finish(t *testing.T) {
	t.Helper()
	f.release()
	select {
	case <-f.finished:
		if f.writerErr != nil {
			t.Fatal(f.writerErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("actual archive publication did not finish")
	}
}

func TestArchivePublicationStagesArePrivateAndReadersResponsive(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		phase    string
		manifest string
	}{
		{phase: "before-copy-archive-tree", manifest: "missing"},
		{phase: "after-open-archive-manifest", manifest: "empty"},
		{phase: "after-write-archive-manifest-prefix", manifest: "partial"},
		{phase: "before-publish-archive", manifest: "complete"},
	} {
		t.Run(test.manifest, func(t *testing.T) {
			t.Parallel()
			f := newPublicationFixture(t, test.phase, false)
			marker, err := os.ReadFile(filepath.Join(f.root, f.stage, archivePublicationIntentName))
			if err != nil {
				t.Fatal(err)
			}
			var intent archivePublicationIntent
			if err := json.Unmarshal(marker, &intent); err != nil || intent.Format != archivePublicationIntentFormat || intent.FinalID != f.spec.WorkareaID || intent.StageLeaf != f.stage {
				t.Fatalf("actual producer intent = %+v, %v", intent, err)
			}
			body, err := os.ReadFile(filepath.Join(f.root, f.stage, "manifest.json"))
			switch test.manifest {
			case "missing":
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("manifest exists before copy: %q %v", body, err)
				}
			case "empty":
				if err != nil || len(body) != 0 {
					t.Fatalf("actual empty manifest = %q %v", body, err)
				}
			case "partial":
				if err != nil || len(body) == 0 || json.Valid(body) {
					t.Fatalf("actual partial producer bytes = %q %v", body, err)
				}
			case "complete":
				if err != nil || !json.Valid(body) {
					t.Fatalf("actual complete producer bytes = %q %v", body, err)
				}
			}
			// The writer mutex is held at this real copy/publication boundary.
			// Same-registry readers must respond before release, never wait for copy.
			result := make(chan error, 6)
			for range 6 {
				go func() {
					_, rows, err := f.writer.ListV1()
					if err == nil && len(rows) != 0 {
						err = fmt.Errorf("unpublished rows: %+v", rows)
					}
					result <- err
				}()
			}
			for range 6 {
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("reader blocked behind actual archive writer")
				}
			}
			reader := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: f.root})
			if _, err := reader.GetV1(f.stage); !errors.Is(err, ErrArchiveNotFound) {
				t.Fatalf("unpublished stage Get = %v", err)
			}
			if _, _, err := reader.RestoreV1(f.stage, afclient.WorkareaRestoreRequest{}); !errors.Is(err, ErrArchiveNotFound) {
				t.Fatalf("unpublished stage Restore = %v", err)
			}
			if _, err := reader.GetV1(f.spec.WorkareaID); !errors.Is(err, ErrArchiveNotFound) {
				t.Fatalf("unpublished final identity Get = %v", err)
			}
			original, err := os.Stat(f.source)
			if err != nil {
				t.Fatal(err)
			}
			f.finish(t)
			_, rows, err := reader.ListV1()
			if err != nil || len(rows) != 1 || rows[0].ID != f.spec.WorkareaID {
				t.Fatalf("final list = %+v, %v", rows, err)
			}
			wa, err := reader.GetV1(rows[0].ID)
			if err != nil || wa.ID != f.spec.WorkareaID || wa.SessionID != f.spec.SessionID || wa.WorkareaRoot != filepath.Join(f.root, f.spec.WorkareaID, "tree") {
				t.Fatalf("final Get = %+v, %v", wa, err)
			}
			restored, _, err := reader.RestoreV1(rows[0].ID, afclient.WorkareaRestoreRequest{IntoSessionID: "restored-session"})
			if err != nil {
				t.Fatal(err)
			}
			for name, want := range map[string]string{"source.txt": "source generation", "nested/context.txt": "captured context"} {
				for _, root := range []string{wa.WorkareaRoot, restored.WorkareaRoot, f.source} {
					body, err := os.ReadFile(filepath.Join(root, name))
					if err != nil || string(body) != want {
						t.Fatalf("captured/restored/source %s = %q, %v", name, body, err)
					}
				}
			}
			current, err := os.Stat(f.source)
			if err != nil || !os.SameFile(original, current) {
				t.Fatal("publication changed source identity")
			}
			if _, err := os.Stat(filepath.Join(wa.WorkareaRoot, archivePublicationIntentName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("private publication intent entered captured tree")
			}
			if err := f.writer.ArchiveRoot(t.Context(), f.spec); err != nil {
				t.Fatalf("idempotent published retry: %v", err)
			}
		})
	}
}

func TestArchivePublicationStatThenWriterRenameOrAbortHasNoPhantom(t *testing.T) {
	t.Parallel()
	for _, abort := range []bool{false, true} {
		t.Run(fmt.Sprintf("abort=%v", abort), func(t *testing.T) {
			t.Parallel()
			f := newPublicationFixture(t, "before-publish-archive", abort)
			var once sync.Once
			reader := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: f.root, ArchiveHook: func(stage string) error {
				if stage == "after-stat-archive-manifest" {
					once.Do(func() { f.release(); <-f.finished })
				}
				return nil
			}})
			_, rows, err := reader.ListV1()
			if err != nil || len(rows) != 0 {
				t.Fatalf("stat then real writer rename/cleanup leaked phantom rows: %+v, %v", rows, err)
			}
			<-f.finished
			if abort {
				if f.writerErr == nil {
					t.Fatal("controlled actual producer abort did not execute")
				}
				_, rows, err = reader.ListV1()
				if err != nil || len(rows) != 0 {
					t.Fatalf("aborted publication exposed rows: %+v, %v", rows, err)
				}
			} else {
				if f.writerErr != nil {
					t.Fatal(f.writerErr)
				}
				_, rows, err = reader.ListV1()
				if err != nil || len(rows) != 1 || rows[0].ID != f.spec.WorkareaID {
					t.Fatalf("post-rename final row = %+v, %v", rows, err)
				}
				if _, err := reader.GetV1(rows[0].ID); err != nil {
					t.Fatalf("post-rename listed ID not resolvable: %v", err)
				}
			}
			if body, err := os.ReadFile(filepath.Join(f.source, "source.txt")); err != nil || string(body) != "source generation" {
				t.Fatalf("rename/abort changed source: %q, %v", body, err)
			}
		})
	}
}

func TestArchivePublicationLegacyCustomAndCorruptIntentCollisions(t *testing.T) {
	t.Parallel()
	cases := []string{"none", "malformed", "unknown-format", "unrelated-intent", "extra-field", "duplicate-field", "oversized", "directory", "symlink", "fifo", "retained-final"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			token := strings.Repeat("a", 24)
			id := ".archive-custom-" + token
			writeFixtureArchive(t, root, fixtureArchive{id: id, manifest: archiveManifest{SessionID: "legacy-session"}, tree: map[string]string{"file": "legacy"}})
			marker := filepath.Join(root, id, archivePublicationIntentName)
			intent := archivePublicationIntent{Format: archivePublicationIntentFormat, FinalID: "custom", StageLeaf: id, Token: token}
			body, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "none":
			case "malformed":
				body = []byte("{partial")
			case "unknown-format":
				intent.Format = "unrelated-bookkeeping"
				body, err = json.Marshal(intent)
			case "unrelated-intent":
				intent.FinalID = "unrelated-final"
				intent.StageLeaf = ".archive-" + intent.FinalID + "-" + token
				body, err = json.Marshal(intent)
			case "duplicate-field":
				body = append(body[:len(body)-1], []byte(`,"token":"`+token+`"}`)...)
			case "extra-field":
				body = append(body[:len(body)-1], []byte(`,"unrelated":true}`)...)
			case "oversized":
				body = []byte(strings.Repeat("x", archivePublicationIntentLimit+1))
			case "directory":
				if err := os.Mkdir(marker, 0o700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "unrelated"), marker); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(marker, 0o600); err != nil {
					t.Fatalf("create owned FIFO marker: %v", err)
				}
			case "retained-final":
				intent.FinalID = id
				intent.StageLeaf = ".archive-" + id + "-" + token
				body, err = json.Marshal(intent)
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind != "none" && kind != "directory" && kind != "symlink" && kind != "fifo" {
				if err := os.WriteFile(marker, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			reader := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
			result := make(chan error, 1)
			go func() {
				_, rows, err := reader.ListV1()
				if err == nil && (len(rows) != 1 || rows[0].ID != id) {
					err = fmt.Errorf("legacy/custom row hidden: %+v", rows)
				}
				result <- err
			}()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("non-producer intent blocked legacy listing")
			}
			if wa, err := reader.GetV1(id); err != nil || wa.ID != id {
				t.Fatalf("legacy/custom Get = %+v, %v", wa, err)
			}
			// A true committed corrupt archive remains visible even with arbitrary
			// ignored bookkeeping or the retained valid final publication marker.
			if err := os.WriteFile(filepath.Join(root, id, "manifest.json"), []byte("{corrupted"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, rows, err := reader.ListV1()
			if err != nil || len(rows) != 1 || rows[0].ID != id || !strings.Contains(rows[0].Disposition, "corrupted") {
				t.Fatalf("committed corrupt row hidden: %+v, %v", rows, err)
			}
			if _, err := reader.GetV1(id); !errors.Is(err, ErrArchiveCorrupted) {
				t.Fatalf("committed corrupt Get lost typed error: %v", err)
			}
		})
	}
}

func TestArchivePublicationReadRefusesDirectoryAndManifestSwaps(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"directory", "manifest", "registry"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "archives")
			writeFixtureArchive(t, root, fixtureArchive{id: "committed", manifest: archiveManifest{SessionID: "original"}, tree: map[string]string{"file": "original"}})
			var once sync.Once
			reader := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root, ArchiveHook: func(stage string) error {
				var err error
				if stage == "after-stat-archive-manifest" {
					once.Do(func() {
						switch kind {
						case "directory":
							err = os.Rename(filepath.Join(root, "committed"), filepath.Join(root, "old"))
							if err == nil {
								writeFixtureArchive(t, root, fixtureArchive{id: "committed", manifest: archiveManifest{SessionID: "replacement"}})
							}
						case "manifest":
							err = os.Rename(filepath.Join(root, "committed", "manifest.json"), filepath.Join(root, "committed", "manifest-old.json"))
							if err == nil {
								err = os.WriteFile(filepath.Join(root, "committed", "manifest.json"), []byte(`{"id":"committed","sessionId":"replacement"}`), 0o600)
							}
						case "registry":
							err = os.Rename(root, root+"-old")
							if err == nil {
								writeFixtureArchive(t, root, fixtureArchive{id: "committed", manifest: archiveManifest{SessionID: "replacement"}})
							}
						}
					})
				}
				return err
			}})
			if wa, err := reader.GetV1("committed"); !errors.Is(err, ErrArchiveCorrupted) {
				t.Fatalf("%s swap returned mixed-generation metadata: %+v, %v", kind, wa, err)
			}
		})
	}
}

func TestArchivePublicationAbortRetainsProofUntilManifestUnlinked(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"after-remove-unpublished-archive-manifest", "after-remove-archive-publication-intent"} {
		t.Run(boundary, func(t *testing.T) {
			t.Parallel()
			cleanupReady, cleanupResume := make(chan struct{}), make(chan struct{})
			var resumeOnce sync.Once
			resume := func() { resumeOnce.Do(func() { close(cleanupResume) }) }
			// Registered first: resume cleanup before fixture teardown awaits writer.
			t.Cleanup(resume)
			f := newPublicationFixture(t, "before-publish-archive", true, func(stage string) error {
				if stage == boundary {
					close(cleanupReady)
					<-cleanupResume
				}
				return nil
			})
			t.Cleanup(resume)
			reader := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: f.root, ArchiveHook: func(stage string) error {
				if stage == "after-open-archive-manifest-reader" {
					f.release()
					select {
					case <-cleanupReady:
					case <-time.After(10 * time.Second):
						return fmt.Errorf("actual abort cleanup boundary not reached")
					}
				}
				return nil
			}})
			_, rows, err := reader.ListV1()
			if err != nil || len(rows) != 0 {
				t.Fatalf("actual abort cleanup leaked readable/phantom archive: %+v, %v", rows, err)
			}
			// Directory/tree remain present at the controlled producer cleanup seam.
			if _, err := os.Stat(filepath.Join(f.root, f.stage, "tree")); err != nil {
				t.Fatalf("actual cleanup was not paused before RemoveAll: %v", err)
			}
			if _, err := os.Stat(filepath.Join(f.root, f.stage, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("cleanup deleted intent while manifest remained readable: %v", err)
			}
			_, markerErr := os.Stat(filepath.Join(f.root, f.stage, archivePublicationIntentName))
			if boundary == "after-remove-unpublished-archive-manifest" && markerErr != nil {
				t.Fatalf("producer lost intent before cleanup: %v", markerErr)
			}
			if boundary == "after-remove-archive-publication-intent" && !errors.Is(markerErr, os.ErrNotExist) {
				t.Fatalf("producer intent-removal boundary not exercised: %v", markerErr)
			}
			ordinary := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: f.root})
			if _, err := ordinary.GetV1(f.stage); !errors.Is(err, ErrArchiveNotFound) {
				t.Fatalf("cleaning unpublished Get = %v", err)
			}
			if _, _, err := ordinary.RestoreV1(f.stage, afclient.WorkareaRestoreRequest{}); !errors.Is(err, ErrArchiveNotFound) {
				t.Fatalf("cleaning unpublished Restore = %v", err)
			}
			resume()
			<-f.finished
			if f.writerErr == nil {
				t.Fatal("actual abort unexpectedly published")
			}
			if body, err := os.ReadFile(filepath.Join(f.source, "source.txt")); err != nil || string(body) != "source generation" {
				t.Fatal("abort cleanup changed source")
			}
		})
	}
}

func TestArchivePublicationCleanupUnlinkFailureRetainsProof(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stageLeaf string
	writer := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root, ArchiveHook: func(stage string) error {
		if stage != "before-publish-archive" {
			return nil
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), ".archive-") {
				stageLeaf = entry.Name()
			}
		}
		path := filepath.Join(root, stageLeaf, "manifest.json")
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, "block-unlink"), []byte("owned"), 0o600); err != nil {
			return err
		}
		return fmt.Errorf("controlled archive abort with manifest unlink failure")
	}})
	if err := writer.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{WorkareaID: "failed-unlink", SessionID: "session", WorkareaRoot: source, SelectedPath: source}); err == nil {
		t.Fatal("controlled producer abort did not execute")
	}
	if _, err := os.Stat(filepath.Join(root, stageLeaf, archivePublicationIntentName)); err != nil {
		t.Fatalf("unlink failure lost proof: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, stageLeaf, "tree", "file")); err != nil {
		t.Fatalf("unlink failure removed retained tree: %v", err)
	}
	reader := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	_, rows, err := reader.ListV1()
	if err != nil || len(rows) != 0 {
		t.Fatalf("retained unpublished proof exposed rows: %+v, %v", rows, err)
	}
}

func TestArchivePublicationPostRenameFailurePreservesCommittedArchive(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("committed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := WorkareaRootArchiveSpec{WorkareaID: "published-final", SessionID: "same-session", WorkareaRoot: source, SelectedPath: source}
	writer := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root, ArchiveHook: func(stage string) error {
		if stage == "after-publish-archive" {
			return fmt.Errorf("controlled post-rename failure")
		}
		return nil
	}})
	if err := writer.ArchiveRoot(t.Context(), spec); err == nil {
		t.Fatal("post-rename failure did not execute")
	}
	reader := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	_, rows, err := reader.ListV1()
	if err != nil || len(rows) != 1 || rows[0].ID != spec.WorkareaID {
		t.Fatalf("cleanup erased committed final: %+v, %v", rows, err)
	}
	wa, err := reader.GetV1(spec.WorkareaID)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(wa.WorkareaRoot, "file")); err != nil || string(body) != "committed bytes" {
		t.Fatalf("committed manifest/tree lost: %q, %v", body, err)
	}
	if err := writer.ArchiveRoot(t.Context(), spec); err != nil {
		t.Fatalf("idempotent retry after published failure: %v", err)
	}
}

func TestArchivePublicationAbortStageSubstitutionPreservesVictim(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	writeFixtureArchive(t, root, fixtureArchive{id: "committed-victim", manifest: archiveManifest{SessionID: "victim-session"}, tree: map[string]string{"victim.txt": "victim generation"}})
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("source generation"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stageLeaf string
	writer := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root, ArchiveHook: func(stage string) error {
		if stage != "before-publish-archive" {
			return nil
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), ".archive-actual-work-") {
				stageLeaf = entry.Name()
			}
		}
		if stageLeaf == "" {
			return fmt.Errorf("actual producer stage missing")
		}
		if err := os.Rename(filepath.Join(root, stageLeaf), filepath.Join(root, "moved-private-stage")); err != nil {
			return err
		}
		if err := os.Symlink("committed-victim", filepath.Join(root, stageLeaf)); err != nil {
			return err
		}
		return fmt.Errorf("controlled producer abort after stage substitution")
	}})
	if err := writer.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{WorkareaID: "actual-work", SessionID: "source-session", WorkareaRoot: source, SelectedPath: source}); err == nil {
		t.Fatal("actual substitution/abort did not execute")
	}
	reader := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: root})
	wa, err := reader.GetV1("committed-victim")
	if err != nil || wa.SessionID != "victim-session" {
		t.Fatalf("abort cleanup damaged other committed archive: %+v, %v", wa, err)
	}
	restored, _, err := reader.RestoreV1("committed-victim", afclient.WorkareaRestoreRequest{IntoSessionID: "victim-restore"})
	if err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(restored.Path, "victim.txt")); err != nil || string(body) != "victim generation" {
		t.Fatalf("other archive tree changed: %q, %v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(source, "source.txt")); err != nil || string(body) != "source generation" {
		t.Fatal("abort cleanup changed source")
	}
	if _, err := os.Stat(filepath.Join(root, "moved-private-stage", archivePublicationIntentName)); err != nil {
		t.Fatalf("substituted stage lost intent: %v", err)
	}
}
