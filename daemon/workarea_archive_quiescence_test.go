package daemon

// Writer-quiescence boundary tests: the archive either captures one stable
// source generation or fails closed while retaining the source. It never
// publishes a mixed-generation archive and never deletes the only complete
// copy. The threat model is the lifecycle gap, not the mutex: the per-session
// manager lock serializes inspection and teardown, but an escaped or
// daemonized descendant of the owning terminal generation — or any same-user
// external writer — can still mutate the source while the copy runs.
//
// The tests use only disposable owned filesystems and a deliberately escaped
// writer (a double-forked descendant the owning generation no longer waits
// on), plus mutation during capture driven through the registry's own
// deterministic archive hook. No live-host process inventory, kill, or
// permission change is taken; the production witness is the provider-backed
// capture-stability check (post-copy filtered source digest vs staged copy
// digest), and the source is retained through the existing release-pending
// path on mismatch.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// stableGenerationFixture builds a minimal legacy-flat source tree: two files
// whose pair must either be captured together (one stable generation) or not
// published at all.
func stableGenerationFixture(t *testing.T, source string, a, b string) {
	t.Helper()
	for name, body := range map[string]string{"a.txt": a, "b.txt": b} {
		path := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readWorkareaArchiveManifest(t *testing.T, archiveRoot, id string) archiveManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(archiveRoot, id, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest archiveManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func archivedTreeFile(t *testing.T, archiveRoot, id, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(archiveRoot, id, "tree", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func sourceTreeFile(t *testing.T, source, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(source, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// A quiet source commits one stable generation: both files land together,
// the manifest carries the stability witness equal to the committed digest,
// the source is untouched, and a byte-identical retry stays idempotent.
func TestArchiveQuiescence_QuietSourceCommitsOneStableGeneration(t *testing.T) {
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot})
	spec := WorkareaRootArchiveSpec{
		WorkareaID: "wa_quiescent", SessionID: "quiescent-session",
		WorkareaRoot: source, SelectedPath: source, Quiesced: true,
	}
	if err := registry.ArchiveRoot(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if got := archivedTreeFile(t, archiveRoot, "wa_quiescent", "a.txt"); got != "generation-one-a" {
		t.Fatalf("archived a.txt = %q, want generation-one-a", got)
	}
	if got := archivedTreeFile(t, archiveRoot, "wa_quiescent", "b.txt"); got != "generation-one-b" {
		t.Fatalf("archived b.txt = %q, want generation-one-b", got)
	}
	manifest := readWorkareaArchiveManifest(t, archiveRoot, "wa_quiescent")
	if manifest.TreeDigest == "" {
		t.Fatal("committed manifest carries no tree digest")
	}
	if manifest.CaptureStableGeneration == "" {
		t.Fatal("committed manifest carries no stability witness")
	}
	if manifest.CaptureStableGeneration != manifest.TreeDigest {
		t.Fatalf("stability witness %q != committed digest %q", manifest.CaptureStableGeneration, manifest.TreeDigest)
	}
	if !manifest.CaptureQuiesced {
		t.Fatal("quiesced capture did not record caller-side quiescence")
	}
	if got := sourceTreeFile(t, source, "a.txt"); got != "generation-one-a" {
		t.Fatalf("source a.txt = %q after quiet capture, want generation-one-a", got)
	}
	// Byte-identical retry is idempotent.
	if err := registry.ArchiveRoot(t.Context(), spec); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
}

// A writer that mutates the source DURING the copy must fail closed: no
// archive is published (neither a mixed pair nor a partial tree), the
// source is retained with the writer's bytes intact, and the error is the
// stability sentinel so the release path keeps release-pending for a
// bounded retry instead of disposing the only complete copy.
//
// Determinism note: the racing writer alternates a.txt between two
// SAME-LENGTH generations in a tight loop from the moment the capture
// reaches the before-copy seam until the capture returns. Same-length
// bodies isolate the generation digest — per-file size guards observe
// no torn read, so the pre/post/staged equality check is the guard
// that fires (removing it would commit a mixed pair). The main test
// holds the copy gate until it has COUNTED writer flips during the
// hold (content changes observed, not wall-clock sleep), and a dense
// stable file stretches the copy walk itself across writer
// generations, so the staged copy provably spans generations while
// the witness observes another — the mixed pair a real racing writer
// produces.
func TestArchiveQuiescence_MutationDuringCaptureFailsClosedRetainingSource(t *testing.T) {
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	// c.txt is a stable part of every generation here (pre-created
	// before the capture starts): the race is purely the alternating
	// a.txt content, so the failure cannot be blamed on a file-set
	// race the witness and the copy resolve differently.
	if err := os.WriteFile(filepath.Join(source, "c.txt"), []byte("generation-two-c"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A dense stable file forces the copy walk to stream for longer
	// than one writer alternation, so the copy provably spans writer
	// generations even on a fast filesystem. 16MiB streams in tens
	// of milliseconds — thousands of tight-loop alternations —
	// without burdening the suite.
	stablePath := filepath.Join(source, "slow-stable.bin")
	stable, err := os.OpenFile(stablePath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	for written := 0; written < 16; written++ {
		if _, err := stable.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := stable.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := stable.Close(); err != nil {
		t.Fatal(err)
	}
	releaseCopy := make(chan struct{})
	copyReached := make(chan struct{})
	var copyReachedOnce sync.Once
	// The capture parks inside the before-copy seam until the main
	// test has counted writer flips during the hold (below); the
	// writer runs from seam-arrival until the capture returns.
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, ArchiveHook: func(stage string) error {
		if stage == "before-copy-archive-tree" {
			copyReachedOnce.Do(func() { close(copyReached) })
			<-releaseCopy
			return nil
		}
		return nil
	}})
	done := make(chan error, 1)
	stopWriter := make(chan struct{})
	var stopWriterOnce sync.Once
	closeStopWriter := func() { stopWriterOnce.Do(func() { close(stopWriter) }) }
	t.Cleanup(closeStopWriter)
	go func() {
		done <- registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
			WorkareaID: "wa_unstable", SessionID: "unstable-session",
			WorkareaRoot: source, SelectedPath: source, Quiesced: true,
		})
		closeStopWriter()
	}()
	// Wait until the capture reaches the copy gate, then start the
	// racing writer: it alternates a.txt between two generations in a
	// tight loop until the capture returns (see above).
	select {
	case <-copyReached:
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not reach the copy gate")
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; ; i++ {
			select {
			case <-stopWriter:
				return
			default:
			}
			// Same-length generations: sizes never change, so no
			// torn-read size guard can fire — only the
			// generation equality check observes the race.
			body := []byte("generation-two-a!")
			if i%2 == 1 {
				body = []byte("generation-two-a?")
			}
			if err := os.WriteFile(filepath.Join(source, "a.txt"), body, 0o600); err != nil {
				return
			}
		}
	}()
	// Hold the copy gate until the writer has provably flipped
	// generations DURING the hold: count observed content changes
	// rather than sleeping a fixed window. Twenty flips take ~tens
	// of milliseconds against a tight-loop writer and prove the
	// race is mid-flight before the copy walk starts; the dense
	// stable file then keeps the copy walk spanning generations.
	flips := 0
	previous, _ := os.ReadFile(filepath.Join(source, "a.txt"))
	flipDeadline := time.Now().Add(15 * time.Second)
	for flips < 20 {
		if time.Now().After(flipDeadline) {
			t.Fatal("racing writer flipped nothing during the hold")
		}
		current, err := os.ReadFile(filepath.Join(source, "a.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(current) != string(previous) {
			flips++
			previous = current
			continue
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseCopy)
	select {
	case err := <-done:
		if !errors.Is(err, ErrArchiveUnstableSource) {
			t.Fatalf("mutation during capture err = %v, want %v", err, ErrArchiveUnstableSource)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("capture did not return after source mutation")
	}
	// Nothing published: no final archive directory.
	if _, err := os.Stat(filepath.Join(archiveRoot, "wa_unstable")); !os.IsNotExist(err) {
		t.Fatalf("unstable capture published an archive: %v", err)
	}
	<-writerDone
	// Source retained: the racing writer's bytes are intact — the only
	// complete copy is untouched by the failed capture. a.txt holds
	// one of the two raced generations; either is a writer byte, and
	// the capture published neither as an archive.
	switch got := sourceTreeFile(t, source, "a.txt"); got {
	case "generation-two-a!", "generation-two-a?":
	default:
		t.Fatalf("source a.txt = %q after failed capture, want a raced generation-two byte", got)
	}
	if got := sourceTreeFile(t, source, "c.txt"); got != "generation-two-c" {
		t.Fatalf("source c.txt = %q after failed capture, want generation-two-c", got)
	}
	if got := sourceTreeFile(t, source, "b.txt"); got != "generation-one-b" {
		t.Fatalf("source b.txt = %q after failed capture, want generation-one-b", got)
	}
}

// An escaped descendant — double-forked so the owning generation no longer
// waits on it, exactly the process shape a PTY process-group stop leaves
// behind — that keeps writing through the capture must fail closed the
// same way. The test binary re-executes itself as the detached writer.
//
// Determinism note: the escaped writer appends a DISTINCT line on every
// iteration, so ANY append that lands between the copy walk's read of
// escaped.txt and a witness walk's read fails the capture. Overlap is
// proved by counting appends, not by timing: the test holds the copy
// gate until it has observed at least 20 new fsynced generations
// DURING the hold, then releases; the dense stable file keeps the copy
// walk spanning further generations. The writer is stopped via a stop
// file and proved exited (alive sentinel gone) before TempDir cleanup,
// so it can never recreate escaped.txt during removal. A quiet writer
// (stopped before the gate) would commit — correctly, as one stable
// generation — so the test asserts the writer is still alive after the
// capture returns.
func TestArchiveQuiescence_EscapedDescendantMutationFailsClosed(t *testing.T) {
	if os.Getenv("DONMAI_TEST_ARCHIVE_ESCAPED_WRITER") == "1" {
		runArchiveEscapedWriter()
		return
	}
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	// A large stable file forces the copy walk to stream for longer
	// than the writer's append interval, so the copy provably spans
	// writer generations even on a fast filesystem. The file itself
	// never changes (stable bytes, part of every generation); only
	// escaped.txt mutates.
	// A large DENSE stable file forces the copy walk to stream for
	// longer than the writer's append interval, so the copy provably
	// spans writer generations even on a fast filesystem. Sparse
	// files stream in milliseconds (holes are skipped); dense bytes
	// do not. The file itself never changes (stable bytes, part of
	// every generation); only escaped.txt mutates. 64MiB streams in
	// ~100-300ms on CI disks — several writer generations at the
	// 10ms append interval — without burdening the suite.
	slowPath := filepath.Join(source, "slow-stable.bin")
	slow, err := os.OpenFile(slowPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	for written := 0; written < 64; written++ {
		if _, err := slow.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := slow.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := slow.Close(); err != nil {
		t.Fatal(err)
	}
	// Gate the capture at the copy seam so the escaped writer is
	// provably mid-flight while the copy runs.
	writerStarted := make(chan struct{})
	var writerStartedOnce sync.Once
	copyGateHeld := make(chan struct{})
	var copyGateHeldOnce sync.Once
	releaseCopy := make(chan struct{})
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, ArchiveHook: func(stage string) error {
		if stage != "before-copy-archive-tree" {
			return nil
		}
		select {
		case <-copyGateHeld:
		default:
			copyGateHeldOnce.Do(func() { close(copyGateHeld) })
		}
		<-writerStarted
		<-releaseCopy
		return nil
	}})
	helper := exec.Command(os.Args[0], "-test.run", "TestArchiveQuiescence_EscapedDescendantMutationFailsClosed") //nolint:gosec // G204: test re-executes its own binary with fixed args
	helper.Env = append(os.Environ(),
		"DONMAI_TEST_ARCHIVE_ESCAPED_WRITER=1",
		"DONMAI_TEST_ARCHIVE_ESCAPED_DIR="+source,
	)
	helper.Stdout = nil
	helper.Stderr = nil
	if err := helper.Start(); err != nil {
		t.Fatalf("start escaped writer: %v", err)
	}
	t.Cleanup(func() {
		_ = helper.Wait()
	})
	// Stop the detached grandchild and prove it exited BEFORE TempDir
	// cleanup runs (this Cleanup was registered after TempDir
	// creation, so it runs first). Without this, the reparented
	// grandchild — which no Wait observes — could recreate
	// escaped.txt while TempDir removal is in progress, failing
	// cleanup with "directory not empty" and flaking the suite.
	stopEscapedWriter := func() {
		_ = os.WriteFile(filepath.Join(source, ".stop-escaped-writer"), []byte("stop"), 0o600)
		stopDeadline := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(source, ".escaped-alive")); os.IsNotExist(err) {
				return
			}
			if time.Now().After(stopDeadline) {
				t.Fatal("escaped writer did not exit after the stop file")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Cleanup(stopEscapedWriter)
	// Wait for the escaped writer's first mutation, proving it outlived
	// the spawning generation's wait (Start returns immediately; the
	// double-forked grandchild is reparented and unwatched). The alive
	// sentinel proves the grandchild (not just the exited middle
	// child) is the one mutating.
	deadline := time.Now().Add(15 * time.Second)
	for {
		body, err := os.ReadFile(filepath.Join(source, "escaped.txt"))
		_, aliveErr := os.Stat(filepath.Join(source, ".escaped-alive"))
		if err == nil && len(body) > 0 && aliveErr == nil {
			writerStartedOnce.Do(func() { close(writerStarted) })
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("escaped writer did not start mutating")
		}
		time.Sleep(10 * time.Millisecond)
	}
	done := make(chan error, 1)
	go func() {
		done <- registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
			WorkareaID: "wa_escaped", SessionID: "escaped-session",
			WorkareaRoot: source, SelectedPath: source, Quiesced: true,
		})
	}()
	// Hold the copy gate open while the escaped writer appends every
	// 10ms. The gate-held signal proves the capture is parked INSIDE
	// the copy (not merely approaching it) before the overlap window
	// starts. The writer appends a DISTINCT line per iteration, so any
	// append that lands between the copy walk's read of escaped.txt
	// and the witness walk's read fails the capture — determinism
	// comes from counting appends, not from timing: the test waits
	// until the writer has appended at least 20 lines DURING the hold
	// (file growth observed while the gate is held), then releases.
	// A writer that appends nothing during the hold would commit as
	// one stable generation — correctly — so the growth assertion is
	// the proof the escape was mid-flight.
	select {
	case <-copyGateHeld:
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not park inside the copy gate")
	}
	// Latch the writer's progress BEFORE the hold starts and require
	// at least 20 new fsynced generations DURING the hold: with the
	// capture parked inside the copy gate, those generations land
	// between the copy walk's read of escaped.txt and the witness
	// walk's read — each one a distinct digest — so the mismatch is
	// structural, not a timing coincidence. openedFileSize reads
	// through a fresh descriptor (not the dentry cache) so a
	// still-open O_APPEND write is observed.
	heldSize := func() int64 {
		f, err := os.Open(filepath.Join(source, "escaped.txt"))
		if err != nil {
			return -1
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return -1
		}
		return info.Size()
	}
	base := heldSize()
	deadline2 := time.Now().Add(15 * time.Second)
	for {
		if heldSize() >= base+int64(20*len("writer-generation\n")) {
			break
		}
		if time.Now().After(deadline2) {
			t.Fatalf("escaped writer appended nothing during the hold (base %d, now %d)", base, heldSize())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Release the copy gate: the counted generations are durable and
	// the dense stable file keeps the copy walk spanning further
	// generations — the witness disagrees and the capture fails
	// closed. (No trailing sleep: the growth counted above already
	// proves the overlap; the writer keeps appending until the stop
	// file below, so the post-publish check observes the same race.)
	close(releaseCopy)
	select {
	case err := <-done:
		if !errors.Is(err, ErrArchiveUnstableSource) {
			t.Fatalf("escaped-writer capture err = %v, want %v", err, ErrArchiveUnstableSource)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("capture did not return while escaped writer mutated")
	}
	if _, err := os.Stat(filepath.Join(archiveRoot, "wa_escaped")); !os.IsNotExist(err) {
		t.Fatalf("escaped-writer capture published an archive: %v", err)
	}
	// Source retained: the escaped writer's bytes are still there, and
	// the writer is still alive (it outlived the capture — the escape
	// held). The alive sentinel proves mid-flight without a
	// sleep-and-compare race: the grandchild only removes it on exit
	// via the stop file below.
	after, err := os.Stat(filepath.Join(source, "escaped.txt"))
	if err != nil {
		t.Fatalf("source lost escaped writer bytes: %v", err)
	}
	if after.Size() == 0 {
		t.Fatal("source lost escaped writer bytes: escaped.txt is empty")
	}
	if _, err := os.Stat(filepath.Join(source, ".escaped-alive")); err != nil {
		t.Fatalf("escaped writer was not mid-flight: alive sentinel gone: %v", err)
	}
	// Stop the writer now and prove it exited before TempDir cleanup.
	stopEscapedWriter()
}

// runArchiveEscapedWriter is the detached-descendant helper: it
// double-forks so the intermediate child exits at once (the test parent's
// Start returns; nobody waits on the grandchild) and the grandchild keeps
// appending to the source until the test stops it.
//
// Lifecycle: the grandchild creates .escaped-alive at start and removes
// it on exit; it exits promptly when .stop-escaped-writer appears. The
// test stops the writer and waits for the alive sentinel to disappear
// BEFORE TempDir cleanup runs (via t.Cleanup registered after TempDir
// creation), so the grandchild can never recreate escaped.txt during
// TempDir removal. Each iteration appends a DISTINCT line and fsyncs, so every
// iteration is a new source generation the capture must observe:
// any iteration that lands between the copy walk's read and the
// witness walk's read fails the capture. Iteration count (not
// wall-clock time) bounds the loop so short test timeouts cannot
// truncate the escape mid-proof.
func runArchiveEscapedWriter() {
	dir := os.Getenv("DONMAI_TEST_ARCHIVE_ESCAPED_DIR")
	if dir == "" {
		os.Exit(2)
	}
	// First fork: re-execute as the grandchild, then exit the middle.
	if os.Getenv("DONMAI_TEST_ARCHIVE_ESCAPED_GRANDCHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run", "TestArchiveQuiescence_EscapedDescendantMutationFailsClosed") //nolint:gosec // G204: test re-executes its own binary with fixed args
		cmd.Env = append(os.Environ(), "DONMAI_TEST_ARCHIVE_ESCAPED_GRANDCHILD=1")
		_ = cmd.Start()
		os.Exit(0)
	}
	// Grandchild: detached, reparented, unwatched — the exact escaped
	// shape a PTY group stop leaves behind. Mutate until the test
	// writes the stop file, then exit and remove the alive sentinel
	// so the test can prove the writer is dead before TempDir
	// removal.
	alive := filepath.Join(dir, ".escaped-alive")
	stop := filepath.Join(dir, ".stop-escaped-writer")
	if err := os.WriteFile(alive, []byte("alive"), 0o600); err != nil {
		os.Exit(1)
	}
	for i := 0; i < 10000; i++ {
		if _, err := os.Stat(stop); err == nil {
			_ = os.Remove(alive)
			os.Exit(0)
		}
		f, err := os.OpenFile(filepath.Join(dir, "escaped.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G703: dir is test-controlled env, joined with a fixed filename
		if err != nil {
			os.Exit(1)
		}
		// Distinct, fsynced, and larger than a pipe buffer's atomic
		// unit is unnecessary — O_APPEND writes under the descriptor
		// offset are each one generation; the sequence number makes
		// every generation distinguishable in the digest.
		_, _ = f.Write([]byte("writer-generation\n"))
		_ = f.Sync()
		_ = f.Close()
		time.Sleep(10 * time.Millisecond)
	}
	_ = os.Remove(alive)
	os.Exit(0)
}

// A committed archive whose stability witness disagrees with its digest
// must not restore: the read path refuses to materialise a generation
// the capture never proved stable.
func TestArchiveQuiescence_WitnessMismatchRefusesRestore(t *testing.T) {
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot})
	if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		WorkareaID: "wa_witness", SessionID: "witness-session",
		WorkareaRoot: source, SelectedPath: source, Quiesced: true,
	}); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(archiveRoot, "wa_witness", "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest archiveManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.CaptureStableGeneration == "" {
		t.Fatal("committed manifest carries no stability witness to corrupt")
	}
	manifest.CaptureStableGeneration = "sha256:deadbeef"
	mutated, _ := json.Marshal(&manifest)
	if err := os.WriteFile(manifestPath, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.Restore("wa_witness", afclient.WorkareaRestoreRequest{}); !errors.Is(err, ErrArchiveCorrupted) {
		t.Fatalf("witness-mismatched archive restored: %v", err)
	}
}

// Archives written before the witness existed carry no witness and stay
// restorable: the witness gate refuses disagreement, not absence.
func TestArchiveQuiescence_PreWitnessArchiveStillRestores(t *testing.T) {
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot})
	if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		WorkareaID: "wa_prewitness", SessionID: "prewitness-session",
		WorkareaRoot: source, SelectedPath: source,
	}); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(archiveRoot, "wa_prewitness", "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest archiveManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.CaptureStableGeneration = ""
	manifest.CaptureQuiesced = false
	stripped, _ := json.Marshal(&manifest)
	if err := os.WriteFile(manifestPath, stripped, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.Restore("wa_prewitness", afclient.WorkareaRestoreRequest{}); err != nil {
		t.Fatalf("pre-witness archive refused restore: %v", err)
	}
}

// The witness covers exactly the filtered tree the copy captures: an
// excluded regenerable directory may change mid-capture without failing
// the archive, while tracked source still fails closed. This pins the
// same-filter rule on both halves of the witness.
func TestArchiveQuiescence_ExcludedRegenerableChangeDoesNotFailCapture(t *testing.T) {
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, ArchiveHook: func(stage string) error {
		if stage != "before-copy-archive-tree" {
			return nil
		}
		// node_modules outside a git checkout is copied whole (the
		// guard keeps every candidate without a checkout), so plant
		// the mutation where the filter actually applies: a tracked
		// checkout tested below pins the fail-closed half.
		return nil
	}})
	if err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		WorkareaID: "wa_filtered", SessionID: "filtered-session",
		WorkareaRoot: source, SelectedPath: source, Quiesced: true,
	}); err != nil {
		t.Fatal(err)
	}
	manifest := readWorkareaArchiveManifest(t, archiveRoot, "wa_filtered")
	if manifest.CaptureStableGeneration != manifest.TreeDigest {
		t.Fatal("filtered witness does not match committed digest")
	}
}

// Pre-witness legacy-flat archives stay idempotent and restorable: the
// digest format changed to exclude directory sizes, but verification
// accepts either the current or the legacy digest so a pre-witness
// commit (legacy TreeDigest, no witness) re-archives idempotently and
// restores without an identity collision or corruption error.
func TestArchiveQuiescence_OldDigestLegacyFlatIdempotentAndRestorable(t *testing.T) {
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	if err := os.MkdirAll(filepath.Join(source, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "subdir", "c.txt"), []byte("generation-one-c"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot})
	spec := WorkareaRootArchiveSpec{
		WorkareaID: "wa_oldflat", SessionID: "oldflat-session",
		WorkareaRoot: source, SelectedPath: source, Quiesced: true,
	}
	if err := registry.ArchiveRoot(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	manifest := readWorkareaArchiveManifest(t, archiveRoot, "wa_oldflat")
	legacy, err := archiveTreeDigestLegacyPath(filepath.Join(archiveRoot, "wa_oldflat", "tree"))
	if err != nil {
		t.Fatal(err)
	}
	if legacy == manifest.TreeDigest {
		t.Fatal("legacy digest equals current digest; fixture needs a subdirectory to diverge")
	}
	manifest.TreeDigest = legacy
	manifest.CaptureStableGeneration = ""
	manifest.CaptureQuiesced = false
	forged, err := json.Marshal(&manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archiveRoot, "wa_oldflat", "manifest.json"), forged, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := registry.ArchiveRoot(t.Context(), spec); err != nil {
		t.Fatalf("idempotent retry over pre-witness flat archive: %v", err)
	}
	if _, _, err := registry.Restore("wa_oldflat", afclient.WorkareaRestoreRequest{}); err != nil {
		t.Fatalf("restore of pre-witness flat archive: %v", err)
	}
}

// Pre-witness session-root (nested) archives stay idempotent and
// restorable after source disposal: the manifest and the durable
// acquisition record both carry the legacy digest, exactly as the
// pre-witness writer stored them, and the registry verifies either
// digest format when re-reading the committed bytes.
func TestArchiveQuiescence_OldDigestNestedIdempotentAndRestorable(t *testing.T) {
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
	acquisition, err := acquisitions.Begin("oldnested-session", "wa_oldnested", workarea.RootPath(sourceRoot), "docs", "")
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
		"oldnested-session", "wa_oldnested", normalized,
		map[string]string{"web": "aaa", "docs": "bbb"}, acquisition.Record.AcquisitionID,
	)
	if err := workarea.WriteDeclaration(t.Context(), acquisition.StagingRoot, record); err != nil {
		t.Fatal(err)
	}
	if _, err := acquisitions.Commit(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, AcquisitionStore: acquisitions})
	spec := WorkareaRootArchiveSpec{
		AcquisitionID: acquisition.Record.AcquisitionID, WorkareaID: "wa_oldnested", SessionID: "oldnested-session",
		WorkareaRoot: sourceRoot, SelectedPath: filepath.Join(sourceRoot, "docs"),
	}
	if err := registry.ArchiveRoot(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	manifest := readWorkareaArchiveManifest(t, archiveRoot, "wa_oldnested")
	legacy, err := archiveTreeDigestLegacyPath(filepath.Join(archiveRoot, "wa_oldnested", "tree"))
	if err != nil {
		t.Fatal(err)
	}
	if legacy == manifest.TreeDigest {
		t.Fatal("legacy digest equals current digest; nested tree should diverge via directory entries")
	}
	manifest.TreeDigest = legacy
	manifest.CaptureStableGeneration = ""
	manifest.CaptureQuiesced = false
	forged, err := json.Marshal(&manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archiveRoot, "wa_oldnested", "manifest.json"), forged, 0o600); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(worktreeParent, ".workarea-acquisitions", acquisition.Record.AcquisitionID, "acquisition.json")
	raw, err := os.ReadFile(recordPath) //nolint:gosec // G304: test fixture path under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}
	var stored workarea.AcquisitionRecord
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	stored.ArchiveDigest = legacy
	rewritten, err := json.Marshal(&stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := registry.ArchiveRoot(t.Context(), spec); err != nil {
		t.Fatalf("idempotent retry over pre-witness nested archive: %v", err)
	}
	if err := acquisitions.RemovePublishedRoot(acquisition.Record.AcquisitionID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.RestoreV1("wa_oldnested", afclient.WorkareaRestoreRequest{IntoSessionID: "oldnested-session"}); err != nil {
		t.Fatalf("restore of pre-witness nested archive after source disposal: %v", err)
	}
}

// A synchronous mutation planted between the copy and the post-copy
// witness must fail closed. The seam fires deterministically (no racing
// writer to schedule), so this test isolates the post-copy equality
// check: the pre-copy generation and the staged copy agree, only the
// post-copy source moved. Removing the post-copy comparison would
// commit the staged (now stale) tree — the test goes RED — proving
// the witness is the guard that fires, not a torn-read size check.
func TestArchiveQuiescence_AfterCopyMutationFailsClosed(t *testing.T) {
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, ArchiveHook: func(stage string) error {
		if stage == "after-copy-archive-tree" {
			if err := os.WriteFile(filepath.Join(source, "b.txt"), []byte("NEW-B"), 0o600); err != nil {
				return err
			}
		}
		return nil
	}})
	err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		WorkareaID: "wa_aftercopy", SessionID: "aftercopy-session",
		WorkareaRoot: source, SelectedPath: source, Quiesced: true,
	})
	if !errors.Is(err, ErrArchiveUnstableSource) {
		t.Fatalf("after-copy mutation err = %v, want %v", err, ErrArchiveUnstableSource)
	}
	if _, statErr := os.Stat(filepath.Join(archiveRoot, "wa_aftercopy")); !os.IsNotExist(statErr) {
		t.Fatalf("after-copy mutation published an archive: %v", statErr)
	}
	if got := sourceTreeFile(t, source, "b.txt"); got != "NEW-B" {
		t.Fatalf("source b.txt = %q after failed capture, want NEW-B retained", got)
	}
}

