package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// archiveTestHandle is a live session that owns one evidence file inside the
// session workarea and refuses descriptor access before Stop. It models a
// session whose writers stop only when Stop returns.
type archiveTestHandle struct {
	events       chan agent.Event
	mu           sync.Mutex
	stopped      bool
	evidencePath string
	calls        int
	preStop      int
}

func (h *archiveTestHandle) SessionID() string          { return "archive-session" }
func (h *archiveTestHandle) Events() <-chan agent.Event { return h.events }
func (h *archiveTestHandle) Inject(context.Context, string) error {
	return nil
}

func (h *archiveTestHandle) Stop(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	return nil
}

func (h *archiveTestHandle) TerminalTranscripts() ([]agent.TerminalTranscriptDescriptor, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if !h.stopped {
		h.preStop++
		return nil, errors.New("archive: session still running")
	}
	if h.evidencePath == "" {
		return nil, nil
	}
	return []agent.TerminalTranscriptDescriptor{{Path: h.evidencePath}}, nil
}

// archiveTestProvider writes one evidence file into the provisioned checkout
// during Spawn and serves a scripted handle. crash drives a failed terminal;
// hang leaves the turn open for a context-cancel stopped exit. plantWork
// additionally leaves an uncommitted work.txt so the patch phase archives a
// patch alongside the evidence.
type archiveTestProvider struct {
	agent.HarnessProvider
	t            *testing.T
	evidenceBody string
	crash        bool
	hang         bool
	plantWork    bool
	mu           sync.Mutex
	lastHandle   *archiveTestHandle
}

func (p *archiveTestProvider) getLastHandle() *archiveTestHandle {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastHandle
}

func (p *archiveTestProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	p.t.Helper()
	if p.plantWork && spec.Cwd != "" {
		if err := os.WriteFile(filepath.Join(spec.Cwd, "work.txt"), []byte("uncommitted\n"), 0o600); err != nil {
			return nil, err
		}
	}
	dir := filepath.Join(spec.Cwd, ".agent")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "evidence-test.cast")
	if err := os.WriteFile(path, []byte(p.evidenceBody), 0o600); err != nil {
		return nil, err
	}
	ch := make(chan agent.Event, 8)
	ch <- agent.InitEvent{SessionID: "archive-session"}
	switch {
	case p.crash:
		ch <- agent.AssistantTextEvent{Text: "working"}
		ch <- agent.ErrorEvent{Message: "provider crashed mid turn"}
		close(ch)
	case p.hang:
		ch <- agent.InitEvent{SessionID: "archive-session"}
		// No terminal event: the run parks until the caller cancels.
	default:
		ch <- agent.AssistantTextEvent{Text: "WORK_RESULT:passed"}
		ch <- agent.ResultEvent{Success: true, Message: "done"}
		close(ch)
	}
	h := &archiveTestHandle{events: ch, evidencePath: path}
	p.mu.Lock()
	p.lastHandle = h
	p.mu.Unlock()
	return h, nil
}

func (p *archiveTestProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

func stubBaseForArchive(t *testing.T) agent.HarnessProvider {
	t.Helper()
	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	hp, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider is not a HarnessProvider")
	}
	return hp
}

func runArchiveSession(t *testing.T, provider *archiveTestProvider, workType, mode string, teardown bool, rescueDir string) (*Result, QueuedWork) {
	t.Helper()
	h := newRunnerHarness(t)
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	if teardown {
		h.runner.preserveAlways = false
	}
	if rescueDir != "" {
		h.runner.rescueDir = rescueDir
	}
	qw := h.queuedWork("ARCHIVE")
	qw.WorkType = workType
	qw.Mode = mode
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil && res == nil {
		t.Fatalf("Run: %v", err)
	}
	return res, qw
}

func findEvidenceArchives(t *testing.T, rescueDir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(rescueDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasPrefix(filepath.Base(path), "evidence-") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// attemptDirsWithManifest returns the teardown attempt directories under
// rescueDir that hold an evidence manifest.
func attemptDirsWithManifest(t *testing.T, rescueDir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(rescueDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && filepath.Base(path) == terminalEvidenceFileName {
			out = append(out, filepath.Dir(path))
		}
		return nil
	})
	return out
}

func readEvidenceManifest(t *testing.T, dir string) terminalEvidenceManifest {
	t.Helper()
	//nolint:gosec // G304: test-owned archive path under t.TempDir().
	body, err := os.ReadFile(filepath.Join(dir, terminalEvidenceFileName))
	if err != nil {
		t.Fatalf("read terminal evidence manifest: %v", err)
	}
	var manifest terminalEvidenceManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode terminal evidence manifest: %v", err)
	}
	return manifest
}

