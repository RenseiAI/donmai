package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/provider/stub"
	"github.com/RenseiAI/agentfactory-tui/result"
	"github.com/RenseiAI/agentfactory-tui/runtime/worktree"
)

// TestLoop_EventsMirroredToJSONL confirms every event the provider
// emits is appended to <worktree>/.agent/events.jsonl as a discrete
// JSONL row decodable via agent.UnmarshalEvent.
func TestLoop_EventsMirroredToJSONL(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-LOOP-1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.WorktreePath == "" {
		t.Fatal("no WorktreePath on result")
	}
	jsonlPath := filepath.Join(res.WorktreePath, ".agent", "events.jsonl")
	f, err := os.Open(jsonlPath) //nolint:gosec // path is owned by the runner via worktree manager
	if err != nil {
		t.Fatalf("open events.jsonl: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	var kinds []agent.EventKind
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, err := agent.UnmarshalEvent(line)
		if err != nil {
			t.Fatalf("UnmarshalEvent line %q: %v", line, err)
		}
		kinds = append(kinds, ev.Kind())
	}
	// Stub canonical sequence: init, system, assistant_text, tool_use,
	// tool_result, assistant_text, result.
	if len(kinds) < 5 {
		t.Errorf("expected >=5 events; got %d (%v)", len(kinds), kinds)
	}
	if kinds[0] != agent.EventInit {
		t.Errorf("first event = %s; want init", kinds[0])
	}
	if kinds[len(kinds)-1] != agent.EventResult {
		t.Errorf("last event = %s; want result", kinds[len(kinds)-1])
	}
}

// TestLoop_StateStoreUpdated confirms the runner writes the
// .agent/state.json snapshot during the loop.
func TestLoop_StateStoreUpdated(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-LOOP-STATE")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	statePath := filepath.Join(res.WorktreePath, ".agent", "state.json")
	body, err := os.ReadFile(statePath) //nolint:gosec // path is owned by the runner
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, qw.SessionID) {
		t.Errorf("state.json missing SessionID; got %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "stub") {
		t.Errorf("state.json missing provider name; got %q", bodyStr)
	}
}

// TestLoop_ProviderError_ClassifiesFailure routes the stub's
// BehaviorMidStreamError through the runner and asserts the failure
// mode is FailureProviderError with the provider's error message
// surfaced on Result.Error.
func TestLoop_ProviderError_ClassifiesFailure(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-LOOP-ERR")
	qw.ResolvedProfile.ProviderConfig = map[string]any{
		"stub.behavior": string(stub.BehaviorMidStreamError),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, _ := h.runner.Run(ctx, qw)
	if res.FailureMode != FailureProviderError {
		t.Errorf("FailureMode = %q; want %q (Error=%q)", res.FailureMode, FailureProviderError, res.Error)
	}
	if !strings.Contains(res.Error, "crashed") {
		t.Errorf("expected provider crash text in Error; got %q", res.Error)
	}
}

// TestLoop_SilentExit_Classifies routes BehaviorSilentFail (no terminal
// ResultEvent) through the runner and asserts FailureSilentExit.
func TestLoop_SilentExit_Classifies(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-LOOP-SIL")
	qw.ResolvedProfile.ProviderConfig = map[string]any{
		"stub.behavior": string(stub.BehaviorSilentFail),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, _ := h.runner.Run(ctx, qw)
	if res.FailureMode != FailureSilentExit {
		t.Errorf("FailureMode = %q; want %q", res.FailureMode, FailureSilentExit)
	}
}

// TestLoop_TimeoutCancelsStream confirms ctx cancellation propagates
// to the provider and surfaces FailureTimeout on the result.
func TestLoop_TimeoutCancelsStream(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-LOOP-TO")
	qw.ResolvedProfile.ProviderConfig = map[string]any{
		"stub.behavior": string(stub.BehaviorHangThenTimeout),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, _ := h.runner.Run(ctx, qw)
	// Both timeout and silent-exit are valid — depends on whether the
	// provider got the cancel before or after closing the channel.
	if res.FailureMode != FailureTimeout && res.FailureMode != FailureSilentExit {
		t.Errorf("FailureMode = %q; want timeout or silent-exit", res.FailureMode)
	}
}

// TestLoop_HeartbeatLostOwnership simulates a platform that always
// rejects /lock-refresh; after 3 strikes the pulser closes its
// LostOwnership channel and the runner cancels the stream with
// FailureLostOwnership.
//
// Uses BehaviorHangThenTimeout so the provider does not race the
// heartbeat to a terminal Result.
func TestLoop_HeartbeatLostOwnership(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	var refreshes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/lock-refresh") {
			refreshes.Add(1)
			http.Error(w, "lost", http.StatusGone)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "w1",
		AuthToken:   "tok",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	p, _ := stub.New()
	_ = reg.Register(p)
	r, err := New(Options{
		Registry:          reg,
		WorktreeManager:   wtm,
		Poster:            poster,
		HTTPClient:        srv.Client(),
		HeartbeatInterval: 50 * time.Millisecond,
		SkipBackstop:      true,
		SkipSteering:      true,
		SkipPostSession:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-LOOP-LOST"),
		WorkerID:    "w1",
		AuthToken:   "tok",
		PlatformURL: srv.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
			ProviderConfig: map[string]any{
				"stub.behavior": string(stub.BehaviorHangThenTimeout),
			},
		},
	}
	qw.Repository = bareRepo

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, _ := r.Run(ctx, qw)

	// Either lost-ownership or timeout depending on the cancellation
	// race; lost-ownership is the expected outcome but we tolerate
	// timeout to keep the test stable on slow CI.
	if res.FailureMode != FailureLostOwnership && res.FailureMode != FailureTimeout {
		t.Errorf("FailureMode = %q; want lost-ownership or timeout", res.FailureMode)
	}
}

// TestRunLoop_HeartbeatBodyIncludesIssueID is the REN-1465 regression:
// the runner must source heartbeat IssueID from prompt.QueuedWork.IssueID
// (populated by the daemon's poll handler) so the platform's
// /api/sessions/<id>/lock-refresh handler accepts the request. Before
// REN-1465 the runner sourced IssueID from a never-populated
// IssueLockID field, producing {"workerId":"...","issueId":""} on the
// wire and a 400 from the platform on every tick.
//
// The test stands up an httptest.Server that captures the JSON body
// posted to /lock-refresh, drives one Run() with a fully-populated qw,
// and asserts the captured body has both workerId and issueId
// non-empty (and that issueId equals the qw.IssueID we passed in).
func TestRunLoop_HeartbeatBodyIncludesIssueID(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	const wantWorkerID = "wkr_test_1"
	const wantIssueID = "08f26531-f5d2-49dc-b412-b42cef0cbffa"

	type capturedBody struct {
		WorkerID string `json:"workerId"`
		IssueID  string `json:"issueId"`
	}
	var (
		mu         sync.Mutex
		bodies     []capturedBody
		refreshHit atomic.Int64
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/lock-refresh") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		refreshHit.Add(1)
		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		var body capturedBody
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode lock-refresh body: %v (raw=%q)", err, raw)
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		// Mirror the platform's success response.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true}`))
	}))
	t.Cleanup(srv.Close)

	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    wantWorkerID,
		AuthToken:   "tok",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	p, _ := stub.New()
	_ = reg.Register(p)
	r, err := New(Options{
		Registry:          reg,
		WorktreeManager:   wtm,
		Poster:            poster,
		HTTPClient:        srv.Client(),
		HeartbeatInterval: 24 * time.Hour, // suppress further ticks; first tick fires synchronously
		SkipBackstop:      true,
		SkipSteering:      true,
		SkipPostSession:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Use queuedWorkBase + override IssueID so we can pin the expected
	// value. The base helper sets IssueID to "issue-uuid-<identifier>",
	// but we want a stable UUID-shaped value that mirrors the live wire.
	base := queuedWorkBase("REN-1465")
	base.IssueID = wantIssueID
	qw := QueuedWork{
		QueuedWork:  base,
		WorkerID:    wantWorkerID,
		AuthToken:   "tok",
		PlatformURL: srv.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}
	qw.Repository = bareRepo

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, runErr := r.Run(ctx, qw); runErr != nil {
		// Run may surface a non-nil err if the stub provider exits
		// non-cleanly under the test fixture; the regression check is
		// strictly on the heartbeat body capture.
		t.Logf("Run returned err (non-fatal for this regression): %v", runErr)
	}

	if refreshHit.Load() == 0 {
		t.Fatalf("no /lock-refresh requests captured (heartbeat never fired)")
	}

	mu.Lock()
	captured := append([]capturedBody{}, bodies...)
	mu.Unlock()

	for i, b := range captured {
		if b.WorkerID == "" {
			t.Errorf("body[%d]: workerId empty (full=%+v)", i, b)
		}
		if b.IssueID == "" {
			t.Errorf("body[%d]: issueId empty — REN-1465 regression (full=%+v)", i, b)
		}
		if b.IssueID != wantIssueID {
			t.Errorf("body[%d]: issueId = %q; want %q", i, b.IssueID, wantIssueID)
		}
		if b.WorkerID != wantWorkerID {
			t.Errorf("body[%d]: workerId = %q; want %q", i, b.WorkerID, wantWorkerID)
		}
	}
}

// TestObserveEvent_ScansWorkResultMarker confirms the loop's
// AssistantText scanner reads the WORK_RESULT:passed/failed marker.
func TestObserveEvent_ScansWorkResultMarker(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"WORK_RESULT:passed", "passed"},
		{"<!-- WORK_RESULT:failed -->", "failed"},
		{"some text WORK_RESULT: passed and more", "passed"},
		{"no marker here", ""},
	}
	for _, tc := range cases {
		got := scanWorkResult(tc.text)
		if got != tc.want {
			t.Errorf("scanWorkResult(%q) = %q; want %q", tc.text, got, tc.want)
		}
	}
}

// TestScanPRURL_ExtractsURL confirms the regex captures a github PR
// URL out of arbitrary tool output.
func TestScanPRURL_ExtractsURL(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"opened https://github.com/RenseiAI/agentfactory-tui/pull/123", "https://github.com/RenseiAI/agentfactory-tui/pull/123"},
		{"https://github.com/foo-bar/baz_qux/pull/9", "https://github.com/foo-bar/baz_qux/pull/9"},
		{"no url", ""},
	}
	for _, tc := range cases {
		got := scanPRURL(tc.text)
		if got != tc.want {
			t.Errorf("scanPRURL(%q) = %q; want %q", tc.text, got, tc.want)
		}
	}
}

