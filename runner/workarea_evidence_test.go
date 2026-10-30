package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/shell"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// hangingEvidenceHandle is a live session that blocks until Stop and then
// serves one transcript descriptor. It models a session whose writers stop
// only when Stop returns; descriptor access before then refuses.
type hangingEvidenceHandle struct {
	events  chan agent.Event
	mu      sync.Mutex
	stopped bool
	path    string
}

func (h *hangingEvidenceHandle) SessionID() string          { return "hanging-evidence" }
func (h *hangingEvidenceHandle) Events() <-chan agent.Event { return h.events }
func (h *hangingEvidenceHandle) Inject(context.Context, string) error {
	return nil
}

func (h *hangingEvidenceHandle) Stop(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	return nil
}

func (h *hangingEvidenceHandle) TerminalTranscripts() ([]agent.TerminalTranscriptDescriptor, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopped {
		return nil, errors.New("evidence: session still running")
	}
	return []agent.TerminalTranscriptDescriptor{{Path: h.path}}, nil
}

// hangingEvidenceProvider serves one hanging transcript-capable handle and
// signals gate on Spawn so the test can cancel once provisioning finished.
type hangingEvidenceProvider struct {
	agent.HarnessProvider
	gate chan struct{}
	once *sync.Once
	path string
}

func (p *hangingEvidenceProvider) Spawn(context.Context, agent.Spec) (agent.Handle, error) {
	p.once.Do(func() { close(p.gate) })
	return &hangingEvidenceHandle{events: make(chan agent.Event), path: p.path}, nil
}

// TestStoppedSessionArchivesEvidenceBeforeTeardown pins the stopped side of
// the terminal-state policy end to end: a cancelled interview session whose
// handle carries a transcript descriptor archives that descriptor as local
// evidence before the worktree is torn down.
func TestStoppedSessionArchivesEvidenceBeforeTeardown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const wantPath = "evidence/stopped-archive-4d2e9a.cast"
	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	srv := mockPlatformServer(t)
	t.Cleanup(srv.Close)
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL, WorkerID: "worker-1", AuthToken: "tok",
		HTTPClient: srv.Client(), BaseDelay: 1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	harness, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider is not a HarnessProvider")
	}
	gate := make(chan struct{})
	provider := &hangingEvidenceProvider{HarnessProvider: harness, gate: gate, once: &sync.Once{}, path: wantPath}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := New(Options{
		Registry: reg, WorktreeManager: manager, Poster: poster,
		HTTPClient:   srv.Client(),
		SkipBackstop: true, SkipSteering: true, SkipPostSession: true,
		MaxSessionDuration: -1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rescueDir := t.TempDir()
	r.rescueDir = rescueDir
	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("STOPPED-EVIDENCE"),
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		PlatformURL: srv.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}
	qw.SessionID = "test-session-STOPPED-EVIDENCE"
	qw.Repository = bareRepo
	qw.Mode = "interview"
	runCtx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		res *Result
		err error
	}
	done := make(chan runResult, 1)
	go func() {
		res, runErr := r.Run(runCtx, qw)
		done <- runResult{res: res, err: runErr}
	}()
	select {
	case <-gate:
		cancel()
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("Run did not reach provider spawn before cancellation")
	}
	var got runResult
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if got.res == nil || got.res.Status != "stopped" {
		t.Fatalf("Status = %+v err = %v; want stopped", got.res, got.err)
	}
	// The rescue runs before the teardown: the evidence attempt carries the
	// stopped session's transcript descriptor, and the worktree is gone.
	sessionRoot := filepath.Join(rescueDir, "test-session-STOPPED-EVIDENCE")
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		t.Fatalf("read session rescue root: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("evidence attempts = %d; want exactly 1", len(entries))
	}
	evidencePath := filepath.Join(sessionRoot, entries[0].Name(), terminalEvidenceFileName)
	body, err := os.ReadFile(evidencePath) //nolint:gosec // G304: test-owned path.
	if err != nil {
		t.Fatalf("read terminal evidence: %v", err)
	}
	var manifest terminalEvidenceManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode terminal evidence: %v", err)
	}
	if manifest.Status != "stopped" || len(manifest.Transcripts) != 1 || manifest.Transcripts[0] != wantPath {
		t.Fatalf("evidence manifest = %+v; want stopped with exactly [%q]", manifest, wantPath)
	}
	if got.res.WorktreePath == "" {
		t.Fatal("stopped session has no worktree path")
	}
	if _, err := os.Stat(got.res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the stopped session's worktree torn down", err)
	}
}

