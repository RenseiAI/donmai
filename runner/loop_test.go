package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
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

// TestRunLoop_HeartbeatBodyIncludesIssueID is the heartbeat-issue-id regression:
// the runner must source heartbeat IssueID from prompt.QueuedWork.IssueID
// (populated by the daemon's poll handler) so the platform's
// /api/sessions/<id>/lock-refresh handler accepts the request. Before
// the fix the runner sourced IssueID from a never-populated
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
	base := queuedWorkBase("ENG-1465")
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
			t.Errorf("body[%d]: issueId empty — ENG-1465 regression (full=%+v)", i, b)
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

// TestScanBlocked_DetectsDeclineMarkers verifies the structural
// blocked-agent signal: scanBlocked recognises both the
// "WORK_RESULT:blocked" verdict form and the "AGENT_BLOCKED: <reason>"
// reason form (on a line of their own, as agents are instructed to emit
// them), captures the reason, and does NOT false-positive on ordinary
// text, a passing verdict, or a mid-sentence quote of the marker.
func TestScanBlocked_DetectsDeclineMarkers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		text       string
		wantOK     bool
		wantReason string
	}{
		{"work-result-blocked", "WORK_RESULT:blocked", true, ""},
		{"work-result-blocked-final-line", "Here is my summary.\nWORK_RESULT:blocked", true, ""},
		{"work-result-blocked-indented", "  WORK_RESULT:blocked", true, ""},
		{"work-result-blocked-comment", "<!-- WORK_RESULT:blocked -->", true, ""},
		{"agent-blocked-reason", "AGENT_BLOCKED: spec is ambiguous, no acceptance criteria", true, "spec is ambiguous, no acceptance criteria"},
		{"agent-blocked-reason-final-line", "Some narrative.\nAGENT_BLOCKED: spec ambiguous", true, "spec ambiguous"},
		{"agent-blocked-reason-trailing-newline", "AGENT_BLOCKED: missing repo access\nmore output", true, "missing repo access"},
		{"agent-blocked-reason-comment", "<!-- AGENT_BLOCKED: missing repo access -->", true, "missing repo access"},
		{"passed-not-blocked", "WORK_RESULT:passed", false, ""},
		{"failed-not-blocked", "WORK_RESULT:failed", false, ""},
		{"no-marker", "I am working on the task now", false, ""},
		// False-positive guards: the markers must NOT fire when merely
		// quoted/discussed mid-sentence in a narrative turn. These two
		// strings come from the adversarial probe and previously matched.
		{"agent-blocked-quoted-midsentence", "I would print AGENT_BLOCKED: <reason> if I could not proceed, but I can.", false, ""},
		{"work-result-blocked-quoted-midsentence", "I'd emit WORK_RESULT: blocked. It isn't, so here is the code.", false, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reason, ok := scanBlocked(tc.text)
			if ok != tc.wantOK {
				t.Fatalf("scanBlocked(%q) ok = %v; want %v", tc.text, ok, tc.wantOK)
			}
			if reason != tc.wantReason {
				t.Errorf("scanBlocked(%q) reason = %q; want %q", tc.text, reason, tc.wantReason)
			}
		})
	}
}

// TestScanBlocked_DoesNotShadowWorkResult guards the regex split: a
// "WORK_RESULT:blocked" line is a blocked signal but must NOT be parsed as
// a passed/failed/unknown QA verdict by scanWorkResult (which drives the
// Linear status transition). Keeping them separate prevents a deliberate
// decline from accidentally transitioning the issue.
func TestScanBlocked_DoesNotShadowWorkResult(t *testing.T) {
	t.Parallel()
	if got := scanWorkResult("WORK_RESULT:blocked"); got != "" {
		t.Errorf("scanWorkResult(blocked) = %q; want \"\" (blocked is not a QA verdict)", got)
	}
	if _, ok := scanBlocked("WORK_RESULT:blocked"); !ok {
		t.Error("scanBlocked(blocked) = false; want true")
	}
}

