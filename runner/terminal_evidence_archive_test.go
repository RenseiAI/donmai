package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
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
// hang leaves the turn open for a context-cancel stopped exit.
type archiveTestProvider struct {
	agent.HarnessProvider
	t            *testing.T
	evidenceBody string
	crash        bool
	hang         bool
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

// TestRun_FailedSessionArchivesEvidenceBeforeTeardown proves the failed path
// archives before teardown and then deletes the workarea. Removing the call
// leaves no archive; moving it after teardown finds no source to copy.
func TestRun_FailedSessionArchivesEvidenceBeforeTeardown(t *testing.T) {
	const body = "evidence-body-failed-9f3a1c\n"
	rescueDir := t.TempDir()
	provider := &archiveTestProvider{HarnessProvider: stubBaseForArchive(t), t: t, evidenceBody: body, crash: true}

	res, _ := runArchiveSession(t, provider, "research", "", true, rescueDir)
	if res.Status != "failed" {
		t.Fatalf("Status = %q; want failed (failureMode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	archives := findEvidenceArchives(t, rescueDir)
	if len(archives) != 1 {
		t.Fatalf("evidence archives = %v; want exactly one", archives)
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
// path archives before teardown and then deletes the workarea. It drives a
// stopped exit through an interview-mode hang plus context cancel, the
// reachable stopped shape through production Run.
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
	archives := findEvidenceArchives(t, rescueDir)
	if len(archives) != 1 {
		t.Fatalf("evidence archives = %v; want exactly one for the stopped session", archives)
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
// a completed session with a declared descriptor writes no archive.
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
}

// TestPreserveTerminalEvidence_ReadFailureRetainsWorkarea proves the
// fail-closed side: a missing evidence file suppresses teardown and emits
// only the value-free warning. Ignoring the error would tear down.
func TestPreserveTerminalEvidence_ReadFailureRetainsWorkarea(t *testing.T) {
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	missing := filepath.Join(t.TempDir(), "missing.cast")
	res := &Result{Result: agent.Result{Status: "failed"}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: missing}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("EVIDENCE-READ-FAIL")}

	if r.preserveTerminalEvidence(qw, res) {
		t.Fatal("preserveTerminalEvidence = true for a missing evidence file; want false so the workarea is kept")
	}
	if len(res.PostSessionWarnings) != 1 || res.PostSessionWarnings[0] != terminalEvidenceWarnMessage {
		t.Fatalf("PostSessionWarnings = %v; want exactly [%q]", res.PostSessionWarnings, terminalEvidenceWarnMessage)
	}
	if len(res.Result.PostSessionWarnings) != 1 || res.Result.PostSessionWarnings[0] != terminalEvidenceWarnMessage {
		t.Fatalf("wire PostSessionWarnings = %v; want exactly [%q]", res.Result.PostSessionWarnings, terminalEvidenceWarnMessage)
	}
	for _, w := range append(append([]string{}, res.PostSessionWarnings...), res.Result.PostSessionWarnings...) {
		if strings.Contains(w, missing) || strings.Contains(w, ".cast") {
			t.Fatalf("warning %q carries a path; want value-free", w)
		}
	}
	if archives := findEvidenceArchives(t, r.rescueDir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none on read failure", archives)
	}
}

// TestPreserveTerminalEvidence_WriteFailureRetainsWorkarea proves the
// write-failure side: an unwritable rescue directory suppresses teardown
// with the same value-free warning.
func TestPreserveTerminalEvidence_WriteFailureRetainsWorkarea(t *testing.T) {
	r := minimalRunner(t)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("a file where the rescue directory should go\n"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	r.rescueDir = blocked
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "evidence.cast")
	const secretBody = "secret-value-should-never-appear-in-warning\n"
	if err := os.WriteFile(src, []byte(secretBody), 0o600); err != nil {
		t.Fatalf("write source evidence: %v", err)
	}
	res := &Result{Result: agent.Result{Status: "failed"}}
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: src}}
	qw := QueuedWork{QueuedWork: queuedWorkBase("EVIDENCE-WRITE-FAIL")}

	if r.preserveTerminalEvidence(qw, res) {
		t.Fatal("preserveTerminalEvidence = true with an unwritable rescue directory; want false")
	}
	if len(res.PostSessionWarnings) != 1 || res.PostSessionWarnings[0] != terminalEvidenceWarnMessage {
		t.Fatalf("PostSessionWarnings = %v; want exactly [%q]", res.PostSessionWarnings, terminalEvidenceWarnMessage)
	}
	for _, w := range append(append([]string{}, res.PostSessionWarnings...), res.Result.PostSessionWarnings...) {
		if strings.Contains(w, secretBody) || strings.Contains(w, src) || strings.Contains(w, blocked) {
			t.Fatalf("warning %q leaks a path or value; want value-free", w)
		}
	}
}

// TestRun_FailedSessionWithUnreadableEvidenceKeepsWorkarea proves the Run
// wiring for the failure side: when the declared evidence cannot be safely
// preserved, the failed session keeps its workarea instead of tearing it
// down.
func TestRun_FailedSessionWithUnreadableEvidenceKeepsWorkarea(t *testing.T) {
	rescueDir := t.TempDir()
	// The handle declares a path that does not exist, so the read fails and
	// the workarea must be retained.
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
	if _, err := os.Stat(res.WorktreePath); err != nil {
		t.Fatalf("worktree stat err = %v; want the workarea kept when evidence cannot be preserved", err)
	}
	found := false
	for _, w := range append(append([]string{}, res.PostSessionWarnings...), res.Result.PostSessionWarnings...) {
		if w == terminalEvidenceWarnMessage {
			found = true
		}
		if strings.Contains(w, ".cast") || strings.Contains(w, "/") && strings.Contains(w, "missing") {
			t.Fatalf("warning %q leaks a path; want value-free", w)
		}
	}
	if !found {
		t.Fatalf("PostSessionWarnings = %v + wire %v; want the value-free warning", res.PostSessionWarnings, res.Result.PostSessionWarnings)
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

// TestPreserveTerminalEvidence_OversizeRetainsWorkarea pins the size bound:
// an evidence file larger than terminalEvidenceMaxBytes is rejected before any
// unbounded read, the workarea is kept, exactly the value-free warning is
// emitted, and no archive is written. The sparse 2x-bound fixture plus the
// allocation cap proves the read is bounded: an unbounded ReadFile would
// allocate the whole file before rejecting. Deleting the bound would archive
// the oversize file.
func TestPreserveTerminalEvidence_OversizeRetainsWorkarea(t *testing.T) {
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
	qw := QueuedWork{QueuedWork: queuedWorkBase("EVIDENCE-OVERSIZE")}

	// Bound the read: rejecting a 2x-bound sparse file must not allocate the
	// file. An unbounded pre-check-after-read allocates ~128 MiB here.
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	preserved := r.preserveTerminalEvidence(qw, res)
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	if grew := memAfter.TotalAlloc - memBefore.TotalAlloc; grew > uint64(terminalEvidenceMaxBytes) {
		t.Fatalf("preserve allocated %d bytes rejecting oversize evidence; want < %d (bounded read)", grew, uint64(terminalEvidenceMaxBytes))
	}

	if preserved {
		t.Fatal("preserveTerminalEvidence = true for an oversize evidence file; want false so the workarea is kept")
	}
	if len(res.PostSessionWarnings) != 1 || res.PostSessionWarnings[0] != terminalEvidenceWarnMessage {
		t.Fatalf("PostSessionWarnings = %v; want exactly [%q]", res.PostSessionWarnings, terminalEvidenceWarnMessage)
	}
	if len(res.Result.PostSessionWarnings) != 1 || res.Result.PostSessionWarnings[0] != terminalEvidenceWarnMessage {
		t.Fatalf("wire PostSessionWarnings = %v; want exactly [%q]", res.Result.PostSessionWarnings, terminalEvidenceWarnMessage)
	}
	for _, w := range append(append([]string{}, res.PostSessionWarnings...), res.Result.PostSessionWarnings...) {
		if strings.Contains(w, src) || strings.Contains(w, ".cast") {
			t.Fatalf("warning %q carries a path; want value-free", w)
		}
	}
	if archives := findEvidenceArchives(t, r.rescueDir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none on oversize rejection", archives)
	}
}

// TestPreserveTerminalEvidence_SymlinkRetainsWorkarea pins the non-regular
// rejection: a symlink descriptor is refused before any read, the workarea
// is kept, exactly the value-free warning is emitted, and the outside target
// is never archived. Weakening the gate to error-only would follow the link.
func TestPreserveTerminalEvidence_SymlinkRetainsWorkarea(t *testing.T) {
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
	qw := QueuedWork{QueuedWork: queuedWorkBase("EVIDENCE-SYMLINK")}

	if r.preserveTerminalEvidence(qw, res) {
		t.Fatal("preserveTerminalEvidence = true for a symlink evidence path; want false so the workarea is kept")
	}
	if len(res.PostSessionWarnings) != 1 || res.PostSessionWarnings[0] != terminalEvidenceWarnMessage {
		t.Fatalf("PostSessionWarnings = %v; want exactly [%q]", res.PostSessionWarnings, terminalEvidenceWarnMessage)
	}
	if len(res.Result.PostSessionWarnings) != 1 || res.Result.PostSessionWarnings[0] != terminalEvidenceWarnMessage {
		t.Fatalf("wire PostSessionWarnings = %v; want exactly [%q]", res.Result.PostSessionWarnings, terminalEvidenceWarnMessage)
	}
	for _, w := range append(append([]string{}, res.PostSessionWarnings...), res.Result.PostSessionWarnings...) {
		if strings.Contains(w, secretBody) || strings.Contains(w, secret) || strings.Contains(w, link) || strings.Contains(w, ".cast") {
			t.Fatalf("warning %q leaks a path or value; want value-free", w)
		}
	}
	if archives := findEvidenceArchives(t, r.rescueDir); len(archives) != 0 {
		t.Fatalf("evidence archives = %v; want none on symlink rejection", archives)
	}
}