// TestCompletedSessionCreatesNoEvidenceAttempt pins the evidence-free side
// of the terminal-state policy: a completed session whose handle carries a
// transcript descriptor creates no rescue attempt for that descriptor.
func TestCompletedSessionCreatesNoEvidenceAttempt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	h.runner.preserveAlways = false
	rescueDir := t.TempDir()
	h.runner.rescueDir = rescueDir
	const sentinel = "evidence/completed-evidence-free-7b1e3a.cast"
	handle := newOrderedEvidenceHandle(sentinel, "WORK_RESULT:passed", nil)
	base := stubBaseForEvidence(t, nil)
	provider := &evidenceProvider{HarnessProvider: base, spawnHandle: handle}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	qw := h.queuedWork("EVIDENCE-COMPLETED-FREE")
	qw.WorkType = "research"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v (failureMode=%q error=%q)", err, res.FailureMode, res.Error)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (failureMode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	if len(res.terminalTranscripts) != 1 || res.terminalTranscripts[0].Path != sentinel {
		t.Fatalf("terminalTranscripts = %+v; want exactly [%q]", res.terminalTranscripts, sentinel)
	}
	// No unpublished work and a completed status: no rescue attempt at all.
	entries, err := os.ReadDir(rescueDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read rescue root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("rescue entries = %v; want none for a completed session", entries)
	}
}

// TestMixedEvidenceAndPatchAttemptsShareRetention pins the shared
// per-session retention boundary: evidence-only and patch-bearing attempts
// count together under rescueKeepPerSession, another session's archives are
// untouched, and non-attempt entries are ignored.
func TestMixedEvidenceAndPatchAttemptsShareRetention(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r.now = func() time.Time { return clock }
	preservePatch := func(session string) {
		t.Helper()
		clock = clock.Add(time.Minute)
		writeFile(t, clone, "work.txt", "uncommitted\n")
		res := &Result{rescueTargets: []rescueTarget{{path: clone, base: base}}}
		res.Status = "failed"
		if !r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase(session)}, res) {
			t.Fatal("preserveUnpublishedWork = false for patch attempt")
		}
	}
	preserveEvidence := func(session, path, status string) {
		t.Helper()
		clock = clock.Add(time.Minute)
		_ = os.Remove(filepath.Join(clone, "work.txt"))
		res := &Result{rescueTargets: []rescueTarget{{path: clone, base: base}}}
		res.Status = status
		res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: path}}
		if !r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase(session)}, res) {
			t.Fatal("preserveUnpublishedWork = false for evidence attempt")
		}
	}
	preservePatch("OTHER")
	preserveEvidence("OTHER-EVIDENCE", "evidence/other-session.cast", "failed")
	// Six mixed attempts for RETAIN: three patch-bearing and three
	// evidence-only, interleaved so the shared boundary is exercised.
	preservePatch("RETAIN")
	preserveEvidence("RETAIN", "evidence/retain-1.cast", "failed")
	preservePatch("RETAIN")
	preserveEvidence("RETAIN", "evidence/retain-2.cast", "stopped")
	preservePatch("RETAIN")

	sessionRoot := filepath.Join(r.rescueDir, "test-session-RETAIN")
	// A non-attempt entry must never be pruned or counted.
	if err := os.WriteFile(filepath.Join(sessionRoot, "notes.txt"), []byte("operator notes\n"), 0o600); err != nil {
		t.Fatalf("write non-attempt file: %v", err)
	}
	// The sixth mixed attempt triggers pruning with the stray file present:
	// six attempts retain the newest five alongside the stray file.
	preserveEvidence("RETAIN", "evidence/retain-3.cast", "failed")

	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	var archives []string
	for _, entry := range entries {
		if entry.IsDir() && rescueArchiveDirRE.MatchString(entry.Name()) {
			archives = append(archives, entry.Name())
		}
	}
	if len(archives) != rescueKeepPerSession {
		t.Fatalf("archives kept = %d (%v); want %d", len(archives), archives, rescueKeepPerSession)
	}
	// Evidence-only and patch-bearing attempts share one boundary: the
	// survivors must include both kinds.
	var sawPatch, sawEvidence bool
	for _, name := range archives {
		dir := filepath.Join(sessionRoot, name)
		if patches := rescuePatches(t, dir); len(patches) > 0 {
			sawPatch = true
		}
		if _, err := os.Stat(filepath.Join(dir, terminalEvidenceFileName)); err == nil {
			sawEvidence = true
		}
	}
	if !sawPatch || !sawEvidence {
		t.Fatalf("survivors patch=%v evidence=%v; want both kinds retained", sawPatch, sawEvidence)
	}
	if _, err := os.Stat(filepath.Join(sessionRoot, "notes.txt")); err != nil {
		t.Fatalf("non-attempt file was touched: %v", err)
	}
	// Other sessions are untouched: OTHER kept its patch, OTHER-EVIDENCE
	// kept its evidence-only attempt.
	if got := len(rescuePatches(t, filepath.Join(r.rescueDir, "test-session-OTHER"))); got != 1 {
		t.Errorf("other session patches = %d; want its 1 untouched", got)
	}
	otherEvidenceRoot := filepath.Join(r.rescueDir, "test-session-OTHER-EVIDENCE")
	otherEntries, err := os.ReadDir(otherEvidenceRoot)
	if err != nil {
		t.Fatalf("read other evidence session root: %v", err)
	}
	if len(otherEntries) != 1 {
		t.Errorf("other evidence session attempts = %d; want its 1 untouched", len(otherEntries))
	} else if _, err := os.Stat(filepath.Join(otherEvidenceRoot, otherEntries[0].Name(), terminalEvidenceFileName)); err != nil {
		t.Errorf("other session evidence file missing: %v", err)
	}
	if got := strings.Join(archives, ","); strings.Contains(got, "notes.txt") {
		t.Errorf("non-attempt entry counted as archive: %v", archives)
	}
}