// archiveEvidenceInto runs the production manifest writer for one result
// into a fresh directory and returns it. Manifest failures fail the test;
// copy failures never do (best-effort), so callers assert on the copies.
func archiveEvidenceInto(t *testing.T, r *Runner, sessionID string, res *Result) string {
	t.Helper()
	dir := t.TempDir()
	dirFor := func() (string, error) { return dir, nil }
	if err := r.archiveTerminalEvidence(sessionID, res, dirFor); err != nil {
		t.Fatalf("archiveTerminalEvidence: %v", err)
	}
	return dir
}

// TestRun_FailedSessionArchivesEvidenceBeforeTeardown proves the failed path
// archives the verified content before teardown and then deletes the
// workarea: the manifest records the descriptor and the evidence-00 copy
// holds its exact bytes. Removing the content-copy call leaves the manifest
// but no copy, failing this test.
func TestRun_FailedSessionArchivesEvidenceBeforeTeardown(t *testing.T) {
	const body = "evidence-body-failed-9f3a1c\n"
	rescueDir := t.TempDir()
	provider := &archiveTestProvider{HarnessProvider: stubBaseForArchive(t), t: t, evidenceBody: body, crash: true}

	res, _ := runArchiveSession(t, provider, "research", "", true, rescueDir)
	if res.Status != "failed" {
		t.Fatalf("Status = %q; want failed (failureMode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	dirs := attemptDirsWithManifest(t, rescueDir)
	if len(dirs) != 1 {
		t.Fatalf("manifest attempts = %v; want exactly one for the failed session", dirs)
	}
	manifest := readEvidenceManifest(t, dirs[0])
	if manifest.Status != "failed" || len(manifest.Transcripts) != 1 {
		t.Fatalf("evidence manifest = %+v; want failed with exactly one transcript", manifest)
	}
	archives := findEvidenceArchives(t, rescueDir)
	if len(archives) != 1 {
		t.Fatalf("evidence archives = %v; want exactly one verified copy", archives)
	}
	//nolint:gosec // G304: test-owned archive path under t.TempDir().
	archived, err := os.ReadFile(archives[0])
	if err != nil {
		t.Fatalf("read archived evidence: %v", err)
	}
	if string(archived) != body {
		t.Fatalf("archived evidence = %q; want %q", archived, body)
	}
	if res.WorktreePath == "" {
		t.Fatal("failed session has no worktree path")
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the failed session's workarea torn down after archiving", err)
	}
	if h := provider.getLastHandle(); h == nil {
		t.Fatal("no handle captured")
	} else if _, err := os.Stat(h.evidencePath); !os.IsNotExist(err) {
		t.Fatalf("evidence source stat err = %v; want the workarea lifecycle to own it", err)
	}
}

// TestRun_StoppedSessionArchivesEvidenceBeforeTeardown proves the stopped
// path archives the verified content before teardown and then deletes the
// workarea. It drives a stopped exit through an interview-mode hang plus
// context cancel, the reachable stopped shape through production Run.
func TestRun_StoppedSessionArchivesEvidenceBeforeTeardown(t *testing.T) {
	if _, err := stub.New(); err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	const body = "evidence-body-stopped-4d2e8a\n"
	rescueDir := t.TempDir()

	h := newRunnerHarness(t)
	provider := &archiveTestProvider{HarnessProvider: stubBaseForArchive(t), t: t, evidenceBody: body, hang: true}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	h.runner.preserveAlways = false
	h.runner.rescueDir = rescueDir

	qw := h.queuedWork("ARCHIVE-STOPPED")
	qw.WorkType = "research"
	qw.Mode = "interview"

	runCtx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		res *Result
		err error
	}
	done := make(chan runResult, 1)
	go func() {
		res, runErr := h.runner.Run(runCtx, qw)
		done <- runResult{res: res, err: runErr}
	}()
	// Wait for the evidence file to exist, proving Spawn finished and the
	// session is parked, then cancel to end it stopped.
	deadline := time.Now().Add(10 * time.Second)
	for func() bool { h := provider.getLastHandle(); return h == nil || h.evidencePath == "" }() {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Run did not reach provider spawn before cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give the run a moment to park on the open stream before cancelling.
	time.Sleep(200 * time.Millisecond)
	cancel()
	var got runResult
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if got.res == nil || got.res.Status != "stopped" {
		t.Fatalf("Status = %+v err=%v; want stopped", got.res, got.err)
	}
	dirs := attemptDirsWithManifest(t, rescueDir)
	if len(dirs) != 1 {
		t.Fatalf("manifest attempts = %v; want exactly one for the stopped session", dirs)
	}
	manifest := readEvidenceManifest(t, dirs[0])
	if manifest.Status != "stopped" || len(manifest.Transcripts) != 1 {
		t.Fatalf("evidence manifest = %+v; want stopped with exactly one transcript", manifest)
	}
	archives := findEvidenceArchives(t, rescueDir)
	if len(archives) != 1 {
		t.Fatalf("evidence archives = %v; want exactly one verified copy for the stopped session", archives)
	}
	//nolint:gosec // G304: test-owned archive path under t.TempDir().
	archived, err := os.ReadFile(archives[0])
	if err != nil {
		t.Fatalf("read archived evidence: %v", err)
	}
	if string(archived) != body {
		t.Fatalf("archived evidence = %q; want %q", archived, body)
	}
	if _, err := os.Stat(got.res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the stopped session's workarea torn down after archiving", err)
	}
}

// TestRun_CompletedSessionCreatesNoEvidenceArchive proves the negative gate:
// a completed session with a declared descriptor writes no manifest and no
// copy. Broadening the status gate to completed would write both and fail.
func TestRun_CompletedSessionCreatesNoEvidenceArchive(t *testing.T) {
	const body = "evidence-body-completed-7b1e5d\n"
	rescueDir := t.TempDir()
	provider := &archiveTestProvider{HarnessProvider: stubBaseForArchive(t), t: t, evidenceBody: body}

	res, _ := runArchiveSession(t, provider, "research", "", true, rescueDir)
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (failureMode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	if archives := findEvidenceArchives(t, rescueDir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none for a completed session", archives)
	}
	if dirs := attemptDirsWithManifest(t, rescueDir); len(dirs) != 0 {
		t.Fatalf("manifest attempts = %v; want none for a completed session", dirs)
	}
}

// TestRun_FailedSessionWithUnreadableEvidenceStillTearsDown proves the
// best-effort side through production Run: when the declared evidence
// cannot be read, the manifest still records the descriptor, no copy is
// written, and the teardown still proceeds. Evidence failure never keeps
// the workarea.
func TestRun_FailedSessionWithUnreadableEvidenceStillTearsDown(t *testing.T) {
	rescueDir := t.TempDir()
	// The handle declares a path that does not exist, so the copy is
	// skipped while the manifest still records it.
	h := newRunnerHarness(t)
	base := stubBaseForArchive(t)
	provider := &missingEvidenceProvider{HarnessProvider: base}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	h.runner.preserveAlways = false
	h.runner.rescueDir = rescueDir

	qw := h.queuedWork("ARCHIVE-UNREADABLE")
	qw.WorkType = "research"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil && res == nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("Status = %q; want failed (failureMode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	dirs := attemptDirsWithManifest(t, rescueDir)
	if len(dirs) != 1 {
		t.Fatalf("manifest attempts = %v; want exactly one: the manifest records even unreadable descriptors", dirs)
	}
	if archives := findEvidenceArchives(t, rescueDir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none: the unreadable file must be skipped, not copied", archives)
	}
	if res.WorktreePath == "" {
		t.Fatal("failed session has no worktree path")
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the failed session's workarea torn down despite the unreadable evidence", err)
	}
}

// missingEvidenceProvider drives a failed terminal while declaring an
// evidence path that cannot be read.
type missingEvidenceProvider struct {
	agent.HarnessProvider
}

func (p *missingEvidenceProvider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	ch := make(chan agent.Event, 8)
	ch <- agent.InitEvent{SessionID: "archive-session"}
	ch <- agent.AssistantTextEvent{Text: "working"}
	ch <- agent.ErrorEvent{Message: "provider crashed mid turn"}
	close(ch)
	return &missingEvidenceHandle{events: ch}, nil
}

func (p *missingEvidenceProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

type missingEvidenceHandle struct {
	events  chan agent.Event
	mu      sync.Mutex
	stopped bool
}

func (h *missingEvidenceHandle) SessionID() string          { return "archive-session" }
func (h *missingEvidenceHandle) Events() <-chan agent.Event { return h.events }
func (h *missingEvidenceHandle) Inject(context.Context, string) error {
	return nil
}

func (h *missingEvidenceHandle) Stop(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	return nil
}

func (h *missingEvidenceHandle) TerminalTranscripts() ([]agent.TerminalTranscriptDescriptor, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopped {
		return nil, errors.New("archive: session still running")
	}
	return []agent.TerminalTranscriptDescriptor{{Path: filepath.Join(os.TempDir(), "donmai-missing-evidence-9f3a1c.cast")}}, nil
}

// TestRun_MixedPatchAndEvidenceShareSingleAttempt proves the shared-scope
// teardown end to end: one failed session carrying both unpublished work
// and a readable transcript archives the patch, the manifest and the
// verified copy into a single attempt directory.
func TestRun_MixedPatchAndEvidenceShareSingleAttempt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const body = "evidence-body-mixed-5c9a2e\n"
	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	srv := mockPlatformServer(t)
	t.Cleanup(srv.Close)
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL, WorkerID: "worker-1", AuthToken: "tok",
		HTTPClient: srv.Client(), BaseDelay: 1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	provider := &archiveTestProvider{HarnessProvider: stubBaseForArchive(t), t: t, evidenceBody: body, crash: true, plantWork: true}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := New(Options{
		Registry: reg, WorktreeManager: manager, Poster: poster,
		HTTPClient: srv.Client(), SkipBackstop: true, SkipSteering: true, SkipPostSession: true,
		MaxSessionDuration: -1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rescueDir := t.TempDir()
	r.rescueDir = rescueDir
	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("MIXED-SINGLE"),
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		PlatformURL: srv.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}
	qw.SessionID = "test-session-MIXED-SINGLE"
	qw.Repository = bareRepo
	qw.WorkType = "research"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, runErr := r.Run(ctx, qw)
	if runErr != nil && res == nil {
		t.Fatalf("Run: %v", runErr)
	}
	if res.Status != "failed" {
		t.Fatalf("Status = %q; want failed (failureMode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	entries, err := os.ReadDir(filepath.Join(rescueDir, "test-session-MIXED-SINGLE"))
	if err != nil {
		t.Fatalf("read session rescue root: %v", err)
	}
	var archives []string
	for _, entry := range entries {
		if entry.IsDir() && rescueArchiveDirRE.MatchString(entry.Name()) {
			archives = append(archives, entry.Name())
		}
	}
	if len(archives) != 1 {
		t.Fatalf("attempt directories = %v; want exactly 1 sharing patch and evidence", archives)
	}
	dir := filepath.Join(rescueDir, "test-session-MIXED-SINGLE", archives[0])
	if patches := rescuePatches(t, dir); len(patches) == 0 {
		t.Fatal("mixed teardown directory holds no patch; want the patch and the evidence together")
	}
	manifest := readEvidenceManifest(t, dir)
	if manifest.Status != "failed" || len(manifest.Transcripts) != 1 {
		t.Fatalf("evidence manifest = %+v; want failed with exactly one transcript", manifest)
	}
	copies := findEvidenceArchives(t, dir)
	if len(copies) != 1 {
		t.Fatalf("evidence copies = %v; want exactly one verified copy beside the patch", copies)
	}
	//nolint:gosec // G304: test-owned archive path under t.TempDir().
	archived, err := os.ReadFile(copies[0])
	if err != nil {
		t.Fatalf("read archived evidence: %v", err)
	}
	if string(archived) != body {
		t.Fatalf("archived evidence = %q; want %q", archived, body)
	}
	if res.WorktreePath == "" {
		t.Fatal("failed session has no worktree path")
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the failed session's workarea torn down", err)
	}
}

// TestArchiveTerminalEvidence_ReadFailureSkipsCopy proves the fail-open
// read side: a missing evidence file is skipped, the manifest still
// records its descriptor, and the writer returns nil so the teardown
// proceeds. The copy is best-effort; only the manifest is load-bearing.
func TestArchiveTerminalEvidence_ReadFailureSkipsCopy(t *testing.T) {
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	missing := filepath.Join(t.TempDir(), "missing.cast")
	res := &Result{Result: agent.Result{Status: "failed"}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: missing}}

	dir := archiveEvidenceInto(t, r, "test-session-EVIDENCE-READ-FAIL", res)
	manifest := readEvidenceManifest(t, dir)
	if len(manifest.Transcripts) != 1 || manifest.Transcripts[0] != missing {
		t.Fatalf("evidence manifest = %+v; want exactly [%q]", manifest, missing)
	}
	if archives := findEvidenceArchives(t, dir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none on read failure", archives)
	}
}

// TestArchiveTerminalEvidence_EmptyDescriptorsWriteNoAttempt proves the
// empty-evidence boundary: descriptors that are all empty write no
// archive directory at all. Creating the directory before validating the
// descriptors would record a phantom attempt.
func TestArchiveTerminalEvidence_EmptyDescriptorsWriteNoAttempt(t *testing.T) {
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{Result: agent.Result{Status: "failed"}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: ""}, {Path: ""}}

	called := false
	dirFor := func() (string, error) {
		called = true
		return t.TempDir(), nil
	}
	if err := r.archiveTerminalEvidence("test-session-EVIDENCE-EMPTY", res, dirFor); err != nil {
		t.Fatalf("archiveTerminalEvidence: %v", err)
	}
	if called {
		t.Fatal("archiveTerminalEvidence created an attempt directory for all-empty descriptors; want no directory")
	}
}

// TestWriteVerifiedEvidenceCopy_WriteFailureReturnsFalse pins the
// archive-stage write refusal: a failing copy write returns false so the
// file is skipped. Ignoring the write error while a readback seam reports
// matching bytes would return true for a copy that was never written.
func TestWriteVerifiedEvidenceCopy_WriteFailureReturnsFalse(t *testing.T) {
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	r.terminalEvidenceWriteFile = func(string, []byte, os.FileMode) error { return errors.New("injected archive write failure") }
	r.terminalEvidenceReadFile = func(string) ([]byte, error) { return []byte("archive-write-pin\n"), nil }

	if r.writeVerifiedEvidenceCopy(filepath.Join(t.TempDir(), "evidence-00-x.cast"), []byte("archive-write-pin\n")) {
		t.Fatal("writeVerifiedEvidenceCopy = true with archive write failure; want false so the file is skipped")
	}
}

// TestArchiveTerminalEvidence_OversizeSkipsCopy pins the size bound: an
// evidence file larger than terminalEvidenceMaxBytes is skipped before any
// unbounded read, the manifest still records it, and no copy is written.
// The sparse 2x-bound fixture plus the allocation cap proves the read is
// bounded: an unbounded ReadFile would allocate the whole file before
// rejecting. Deleting the bound would copy the oversize file.
func TestArchiveTerminalEvidence_OversizeSkipsCopy(t *testing.T) {
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	src := filepath.Join(t.TempDir(), "evidence.cast")
	f, err := os.Create(src)
	if err != nil {
		t.Fatalf("create oversize evidence: %v", err)
	}
	if err := f.Truncate(int64(terminalEvidenceMaxBytes) * 2); err != nil {
		_ = f.Close()
		t.Fatalf("truncate oversize evidence: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close oversize evidence: %v", err)
	}
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat oversize evidence: %v", err)
	}
	if fi.Size() <= int64(terminalEvidenceMaxBytes) {
		t.Fatalf("oversize fixture size = %d; want more than %d", fi.Size(), int64(terminalEvidenceMaxBytes))
	}
	res := &Result{Result: agent.Result{Status: "failed"}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: src}}

	// Bound the read: rejecting a 2x-bound sparse file must not allocate
	// the file. An unbounded pre-check-after-read allocates ~128 MiB here.
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	dir := archiveEvidenceInto(t, r, "test-session-EVIDENCE-OVERSIZE", res)
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	if grew := memAfter.TotalAlloc - memBefore.TotalAlloc; grew > uint64(terminalEvidenceMaxBytes) {
		t.Fatalf("archive allocated %d bytes rejecting oversize evidence; want < %d (bounded read)", grew, uint64(terminalEvidenceMaxBytes))
	}

	manifest := readEvidenceManifest(t, dir)
	if len(manifest.Transcripts) != 1 || manifest.Transcripts[0] != src {
		t.Fatalf("evidence manifest = %+v; want exactly [%q]", manifest, src)
	}
	if archives := findEvidenceArchives(t, dir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none on oversize rejection", archives)
	}
}

// TestArchiveTerminalEvidence_SymlinkSkipsOutsideTarget pins the
// non-regular rejection: a symlink descriptor is refused before any read,
// the outside target is never copied, and the manifest still records the
// declared path. Weakening the gate to error-only would follow the link.
func TestArchiveTerminalEvidence_SymlinkSkipsOutsideTarget(t *testing.T) {
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "outside-secret.cast")
	const secretBody = "outside-secret-should-never-be-archived\n"
	if err := os.WriteFile(secret, []byte(secretBody), 0o600); err != nil {
		t.Fatalf("write outside secret: %v", err)
	}
	link := filepath.Join(t.TempDir(), "evidence.cast")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatalf("symlink evidence: %v", err)
	}
	res := &Result{Result: agent.Result{Status: "failed"}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: link}}

	dir := archiveEvidenceInto(t, r, "test-session-EVIDENCE-SYMLINK", res)
	if archives := findEvidenceArchives(t, dir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none on symlink rejection", archives)
	}
	manifest := readEvidenceManifest(t, dir)
	if len(manifest.Transcripts) != 1 || manifest.Transcripts[0] != link {
		t.Fatalf("evidence manifest = %+v; want exactly [%q]", manifest, link)
	}
}