// TestClassifyBlocked_ForksOutcome exercises the classification fork: a
// blocked observation with no PR becomes FailureAgentBlocked; a blocked
// observation that nonetheless produced a PR is NOT blocked (the work
// landed); a non-blocked observation is untouched.
func TestClassifyBlocked_ForksOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		obs         streamObservation
		startPR     string
		wantBlocked bool
		wantMode    string
		wantErrSub  string
	}{
		{
			name:        "blocked-no-pr-with-reason",
			obs:         streamObservation{blocked: true, blockedReason: "spec ambiguous"},
			wantBlocked: true,
			wantMode:    FailureAgentBlocked,
			wantErrSub:  "spec ambiguous",
		},
		{
			name:        "blocked-no-pr-no-reason",
			obs:         streamObservation{blocked: true},
			wantBlocked: true,
			wantMode:    FailureAgentBlocked,
			wantErrSub:  "blocked",
		},
		{
			name:        "blocked-but-pr-produced-not-blocked",
			obs:         streamObservation{blocked: true, blockedReason: "x"},
			startPR:     "https://github.com/o/r/pull/1",
			wantBlocked: false,
			wantMode:    "",
		},
		{
			name:        "not-blocked-untouched",
			obs:         streamObservation{blocked: false},
			wantBlocked: false,
			wantMode:    "",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := &Result{}
			res.PullRequestURL = tt.startPR
			got := classifyBlocked(res, tt.obs)
			if got != tt.wantBlocked {
				t.Fatalf("classifyBlocked = %v; want %v", got, tt.wantBlocked)
			}
			if res.FailureMode != tt.wantMode {
				t.Errorf("FailureMode = %q; want %q", res.FailureMode, tt.wantMode)
			}
			if tt.wantBlocked {
				if res.Status != "failed" {
					t.Errorf("Status = %q; want failed", res.Status)
				}
				if tt.wantErrSub != "" && !strings.Contains(res.Error, tt.wantErrSub) {
					t.Errorf("Error = %q; want substring %q", res.Error, tt.wantErrSub)
				}
			}
		})
	}
}

// TestObserveEvent_SetsBlockedFlag confirms the AssistantTextEvent branch
// of observeEvent records the blocked flag + reason on the observation so
// the post-stream classifier can fork to FailureAgentBlocked.
func TestObserveEvent_SetsBlockedFlag(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	obs := &streamObservation{}
	h.runner.observeEvent(
		agent.AssistantTextEvent{Text: "AGENT_BLOCKED: no acceptance criteria on the issue"},
		obs, t.TempDir(), QueuedWork{},
	)
	if !obs.blocked {
		t.Fatal("observeEvent did not set obs.blocked")
	}
	if obs.blockedReason != "no acceptance criteria on the issue" {
		t.Errorf("obs.blockedReason = %q; want captured reason", obs.blockedReason)
	}
}

// TestObserveEvent_CapturesLastAssistantText confirms the
// AssistantTextEvent branch tracks the most recent non-empty assistant
// message (the codex-path summary fallback) and ignores whitespace-only
// frames.
func TestObserveEvent_CapturesLastAssistantText(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	obs := &streamObservation{}
	wt := t.TempDir()
	for _, text := range []string{"first message", "  \n\t", "final message <!-- WORK_RESULT:passed -->"} {
		h.runner.observeEvent(agent.AssistantTextEvent{Text: text}, obs, wt, QueuedWork{})
	}
	if want := "final message <!-- WORK_RESULT:passed -->"; obs.lastAssistantText != want {
		t.Errorf("obs.lastAssistantText = %q; want %q", obs.lastAssistantText, want)
	}
	if obs.workResult != "passed" {
		t.Errorf("obs.workResult = %q; want passed", obs.workResult)
	}
}

// TestObserveEvent_MarkerInPenultimateMessage pins the agy-shaped stream:
// the WORK_RESULT marker arrives as a standalone message BEFORE the final
// assistant message. The scan covers every assistant message (latest
// marker wins; a later marker-less message never resets the verdict), so
// the verdict and the final-message summary both survive. The platform's
// prompt contract still documents marker-on-the-final-message — this is
// tolerance, not a contract change.
func TestObserveEvent_MarkerInPenultimateMessage(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	obs := &streamObservation{}
	wt := t.TempDir()
	for _, text := range []string{
		"research synthesis complete",
		"<!-- WORK_RESULT:passed -->",
		"Here is the final report body.",
	} {
		h.runner.observeEvent(agent.AssistantTextEvent{Text: text}, obs, wt, QueuedWork{})
	}
	if obs.workResult != "passed" {
		t.Errorf("obs.workResult = %q; want passed (penultimate-message marker dropped)", obs.workResult)
	}
	if want := "Here is the final report body."; obs.lastAssistantText != want {
		t.Errorf("obs.lastAssistantText = %q; want %q", obs.lastAssistantText, want)
	}
}