// TestTerminalEvidence_PreservesPatchMetadataForTranscriptsRepository pins
// the evidence/patch filename boundary: a failed session with unpublished
// work in a repository named "transcripts" and a transcript descriptor
// keeps both the patch recovery metadata (transcripts.json) and the
// terminal evidence sidecar in the same teardown directory. The evidence
// name contains a "+" no sanitized repository label can produce, so the
// evidence write cannot truncate the recovery metadata.
func TestTerminalEvidence_PreservesPatchMetadataForTranscriptsRepository(t *testing.T) {
	clone, _, base := cloneForRescue(t)
	writeFile(t, clone, "work.txt", "uncommitted\n")
	r := minimalRunner(t)
	r.rescueDir = t.TempDir()
	res := &Result{rescueTargets: []rescueTarget{{name: "transcripts", path: clone, base: base}}}
	res.Status = "failed"
	res.terminalTranscripts = []agent.TerminalTranscriptDescriptor{{Path: "evidence/collision-9f3a1c.cast"}}
	if !r.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase("COLLISION")}, res) {
		t.Fatal("preserveUnpublishedWork = false; want the patch and the evidence archived")
	}
	sessionRoot := filepath.Join(r.rescueDir, "test-session-COLLISION")
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		t.Fatalf("read session rescue root: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("archive attempts = %d; want exactly 1", len(entries))
	}
	dir := filepath.Join(sessionRoot, entries[0].Name())
	patch := onlyRescuePatch(t, dir)
	if body := readFile(t, patch); !strings.Contains(body, "work.txt") {
		t.Errorf("rescue patch does not carry the uncommitted work.txt")
	}
	var meta rescueMetadata
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, "transcripts.json"))), &meta); err != nil {
		t.Fatalf("decode patch recovery metadata: %v", err)
	}
	if meta.Repository != "transcripts" || meta.Base != base || len(meta.ChangedPaths) == 0 {
		t.Fatalf("recovery metadata = %+v; want repository transcripts with base %s and changed paths", meta, base)
	}
	var manifest terminalEvidenceManifest
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, terminalEvidenceFileName))), &manifest); err != nil {
		t.Fatalf("decode terminal evidence: %v", err)
	}
	if manifest.Status != "failed" || len(manifest.Transcripts) != 1 || manifest.Transcripts[0] != "evidence/collision-9f3a1c.cast" {
		t.Fatalf("evidence manifest = %+v; want failed with exactly [evidence/collision-9f3a1c.cast]", manifest)
	}
}

// stoppedMultiEvidenceHandle is a live interview session that blocks until
// Stop and then serves a fixed descriptor list. Descriptor access before
// Stop refuses, modelling writers that stop only when Stop returns.
type stoppedMultiEvidenceHandle struct {
	events  chan agent.Event
	mu      sync.Mutex
	stopped bool
	descs   []agent.TerminalTranscriptDescriptor
}

func (h *stoppedMultiEvidenceHandle) SessionID() string          { return "stopped-multi-evidence" }
func (h *stoppedMultiEvidenceHandle) Events() <-chan agent.Event { return h.events }
func (h *stoppedMultiEvidenceHandle) Inject(context.Context, string) error {
	return nil
}

func (h *stoppedMultiEvidenceHandle) Stop(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	return nil
}

func (h *stoppedMultiEvidenceHandle) TerminalTranscripts() ([]agent.TerminalTranscriptDescriptor, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopped {
		return nil, errors.New("evidence: session still running")
	}
	return h.descs, nil
}

// stoppedMultiEvidenceProvider serves one stoppedMultiEvidenceHandle and
// signals gate on Spawn so the test can cancel once provisioning finished.
type stoppedMultiEvidenceProvider struct {
	agent.HarnessProvider
	gate  chan struct{}
	once  *sync.Once
	descs []agent.TerminalTranscriptDescriptor
}