// TestOpenTerminalEvidenceFile_RejectsSymlinkAndFIFO pins the safe open:
// a trailing symlink fails instead of archiving its target, and a FIFO
// opens without blocking so the regular-file check can reject it.
// Ordinary os.Open would follow the symlink and block on the FIFO.
func TestOpenTerminalEvidenceFile_RejectsSymlinkAndFIFO(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "outside.cast")
	if err := os.WriteFile(outside, []byte("outside-content\n"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	link := filepath.Join(t.TempDir(), "evidence.cast")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if f, err := openTerminalEvidenceFile(link); err == nil {
		_ = f.Close()
		t.Fatal("openTerminalEvidenceFile(symlink) = nil error; want ELOOP so the target is never archived")
	}
	if runtime.GOOS == "windows" {
		t.Skip("FIFO open semantics are unix-only")
	}
	fifo := filepath.Join(t.TempDir(), "evidence.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	type openResult struct {
		f   *os.File
		err error
	}
	done := make(chan openResult, 1)
	go func() {
		f, err := openTerminalEvidenceFile(fifo)
		done <- openResult{f: f, err: err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("openTerminalEvidenceFile(fifo) err = %v; want success without blocking so Stat can reject", r.err)
		}
		defer func() { _ = r.f.Close() }()
		if st, err := r.f.Stat(); err != nil || st.Mode().IsRegular() {
			t.Fatalf("fifo Stat regular = %v err = %v; want non-regular rejection", st, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("openTerminalEvidenceFile(fifo) blocked over 5s; want O_NONBLOCK so terminal delivery never stalls")
	}
}

// TestCopyTerminalEvidenceContents_SwappedSymlinkNeverCopiesOutside drives
// the production copy while another process swaps the declared path
// between a regular file and a symlink to outside content. No copy may
// ever hold the outside bytes: with O_NOFOLLOW the swapped open fails and
// the file is skipped.
func TestCopyTerminalEvidenceContents_SwappedSymlinkNeverCopiesOutside(t *testing.T) {
	r := minimalRunner(t)
	dir := t.TempDir()
	work := t.TempDir()
	const victimBody = "victim-transcript-body\n"
	const outsideBody = "outside-content-must-never-be-archived\n"
	outside := filepath.Join(work, "outside.cast")
	if err := os.WriteFile(outside, []byte(outsideBody), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	path := filepath.Join(work, "evidence.cast")
	if err := os.WriteFile(path, []byte(victimBody), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(path)
			_ = os.WriteFile(path, []byte(victimBody), 0o600)
			_ = os.Remove(path)
			_ = os.Symlink(outside, path)
		}
	}()
	res := &Result{Result: agent.Result{Status: "failed"}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: path}}
	for i := 0; i < 100; i++ {
		r.copyTerminalEvidenceContents("test-session-EVIDENCE-SWAP", res, dir)
		_ = i
	}
	close(stop)
	wg.Wait()
	for _, a := range findEvidenceArchives(t, dir) {
		//nolint:gosec // G304: test-owned archive path under t.TempDir().
		raw, err := os.ReadFile(a)
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if string(raw) == outsideBody {
			t.Fatalf("archive %s holds outside content; swapped symlink must skip the copy", a)
		}
	}
}

// TestCopyTerminalEvidenceContents_FIFONeverBlocksCopy proves a FIFO at
// the declared path fails fast instead of stalling terminal completion.
// Ordinary os.Open would block until a writer appears, ahead of result
// posting, with no deadline.
func TestCopyTerminalEvidenceContents_FIFONeverBlocksCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO semantics are unix-only")
	}
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	dir := t.TempDir()
	fifo := filepath.Join(t.TempDir(), "evidence.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	res := &Result{Result: agent.Result{Status: "failed"}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: fifo}}
	done := make(chan struct{}, 1)
	go func() {
		r.copyTerminalEvidenceContents("test-session-EVIDENCE-FIFO", res, dir)
		done <- struct{}{}
	}()
	select {
	case <-done:
		if archives := findEvidenceArchives(t, dir); len(archives) != 0 {
			t.Fatalf("evidence archives = %v; want none: a FIFO must be skipped, never copied", archives)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("copyTerminalEvidenceContents(fifo) blocked over 10s; swapped FIFO must skip fast")
	}
}

// TestWriteVerifiedEvidenceCopy_ReadbackMismatchRemovesCopy pins the
// archive readback refusal: a corrupted or unreadable copy is removed so
// no partial copy is mistaken for evidence, and the file is skipped.
// Disabling the comparison would keep the corrupted copy.
func TestWriteVerifiedEvidenceCopy_ReadbackMismatchRemovesCopy(t *testing.T) {
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	dir := t.TempDir()
	dest := filepath.Join(dir, "evidence-00-evidence.cast")
	const body = "readback-pin-body\n"
	r.terminalEvidenceReadFile = func(string) ([]byte, error) { return []byte("corrupted-mismatch\n"), nil }
	if r.writeVerifiedEvidenceCopy(dest, []byte(body)) {
		t.Fatal("writeVerifiedEvidenceCopy = true with mismatched readback; want false so the file is skipped")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("copy stat err = %v; want the mismatched copy removed, not kept", err)
	}
}

// TestCopyTerminalEvidenceContents_SwappedFIFORefusesEmptyCopy pins the
// post-open regular-file refusal through the production copy path: the
// declared path is a regular file at the Lstat hint but a FIFO when
// opened (swapped between the two, then opened with the production
// helper so O_NONBLOCK semantics are exercised, not a stub). The
// post-open Stat must refuse; otherwise the FIFO reads zero bytes as
// success and an empty copy is written. Removing the post-open IsRegular
// check archives the empty bytes as success.
func TestCopyTerminalEvidenceContents_SwappedFIFORefusesEmptyCopy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO semantics are unix-only")
	}
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "evidence.cast")
	if err := os.WriteFile(path, []byte("victim-transcript-body\n"), 0o600); err != nil {
		t.Fatalf("write victim evidence: %v", err)
	}
	// Swap between the Lstat hint and Open: by the time the production
	// open runs, the declared path is a FIFO. The Lstat inside the copy
	// still sees the regular victim file.
	r.terminalEvidenceOpenFile = func(p string) (*os.File, error) {
		if err := os.Remove(path); err != nil {
			return nil, err
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			return nil, err
		}
		return openTerminalEvidenceFile(p)
	}
	res := &Result{Result: agent.Result{Status: "failed"}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: path}}

	r.copyTerminalEvidenceContents("test-session-EVIDENCE-SWAP-FIFO", res, dir)
	if archives := findEvidenceArchives(t, dir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none: a swapped FIFO must never archive empty bytes as success", archives)
	}
}