// TestApplyTo_SummaryStamping pins the terminal-summary semantics:
//
//   - terminal message present → it IS the summary, and it LAST-wins so a
//     resume turn after a background-poll wakeup restamps the TRUE final
//     assistant message instead of keeping the stale pre-wakeup text
//     (2026-06-10 rehearsal 3, claude wakeup path);
//   - terminal message absent (codex turn/completed carries no text) →
//     fall back to the latest assistant text observed on the stream so
//     the WORK_RESULT marker still reaches the platform's exit event;
//   - a partial stream (no terminal event) never clobbers an existing
//     summary but does fill an empty one.
func TestApplyTo_SummaryStamping(t *testing.T) {
	t.Parallel()

	terminal := func(msg string) *agent.ResultEvent {
		return &agent.ResultEvent{Success: true, Message: msg}
	}

	tests := []struct {
		name string
		obs  []streamObservation
		want string
	}{
		{
			name: "terminal message wins",
			obs: []streamObservation{
				{terminalEvent: terminal("final answer"), lastAssistantText: "narration"},
			},
			want: "final answer",
		},
		{
			name: "codex: terminal without message falls back to last assistant text",
			obs: []streamObservation{
				{terminalEvent: terminal(""), lastAssistantText: "QA verdict <!-- WORK_RESULT:passed -->"},
			},
			want: "QA verdict <!-- WORK_RESULT:passed -->",
		},
		{
			name: "wakeup restamps post-wakeup terminal message (last-wins)",
			obs: []streamObservation{
				{terminalEvent: terminal("stale pre-wakeup text")},
				{terminalEvent: terminal("true final message <!-- WORK_RESULT:passed -->")},
			},
			want: "true final message <!-- WORK_RESULT:passed -->",
		},
		{
			name: "codex wakeup restamps post-wakeup assistant text",
			obs: []streamObservation{
				{terminalEvent: terminal(""), lastAssistantText: "stale pre-wakeup text"},
				{terminalEvent: terminal(""), lastAssistantText: "true final message"},
			},
			want: "true final message",
		},
		{
			name: "partial resume stream keeps existing summary",
			obs: []streamObservation{
				{terminalEvent: terminal("final answer")},
				{lastAssistantText: "mid-turn narration, stream cut"},
			},
			want: "final answer",
		},
		{
			name: "partial stream fills empty summary",
			obs: []streamObservation{
				{lastAssistantText: "only narration before crash"},
			},
			want: "only narration before crash",
		},
		{
			name: "empty observation leaves summary alone",
			obs: []streamObservation{
				{terminalEvent: terminal("final answer")},
				{},
			},
			want: "final answer",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := &Result{}
			for _, o := range tc.obs {
				o.applyTo(res, agent.ProviderClaude)
			}
			if res.Summary != tc.want {
				t.Errorf("Summary = %q; want %q", res.Summary, tc.want)
			}
		})
	}
}

// TestShouldBackstop_SkipsBlocked verifies a blocked outcome never
// triggers the empty-branch backstop — a deliberate decline has no work
// to commit and an auto-PR would misrepresent the refusal.
func TestShouldBackstop_SkipsBlocked(t *testing.T) {
	t.Parallel()
	res := &Result{}
	res.FailureMode = FailureAgentBlocked
	// Use a result-sensitive work type so the contract gate is open and
	// the FailureAgentBlocked rule (not the work-type gate) is what skips.
	if shouldBackstop(res, WorkTypeDevelopmentStr) {
		t.Error("shouldBackstop(FailureAgentBlocked) = true; want false")
	}
}

// TestDefaultMCPServers_EmitsHTTPEntryPerSession pins the A2A per-session
// MCP wire-up: when QueuedWork has PlatformURL + AuthToken + SessionID,
// defaultMCPServers emits a single HTTP entry pointing at the platform's
// /api/mcp/<sessionId> route with the worker bearer in Authorization.
func TestDefaultMCPServers_EmitsHTTPEntryPerSession(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{}
	qw.SessionID = "sess_abc"
	qw.PlatformURL = "https://platform.example.com"
	qw.AuthToken = "rsk_test"

	servers := defaultMCPServers(qw)
	if len(servers) != 1 {
		t.Fatalf("len(servers)=%d, want 1", len(servers))
	}
	got := servers[0]
	// Name is brand-derived (statehome.Brand()+"-platform"): OSS default brand
	// "donmai" -> "donmai-platform"; the closed rensei binary (brand "rensei")
	// renders "rensei-platform". This parallel test uses the default brand.
	if got.Name != "donmai-platform" {
		t.Errorf("name=%q, want donmai-platform", got.Name)
	}
	if got.Type != "http" {
		t.Errorf("type=%q, want http", got.Type)
	}
	if got.URL != "https://platform.example.com/api/mcp/sess_abc" {
		t.Errorf("url=%q", got.URL)
	}
	if got.Headers["Authorization"] != "Bearer rsk_test" {
		t.Errorf("auth header=%q", got.Headers["Authorization"])
	}
}