func (p *stoppedMultiEvidenceProvider) Spawn(context.Context, agent.Spec) (agent.Handle, error) {
	p.once.Do(func() { close(p.gate) })
	return &stoppedMultiEvidenceHandle{events: make(chan agent.Event), descs: p.descs}, nil
}

// runStoppedEvidenceSession runs one cancelled interview session whose
// handle serves descs after Stop, and returns the terminal result. repository
// selects the provisioned workarea: empty provisions a repository-free
// session with no rescue targets.
func runStoppedEvidenceSession(t *testing.T, r *Runner, platformURL, repository string, descs []agent.TerminalTranscriptDescriptor, qwSessionID string) *Result {
	t.Helper()
	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	harness, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider is not a HarnessProvider")
	}
	gate := make(chan struct{})
	provider := &stoppedMultiEvidenceProvider{HarnessProvider: harness, gate: gate, once: &sync.Once{}, descs: descs}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.registry = reg
	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("STOPPED-EVIDENCE"),
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		PlatformURL: platformURL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}
	qw.SessionID = qwSessionID
	qw.Repository = repository
	qw.Mode = "interview"
	runCtx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		res *Result
		err error
	}
	done := make(chan runResult, 1)
	go func() {
		res, runErr := r.Run(runCtx, qw)
		done <- runResult{res: res, err: runErr}
	}()
	select {
	case <-gate:
		cancel()
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("Run did not reach provider spawn before cancellation")
	}
	var got runResult
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if got.res == nil || got.res.Status != "stopped" {
		t.Fatalf("Status = %+v err = %v; want stopped", got.res, got.err)
	}
	return got.res
}

func newEvidenceRunner(t *testing.T, wtParent, rescueDir string) (*Runner, string) {
	t.Helper()
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
	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	reg := NewRegistry()
	if err := reg.Register(base); err != nil {
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
	r.rescueDir = rescueDir
	return r, srv.URL
}

// TestRepositoryFreeStoppedEvidenceViaRun pins the repository-free
// boundary end to end: a stopped session with no checkout targets still
// archives its transcript descriptor. Restoring the old early return for
// sessions without checkout targets drops it and this test fails with zero
// evidence attempts.
func TestRepositoryFreeStoppedEvidenceViaRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const wantPath = "evidence/repo-free-8c1d2a.cast"
	rescueDir := t.TempDir()
	r, platformURL := newEvidenceRunner(t, t.TempDir(), rescueDir)
	res := runStoppedEvidenceSession(t, r, platformURL, "", []agent.TerminalTranscriptDescriptor{{Path: wantPath}}, "test-session-REPO-FREE-EVIDENCE")
	if res.WorktreePath == "" {
		t.Fatal("stopped session has no worktree path")
	}
	sessionRoot := filepath.Join(rescueDir, "test-session-REPO-FREE-EVIDENCE")
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		t.Fatalf("read session rescue root: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("evidence attempts = %d; want exactly 1 for a repository-free stopped session", len(entries))
	}
	body, err := os.ReadFile(filepath.Join(sessionRoot, entries[0].Name(), terminalEvidenceFileName)) //nolint:gosec // G304: test-owned path.
	if err != nil {
		t.Fatalf("read terminal evidence: %v", err)
	}
	var manifest terminalEvidenceManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode terminal evidence: %v", err)
	}
	if manifest.Status != "stopped" || len(manifest.Transcripts) != 1 || manifest.Transcripts[0] != wantPath {
		t.Fatalf("evidence manifest = %+v; want stopped with exactly [%q]", manifest, wantPath)
	}
}

// TestStoppedEvidenceDescriptorFilteringViaRun pins the descriptor filter
// end to end: empty descriptor paths are dropped while a valid
// one-character path is kept. Admitting empty paths leaves "" in the
// manifest; rejecting one-character paths drops "x"; both fail this test.
func TestStoppedEvidenceDescriptorFilteringViaRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const validPath = "evidence/filtered-valid-6b3e1f.cast"
	rescueDir := t.TempDir()
	r, platformURL := newEvidenceRunner(t, t.TempDir(), rescueDir)
	descs := []agent.TerminalTranscriptDescriptor{{Path: ""}, {Path: "x"}, {Path: validPath}}
	runStoppedEvidenceSession(t, r, platformURL, makeBareRepo(t), descs, "test-session-FILTER-EVIDENCE")
	sessionRoot := filepath.Join(rescueDir, "test-session-FILTER-EVIDENCE")
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		t.Fatalf("read session rescue root: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("evidence attempts = %d; want exactly 1", len(entries))
	}
	body, err := os.ReadFile(filepath.Join(sessionRoot, entries[0].Name(), terminalEvidenceFileName)) //nolint:gosec // G304: test-owned path.
	if err != nil {
		t.Fatalf("read terminal evidence: %v", err)
	}
	var manifest terminalEvidenceManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode terminal evidence: %v", err)
	}
	if len(manifest.Transcripts) != 2 || manifest.Transcripts[0] != "x" || manifest.Transcripts[1] != validPath {
		t.Fatalf("evidence manifest transcripts = %q; want [x %q] with empty paths dropped", manifest.Transcripts, validPath)
	}
}