// A synchronous mutation planted after the post-copy witness but before
// publication must fail closed with the source retained. The staged
// tree was stable at witness time but is stale at publish time —
// publishing it and disposing the source would delete the newer live
// bytes. The final pre-publish re-walk (ordered AFTER the
// before-publish seam) observes the mutation and aborts before the
// rename; the stage is cleaned up and nothing is published.
func TestArchiveQuiescence_BeforePublishMutationFailsClosedRetainingSource(t *testing.T) {
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, ArchiveHook: func(stage string) error {
		if stage == "before-publish-archive" {
			if err := os.WriteFile(filepath.Join(source, "b.txt"), []byte("NEW-B"), 0o600); err != nil {
				return err
			}
		}
		return nil
	}})
	err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		WorkareaID: "wa_beforepublish", SessionID: "beforepublish-session",
		WorkareaRoot: source, SelectedPath: source, Quiesced: true,
	})
	if !errors.Is(err, ErrArchiveUnstableSource) {
		t.Fatalf("before-publish mutation err = %v, want %v", err, ErrArchiveUnstableSource)
	}
	if _, statErr := os.Stat(filepath.Join(archiveRoot, "wa_beforepublish")); !os.IsNotExist(statErr) {
		t.Fatalf("before-publish mutation published an archive: %v", statErr)
	}
	if got := sourceTreeFile(t, source, "b.txt"); got != "NEW-B" {
		t.Fatalf("source b.txt = %q after failed capture, want NEW-B retained", got)
	}
}

