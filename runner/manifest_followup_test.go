package runner

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/heartbeat"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// verdictScriptTurn is one agent turn played by verdictScriptProvider. The turn first
// writes (or rewrites) the turn-result manifest when manifest is set, then
// emits text as an assistant message, then ends with a clean terminal
// ResultEvent — or, when crash is set, with a provider ErrorEvent and a closed
// stream.
type verdictScriptTurn struct {
	manifest string
	text     string
	crash    bool
	// laterMtime stamps the written manifest one minute ahead, so a rewrite
	// with identical bytes still moves the mtime on filesystems whose
	// timestamp tick is coarser than the gap between two turns.
	laterMtime bool
}

// verdictScriptProvider wraps the stub harness (for its manifest + capabilities) and
// replaces its event script with a list of turns: turn 0 runs at Spawn, and
// every Inject — steering or a memory inject — runs the next one. It plays the
// agent's side of the turn-result contract with real files in the session
// worktree, so the runner's own resolution code is what the tests observe.
type verdictScriptProvider struct {
	agent.HarnessProvider
	t     *testing.T
	turns []verdictScriptTurn
}

func (p *verdictScriptProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	h := &verdictScriptHandle{t: p.t, cwd: spec.Cwd, turns: p.turns, events: make(chan agent.Event, 64)}
	h.events <- agent.InitEvent{SessionID: "scripted-session"}
	h.play()
	return h, nil
}

func (p *verdictScriptProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}

type verdictScriptHandle struct {
	t      *testing.T
	cwd    string
	turns  []verdictScriptTurn
	next   int
	mu     sync.Mutex
	closed bool
	events chan agent.Event
}

func (h *verdictScriptHandle) SessionID() string          { return "scripted-session" }
func (h *verdictScriptHandle) Events() <-chan agent.Event { return h.events }
func (h *verdictScriptHandle) Stop(context.Context) error { h.closeEvents(); return nil }
func (h *verdictScriptHandle) Inject(context.Context, string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.next >= len(h.turns) {
		return errors.New("scripted: no turn left to play")
	}
	h.playLocked()
	return nil
}

func (h *verdictScriptHandle) play() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.playLocked()
}

func (h *verdictScriptHandle) playLocked() {
	turn := h.turns[h.next]
	h.next++
	if turn.manifest != "" {
		dir := filepath.Join(h.cwd, state.AgentDirName)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			h.t.Errorf("mkdir %s: %v", dir, err)
		}
		path := filepath.Join(dir, ManifestFileName)
		if err := os.WriteFile(path, []byte(turn.manifest), 0o600); err != nil {
			h.t.Errorf("write turn-result manifest: %v", err)
		}
		if turn.laterMtime {
			later := time.Now().Add(time.Minute)
			if err := os.Chtimes(path, later, later); err != nil {
				h.t.Errorf("chtimes turn-result manifest: %v", err)
			}
		}
	}
	if turn.text != "" {
		h.events <- agent.AssistantTextEvent{Text: turn.text}
	}
	if turn.crash {
		h.events <- agent.ErrorEvent{Message: "provider crashed mid turn"}
		h.closed = true
		close(h.events)
		return
	}
	h.events <- agent.ResultEvent{Success: true, Message: turn.text}
}

func (h *verdictScriptHandle) closeEvents() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.events)
	}
}

const (
	followUpPR      = "https://github.com/example/repo/pull/7"
	manifestSummary = "round-two items not implemented; round-one work pushed"
	passedManifest  = `{"schemaVersion":1,"verdict":"passed","summary":"all done"}`
	failedManifest  = `{"schemaVersion":1,"verdict":"failed","summary":"` + manifestSummary + `"}`
)

// runScripted runs one development session against the scripted turns and
// returns the terminal envelope. inject, when non-empty, arms a runtime memory
// inject the heartbeat delivers so the runner drains it as a follow-up turn.
func runScripted(t *testing.T, workType string, skipSteering bool, inject string, turns ...verdictScriptTurn) *Result {
	t.Helper()
	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	harness, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider is not a HarnessProvider")
	}
	platform := newRecordingPlatformServer(t)
	if inject != "" {
		platform.queueInject(heartbeat.InjectPayload{DeliveryID: "dlv-followup-1", Text: inject})
	}
	r := newFollowUpRunner(t, platform.URL, platform.Client(), &verdictScriptProvider{HarnessProvider: harness, t: t, turns: turns})
	r.skipSteering = skipSteering
	qw := QueuedWork{
		QueuedWork:      queuedWorkBase("MANIFEST-FOLLOWUP"),
		WorkerID:        "worker-1",
		AuthToken:       "tok",
		PlatformURL:     platform.URL,
		ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
	}
	qw.WorkType = workType
	qw.Repository = makeBareRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx, qw)
	if err != nil && res == nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