// TestStoppedEmptyEvidenceWritesNoAttemptViaRun pins the empty-evidence
// boundary end to end: descriptors that are all empty write no archive
// directory at all. Writing an empty evidence attempt creates one and this
// test fails.
func TestStoppedEmptyEvidenceWritesNoAttemptViaRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	rescueDir := t.TempDir()
	r, platformURL := newEvidenceRunner(t, t.TempDir(), rescueDir)
	runStoppedEvidenceSession(t, r, platformURL, makeBareRepo(t), []agent.TerminalTranscriptDescriptor{{Path: ""}, {Path: ""}}, "test-session-EMPTY-EVIDENCE")
	sessionRoot := filepath.Join(rescueDir, "test-session-EMPTY-EVIDENCE")
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("read session rescue root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("evidence attempts = %d; want none when every descriptor path is empty", len(entries))
	}
}

// TestStoppedEvidenceWriteFailureStillTearsDownViaRun pins the
// best-effort boundary end to end: when the evidence cannot be written the
// teardown still proceeds. Making evidence failure prevent teardown keeps
// the workarea and this test fails on the surviving worktree.
func TestStoppedEvidenceWriteFailureStillTearsDownViaRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("a file where the rescue directory should go\n"), 0o600); err != nil {
		t.Fatalf("write rescue blocker: %v", err)
	}
	r, platformURL := newEvidenceRunner(t, t.TempDir(), blocked)
	res := runStoppedEvidenceSession(t, r, platformURL, makeBareRepo(t), []agent.TerminalTranscriptDescriptor{{Path: "evidence/best-effort-4f2a9c.cast"}}, "test-session-EVIDENCE-BEST-EFFORT")
	if res.WorktreePath == "" {
		t.Fatal("stopped session has no worktree path")
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the stopped session's worktree torn down despite evidence failure", err)
	}
	// The helper contract matches: evidence failure never flips the
	// preservation result, so the caller still tears down.
	helper := minimalRunner(t)
	helper.rescueDir = blocked
	helperRes := &Result{terminalTranscripts: []agent.TerminalTranscriptDescriptor{{Path: "evidence/best-effort-4f2a9c.cast"}}}
	helperRes.Status = "stopped"
	if !helper.preserveUnpublishedWork(QueuedWork{QueuedWork: queuedWorkBase("BEST-EFFORT")}, helperRes) {
		t.Fatal("preserveUnpublishedWork = false on evidence-write failure; want true (best-effort)")
	}
}