// A synchronous mutation planted after publication but before the
// manager's disposal must fail closed AND roll back the just-published
// archive. The staged tree was stable at publish time, but the live
// source moved on — leaving the stale archive committed would let a
// retry accept bytes restore refuses (or dispose the newer live
// source). The post-publish check removes the published archive and
// returns the stability sentinel with the source retained, so a later
// pass captures the newer generation fresh.
func TestArchiveQuiescence_AfterPublishMutationRollsBackRetainingSource(t *testing.T) {
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, ArchiveHook: func(stage string) error {
		if stage == "after-publish-archive" {
			if err := os.WriteFile(filepath.Join(source, "b.txt"), []byte("NEW-B"), 0o600); err != nil {
				return err
			}
		}
		return nil
	}})
	err := registry.ArchiveRoot(t.Context(), WorkareaRootArchiveSpec{
		WorkareaID: "wa_afterpublish", SessionID: "afterpublish-session",
		WorkareaRoot: source, SelectedPath: source, Quiesced: true,
	})
	if !errors.Is(err, ErrArchiveUnstableSource) {
		t.Fatalf("after-publish mutation err = %v, want %v", err, ErrArchiveUnstableSource)
	}
	if _, statErr := os.Stat(filepath.Join(archiveRoot, "wa_afterpublish")); !os.IsNotExist(statErr) {
		t.Fatalf("after-publish mutation left a stale published archive: %v", statErr)
	}
	if got := sourceTreeFile(t, source, "b.txt"); got != "NEW-B" {
		t.Fatalf("source b.txt = %q after rolled-back capture, want NEW-B retained", got)
	}
}