// TestCopyTerminalEvidenceContents_GrownFileRefusesTruncatedCopy pins the
// post-read truncation refusal through the production copy path: the file
// is small at both size checks but grows past the bound before the read
// completes (simulated by observing over-bound bytes for a small
// on-disk file, exactly the Lstat-to-read growth window the check
// guards). The grown content is skipped, never copied truncated.
// Disabling the post-read length check archives the truncated copy.
func TestCopyTerminalEvidenceContents_GrownFileRefusesTruncatedCopy(t *testing.T) {
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "evidence.cast")
	const body = "small-at-stat-grows-before-read\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatalf("write source evidence: %v", err)
	}
	// The size checks see the small on-disk file; the read observes the
	// grown content. One probe byte past the bound is enough to refuse.
	r.terminalEvidenceReadSource = func(*os.File) ([]byte, error) {
		return make([]byte, terminalEvidenceMaxBytes+1), nil
	}
	res := &Result{Result: agent.Result{Status: "failed"}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: src}}

	r.copyTerminalEvidenceContents("test-session-EVIDENCE-GROWN", res, dir)
	if archives := findEvidenceArchives(t, dir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none: grown content must never archive a truncated copy", archives)
	}
}

// TestRun_FailedSessionParentSwapNeverArchivesOutside pins the post-open
// identity refusal through production Run: the declared path is a regular
// victim file at the Lstat hint, but its parent directory is swapped for a
// symlink to an outside directory holding the same basename before the
// production open runs. O_NOFOLLOW protects only the trailing component,
// so the open succeeds on the outside file; only the SameFile identity
// comparison against the Lstat result refuses. Without it the outside
// content would be copied and the teardown would delete the workarea
// holding the original evidence. Best-effort: the file is skipped, the
// manifest still records it, and the teardown proceeds.
func TestRun_FailedSessionParentSwapNeverArchivesOutside(t *testing.T) {
	const victimBody = "victim-transcript-body-b1\n"
	const outsideBody = "outside-content-must-never-be-archived-b1\n"
	rescueDir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "evidence-test.cast"), []byte(outsideBody), 0o600); err != nil {
		t.Fatalf("write outside evidence: %v", err)
	}

	h := newRunnerHarness(t)
	provider := &archiveTestProvider{HarnessProvider: stubBaseForArchive(t), t: t, evidenceBody: victimBody, crash: true}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	h.runner.preserveAlways = false
	h.runner.rescueDir = rescueDir

	swapped := false
	h.runner.terminalEvidenceOpenFile = func(p string) (*os.File, error) {
		if !swapped {
			swapped = true
			parent := filepath.Dir(p)
			backup := parent + ".victim-bak"
			if err := os.Rename(parent, backup); err != nil {
				return nil, err
			}
			if err := os.Symlink(outsideDir, parent); err != nil {
				_ = os.Rename(backup, parent)
				return nil, err
			}
		}
		return openTerminalEvidenceFile(p)
	}

	qw := h.queuedWork("ARCHIVE-PARENT-SWAP")
	qw.WorkType = "research"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil && res == nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("Status = %q; want failed (failureMode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	// Best-effort: the swapped file is skipped, the manifest still records
	// the descriptor, and the teardown proceeds.
	dirs := attemptDirsWithManifest(t, rescueDir)
	if len(dirs) != 1 {
		t.Fatalf("manifest attempts = %v; want exactly one", dirs)
	}
	for _, a := range findEvidenceArchives(t, rescueDir) {
		//nolint:gosec // G304: test-owned archive path under t.TempDir().
		raw, err := os.ReadFile(a)
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if strings.Contains(string(raw), outsideBody) {
			t.Fatalf("archive %s holds outside content; parent swap must skip the copy", a)
		}
		if len(raw) != 0 {
			t.Fatalf("archive %s holds %d bytes; want none: the swapped open must not copy", a, len(raw))
		}
	}
	if archives := findEvidenceArchives(t, rescueDir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none: identity mismatch must skip before any copy", archives)
	}
	if res.WorktreePath == "" {
		t.Fatal("failed session has no worktree path")
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the failed session's workarea torn down", err)
	}
}