// TestLateLeaseFailurePreservesEvidenceViaRun pins the late-lease
// boundary end to end: a research session that completes with a captured
// descriptor but whose requested terminal lease fails acquisition flips to
// failed and still archives its evidence manifest before teardown.
// Sampling evidence before the final status is what loses it: the
// pre-lease decision sees completed and archives nothing.
func TestLateLeaseFailurePreservesEvidenceViaRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const sentinel = "evidence/late-lease-failure-3e7a1c.cast"
	wtParent := t.TempDir()
	srv := mockPlatformServer(t)
	t.Cleanup(srv.Close)
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	// Break lease acquisition after provisioning: the session record stays
	// valid so the clone succeeds, but every lease write fails and the
	// completed session flips to failed in the lease block.
	leaseRecords := filepath.Join(wtParent, ".terminal-leases", "records")
	if err := os.RemoveAll(leaseRecords); err != nil {
		t.Fatalf("remove lease records dir: %v", err)
	}
	if err := os.WriteFile(leaseRecords, []byte("blocked\n"), 0o600); err != nil {
		t.Fatalf("block lease records dir: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL, WorkerID: "worker-1", AuthToken: "tok",
		HTTPClient: srv.Client(), BaseDelay: 1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	handle := newOrderedEvidenceHandle(sentinel, "WORK_RESULT:passed", nil)
	baseStub, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	harness, ok := baseStub.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider is not a HarnessProvider")
	}
	provider := &evidenceProvider{HarnessProvider: harness, spawnHandle: handle}
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
		QueuedWork:  queuedWorkBase("LATE-LEASE-FAILURE"),
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		PlatformURL: srv.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
		},
		TerminalWorkareaLease: terminalLeaseRequest(),
	}
	qw.SessionID = "test-session-LATE-LEASE-FAILURE"
	qw.Repository = makeBareRepo(t)
	qw.WorkType = "research"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx, qw)
	if err == nil {
		t.Fatalf("Run err = nil; want the lease-acquisition failure")
	}
	if res == nil || res.Status != "failed" || res.FailureMode != FailureTerminalWorkareaLease {
		t.Fatalf("Status = %+v err = %v; want failed with %q", res, err, FailureTerminalWorkareaLease)
	}
	if len(res.terminalTranscripts) != 1 || res.terminalTranscripts[0].Path != sentinel {
		t.Fatalf("terminalTranscripts = %+v; want exactly [%q]", res.terminalTranscripts, sentinel)
	}
	sessionRoot := filepath.Join(rescueDir, "test-session-LATE-LEASE-FAILURE")
	entries, readErr := os.ReadDir(sessionRoot)
	if readErr != nil {
		t.Fatalf("read session rescue root: %v", readErr)
	}
	if len(entries) != 1 {
		t.Fatalf("evidence attempts = %d; want exactly 1 after the late lease failure", len(entries))
	}
	body, readErr := os.ReadFile(filepath.Join(sessionRoot, entries[0].Name(), terminalEvidenceFileName)) //nolint:gosec // G304: test-owned path.
	if readErr != nil {
		t.Fatalf("read terminal evidence: %v", readErr)
	}
	var manifest terminalEvidenceManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode terminal evidence: %v", err)
	}
	if manifest.Status != "failed" || len(manifest.Transcripts) != 1 || manifest.Transcripts[0] != sentinel {
		t.Fatalf("evidence manifest = %+v; want failed with exactly [%q]", manifest, sentinel)
	}
	if res.WorktreePath == "" {
		t.Fatal("failed session has no worktree path")
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the failed session's worktree torn down", err)
	}
}

// retentionRunProvider serves one cancelled interview session per Spawn while
// optionally planting unpublished work into the provisioned workarea. It
// embeds the stub harness so registry identity, capabilities and manifest
// stay production-faithful; only Spawn is overridden.
type retentionRunProvider struct {
	agent.HarnessProvider
	gate  chan struct{}
	once  *sync.Once
	descs []agent.TerminalTranscriptDescriptor
	plant bool
}

func (p *retentionRunProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	if p.plant && spec.Cwd != "" {
		_ = os.MkdirAll(spec.Cwd, 0o750)
		_ = os.WriteFile(filepath.Join(spec.Cwd, "work.txt"), []byte("uncommitted\n"), 0o600) //nolint:gosec // G306: test-owned worktree.
	}
	p.once.Do(func() { close(p.gate) })
	return &stoppedMultiEvidenceHandle{events: make(chan agent.Event), descs: p.descs}, nil
}

// runRetentionStoppedSession runs one cancelled interview session to the
// stopped terminal state through the production Runner.Run entry point.
// plant selects a patch-bearing attempt (unpublished work planted during
// Spawn); descs selects the terminal evidence descriptors for the attempt.
func runRetentionStoppedSession(t *testing.T, r *Runner, platformURL, repository, sessionID string, descs []agent.TerminalTranscriptDescriptor, plant bool) {
	t.Helper()
	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	harness, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider is not a HarnessProvider")
	}
	gate := make(chan struct{})
	provider := &retentionRunProvider{HarnessProvider: harness, gate: gate, once: &sync.Once{}, descs: descs, plant: plant}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.registry = reg
	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("RETAIN-VIA-RUN"),
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		PlatformURL: platformURL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}
	qw.SessionID = sessionID
	qw.Repository = repository
	qw.Mode = "interview"
	runCtx, cancel := context.WithCancel(context.Background())
	type runResult struct {
		res *Result
		err error
	}
	done := make(chan runResult, 1)
	go func() {
		res, runErr := r.Run(runCtx, qw)
		done <- runResult{res: res, err: runErr}
	}()
	select {
	case <-gate:
		cancel()
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("Run did not reach provider spawn before cancellation")
	}
	var got runResult
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if got.res == nil || got.res.Status != "stopped" {
		t.Fatalf("Status = %+v err = %v; want stopped", got.res, got.err)
	}
}