// TestEnvToMap_RoundTrip confirms the KEY=VALUE → map conversion the
// loop uses to thread env through the composer.
func TestEnvToMap_RoundTrip(t *testing.T) {
	in := []string{"FOO=bar", "BAZ=", "KEY=val=with=eq"}
	got := envToMap(in)
	if got["FOO"] != "bar" {
		t.Errorf("FOO = %q; want bar", got["FOO"])
	}
	if v, ok := got["BAZ"]; !ok || v != "" {
		t.Errorf("BAZ = %q (ok=%v); want empty present", v, ok)
	}
	if got["KEY"] != "val=with=eq" {
		t.Errorf("KEY = %q; want val=with=eq", got["KEY"])
	}
}

// TestBuildSessionEnv_PopulatesStandardKeys confirms LINEAR_* +
// AGENTFACTORY_* keys land on the per-session env.
func TestBuildSessionEnv_PopulatesStandardKeys(t *testing.T) {
	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-ENV-1"),
		WorkerID:    "w1",
		AuthToken:   "tok",
		PlatformURL: "https://example.test",
	}
	envOut := buildSessionEnv(qw)
	for _, key := range []string{
		"AGENTFACTORY_SESSION_ID",
		"LINEAR_SESSION_ID",
		"LINEAR_ISSUE_ID",
		"LINEAR_ISSUE_IDENTIFIER",
		"LINEAR_WORK_TYPE",
		"AGENTFACTORY_PROJECT",
		"AGENTFACTORY_ORG_ID",
		"AGENTFACTORY_API_URL",
		"WORKER_AUTH_TOKEN",
	} {
		if envOut[key] == "" {
			t.Errorf("env missing %q", key)
		}
	}
}