// TestRun_FailedSessionSourceReadErrorSkipsCopy pins the source-read error
// refusal through production Run: the declared file exists and opens, but
// the read returns partial bytes plus an error. The file is skipped, no
// partial copy is written, the manifest still records the descriptor, and
// the teardown proceeds. Ignoring the read error would archive the partial
// bytes and verify that partial copy as success.
func TestRun_FailedSessionSourceReadErrorSkipsCopy(t *testing.T) {
	const victimBody = "victim-transcript-body-b2\n"
	const partialBody = "partial-bytes-b2\n"
	rescueDir := t.TempDir()

	h := newRunnerHarness(t)
	provider := &archiveTestProvider{HarnessProvider: stubBaseForArchive(t), t: t, evidenceBody: victimBody, crash: true}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	h.runner.preserveAlways = false
	h.runner.rescueDir = rescueDir
	h.runner.terminalEvidenceReadSource = func(*os.File) ([]byte, error) {
		return []byte(partialBody), errors.New("injected source read failure")
	}

	qw := h.queuedWork("ARCHIVE-SOURCE-ERROR")
	qw.WorkType = "research"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil && res == nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "failed" {
		t.Fatalf("Status = %q; want failed (failureMode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	dirs := attemptDirsWithManifest(t, rescueDir)
	if len(dirs) != 1 {
		t.Fatalf("manifest attempts = %v; want exactly one", dirs)
	}
	for _, a := range findEvidenceArchives(t, rescueDir) {
		//nolint:gosec // G304: test-owned archive path under t.TempDir().
		raw, err := os.ReadFile(a)
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if strings.Contains(string(raw), partialBody) {
			t.Fatalf("archive %s holds the partial read; a read error must never archive", a)
		}
	}
	if archives := findEvidenceArchives(t, rescueDir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none: a source read error must skip before any copy", archives)
	}
	if res.WorktreePath == "" {
		t.Fatal("failed session has no worktree path")
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the failed session's workarea torn down", err)
	}
}

// TestReadTerminalEvidenceChunk_ProbeByteDetectsGrowth pins the extra
// probe byte in the production bounded reader with a real file: the file
// is opened small and then grows past the limit before the read, exactly
// the Stat-to-read growth window the probe guards. The reader must return
// limit+1 bytes so the caller refuses instead of archiving a truncated
// copy as success. Reading only limit bytes would return success for the
// truncated prefix.
func TestReadTerminalEvidenceChunk_ProbeByteDetectsGrowth(t *testing.T) {
	const limit int64 = 16
	path := filepath.Join(t.TempDir(), "evidence.cast")
	if err := os.WriteFile(path, []byte("01234567"), 0o600); err != nil {
		t.Fatalf("write small evidence: %v", err)
	}
	//nolint:gosec // G304: test-owned path under t.TempDir().
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open evidence: %v", err)
	}
	defer func() { _ = f.Close() }()
	// Grow after open through a second handle: the read below must
	// observe the grown content even though the file was small at open.
	//nolint:gosec // G304: test-owned path under t.TempDir().
	g, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open evidence for growth: %v", err)
	}
	if _, err := g.WriteString("0123456789ABCDEF"); err != nil {
		_ = g.Close()
		t.Fatalf("grow evidence: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("close grown evidence: %v", err)
	}
	raw, err := readTerminalEvidenceChunk(f, limit)
	if err != nil {
		t.Fatalf("readTerminalEvidenceChunk: %v", err)
	}
	if int64(len(raw)) != limit+1 {
		t.Fatalf("readTerminalEvidenceChunk len = %d; want %d (the probe byte past the bound)", len(raw), limit+1)
	}
	if int64(len(raw)) <= limit {
		t.Fatalf("readTerminalEvidenceChunk len = %d; want more than %d so growth refuses", len(raw), limit)
	}
}