// TestDefaultMCPServers_TrimsTrailingSlash makes sure the URL composer
// doesn't emit a double-slash when PlatformURL has a trailing slash.
func TestDefaultMCPServers_TrimsTrailingSlash(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{}
	qw.SessionID = "sess_xyz"
	qw.PlatformURL = "https://platform.example.com/"
	qw.AuthToken = "rsk_test"

	servers := defaultMCPServers(qw)
	if len(servers) != 1 {
		t.Fatalf("len(servers)=%d, want 1", len(servers))
	}
	if servers[0].URL != "https://platform.example.com/api/mcp/sess_xyz" {
		t.Errorf("url=%q (double-slash leak?)", servers[0].URL)
	}
}

// TestDefaultMCPServers_OmitsWhenStandalone pins the back-compat path:
// in standalone mode (no PlatformURL or no AuthToken), no MCP entry is
// emitted at all and the agent runs without the per-session gate.
func TestDefaultMCPServers_OmitsWhenStandalone(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		qw   QueuedWork
	}{
		{"no PlatformURL", func() QueuedWork {
			qw := QueuedWork{AuthToken: "rsk_test"}
			qw.SessionID = "sess_1"
			return qw
		}()},
		{"no AuthToken", func() QueuedWork {
			qw := QueuedWork{PlatformURL: "https://platform.example.com"}
			qw.SessionID = "sess_1"
			return qw
		}()},
		{"no SessionID", QueuedWork{
			PlatformURL: "https://platform.example.com",
			AuthToken:   "rsk_test",
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := defaultMCPServers(tc.qw); got != nil {
				t.Errorf("got %+v; want nil", got)
			}
		})
	}
}

// TestMergeMCPServers_RetainsPlatformGate verifies the WS5 MCP merge: the
// platform per-session HTTP gate (the default) ALWAYS leads and the agent
// card's servers are appended after it.
func TestMergeMCPServers_RetainsPlatformGate(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{}
	qw.SessionID = "sess_abc"
	qw.PlatformURL = "https://platform.example.com"
	qw.AuthToken = "rsk_test"
	qw.McpServers = []agent.MCPServerConfig{
		{Name: "card-linear", Type: "stdio", Command: "pnpm", Args: []string{"af-linear"}},
		{Name: "card-remote", Type: "http", URL: "https://card.test/mcp"},
	}

	merged := mergeMCPServers(defaultMCPServers(qw), qw.McpServers)
	if len(merged) != 3 {
		t.Fatalf("merged len = %d, want 3 (platform gate + 2 card)", len(merged))
	}
	// Platform gate must lead and be retained.
	if merged[0].Name != "donmai-platform" || merged[0].Type != "http" {
		t.Errorf("merged[0] must be the platform gate; got %+v", merged[0])
	}
	if merged[1].Name != "card-linear" || merged[2].Name != "card-remote" {
		t.Errorf("card servers must follow the gate; got %+v", merged)
	}
}

// TestMergeMCPServers_DefaultWinsOnNameCollision verifies a card entry whose
// name collides with the platform gate does NOT shadow the gate.
func TestMergeMCPServers_DefaultWinsOnNameCollision(t *testing.T) {
	t.Parallel()

	defaults := []agent.MCPServerConfig{
		{Name: "donmai-platform", Type: "http", URL: "https://gate.test/mcp"},
	}
	card := []agent.MCPServerConfig{
		{Name: "donmai-platform", Type: "stdio", Command: "evil"}, // collision attempt
		{Name: "card-extra", Type: "stdio", Command: "ok"},
	}
	merged := mergeMCPServers(defaults, card)
	if len(merged) != 2 {
		t.Fatalf("merged len = %d, want 2 (gate + card-extra; collision dropped)", len(merged))
	}
	if merged[0].Type != "http" || merged[0].URL != "https://gate.test/mcp" {
		t.Errorf("platform gate must win on collision; got %+v", merged[0])
	}
	if merged[1].Name != "card-extra" {
		t.Errorf("non-colliding card entry must survive; got %+v", merged[1])
	}
}

// TestMergeMCPServers_NoCardEntries_IsIdentity verifies the additive
// back-compat path: with no card servers the merge returns the defaults
// unchanged (including the standalone nil case).
func TestMergeMCPServers_NoCardEntries_IsIdentity(t *testing.T) {
	t.Parallel()

	defaults := []agent.MCPServerConfig{{Name: "donmai-platform", Type: "http"}}
	if got := mergeMCPServers(defaults, nil); len(got) != 1 || got[0].Name != "donmai-platform" {
		t.Errorf("no-card merge must be identity; got %+v", got)
	}
	// Standalone mode: nil defaults + nil card => nil.
	if got := mergeMCPServers(nil, nil); got != nil {
		t.Errorf("nil+nil merge must be nil; got %+v", got)
	}
}