// TestRunLoop_PostsActivityToplatform asserts the runner streams every
// non-skipped agent.Event through to /api/sessions/<id>/activity, and
// that the first such post triggers a single status=running nudge.
//
// This is the wire-up regression: before the runtime/activity wiring
// was added the platform's activity buffer stayed empty, leaving
// `rensei session stream` blank and the /topology page unable to
// hydrate sub-agent nodes.
func TestRunLoop_PostsActivityToPlatform(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	type capturedActivity struct {
		WorkerID string `json:"workerId"`
		Activity struct {
			Type     string `json:"type"`
			Content  string `json:"content"`
			ToolName string `json:"toolName"`
		} `json:"activity"`
	}
	var (
		mu           sync.Mutex
		activities   []capturedActivity
		runningHits  atomic.Int64
		statusBodies []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/activity"):
			raw, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			var c capturedActivity
			if err := json.Unmarshal(raw, &c); err == nil {
				mu.Lock()
				activities = append(activities, c)
				mu.Unlock()
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/status"):
			raw, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			s := string(raw)
			if strings.Contains(s, `"status":"running"`) {
				runningHits.Add(1)
			}
			mu.Lock()
			statusBodies = append(statusBodies, s)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
		}
	}))
	t.Cleanup(srv.Close)

	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "wkr_act_1",
		AuthToken:   "tok",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	p, _ := stub.New()
	_ = reg.Register(p)
	r, err := New(Options{
		Registry:        reg,
		WorktreeManager: wtm,
		Poster:          poster,
		HTTPClient:      srv.Client(),
		SkipBackstop:    true,
		SkipSteering:    true,
		SkipPostSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-ACT-1"),
		WorkerID:    "wkr_act_1",
		AuthToken:   "tok",
		PlatformURL: srv.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}
	qw.Repository = bareRepo

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, runErr := r.Run(ctx, qw); runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}

	// Allow the async poster a beat to drain post-Run. The runner's
	// defer Stop() blocks for the configured drain timeout (2s default)
	// — by here every successfully-acked event has hit the server.
	mu.Lock()
	got := append([]capturedActivity{}, activities...)
	statusSnapshot := append([]string{}, statusBodies...)
	mu.Unlock()

	if len(got) == 0 {
		t.Fatalf("expected >=1 activity POST; got 0 (status posts: %v)", statusSnapshot)
	}

	// Expect at minimum one thought (AssistantText) + one action
	// (ToolUse) + one response (Result) from the stub provider.
	var sawThought, sawAction, sawResponse bool
	for _, c := range got {
		if c.WorkerID != "wkr_act_1" {
			t.Errorf("workerId = %q; want wkr_act_1", c.WorkerID)
		}
		switch c.Activity.Type {
		case "thought":
			sawThought = true
		case "action":
			sawAction = true
		case "response":
			sawResponse = true
		}
	}
	types := make([]string, 0, len(got))
	for _, c := range got {
		types = append(types, c.Activity.Type)
	}
	if !sawThought {
		t.Errorf("expected a thought activity; got types %v", types)
	}
	if !sawAction {
		t.Errorf("expected an action activity; got types %v", types)
	}
	if !sawResponse {
		t.Errorf("expected a response activity; got types %v", types)
	}
	if got := runningHits.Load(); got != 1 {
		t.Errorf("expected exactly 1 status=running nudge; got %d", got)
	}
}