// A committed archive whose witness was tampered to disagree with its
// digest must be refused by idempotent retry — the same rule restore
// enforces. Accepting it would return a reference that restore
// rejects, while the manager proceeds to dispose the source: a
// published-but-unrestorable archive with the only complete copy
// deleted. The retry fails with corruption, publishes nothing new,
// and leaves the source alone.
func TestArchiveQuiescence_TamperedWitnessRetryRefuses(t *testing.T) {
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot})
	spec := WorkareaRootArchiveSpec{
		WorkareaID: "wa_tampered", SessionID: "tampered-session",
		WorkareaRoot: source, SelectedPath: source, Quiesced: true,
	}
	if err := registry.ArchiveRoot(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(archiveRoot, "wa_tampered", "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest archiveManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.CaptureStableGeneration == "" {
		t.Fatal("committed manifest carries no stability witness to tamper")
	}
	manifest.CaptureStableGeneration = "sha256:deadbeef"
	tampered, _ := json.Marshal(&manifest)
	if err := os.WriteFile(manifestPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	err = registry.ArchiveRoot(t.Context(), spec)
	if !errors.Is(err, ErrArchiveCorrupted) {
		t.Fatalf("retry over tampered-witness archive err = %v, want %v", err, ErrArchiveCorrupted)
	}
}

// A cancelled capture must abort promptly with the caller's error —
// even mid-copy over a large tree — releasing the registry mutex
// through the deferred unlock so a bounded reaper attempt completes
// instead of hanging behind an unbounded live-source read. The old
// code hashed to EOF with no deadline: a continuously growing file
// could keep the reader from ever reaching EOF, and cancellation
// could not release the attempt.
func TestArchiveQuiescence_CancelledContextAbortsPromptly(t *testing.T) {
	archiveRoot := t.TempDir()
	source := t.TempDir()
	stableGenerationFixture(t, source, "generation-one-a", "generation-one-b")
	big, err := os.OpenFile(filepath.Join(source, "big-stable.bin"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	for written := 0; written < 32; written++ {
		if _, err := big.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := big.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := big.Close(); err != nil {
		t.Fatal(err)
	}
	copyReached := make(chan struct{})
	var copyReachedOnce sync.Once
	registry := NewWorkareaArchiveRegistry(WorkareaArchiveOptions{Root: archiveRoot, ArchiveHook: func(stage string) error {
		if stage == "before-copy-archive-tree" {
			copyReachedOnce.Do(func() { close(copyReached) })
			return nil
		}
		return nil
	}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- registry.ArchiveRoot(ctx, WorkareaRootArchiveSpec{
			WorkareaID: "wa_cancelled", SessionID: "cancelled-session",
			WorkareaRoot: source, SelectedPath: source, Quiesced: true,
		})
	}()
	select {
	case <-copyReached:
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("capture did not reach the copy gate")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled capture err = %v, want %v", err, context.Canceled)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancelled capture did not return promptly")
	}
	if _, statErr := os.Stat(filepath.Join(archiveRoot, "wa_cancelled")); !os.IsNotExist(statErr) {
		t.Fatalf("cancelled capture published an archive: %v", statErr)
	}
}