type verdictWant struct {
	status       string
	failureMode  string
	workResult   string
	summary      string
	pr           string
	manifest     string // expected res.Manifest.Verdict; "" = no manifest on the envelope
	wantSteering bool
}

func assertVerdict(t *testing.T, res *Result, want verdictWant) {
	t.Helper()
	if res.SteeringTriggered != want.wantSteering {
		t.Fatalf("SteeringTriggered = %v; want %v", res.SteeringTriggered, want.wantSteering)
	}
	if res.Status != want.status {
		t.Errorf("Status = %q; want %q (FailureMode=%q, Error=%q)", res.Status, want.status, res.FailureMode, res.Error)
	}
	if res.FailureMode != want.failureMode {
		t.Errorf("FailureMode = %q; want %q", res.FailureMode, want.failureMode)
	}
	if res.WorkResult != want.workResult {
		t.Errorf("WorkResult = %q; want %q", res.WorkResult, want.workResult)
	}
	if want.summary != "" && res.Summary != want.summary {
		t.Errorf("Summary = %q; want %q", res.Summary, want.summary)
	}
	if want.pr != "" && res.PullRequestURL != want.pr {
		t.Errorf("PullRequestURL = %q; want %q", res.PullRequestURL, want.pr)
	}
	got := ""
	if res.Manifest != nil {
		got = res.Manifest.Verdict
	}
	if got != want.manifest {
		t.Errorf("Manifest verdict on the envelope = %q; want %q", got, want.manifest)
	}
}