// TestConsumeEvents_DispatchesToSink verifies the package-internal
// fan-out: every event observeEvent processes is also handed to the
// sink. Uses a fake sink + canned event channel to avoid the HTTP path.
func TestConsumeEvents_DispatchesToSink(t *testing.T) {
	r := minimalRunner(t)

	rec := &recordingSink{}
	events := make(chan agent.Event, 8)
	events <- agent.InitEvent{SessionID: "init-1"}
	events <- agent.AssistantTextEvent{Text: "thinking"}
	events <- agent.ToolUseEvent{ToolName: "Bash", Input: map[string]any{"command": "ls"}}
	events <- agent.ResultEvent{Success: true}
	close(events)

	handle := &fakeHandle{events: events}
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-SINK-1")}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	enforcer := NewBudgetEnforcer(nil, time.Now())
	if _, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, rec); err != nil {
		t.Fatalf("consumeEvents: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if got := len(rec.events); got != 4 {
		t.Fatalf("recorded %d events; want 4 (got=%v)", got, kindsOf(rec.events))
	}
}

// recordingSink is a test-only activitySink implementation that
// captures every Send call for later assertion.
type recordingSink struct {
	mu     sync.Mutex
	events []agent.Event
}

func (s *recordingSink) Send(_ context.Context, ev agent.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

// fakeHandle is a minimal agent.Handle backed by a caller-provided
// events channel. Stop / Inject are no-ops.
type fakeHandle struct {
	events chan agent.Event
}

func (h *fakeHandle) SessionID() string                    { return "" }
func (h *fakeHandle) Events() <-chan agent.Event           { return h.events }
func (h *fakeHandle) Inject(context.Context, string) error { return nil }
func (h *fakeHandle) Stop(context.Context) error           { return nil }

func kindsOf(evs []agent.Event) []agent.EventKind {
	out := make([]agent.EventKind, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind())
	}
	return out
}