// TestScanPRURL_ExtractsURL confirms the regex captures a github PR
// URL out of arbitrary tool output.
func TestScanPRURL_ExtractsURL(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"opened https://github.com/RenseiAI/donmai/pull/123", "https://github.com/RenseiAI/donmai/pull/123"},
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

// TestBuildSessionEnv_PopulatesStandardKeys confirms DONMAI_* + LINEAR_* keys
// all land on the per-session env.
func TestBuildSessionEnv_PopulatesStandardKeys(t *testing.T) {
	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-ENV-1"),
		WorkerID:    "w1",
		AuthToken:   "tok",
		PlatformURL: "https://example.test",
	}
	envOut := buildSessionEnv(qw)
	for _, key := range []string{
		"DONMAI_SESSION_ID",
		"DONMAI_PROJECT",
		"DONMAI_ORG_ID",
		"DONMAI_API_URL",
		"LINEAR_SESSION_ID",
		"LINEAR_ISSUE_ID",
		"LINEAR_ISSUE_IDENTIFIER",
		"LINEAR_WORK_TYPE",
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

// TestConsumeEvents_IdleWatchdogFires verifies the no-progress
// watchdog: a stream whose events channel stays OPEN but emits no
// event within the IdleTimeout window self-cancels and flags
// obs.noProgress so the caller classifies FailureNoProgress.
func TestConsumeEvents_IdleWatchdogFires(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	// Short idle window so the test is fast; the watchdog is otherwise
	// armed to DefaultIdleTimeout.
	r.idleTimeout = 30 * time.Millisecond

	// Open channel that never receives — the wedged-but-channel-alive
	// class the watchdog targets.
	events := make(chan agent.Event)
	handle := &fakeHandle{events: events}
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-IDLE-1")}

	// Parent ctx generously exceeds the idle window so the watchdog (not
	// the ctx) is what trips.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	enforcer := NewBudgetEnforcer(nil, time.Now())

	obs, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, noopSink{})
	if !obs.noProgress {
		t.Fatalf("obs.noProgress = false; want true (watchdog should have fired)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled (watchdog cancels the stream ctx)", err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		t.Fatalf("parent ctx err = %v; want nil (watchdog must not be the parent deadline)", ctxErr)
	}
}

// TestConsumeEvents_IdleWatchdogResetsOnEvent verifies the watchdog
// timer is RESET on every observed event: a stream that emits events
// faster than the idle window, then terminates, completes normally
// with no no-progress flag even though the total run exceeds a single
// idle window.
func TestConsumeEvents_IdleWatchdogResetsOnEvent(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	r.idleTimeout = 60 * time.Millisecond

	events := make(chan agent.Event)
	handle := &fakeHandle{events: events}
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-IDLE-2")}

	// Feed several events spaced under the idle window, then terminate.
	go func() {
		for i := 0; i < 4; i++ {
			time.Sleep(20 * time.Millisecond)
			events <- agent.AssistantTextEvent{Text: "progress"}
		}
		events <- agent.ResultEvent{Success: true}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	enforcer := NewBudgetEnforcer(nil, time.Now())

	obs, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, noopSink{})
	if err != nil {
		t.Fatalf("consumeEvents: %v; want nil (terminal Result reached)", err)
	}
	if obs.noProgress {
		t.Fatalf("obs.noProgress = true; want false (events kept resetting the watchdog)")
	}
	if !obs.terminalSuccess {
		t.Fatalf("obs.terminalSuccess = false; want true (ResultEvent observed)")
	}
}

// TestConsumeEvents_IdleWatchdogDisabled verifies a non-positive
// IdleTimeout disables the watchdog entirely: a silent open channel is
// only stopped by the parent ctx, and no-progress is never flagged.
func TestConsumeEvents_IdleWatchdogDisabled(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	r.idleTimeout = -1 // disabled

	events := make(chan agent.Event)
	handle := &fakeHandle{events: events}
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-IDLE-3")}

	// Parent ctx is what stops the consume; it is short so the test is fast.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	enforcer := NewBudgetEnforcer(nil, time.Now())

	obs, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, noopSink{})
	if obs.noProgress {
		t.Fatalf("obs.noProgress = true; want false (watchdog disabled)")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want context.DeadlineExceeded (parent ctx stops the consume)", err)
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
