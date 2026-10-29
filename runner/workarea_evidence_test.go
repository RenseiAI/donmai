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