// TestRun_TurnVerdictAcrossSteeringFollowUp pins how the turn verdict is
// resolved when tail steering runs a follow-up turn. The first turn always
// ends cleanly without a PR, so steering fires. The newest signal must win:
// a manifest the follow-up wrote, else the follow-up's own WORK_RESULT verdict
// over a stale earlier manifest, else the earlier manifest over the
// follow-up's bare terminal text. A runner-recorded failure is never cleared
// or relabelled by an agent manifest.
func TestRun_TurnVerdictAcrossSteeringFollowUp(t *testing.T) {
	cases := []struct {
		name  string
		turns []verdictScriptTurn
		want  verdictWant
	}{
		{
			name: "manifest written during the follow-up is read",
			turns: []verdictScriptTurn{
				{text: "Work in progress."},
				{manifest: `{"schemaVersion":1,"verdict":"failed","summary":"` + manifestSummary + `","pullRequestUrl":"` + followUpPR + `"}`, text: followUpPR},
			},
			want: verdictWant{status: "completed", workResult: "failed", summary: manifestSummary, pr: followUpPR, manifest: "failed", wantSteering: true},
		},
		{
			name: "earlier manifest outranks the follow-up's bare terminal text",
			turns: []verdictScriptTurn{
				{manifest: failedManifest, text: "Stopping here."},
				{text: followUpPR},
			},
			want: verdictWant{status: "completed", workResult: "failed", summary: manifestSummary, pr: followUpPR, manifest: "failed", wantSteering: true},
		},
		{
			name: "stale earlier passed manifest never upgrades the follow-up's failed verdict",
			turns: []verdictScriptTurn{
				{manifest: passedManifest, text: "All done."},
				{text: "Tests fail after rebase; could not open the PR.\nWORK_RESULT:failed"},
			},
			want: verdictWant{status: "completed", workResult: "failed", summary: "Tests fail after rebase; could not open the PR.\nWORK_RESULT:failed", wantSteering: true},
		},
		{
			name: "stale earlier failed manifest is never upgraded by a follow-up passed marker",
			turns: []verdictScriptTurn{
				{manifest: failedManifest, text: "Stopping here."},
				{text: "Fixed and opened " + followUpPR + "\nWORK_RESULT:passed"},
			},
			want: verdictWant{status: "completed", workResult: "failed", summary: manifestSummary, pr: followUpPR, manifest: "failed", wantSteering: true},
		},
		{
			name: "follow-up unknown marker never drops a stale failed manifest",
			turns: []verdictScriptTurn{
				{manifest: failedManifest, text: "Stopping here."},
				{text: followUpPR + "\nWORK_RESULT:unknown"},
			},
			want: verdictWant{status: "completed", workResult: "failed", summary: manifestSummary, pr: followUpPR, manifest: "failed", wantSteering: true},
		},
		{
			name: "passed marker quoted in prose never upgrades a stale failed manifest",
			turns: []verdictScriptTurn{
				{manifest: failedManifest, text: "Stopping here."},
				{text: "I am not claiming WORK_RESULT: passed - round two is still missing. " + followUpPR},
			},
			want: verdictWant{status: "completed", workResult: "failed", summary: manifestSummary, pr: followUpPR, manifest: "failed", wantSteering: true},
		},
		{
			name: "mid-line failed marker does not override a stale passed manifest",
			turns: []verdictScriptTurn{
				{manifest: passedManifest, text: "All done."},
				{text: "Pushed the branch but could not open the PR. WORK_RESULT:failed"},
			},
			want: verdictWant{status: "completed", workResult: "passed", summary: "all done", manifest: "passed", wantSteering: true},
		},
		{
			name: "line-anchored blocked marker downgrades a stale passed manifest",
			turns: []verdictScriptTurn{
				{manifest: passedManifest, text: "All done."},
				{text: "The rebase needs a product decision.\nWORK_RESULT: blocked"},
			},
			want: verdictWant{status: "failed", failureMode: FailureAgentBlocked, wantSteering: true},
		},
		{
			name: "identical-content rewrite during the follow-up counts as the follow-up's manifest",
			turns: []verdictScriptTurn{
				{manifest: passedManifest, text: "All done."},
				{manifest: passedManifest, laterMtime: true, text: "Re-confirmed.\nWORK_RESULT:failed"},
			},
			want: verdictWant{status: "completed", workResult: "passed", summary: "all done", manifest: "passed", wantSteering: true},
		},
		{
			name: "rewritten manifest beats the follow-up's own marker",
			turns: []verdictScriptTurn{
				{manifest: passedManifest, text: "All done."},
				{manifest: failedManifest, text: "WORK_RESULT:passed"},
			},
			want: verdictWant{status: "completed", workResult: "failed", summary: manifestSummary, manifest: "failed", wantSteering: true},
		},
		{
			name: "inline manifest in the follow-up's final message is read",
			turns: []verdictScriptTurn{
				{text: "Work in progress."},
				{text: `Intended manifest: {"schemaVersion":1,"verdict":"failed","summary":"inline says failed"}`},
			},
			want: verdictWant{status: "completed", workResult: "failed", summary: "inline says failed", manifest: "failed", wantSteering: true},
		},
		{
			name: "blocked manifest written during the follow-up takes the blocked fork",
			turns: []verdictScriptTurn{
				{text: "Work in progress."},
				{manifest: `{"schemaVersion":1,"verdict":"blocked","blockedReason":"need a product decision"}`, text: "Need a decision."},
			},
			want: verdictWant{status: "failed", failureMode: FailureAgentBlocked, manifest: "blocked", wantSteering: true},
		},
		{
			name: "blocked manifest never relabels a follow-up provider crash",
			turns: []verdictScriptTurn{
				{text: "Work in progress."},
				{manifest: `{"schemaVersion":1,"verdict":"blocked","blockedReason":"need a decision"}`, crash: true},
			},
			want: verdictWant{status: "failed", failureMode: FailureProviderError, manifest: "blocked", wantSteering: true},
		},
		{
			name: "passed manifest never clears a follow-up provider crash",
			turns: []verdictScriptTurn{
				{text: "Work in progress."},
				{manifest: `{"schemaVersion":1,"verdict":"passed","summary":"all good","pullRequestUrl":"` + followUpPR + `"}`, crash: true},
			},
			want: verdictWant{status: "failed", failureMode: FailureProviderError, workResult: "passed", manifest: "passed", wantSteering: true},
		},
		{
			name: "no manifest keeps the follow-up's terminal text",
			turns: []verdictScriptTurn{
				{text: "Work in progress."},
				{text: followUpPR},
			},
			want: verdictWant{status: "completed", summary: followUpPR, pr: followUpPR, wantSteering: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runScripted(t, "development", false, "", tc.turns...)
			assertVerdict(t, res, tc.want)
		})
	}
}

// TestRun_TurnVerdictAcrossMemoryInjectFollowUp pins the same resolution for a
// runtime memory-inject follow-up turn on result-sensitive work, where the
// verdict drives the issue transition. Steering is off so the memory inject is
// the only follow-up turn.
func TestRun_TurnVerdictAcrossMemoryInjectFollowUp(t *testing.T) {
	cases := []struct {
		name  string
		turns []verdictScriptTurn
		want  verdictWant
	}{
		{
			name: "manifest rewritten during the memory-inject follow-up is read",
			turns: []verdictScriptTurn{
				{manifest: passedManifest, text: "All checks pass."},
				{manifest: failedManifest, text: "Re-checked with the recalled context."},
			},
			want: verdictWant{status: "completed", workResult: "failed", summary: manifestSummary, manifest: "failed"},
		},
		{
			name: "stale failed manifest is never upgraded by a memory-inject follow-up passed marker",
			turns: []verdictScriptTurn{
				{manifest: failedManifest, text: "The regression reproduces."},
				{text: "Looked again with the recalled context.\nWORK_RESULT:passed"},
			},
			want: verdictWant{status: "completed", workResult: "failed", summary: manifestSummary, manifest: "failed"},
		},
		{
			name: "stale passed manifest never upgrades the memory-inject follow-up's failed verdict",
			turns: []verdictScriptTurn{
				{manifest: passedManifest, text: "All checks pass."},
				{text: "The recalled regression reproduces.\nWORK_RESULT:failed"},
			},
			want: verdictWant{status: "completed", workResult: "failed", summary: "The recalled regression reproduces.\nWORK_RESULT:failed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runScripted(t, "qa", true, "recall: a regression in this area last week", tc.turns...)
			assertVerdict(t, res, tc.want)
		})
	}
}