// TestMixedEvidenceAndPatchAttemptsShareRetentionViaRun pins the shared
// retention boundary through the production Runner.Run entry point: six
// mixed patch-bearing and evidence-only stopped runs for one session retain
// the newest five, preserve another session, and ignore non-attempt entries.
// Deleting the production finalization call leaves six attempts and fails
// this test; widening the prune root or counting only patches breaks the
// count or isolation assertions.
func TestMixedEvidenceAndPatchAttemptsShareRetentionViaRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
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
	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	reg := NewRegistry()
	if err := reg.Register(base); err != nil {
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
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r.now = func() time.Time { return clock }
	const retainSession = "test-session-RETAIN-VIA-RUN"
	const otherSession = "test-session-OTHER-VIA-RUN"
	const otherEvidenceSession = "test-session-OTHER-EVIDENCE-VIA-RUN"
	// Another session's archives must survive the retention churn below.
	clock = clock.Add(time.Minute)
	runRetentionStoppedSession(t, r, srv.URL, bareRepo, otherSession, nil, true)
	clock = clock.Add(time.Minute)
	runRetentionStoppedSession(t, r, srv.URL, bareRepo, otherEvidenceSession, []agent.TerminalTranscriptDescriptor{{Path: "evidence/other-session.cast"}}, false)
	// Five mixed attempts for RETAIN: patch-only and evidence-only
	// interleaved so the shared boundary is exercised.
	clock = clock.Add(time.Minute)
	runRetentionStoppedSession(t, r, srv.URL, bareRepo, retainSession, nil, true)
	clock = clock.Add(time.Minute)
	runRetentionStoppedSession(t, r, srv.URL, bareRepo, retainSession, []agent.TerminalTranscriptDescriptor{{Path: "evidence/retain-1.cast"}}, false)
	clock = clock.Add(time.Minute)
	runRetentionStoppedSession(t, r, srv.URL, bareRepo, retainSession, nil, true)
	clock = clock.Add(time.Minute)
	runRetentionStoppedSession(t, r, srv.URL, bareRepo, retainSession, []agent.TerminalTranscriptDescriptor{{Path: "evidence/retain-2.cast"}}, false)
	clock = clock.Add(time.Minute)
	runRetentionStoppedSession(t, r, srv.URL, bareRepo, retainSession, nil, true)
	sessionRoot := filepath.Join(rescueDir, retainSession)
	if err := os.WriteFile(filepath.Join(sessionRoot, "notes.txt"), []byte("operator notes\n"), 0o600); err != nil {
		t.Fatalf("write non-attempt file: %v", err)
	}
	// The sixth mixed attempt triggers pruning with the stray file present.
	clock = clock.Add(time.Minute)
	runRetentionStoppedSession(t, r, srv.URL, bareRepo, retainSession, []agent.TerminalTranscriptDescriptor{{Path: "evidence/retain-3.cast"}}, false)
	entries, err := os.ReadDir(sessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	var archives []string
	for _, entry := range entries {
		if entry.IsDir() && rescueArchiveDirRE.MatchString(entry.Name()) {
			archives = append(archives, entry.Name())
		}
	}
	if len(archives) != rescueKeepPerSession {
		t.Fatalf("production Run archives kept = %d (%v); want %d", len(archives), archives, rescueKeepPerSession)
	}
	var sawPatch, sawEvidence bool
	for _, name := range archives {
		dir := filepath.Join(sessionRoot, name)
		if patches := rescuePatches(t, dir); len(patches) > 0 {
			sawPatch = true
		}
		if _, err := os.Stat(filepath.Join(dir, terminalEvidenceFileName)); err == nil {
			sawEvidence = true
		}
	}
	if !sawPatch || !sawEvidence {
		t.Fatalf("production Run survivors patch=%v evidence=%v; want both kinds retained", sawPatch, sawEvidence)
	}
	if _, err := os.Stat(filepath.Join(sessionRoot, "notes.txt")); err != nil {
		t.Fatalf("non-attempt file was touched: %v", err)
	}
	if got := len(rescuePatches(t, filepath.Join(rescueDir, otherSession))); got != 1 {
		t.Errorf("other session patches = %d; want its 1 untouched", got)
	}
	otherEvidenceRoot := filepath.Join(rescueDir, otherEvidenceSession)
	otherEntries, err := os.ReadDir(otherEvidenceRoot)
	if err != nil {
		t.Fatalf("read other evidence session root: %v", err)
	}
	if len(otherEntries) != 1 {
		t.Errorf("other evidence session attempts = %d; want its 1 untouched", len(otherEntries))
	} else if _, err := os.Stat(filepath.Join(otherEvidenceRoot, otherEntries[0].Name(), terminalEvidenceFileName)); err != nil {
		t.Errorf("other session evidence file missing: %v", err)
	}
}

// interactiveEvidenceHandle is an exited interactive session that also
// serves terminal transcript descriptors after Stop.
type interactiveEvidenceHandle struct {
	*testInteractiveHandle
	descs []agent.TerminalTranscriptDescriptor
}

func (h *interactiveEvidenceHandle) TerminalTranscripts() ([]agent.TerminalTranscriptDescriptor, error) {
	out := make([]agent.TerminalTranscriptDescriptor, len(h.descs))
	copy(out, h.descs)
	return out, nil
}

// interactiveEvidenceProvider serves one already-exited interactive session
// carrying the configured descriptors. The exited session maps to a
// completed result before the terminal lease decision runs.
type interactiveEvidenceProvider struct {
	manifest agent.HarnessManifest
	descs    []agent.TerminalTranscriptDescriptor
}

func (p *interactiveEvidenceProvider) Name() agent.ProviderName { return agent.ProviderShell }
func (p *interactiveEvidenceProvider) Capabilities() agent.Capabilities {
	return (&shell.Provider{}).Capabilities()
}
func (p *interactiveEvidenceProvider) Manifest() agent.HarnessManifest { return p.manifest }
func (*interactiveEvidenceProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}
func (*interactiveEvidenceProvider) Shutdown(context.Context) error { return nil }
func (p *interactiveEvidenceProvider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	sess := completedRecordingInteractiveSession()
	base := &testInteractiveHandle{Handle: &fakeHandle{events: make(chan agent.Event)}, session: sess}
	return &interactiveEvidenceHandle{testInteractiveHandle: base, descs: p.descs}, nil
}

// TestInteractiveLateLeaseFailurePreservesEvidenceViaRun pins the
// interactive fallback preservation scope end to end: an interactive session
// that exits cleanly (completed) but whose requested terminal lease fails
// acquisition flips to failed and still archives its evidence manifest
// through the production Run path. Inverting the fallback scope guard
// (pres == nil to pres != nil) drops the manifest and fails this test; the
// existing research late-lease test cannot catch it because that path
// creates its scope before the lease decision.
func TestInteractiveLateLeaseFailurePreservesEvidenceViaRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const sentinel = "evidence/interactive-late-lease-9d4e2b.cast"
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")
	wtParent := t.TempDir()
	srv := mockPlatformServer(t)
	t.Cleanup(srv.Close)
	manager, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	leaseRecords := filepath.Join(wtParent, ".terminal-leases", "records")
	if err := os.RemoveAll(leaseRecords); err != nil {
		t.Fatalf("remove lease records dir: %v", err)
	}
	if err := os.WriteFile(leaseRecords, []byte("blocked\n"), 0o600); err != nil {
		t.Fatalf("block lease records dir: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL, WorkerID: "worker-1", AuthToken: "tok",
		HTTPClient: srv.Client(), BaseDelay: 1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	provider := &interactiveEvidenceProvider{manifest: (&shell.Provider{}).Manifest(), descs: []agent.TerminalTranscriptDescriptor{{Path: sentinel}}}
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
		QueuedWork:  queuedWorkBase("INTERACTIVE-LATE-LEASE"),
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		PlatformURL: srv.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderShell,
		},
		TerminalWorkareaLease: terminalLeaseRequest(),
	}
	qw.SessionID = "test-session-INTERACTIVE-LATE-LEASE"
	qw.Repository = makeBareRepo(t)
	qw.Mode = prompt.InteractiveRunMode
	qw.WorkType = "development"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx, qw)
	if err == nil {
		t.Fatalf("Run err = nil; want the lease-acquisition failure")
	}
	if res == nil || res.Status != "failed" || res.FailureMode != FailureTerminalWorkareaLease {
		t.Fatalf("Status = %+v err = %v; want failed with %q", res, err, FailureTerminalWorkareaLease)
	}
	if len(res.terminalTranscripts) != 1 || res.terminalTranscripts[0].Path != sentinel {
		t.Fatalf("terminalTranscripts = %+v; want exactly [%q]", res.terminalTranscripts, sentinel)
	}
	sessionRoot := filepath.Join(rescueDir, "test-session-INTERACTIVE-LATE-LEASE")
	entries, readErr := os.ReadDir(sessionRoot)
	if readErr != nil {
		t.Fatalf("read session rescue root: %v", readErr)
	}
	if len(entries) != 1 {
		t.Fatalf("evidence attempts = %d; want exactly 1 after the interactive late lease failure", len(entries))
	}
	body, readErr := os.ReadFile(filepath.Join(sessionRoot, entries[0].Name(), terminalEvidenceFileName)) //nolint:gosec // G304: test-owned path.
	if readErr != nil {
		t.Fatalf("read terminal evidence: %v", readErr)
	}
	var manifest terminalEvidenceManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode terminal evidence: %v", err)
	}
	if manifest.Status != "failed" || len(manifest.Transcripts) != 1 || manifest.Transcripts[0] != sentinel {
		t.Fatalf("evidence manifest = %+v; want failed with exactly [%q]", manifest, sentinel)
	}
	if res.WorktreePath == "" {
		t.Fatal("failed session has no worktree path")
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the failed session's worktree torn down", err)
	}
}