// TestFoldTurnManifest_NeverClearsRunnerFailure pins that folding an agent
// manifest leaves every runner-observed failure field untouched, whatever the
// manifest's verdict.
func TestFoldTurnManifest_NeverClearsRunnerFailure(t *testing.T) {
	for _, verdict := range []string{"passed", "failed", "blocked"} {
		t.Run(verdict, func(t *testing.T) {
			res := &Result{}
			res.Status, res.FailureMode, res.Error = "failed", FailureProviderError, "provider crashed"
			obs := &streamObservation{}
			foldTurnManifest(&TurnManifest{SchemaVersion: 1, Verdict: verdict, Summary: "agent says " + verdict}, res, obs)
			if res.Status != "failed" || res.FailureMode != FailureProviderError || res.Error != "provider crashed" {
				t.Fatalf("fold changed the runner-recorded failure: Status=%q FailureMode=%q Error=%q", res.Status, res.FailureMode, res.Error)
			}
		})
	}
}

// newFollowUpRunner builds a Runner with tail steering enabled around the
// supplied provider. Backstop and post-session stay off so the tests observe
// exactly the manifest / follow-up interaction.
func newFollowUpRunner(t *testing.T, platformURL string, client *http.Client, p agent.Provider) *Runner {
	t.Helper()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: platformURL,
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		HTTPClient:  client,
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	reg := NewRegistry()
	if err := reg.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := New(Options{
		Registry:               reg,
		WorktreeManager:        wtm,
		Poster:                 poster,
		HTTPClient:             client,
		SkipBackstop:           true,
		SkipPostSession:        true,
		PreserveWorktreeAlways: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// TestStampManifest_DetectsEveryRewrite pins the follow-up rewrite detector:
// an untouched file keeps its stamp, while new content, a same-size edit, an
// identical-content rewrite (mtime only) and a deletion each change it.
func TestStampManifest_DetectsEveryRewrite(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, state.AgentDirName)
	if err := os.MkdirAll(agentDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(agentDir, ManifestFileName)
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	write := func(body string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	if got := stampManifest(dir); got.exists {
		t.Fatalf("absent manifest stamped as existing: %+v", got)
	}
	write(passedManifest, base)
	before := stampManifest(dir)
	if !before.exists {
		t.Fatal("written manifest stamped as absent")
	}
	if again := stampManifest(dir); again != before {
		t.Fatal("untouched manifest changed its stamp")
	}

	cases := []struct {
		name   string
		mutate func()
	}{
		{"identical content rewritten later", func() { write(passedManifest, base.Add(time.Second)) }},
		{"same size, different bytes, same mtime", func() {
			write(`{"schemaVersion":1,"verdict":"passed","summary":"all dune"}`, base)
		}},
		{"different content", func() { write(failedManifest, base) }},
		{"deleted", func() {
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			write(passedManifest, base)
			if got := stampManifest(dir); got != before {
				t.Fatal("reset did not restore the original stamp")
			}
			tc.mutate()
			if got := stampManifest(dir); got == before {
				t.Fatalf("stamp unchanged after %s", tc.name)
			}
		})
	}
}

// TestScanLineVerdict pins the anchoring rule a follow-up marker must meet
// before it may supersede a stale manifest.
func TestScanLineVerdict(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"WORK_RESULT:failed", "failed"},
		{"done\n  WORK_RESULT: passed", "passed"},
		{"done\n<!-- WORK_RESULT:blocked -->", "blocked"},
		{"WORK_RESULT passed", "passed"},
		{"first\nWORK_RESULT:passed\nthen\nWORK_RESULT:failed", "failed"},
		{"I am not claiming WORK_RESULT: passed yet", ""},
		{"could not open the PR. WORK_RESULT:failed", ""},
		{"WORK_RESULT:\nfailed", ""},
		{"WORK_RESULT:unknown", ""},
	}
	for _, tc := range cases {
		if got := scanLineVerdict(tc.text); got != tc.want {
			t.Errorf("scanLineVerdict(%q) = %q; want %q", tc.text, got, tc.want)
		}
	}
}
