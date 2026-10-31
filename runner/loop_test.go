package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/pi"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"

	"github.com/RenseiAI/donmai/runtime/heartbeat"
)

func TestWorktreeProvisionStrategyRepositoryFreeOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		work QueuedWork
		want worktree.CloneStrategy
	}{
		{name: "repository free", work: QueuedWork{}, want: worktree.StrategyEmpty},
		{name: "repository clone", work: QueuedWork{QueuedWork: prompt.QueuedWork{Repository: "https://example.test/repository.git"}}, want: worktree.StrategyClone},
		{name: "versioned declaration", work: QueuedWork{RepositoryDeclaration: &workarea.RepositoryDeclarationV1{}}, want: worktree.StrategyClone},
		{name: "shared participant", work: QueuedWork{WorkareaMode: worktree.ModeShared}, want: worktree.StrategyClone},
		{name: "declaration seed", work: QueuedWork{CacheSeedID: "seed-one"}, want: worktree.StrategyClone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := worktreeProvisionStrategy(test.work); got != test.want {
				t.Fatalf("worktreeProvisionStrategy() = %s, want %s", got, test.want)
			}
		})
	}
}

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
	t.Parallel()
	tests := []struct {
		name string
		// status is the lock-refresh answer on every tick.
		status int
		// wantStatus, when non-empty, must appear in res.Error — the
		// terminal result names the HTTP status that tripped the fuse.
		// Empty keeps the legacy tolerance (lost-ownership or timeout)
		// without asserting on the text.
		wantStatus string
	}{
		{name: "gone keeps legacy tolerance", status: http.StatusGone},
		{name: "auth rejection names the status", status: http.StatusUnauthorized, wantStatus: "last lock-refresh status 401"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testLoopHeartbeatLostOwnership(t, tc.status, tc.wantStatus)
		})
	}
}

// testLoopHeartbeatLostOwnership drives one Run against a platform that
// rejects every lock-refresh with status, so the ownership pulser trips
// and the runner cancels the stream with FailureLostOwnership.
//
// Uses BehaviorHangThenTimeout so the provider does not race the
// heartbeat to a terminal Result.
func testLoopHeartbeatLostOwnership(t *testing.T, status int, wantStatus string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	var refreshes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/lock-refresh") {
			refreshes.Add(1)
			http.Error(w, "lost", status)
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

	if wantStatus != "" {
		// The 401 path is deterministic: every lock-refresh tick answers
		// 401, so the fuse trips on the recorded status and the terminal
		// result must name it while keeping its failure mode.
		if res.FailureMode != FailureLostOwnership {
			t.Fatalf("FailureMode = %q; want %q", res.FailureMode, FailureLostOwnership)
		}
		if !strings.Contains(res.Error, wantStatus) {
			t.Fatalf("res.Error = %q; want it to contain %q", res.Error, wantStatus)
		}
		return
	}
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
// AssistantText scanner reads a line-anchored WORK_RESULT:passed/failed/
// unknown marker (last one wins) and ignores one quoted in prose.
func TestObserveEvent_ScansWorkResultMarker(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"WORK_RESULT:passed", "passed"},
		{"<!-- WORK_RESULT:failed -->", "failed"},
		{"Summary of the work.\n  WORK_RESULT: failed", "failed"},
		{"WORK_RESULT unknown", "unknown"},
		// FIRST anchored marker wins within one message (the platform
		// sentinel reader's rule).
		{"WORK_RESULT:passed\nre-checked\nWORK_RESULT:failed", "passed"},
		// Case-insensitive over ASCII only.
		{"work_result: PASSED", "passed"},
		{"Work_Result: Failed", "failed"},
		{"WORK_RESULT: pa\u017f\u017fed", ""},
		{"WOR\u212a_RESULT: passed", ""},
		// The verdict must end at a word boundary.
		{"WORK_RESULT: passedly", ""},
		{"WORK_RESULT: passed.", "passed"},
		// Line-anchored only (the platform sentinel's rule): a marker quoted
		// in prose, or split across a line break, is not a verdict.
		{"some text WORK_RESULT: passed and more", ""},
		{"final message <!-- WORK_RESULT:passed -->", ""},
		{"I am not claiming WORK_RESULT: passed yet", ""},
		{"WORK_RESULT:\npassed", ""},
		{"WORK_RESULT:blocked", ""},
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
		{"agent-blocked-lowercase", "agent_blocked: no staging credentials", true, "no staging credentials"},
		{"agent-blocked-reason-never-crosses-a-line", "AGENT_BLOCKED:\nnext line is prose", true, ""},
		{"work-result-blocked-never-crosses-a-line", "WORK_RESULT:\nblocked", false, ""},
		{"first-marker-wins-passed-then-blocked", "WORK_RESULT: passed\nAGENT_BLOCKED: second thoughts", false, ""},
		{"first-marker-wins-blocked-then-passed", "AGENT_BLOCKED: need a decision\nWORK_RESULT: passed", true, "need a decision"},
		{"work-result-blocked-then-agent-blocked-keeps-reason", "WORK_RESULT:blocked\nAGENT_BLOCKED: spec is ambiguous", true, "spec is ambiguous"},
		{"agent-blocked-without-colon-is-prose", "AGENT_BLOCKED is how I would decline", false, ""},
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
// blocked observation with no verified PR becomes FailureAgentBlocked; a
// blocked observation with a VERIFIED PR is NOT blocked (the work landed);
// a blocked observation with only an unverified scraped URL is still blocked
// (a hallucinated link never suppresses the fork); a non-blocked observation
// is untouched.
func TestClassifyBlocked_ForksOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		obs           streamObservation
		startPR       string
		hasVerifiedPR bool
		wantBlocked   bool
		wantMode      string
		wantErrSub    string
	}{
		{
			name:          "blocked-no-pr-with-reason",
			obs:           streamObservation{blocked: true, blockedReason: "spec ambiguous"},
			hasVerifiedPR: false,
			wantBlocked:   true,
			wantMode:      FailureAgentBlocked,
			wantErrSub:    "spec ambiguous",
		},
		{
			name:          "blocked-no-pr-no-reason",
			obs:           streamObservation{blocked: true},
			hasVerifiedPR: false,
			wantBlocked:   true,
			wantMode:      FailureAgentBlocked,
			wantErrSub:    "blocked",
		},
		{
			name:          "blocked-but-verified-pr-produced-not-blocked",
			obs:           streamObservation{blocked: true, blockedReason: "x"},
			startPR:       "https://github.com/o/r/pull/1",
			hasVerifiedPR: true,
			wantBlocked:   false,
			wantMode:      "",
		},
		{
			name:          "blocked-with-unverified-scraped-url-still-blocked",
			obs:           streamObservation{blocked: true, blockedReason: "x"},
			startPR:       "https://github.com/o/r/pull/1",
			hasVerifiedPR: false,
			wantBlocked:   true,
			wantMode:      FailureAgentBlocked,
			wantErrSub:    "x",
		},
		{
			name:          "not-blocked-untouched",
			obs:           streamObservation{blocked: false},
			hasVerifiedPR: false,
			wantBlocked:   false,
			wantMode:      "",
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res := &Result{}
			res.PullRequestURL = tt.startPR
			got := classifyBlocked(res, tt.obs, tt.hasVerifiedPR)
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
		obs, t.TempDir(), QueuedWork{}, nil,
	)
	if !obs.blocked {
		t.Fatal("observeEvent did not set obs.blocked")
	}
	if obs.blockedReason != "no acceptance criteria on the issue" {
		t.Errorf("obs.blockedReason = %q; want captured reason", obs.blockedReason)
	}
}

// TestObserveEvent_LaterVerdictClearsEarlierBlocked pins latest-message-wins
// across messages: a later message's anchored passed verdict clears an
// earlier message's decline, and a later decline clears an earlier verdict.
func TestObserveEvent_LaterVerdictClearsEarlierBlocked(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	wt := t.TempDir()

	obs := &streamObservation{}
	for _, text := range []string{"AGENT_BLOCKED: waiting on a decision", "Decision arrived; done.\nWORK_RESULT: passed"} {
		h.runner.observeEvent(agent.AssistantTextEvent{Text: text}, obs, wt, QueuedWork{}, nil)
	}
	if obs.blocked || obs.blockedReason != "" || obs.workResult != "passed" {
		t.Errorf("after blocked then passed: blocked=%v reason=%q workResult=%q; want false, \"\", passed", obs.blocked, obs.blockedReason, obs.workResult)
	}

	obs = &streamObservation{}
	for _, text := range []string{"WORK_RESULT: passed", "Second thoughts.\nAGENT_BLOCKED: the spec contradicts itself"} {
		h.runner.observeEvent(agent.AssistantTextEvent{Text: text}, obs, wt, QueuedWork{}, nil)
	}
	if !obs.blocked || obs.blockedReason != "the spec contradicts itself" || obs.workResult != "" {
		t.Errorf("after passed then blocked: blocked=%v reason=%q workResult=%q; want true, reason, \"\"", obs.blocked, obs.blockedReason, obs.workResult)
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
	for _, text := range []string{"first message", "  \n\t", "final message\n<!-- WORK_RESULT:passed -->"} {
		h.runner.observeEvent(agent.AssistantTextEvent{Text: text}, obs, wt, QueuedWork{}, nil)
	}
	if want := "final message\n<!-- WORK_RESULT:passed -->"; obs.lastAssistantText != want {
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
		h.runner.observeEvent(agent.AssistantTextEvent{Text: text}, obs, wt, QueuedWork{}, nil)
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
// MCP wire-up: when QueuedWork has PlatformURL + session bearer + SessionID,
// defaultMCPServers emits a single HTTP entry pointing at the platform's
// /api/mcp/<sessionId> route with the session-scoped bearer in Authorization.
// The worker bearer is present on the work item (it authenticates heartbeat /
// result-post) and must not appear in the gateway header.
func TestDefaultMCPServers_EmitsHTTPEntryPerSession(t *testing.T) {
	t.Parallel()

	qw := QueuedWork{}
	qw.SessionID = "sess_abc"
	qw.PlatformURL = "https://platform.example.com"
	qw.AuthToken = "rsk_test"
	qw.McpAuthToken = "sess_test"

	servers := defaultMCPServersForHarness(qw, "/abs/wt", mcpDeliveringHarness(), agent.PromptModeAutonomous)
	if len(servers) != 1 {
		t.Fatalf("len(servers)=%d, want 1", len(servers))
	}
	got := servers[0]
	// Name is brand-derived (statehome.Brand()+"-platform"): the OSS default
	// brand "donmai" -> "donmai-platform"; a rebranded build renders its own
	// brand's label. This parallel test uses the default brand.
	if got.Name != "donmai-platform" {
		t.Errorf("name=%q, want donmai-platform", got.Name)
	}
	if got.Type != "http" {
		t.Errorf("type=%q, want http", got.Type)
	}
	if got.URL != "https://platform.example.com/api/mcp/sess_abc" {
		t.Errorf("url=%q", got.URL)
	}
	if got.Headers["Authorization"] != "Bearer sess_test" {
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
	qw.McpAuthToken = "sess_test"

	servers := defaultMCPServersForHarness(qw, "/abs/wt", mcpDeliveringHarness(), agent.PromptModeAutonomous)
	if len(servers) != 1 {
		t.Fatalf("len(servers)=%d, want 1", len(servers))
	}
	if servers[0].URL != "https://platform.example.com/api/mcp/sess_xyz" {
		t.Errorf("url=%q (double-slash leak?)", servers[0].URL)
	}
}

// TestDefaultMCPServers_OmitsWhenStandalone pins the back-compat path:
// in standalone mode (no PlatformURL or no session bearer), no MCP entry is
// emitted at all and the agent runs without the per-session gate. The worker
// bearer alone never mounts a gateway.
func TestDefaultMCPServers_OmitsWhenStandalone(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		qw   QueuedWork
	}{
		{"no PlatformURL", func() QueuedWork {
			qw := QueuedWork{AuthToken: "rsk_test", McpAuthToken: "sess_test"}
			qw.SessionID = "sess_1"
			return qw
		}()},
		{"no session bearer", func() QueuedWork {
			qw := QueuedWork{PlatformURL: "https://platform.example.com", AuthToken: "rsk_test"}
			qw.SessionID = "sess_1"
			return qw
		}()},
		{"no SessionID", QueuedWork{
			PlatformURL:  "https://platform.example.com",
			AuthToken:    "rsk_test",
			McpAuthToken: "sess_test",
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := defaultMCPServersForHarness(tc.qw, "/abs/wt", mcpDeliveringHarness(), agent.PromptModeAutonomous); got != nil {
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
	qw.McpAuthToken = "sess_test"
	qw.McpServers = []agent.MCPServerConfig{
		{Name: "card-linear", Type: "stdio", Command: "pnpm", Args: []string{"af-linear"}},
		{Name: "card-remote", Type: "http", URL: "https://card.test/mcp"},
	}

	merged := mergeMCPServers(defaultMCPServersForHarness(qw, "/abs/wt", mcpDeliveringHarness(), agent.PromptModeAutonomous), qw.McpServers)
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

func TestMergeMCPServers_ManyCardEntriesGrowWithoutCapacitySum(t *testing.T) {
	t.Parallel()
	defaults := []agent.MCPServerConfig{{Name: "default", Command: "default-server"}}
	card := make([]agent.MCPServerConfig, 4096)
	for i := range card {
		card[i] = agent.MCPServerConfig{Name: fmt.Sprintf("card-%04d", i), Command: "card-server"}
	}
	merged := mergeMCPServers(defaults, card)
	if len(merged) != 4097 || merged[0].Name != "default" || merged[len(merged)-1].Name != "card-4095" {
		t.Fatalf("large merge boundaries: len=%d first=%q last=%q", len(merged), merged[0].Name, merged[len(merged)-1].Name)
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

// TestObserveEvent_AssistantNarrativePRURLIsHintOnly pins the provenance
// contract: a pull request URL in assistant free text never becomes the
// session's authoritative URL — it is only a candidate for the remote
// verifier. A hallucinated narrative link must not suppress the blocked
// fork or mark the run complete before verification.
func TestObserveEvent_AssistantNarrativePRURLIsHintOnly(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	obs := &streamObservation{}
	h.runner.observeEvent(
		agent.AssistantTextEvent{Text: "Done! See https://github.com/o/r/pull/99 for the fix."},
		obs, t.TempDir(), QueuedWork{}, nil,
	)
	if obs.pullRequestURL != "" {
		t.Fatalf("assistant narrative set pullRequestURL = %q; want empty (hint only)", obs.pullRequestURL)
	}
	if len(obs.pullRequestCandidates) != 1 || obs.pullRequestCandidates[0] != "https://github.com/o/r/pull/99" {
		t.Fatalf("pullRequestCandidates = %v; want the narrative URL as hint", obs.pullRequestCandidates)
	}
}

// TestObserveEvent_ToolResultPRURLIsTrustedChannel pins that tool output is
// the trusted channel: the first tool-result URL becomes the authoritative
// observation (still subject to remote verification before it may suppress
// the blocked fork). Later tool chatter must not displace it: a `gh pr list`
// of unrelated pull requests after the session's own `gh pr create` leaves
// the session's pull request in place so the verifier still checks it first.
func TestObserveEvent_ToolResultPRURLIsTrustedChannel(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	obs := &streamObservation{}
	wt := t.TempDir()
	h.runner.observeEvent(
		agent.AssistantTextEvent{Text: "Working on it, see https://github.com/o/r/pull/99."},
		obs, wt, QueuedWork{}, nil,
	)
	h.runner.observeEvent(
		agent.ToolResultEvent{ToolName: "bash", ToolUseID: "1", Content: "opened https://github.com/o/r/pull/7"},
		obs, wt, QueuedWork{}, nil,
	)
	if obs.pullRequestURL != "https://github.com/o/r/pull/7" {
		t.Fatalf("pullRequestURL = %q; want the trusted tool-result URL", obs.pullRequestURL)
	}
	// Later tool chatter does not displace the first trusted URL.
	h.runner.observeEvent(
		agent.ToolResultEvent{ToolName: "bash", ToolUseID: "2", Content: "opened https://github.com/o/r/pull/8"},
		obs, wt, QueuedWork{}, nil,
	)
	if obs.pullRequestURL != "https://github.com/o/r/pull/7" {
		t.Fatalf("pullRequestURL = %q; want the first trusted tool-result URL (later chatter must not displace it)", obs.pullRequestURL)
	}
	if len(obs.pullRequestCandidates) != 3 {
		t.Fatalf("pullRequestCandidates = %v; want all three URLs as candidates", obs.pullRequestCandidates)
	}
}

// Verified PRs suppress the blocked fork at both callers (forcing either to false must fail).
func TestRun_VerifiedPRSuppressesBlocked(t *testing.T) {
	initial := runScripted(t, "development", true, "",
		verdictScriptTurn{
			events: []agent.Event{toolUse("pr", "gh pr create --fill"), toolResult("pr", followUpPR)},
			text:   "Stuck.\nAGENT_BLOCKED: need a decision",
		})
	if initial.FailureMode == FailureAgentBlocked || initial.PullRequestURL != followUpPR {
		t.Fatalf("initial: FailureMode=%q PR=%q; want verified exemption with %q", initial.FailureMode, initial.PullRequestURL, followUpPR)
	}
	followUp := runScripted(t, "development", false, "",
		verdictScriptTurn{text: "Work in progress."},
		verdictScriptTurn{
			events: []agent.Event{toolUse("pr", "gh pr create --fill"), toolResult("pr", followUpPR)},
			text:   "Need a decision.\nAGENT_BLOCKED: need a decision",
		},
	)
	if followUp.FailureMode == FailureAgentBlocked || followUp.PullRequestURL != followUpPR {
		t.Fatalf("follow-up: FailureMode=%q PR=%q; want verified exemption with %q", followUp.FailureMode, followUp.PullRequestURL, followUpPR)
	}
}

// A decline never relabels a crash: PR URL plus AGENT_BLOCKED plus ErrorEvent
// stays provider-error, for narrative and tool-result URLs.
func TestRun_ProviderCrashKeepsProviderErrorOverBlocked(t *testing.T) {
	for _, turn := range []verdictScriptTurn{
		{text: "Opened " + followUpPR + "\nAGENT_BLOCKED: need a decision", crash: true},
		{events: []agent.Event{toolUse("pr", "gh pr create --fill"), toolResult("pr", followUpPR)}, text: "AGENT_BLOCKED: need a decision", crash: true},
	} {
		res, _ := runScriptedSession(t, scriptedSession{
			workType: WorkTypeQAStr, repository: followUpRepository,
			pulls: map[int]string{7: pullAtSessionCommit}, turns: []verdictScriptTurn{turn},
		})
		if res.FailureMode != FailureProviderError || !strings.Contains(res.Error, "provider crashed mid turn") {
			t.Fatalf("FailureMode=%q Error=%q; want provider-error with the crash", res.FailureMode, res.Error)
		}
	}
}

// Later chatter cannot displace the session PR: real URL first, then eight `gh pr list` URLs.
func TestRun_LaterToolChatterKeepsSessionPR(t *testing.T) {
	var events []agent.Event
	events = append(events, toolUse("pr", "gh pr create --fill"), toolResult("pr", followUpPR))
	pulls := map[int]string{7: pullAtSessionCommit}
	for n := 101; n <= 108; n++ {
		u := "https://github.com/example/repo/pull/" + fmt.Sprint(n)
		events = append(events, toolUse("list", "gh pr list"), toolResult("list", "see "+u))
		pulls[n] = pullAtOtherCommit
	}
	events = append(events, say("Still need a decision.\nAGENT_BLOCKED: need a decision"))
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "development", repository: followUpRepository, pulls: pulls,
		turns: []verdictScriptTurn{{events: events}},
	})
	if res.PullRequestURL != followUpPR || res.FailureMode == FailureAgentBlocked {
		t.Fatalf("PR=%q FailureMode=%q; want %q without agent-blocked", res.PullRequestURL, res.FailureMode, followUpPR)
	}
}

// An earlier unrelated inject URL must not hide the session pull request a
// later inject reports. Inject 1 reads unrelated PR #99; inject 2 creates
// the session PR #7, then reports eight unrelated PRs and declines. The
// merged observation keeps both, and the batched verifier still finds #7.
// Eight is the old window size: with an eight-entry window the union evicted
// #7 and the envelope kept #99, so settlement lost the session PR.
func TestRun_EarlierUnrelatedInjectCannotHideLaterSessionPR(t *testing.T) {
	platform := newRecordingPlatformServer(t)
	platform.queueInject(heartbeat.InjectPayload{DeliveryID: "dlv-1", Text: "recall: first memory block"})
	platform.queueInject(heartbeat.InjectPayload{DeliveryID: "dlv-2", Text: "recall: second memory block"})
	secondBuffered := func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if acks, _ := platform.injectReports(); slices.Contains(acks, "dlv-2") {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("second inject was never buffered")
	}
	unrelated := "https://github.com/example/repo/pull/99"
	var chatter []agent.Event
	for _, n := range []int{101, 102, 103, 104, 105, 106, 107, 108} {
		u := fmt.Sprintf("https://github.com/example/repo/pull/%d", n)
		chatter = append(chatter, toolUse("list", "gh pr list"), toolResult("list", "see "+u))
	}
	pulls := map[int]string{7: pullAtSessionCommit, 99: pullAtOtherCommit, 101: pullAtOtherCommit, 102: pullAtOtherCommit, 103: pullAtOtherCommit, 104: pullAtOtherCommit, 105: pullAtOtherCommit, 106: pullAtOtherCommit, 107: pullAtOtherCommit, 108: pullAtOtherCommit}
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "development", repository: followUpRepository, pulls: pulls,
		heartbeatInterval: 10 * time.Millisecond, platform: platform,
		turns: []verdictScriptTurn{
			{text: "Working."},
			{before: secondBuffered, events: []agent.Event{toolUse("r", "gh pr view 99"), toolResult("r", "see "+unrelated)}, text: "noted 99"},
			{events: append([]agent.Event{toolUse("pr", "gh pr create --fill"), toolResult("pr", followUpPR)}, chatter...), text: "Stuck.\nAGENT_BLOCKED: need decision"},
		},
	})
	if res.PullRequestURL != followUpPR || res.FailureMode == FailureAgentBlocked {
		t.Fatalf("PR=%q FailureMode=%q; want verified %q without agent-blocked", res.PullRequestURL, res.FailureMode, followUpPR)
	}
}

// A genuine session PR reported in narrative before later tool chatter must
// still verify. Narrative #7 is oldest, seventy unrelated tool URLs follow,
// then the turn declines: the verifier checks every candidate in chunked batches.
// Seventy exceeds the old 64-entry window, which evicted the narrative hint
// and flipped this landed session to agent-blocked.
func TestRun_NarrativeBeforeToolChatterKeepsSessionPR(t *testing.T) {
	var events []agent.Event
	pulls := map[int]string{7: pullAtSessionCommit}
	events = append(events, say("Opened "+followUpPR+" for review."))
	for n := 101; n <= 170; n++ {
		u := fmt.Sprintf("https://github.com/example/repo/pull/%d", n)
		events = append(events, toolUse("list", "gh pr list"), toolResult("list", "see "+u))
		pulls[n] = pullAtOtherCommit
	}
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "development", repository: followUpRepository, pulls: pulls,
		turns: []verdictScriptTurn{{events: events, text: "Still stuck.\nAGENT_BLOCKED: need decision"}},
	})
	if res.PullRequestURL != followUpPR || res.FailureMode == FailureAgentBlocked {
		t.Fatalf("PR=%q FailureMode=%q; want verified %q without agent-blocked", res.PullRequestURL, res.FailureMode, followUpPR)
	}
}

// A flood of forty thousand unrelated URLs must not hide the session pull
// request: the verifier reads refs in ~256-ref chunks sharing one deadline,
// so one over-long ls-remote never marks every candidate unconfirmed. The
// session pull request is oldest, so every chunk is read before it verifies.
// Without chunking the single batched read fails with "argument list too
// long" and the landed session flips to agent-blocked.
func TestRun_FortyThousandCandidatesStillFindSessionPR(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("Opened " + followUpPR + " for review. ")
	for i := 0; i < 40000; i++ {
		fmt.Fprintf(&sb, "https://github.com/example/repo/pull/%d ", 100000+i)
	}
	sb.WriteString("\nStill stuck.\nAGENT_BLOCKED: need decision")
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "development", repository: followUpRepository, pulls: map[int]string{7: pullAtSessionCommit},
		turns: []verdictScriptTurn{{text: sb.String()}},
	})
	if res.PullRequestURL != followUpPR || res.FailureMode == FailureAgentBlocked {
		t.Fatalf("PR=%q FailureMode=%q; want verified %q without agent-blocked", res.PullRequestURL, res.FailureMode, followUpPR)
	}
}

// An unverified tool URL must not suppress the blocked fork on the initial
// turn. QA owes no PR so no verifier is built: the envelope still carries
// the scraped URL, but hasVerifiedPR is false and the decline stays blocked.
func TestRun_UnverifiedToolURLStillBlocked(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType: WorkTypeQAStr, repository: followUpRepository, pulls: map[int]string{7: pullAtOtherCommit},
		turns: []verdictScriptTurn{{events: []agent.Event{toolUse("pr", "gh pr create --fill"), toolResult("pr", followUpPR)}, text: "Stuck.\nAGENT_BLOCKED: need decision"}},
	})
	if res.FailureMode != FailureAgentBlocked {
		t.Fatalf("FailureMode=%q Status=%q PR=%q; want agent-blocked for unverified tool URL", res.FailureMode, res.Status, res.PullRequestURL)
	}
}

// The follow-up blocked fork takes the same verified acceptance: a QA
// follow-up turn that reports an unverified tool URL and declines stays
// blocked. Forcing the second caller to envelope presence would complete it.
func TestRun_UnverifiedToolURLStillBlockedAfterFollowUp(t *testing.T) {
	res2, _ := runScriptedSession(t, scriptedSession{
		workType: WorkTypeQAStr, repository: followUpRepository, pulls: map[int]string{7: pullAtOtherCommit},
		turns: []verdictScriptTurn{
			{text: "Work in progress."},
			{events: []agent.Event{toolUse("pr", "gh pr create --fill"), toolResult("pr", followUpPR)}, text: "Need a decision.\nAGENT_BLOCKED: need decision"},
		},
	})
	if res2.FailureMode != FailureAgentBlocked {
		t.Fatalf("follow-up: FailureMode=%q Status=%q PR=%q; want agent-blocked for unverified tool URL", res2.FailureMode, res2.Status, res2.PullRequestURL)
	}
}

// A pull request number with no ref on the session's remote is refused
// through the production settlement path. The session branch was never
// pushed either, so both refs read absent: without the missing-pull guard
// the two empty strings compare equal and the phantom URL would verify.
func TestRun_MissingPullRequestIsRefused(t *testing.T) {
	missing := "https://github.com/example/repo/pull/404"
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "development", repository: followUpRepository, pulls: map[int]string{},
		turns: []verdictScriptTurn{{events: []agent.Event{toolUse("pr", "gh pr create --fill"), toolResult("pr", missing)}, text: "Opened the pull request."}},
	})
	if res.PullRequestURL != "" {
		t.Fatalf("PullRequestURL = %q; want none (no such pull request on the session's repository)", res.PullRequestURL)
	}
}

// A same-number pull request on another repository never consumes the
// session's batched ref. The foreign URL is reported first so it leads the
// settlement order; the session's own PR follows. Production refuses the
// foreign URL and accepts the session's; without the repository guard the
// foreign URL would verify against the session's own pull head.
func TestRun_ForeignRepositorySharingNumberIsRefused(t *testing.T) {
	foreign := "https://github.com/other/repo/pull/7"
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "development", repository: followUpRepository, pulls: map[int]string{7: pullAtSessionCommit},
		turns: []verdictScriptTurn{{events: []agent.Event{
			toolUse("a", "gh pr view"), toolResult("a", "see "+foreign),
			toolUse("b", "gh pr create --fill"), toolResult("b", "opened "+followUpPR),
		}, text: "Opened the session pull request."}},
	})
	if res.PullRequestURL != followUpPR {
		t.Fatalf("PullRequestURL = %q; want the session's own %q (foreign URL must not consume its ref)", res.PullRequestURL, followUpPR)
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

// TestBuildSessionEnv_NeverExportsGHToken pins the credential-scope contract:
// QueuedWork.AuthToken is the worker's platform runtime bearer (heartbeat,
// result post, session preflight) and is never a GitHub token, so the runner
// must not surface it as GH_TOKEN on ANY work item — ref-bearing or not,
// headless or interactive. Doing so clobbers the seat's real gh auth (the
// composer lets Spec.Env win over the host env) and sends the platform bearer
// to api.github.com on every gh call.
func TestBuildSessionEnv_NeverExportsGHToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mode string
		ref  string
	}{
		{name: "headless without ref", mode: "", ref: ""},
		{name: "headless ref-bearing", mode: "", ref: "fix/existing-branch"},
		{name: "interactive without ref", mode: interactiveRunMode, ref: ""},
		{name: "interactive ref-bearing", mode: interactiveRunMode, ref: "fix/existing-branch"},
		{name: "interactive ref-bearing with whitespace", mode: interactiveRunMode, ref: "  fix/existing-branch  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := queuedWorkBase("ENV-GH-1")
			base.Mode = tc.mode
			base.Ref = tc.ref
			qw := QueuedWork{
				QueuedWork:  base,
				WorkerID:    "w1",
				AuthToken:   "platform-runtime-jwt",
				PlatformURL: "https://example.test",
			}
			envOut := buildSessionEnv(qw)
			if v, ok := envOut["GH_TOKEN"]; ok {
				t.Fatalf("GH_TOKEN exported from the runner layer (value present=%t); the platform bearer must never be presented to gh", v != "")
			}
			if envOut["WORKER_AUTH_TOKEN"] != "platform-runtime-jwt" {
				t.Fatalf("WORKER_AUTH_TOKEN = %q; the runtime bearer must still reach the worker env", envOut["WORKER_AUTH_TOKEN"])
			}
		})
	}
}

// TestSessionEnv_ComposerPreservesHostGHToken is the composer-level pin: the
// per-session Spec.Env the runner builds must leave the host's own GH_TOKEN
// (a seat's real gh auth, or a provisioner-stamped short-lived git token in a
// sandbox) intact after composition. Spec.Env wins over the base layer, so
// the only way this holds is for the runner never to put GH_TOKEN there.
func TestSessionEnv_ComposerPreservesHostGHToken(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mode string
		ref  string
	}{
		{name: "interactive ref-bearing", mode: interactiveRunMode, ref: "feature/pinned"},
		{name: "headless ref-bearing", mode: "", ref: "feature/pinned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := queuedWorkBase("ENV-GH-2")
			base.Mode = tc.mode
			base.Ref = tc.ref
			qw := QueuedWork{
				QueuedWork:  base,
				WorkerID:    "w1",
				AuthToken:   "platform-runtime-jwt",
				PlatformURL: "https://example.test",
			}
			host := map[string]string{"GH_TOKEN": "host-real-gh-token", "PATH": "/usr/bin"}
			composed := envToMap(runtimeenv.NewComposer().Compose(host, agent.Spec{Env: buildSessionEnv(qw)}))
			if got := composed["GH_TOKEN"]; got != "host-real-gh-token" {
				t.Fatalf("composed GH_TOKEN = %q; want the host value preserved", got)
			}
			if composed["WORKER_AUTH_TOKEN"] != "platform-runtime-jwt" {
				t.Fatalf("composed WORKER_AUTH_TOKEN = %q; want the runtime bearer", composed["WORKER_AUTH_TOKEN"])
			}
		})
	}
}

// TestBuildSessionEnv_ExportsGoModuleCache pins the module-cache export:
// the seat env carries GOMODCACHE resolved from the host toolchain's own
// `go env` probe, so seats find dependency sources without a whole-disk
// search. The probe is stubbed through the package seam (no toolchain
// needed); reverting the export line leaves the key absent and fails it.
func TestBuildSessionEnv_ExportsGoModuleCache(t *testing.T) {
	orig := goModCache
	goModCache = func(context.Context) string { return "/host/go/pkg/mod" }
	t.Cleanup(func() { goModCache = orig })

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-MODCACHE-1"),
		WorkerID:    "w1",
		AuthToken:   "tok",
		PlatformURL: "https://example.test",
	}
	if got := buildSessionEnv(qw)["GOMODCACHE"]; got != "/host/go/pkg/mod" {
		t.Fatalf("GOMODCACHE = %q; want the probed host module cache", got)
	}
}

// TestBuildSessionEnv_OmitsGoModuleCacheWhenProbeFails pins the additive
// contract: a toolchain the probe cannot read (no `go` on PATH, a wedged
// probe) omits the key instead of exporting an empty or stale pointer, so
// the seat builds exactly as before.
func TestBuildSessionEnv_OmitsGoModuleCacheWhenProbeFails(t *testing.T) {
	orig := goModCache
	goModCache = func(context.Context) string { return "" }
	t.Cleanup(func() { goModCache = orig })

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-MODCACHE-2"),
		WorkerID:    "w1",
		AuthToken:   "tok",
		PlatformURL: "https://example.test",
	}
	if _, ok := buildSessionEnv(qw)["GOMODCACHE"]; ok {
		t.Fatal("GOMODCACHE is set; a failed probe must omit the key")
	}
}

// TestRun_RecordsPreparationSeconds drives the PRODUCTION Run path with a
// clock that reads the base instant at Run entry and +90s at the pre-spawn
// stamp, and asserts the duration lands on the terminal result. The two
// reads bracket the preparation phase (provider resolution, worktree
// provisioning, dependency installs, prompt assembly); reverting the stamp
// line leaves the field zero and fails it.
func TestRun_RecordsPreparationSeconds(t *testing.T) {
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	reads := 0
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		reads++
		if reads == 1 {
			return base
		}
		return base.Add(90 * time.Second)
	}
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "development",
		now:      now,
		turns: []verdictScriptTurn{
			{text: "Opened " + followUpPR + "\nWORK_RESULT: passed"},
		},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	if res.PreparationSeconds != 90 {
		t.Fatalf("PreparationSeconds = %d; want 90", res.PreparationSeconds)
	}
}

// TestPreparationSeconds pins the preparation-duration helper: session
// start to provider spawn in whole seconds, with a skewed or frozen test
// clock omitting the field instead of posting a duration the phase did
// not take.
func TestPreparationSeconds(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		want int64
	}{
		{name: "ninety seconds", now: start.Add(90 * time.Second), want: 90},
		{name: "truncates sub-second", now: start.Add(1500 * time.Millisecond), want: 1},
		{name: "frozen clock omits", now: start, want: 0},
		{name: "skewed clock omits", now: start.Add(-time.Second), want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := preparationSeconds(func() time.Time { return tc.now }, start); got != tc.want {
				t.Fatalf("preparationSeconds = %d; want %d", got, tc.want)
			}
		})
	}
	if got := preparationSeconds(nil, start); got != 0 {
		t.Fatalf("preparationSeconds(nil clock) = %d; want 0", got)
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
	if _, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, rec, nil, nil); err != nil {
		t.Fatalf("consumeEvents: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if got := len(rec.events); got != 4 {
		t.Fatalf("recorded %d events; want 4 (got=%v)", got, kindsOf(rec.events))
	}
}

func TestConsumeEvents_PersistsSyntheticCorrelatedEvents(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	rec := &recordingSink{}
	processor := &recordingSpanProcessor{}
	events := make(chan agent.Event, 1)
	events <- agent.ResultEvent{Success: true, Cost: &agent.CostData{InputTokens: 20, OutputTokens: 4}}
	close(events)

	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("TEST-SPAN-EVENT-1")}
	enforcer := NewBudgetEnforcer(nil, time.Now())
	if _, err := r.consumeEvents(t.Context(), &fakeHandle{events: events}, wpath, qw, nil, enforcer, rec, processor, nil); err != nil {
		t.Fatalf("consumeEvents: %v", err)
	}

	rec.mu.Lock()
	gotEvents := append([]agent.Event(nil), rec.events...)
	rec.mu.Unlock()
	if len(gotEvents) != 2 || gotEvents[0].Kind() != agent.EventLlmCall || gotEvents[1].Kind() != agent.EventResult {
		t.Fatalf("sink events = %v, want [llm_call result]", kindsOf(gotEvents))
	}

	body, err := os.ReadFile(filepath.Join(wpath, ".agent", "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("events.jsonl rows = %d, want 2", len(lines))
	}
	first, err := agent.UnmarshalEvent([]byte(lines[0]))
	if err != nil {
		t.Fatalf("decode first row: %v", err)
	}
	llm := first.(agent.LlmCallEvent)
	if !llm.Synthetic || llm.UsageSource != agent.LlmUsageAggregate || llm.InputTokens != 20 || llm.OutputTokens != 4 {
		t.Fatalf("synthetic event lost provenance/usage: %+v", llm)
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

	obs, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, noopSink{}, nil, nil)
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
// timer is RESET on every observed event: a stream that keeps emitting
// events, then terminates, completes normally with no no-progress flag
// even though the total run exceeds a single idle window.
//
// Event delivery is sequenced through the manual clock's reset-count
// handshake — each event is sent only after the consumer re-armed for
// the previous one — so no sleep-versus-timeout ratio remains to lose
// on a loaded machine.
func TestConsumeEvents_IdleWatchdogResetsOnEvent(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	r.idleTimeout = time.Minute
	clk := newIdleManualClock()
	r.idleClock = clk

	events := make(chan agent.Event)
	handle := &fakeHandle{events: events}
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-IDLE-2")}

	type consumeResult struct {
		obs streamObservation
		err error
	}
	done := make(chan consumeResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		enforcer := NewBudgetEnforcer(nil, time.Now())
		obs, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, noopSink{}, nil, nil)
		done <- consumeResult{obs: obs, err: err}
	}()

	// Feed several events, each only after the consumer re-armed for
	// the previous one, then terminate. A stale-window fire can never
	// slip between them because expiry only happens on clk.fire().
	for i := 1; i <= 4; i++ {
		events <- agent.AssistantTextEvent{Text: "progress"}
		clk.waitForIdleResets(t, i)
	}
	events <- agent.ResultEvent{Success: true}

	var res consumeResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("consumeEvents did not return after the terminal event")
	}
	if res.err != nil {
		t.Fatalf("consumeEvents: %v; want nil (terminal Result reached)", res.err)
	}
	if res.obs.noProgress {
		t.Fatalf("obs.noProgress = true; want false (events kept resetting the watchdog)")
	}
	if !res.obs.terminalSuccess {
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

	obs, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, noopSink{}, nil, nil)
	if obs.noProgress {
		t.Fatalf("obs.noProgress = true; want false (watchdog disabled)")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want context.DeadlineExceeded (parent ctx stops the consume)", err)
	}
}

// TestConsumeEvents_IdleWatchdogRearmsWhileToolCallInFlight verifies an
// in-flight tool call counts as progress: a ToolUseEvent with no matching
// result yet re-arms the watchdog on expiry instead of ending the session.
// A 12-minute shell search (the shape that ended a healthy session) stays
// alive while the tool runs; the tool's own bounded timeout still ends the
// CALL with an error the agent sees.
//
// The expiry is driven by a manual clock, not by sleeping past a
// wall-clock window: each fire() models one silent idle window elapsing,
// and the reset-count handshake proves the consumer re-armed before the
// test proceeds. No sleep-versus-timeout ratio remains to lose on a loaded
// machine.
func TestConsumeEvents_IdleWatchdogRearmsWhileToolCallInFlight(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	r.idleTimeout = time.Minute
	clk := newIdleManualClock()
	r.idleClock = clk

	events := make(chan agent.Event, 4)
	handle := &fakeHandle{events: events}
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-IDLE-TOOL-1")}

	type consumeResult struct {
		obs streamObservation
		err error
	}
	done := make(chan consumeResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		enforcer := NewBudgetEnforcer(nil, time.Now())
		obs, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, noopSink{}, nil, nil)
		done <- consumeResult{obs: obs, err: err}
	}()

	// The ToolUseEvent is observed synchronously by the consumer before
	// the first reset, so waiting for reset 1 proves the consumer saw
	// the call start.
	events <- agent.ToolUseEvent{ToolName: "bash", ToolUseID: "call-1"}
	clk.waitForIdleResets(t, 1)

	// Two silent idle windows elapse while the call runs; each must
	// re-arm (reset count grows) instead of ending the session.
	clk.fire()
	clk.waitForIdleResets(t, 2)
	clk.fire()
	clk.waitForIdleResets(t, 3)
	select {
	case res := <-done:
		t.Fatalf("consumeEvents returned early: err = %v, noProgress = %v; want the running call to keep the stream alive", res.err, res.obs.noProgress)
	default:
	}

	// The bounded tool timeout surfaces as a result and the turn ends.
	events <- agent.ToolResultEvent{ToolName: "bash", ToolUseID: "call-1", Content: "Command timed out after 300 seconds", IsError: true}
	events <- agent.ResultEvent{Success: true}

	var res consumeResult
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("consumeEvents did not return after the tool result and terminal event")
	}
	if res.err != nil {
		t.Fatalf("consumeEvents: %v; want nil (in-flight tool must suppress the watchdog)", res.err)
	}
	if res.obs.noProgress {
		t.Fatalf("obs.noProgress = true; want false (in-flight tool call is progress)")
	}
	if !res.obs.terminalSuccess {
		t.Fatalf("obs.terminalSuccess = false; want true (turn completed after the tool result)")
	}
	if res.obs.toolCalls != 1 {
		t.Fatalf("obs.toolCalls = %d; want 1", res.obs.toolCalls)
	}
}

// TestConsumeEvents_IdleWatchdogFiresAfterToolResult verifies the re-arm
// ends when the call does: once the matching ToolResultEvent lands, a
// further silent window fires the watchdog again.
func TestConsumeEvents_IdleWatchdogFiresAfterToolResult(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	r.idleTimeout = 30 * time.Millisecond

	events := make(chan agent.Event, 2)
	handle := &fakeHandle{events: events}
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-IDLE-TOOL-2")}

	events <- agent.ToolUseEvent{ToolName: "bash", ToolUseID: "call-2"}
	events <- agent.ToolResultEvent{ToolName: "bash", ToolUseID: "call-2", Content: "done"}
	// Then silence: no terminal event follows.

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	enforcer := NewBudgetEnforcer(nil, time.Now())

	obs, err := r.consumeEvents(ctx, handle, wpath, qw, nil, enforcer, noopSink{}, nil, nil)
	if !obs.noProgress {
		t.Fatalf("obs.noProgress = false; want true (completed call must not suppress the watchdog forever)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
}

// recordingSink is a test-only activitySink implementation that
// captures every Send call for later assertion.
type recordingSink struct {
	mu     sync.Mutex
	events []agent.Event
}

type recordingSpanProcessor struct{}

func (*recordingSpanProcessor) Process(ev agent.Event) []agent.Event {
	if result, ok := ev.(agent.ResultEvent); ok {
		llm := agent.LlmCallEvent{Synthetic: true, UsageSource: agent.LlmUsageAggregate}
		if result.Cost != nil {
			llm.InputTokens = result.Cost.InputTokens
			llm.OutputTokens = result.Cost.OutputTokens
		}
		return []agent.Event{llm, result}
	}
	return []agent.Event{ev}
}

func (*recordingSpanProcessor) Finish(string, string) {}

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

// exactHarnessStubProvider drives the real stub event loop while declaring
// ANOTHER harness's exact manifest and capabilities, and runs the same
// agent.PrepareHarness gate every real harness runs in Spawn (e.g.
// provider/harness/pi/pi.go). A Spec the runner over-injected therefore denies
// here exactly as it does in production, before any agent starts.
type exactHarnessStubProvider struct {
	agent.Provider
	name     agent.ProviderName
	caps     agent.Capabilities
	manifest agent.HarnessManifest
}

func (p *exactHarnessStubProvider) Name() agent.ProviderName         { return p.name }
func (p *exactHarnessStubProvider) Capabilities() agent.Capabilities { return p.caps }
func (p *exactHarnessStubProvider) Manifest() agent.HarnessManifest  { return p.manifest }

func (p *exactHarnessStubProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	prepared, err := agent.PrepareHarness(spec, p.manifest)
	if err != nil {
		return nil, err
	}
	return p.Provider.Spawn(ctx, prepared)
}

// TestLoop_SpawnsOnHarnessDeclaringNoMCPDelivery is the end-to-end guard for
// the loop's own MCP composition (loop.go step 4). The runner injects the
// per-session platform MCP gateway on the caller's behalf whenever platform
// credentials are present; on a harness whose tool/lifecycle profile declares
// no MCP delivery that injection used to reach the exact adapter and deny the
// whole spawn ("cannot apply required entry \"mcp-servers\"") for a capability
// the session never requested. The session must run instead.
func TestLoop_SpawnsOnHarnessDeclaringNoMCPDelivery(t *testing.T) {
	h := newRunnerHarness(t)

	inner, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	provider := &exactHarnessStubProvider{
		Provider: inner,
		name:     agent.ProviderPi,
		caps:     (&pi.Provider{}).Capabilities(),
		manifest: (&pi.Provider{}).Manifest(),
	}
	if profile, ok := provider.manifest.ToolLifecycleProfile(agent.PromptModeAutonomous); !ok ||
		profile.MCPDelivery != agent.ToolDeliveryUnsupported {
		t.Fatalf("fixture harness must declare no MCP delivery; got profile %+v (found=%v)", profile, ok)
	}
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}

	qw := h.queuedWork("LOOP-NO-MCP-DELIVERY")
	qw.ResolvedProfile.Provider = agent.ProviderPi
	if qw.PlatformURL == "" || qw.AuthToken == "" {
		t.Fatal("fixture must be a platform-connected session; that is what triggers the injection")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v (failureMode=%q error=%q)", err, res.FailureMode, res.Error)
	}
	if res.Status != "completed" {
		t.Fatalf("Run status = %q (failureMode=%q error=%q), want completed",
			res.Status, res.FailureMode, res.Error)
	}
}

// TestRunLoop_HeartbeatTicksDuringProvisioning is the slow-provision
// liveness regression: provisioning is held open past several shortened
// heartbeat intervals, and the run must post lock-refreshes while the
// clone is still in flight. Before the fix the pulser started only after
// spawn, so a session stuck cloning for longer than the platform's stale
// window was reclaimed as stale; moving the pulse start back after
// provisioning (revert-RED) leaves zero refreshes before Run returns.
func TestRunLoop_HeartbeatTicksDuringProvisioning(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	var refreshes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/lock-refresh") {
			refreshes.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	// Hold provisioning open well past several 20ms heartbeat intervals.
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent, PreProvisionDelay: 500 * time.Millisecond})
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
		HeartbeatInterval: 20 * time.Millisecond,
		SkipBackstop:      true,
		SkipSteering:      true,
		SkipPostSession:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("LOOP-HB-PROV"),
		WorkerID:    "w1",
		AuthToken:   "tok",
		PlatformURL: srv.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}
	qw.Repository = bareRepo

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (FailureMode=%q, Error=%q)",
			res.Status, res.FailureMode, res.Error)
	}
	// The 500ms provision hold spans ~25 ticks at a 20ms cadence; the
	// synchronous first tick alone proves the pulse starts before
	// provisioning, but require several so a single stray tick cannot
	// pass a build where the pulse starts after the hold.
	if got := refreshes.Load(); got < 5 {
		t.Fatalf("lock-refresh posts during provisioning = %d; want >= 5 (pulse must start before provisioning)", got)
	}
}

// TestObserveEvent_LlmCallServingReport pins the per-call serving report:
// a native model call records the provider the wire response itself
// identified (ResponseModelProvider), its served model (ResponseModel,
// else Model) and its attempts onto the observation, mirrors them onto
// the session record, and folds provider/model onto the terminal envelope.
// System is the configured selection — never response evidence — so a
// gateway-routed codex call (System "openai") or a span-fallback
// configured system never yields a serving provider. Synthetic aggregate
// fallbacks carry no serving identity and never clobber a native report;
// an unreported (zero) attempt count adds nothing, so an all-unknown
// stream totals zero (unknown).
func TestObserveEvent_LlmCallServingReport(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	wt := t.TempDir()
	obs := &streamObservation{}

	native := func(provider, system, model, responseModel string, attempts int) agent.LlmCallEvent {
		return agent.LlmCallEvent{
			System: system, Model: model, ResponseModel: responseModel,
			ResponseModelProvider: provider,
			InputTokens:           5, OutputTokens: 3,
			UsageSource: agent.LlmUsageProvider, TurnCompleted: true, Attempts: attempts,
		}
	}
	// A gateway-routed call whose wire response identified its provider.
	h.runner.observeEvent(native("served-provider", "donmai", "requested-model", "served-model-id", 3), obs, wt, QueuedWork{}, nil)
	if obs.finalProvider != "served-provider" || obs.finalModel != "served-model-id" || obs.providerAttempts != 3 {
		t.Fatalf("obs = %+v; want finalProvider served-provider, finalModel served-model-id, providerAttempts 3", obs)
	}

	// A later call that identifies a different response provider replaces
	// the earlier fact (last wins); a call without a served id falls back
	// to the requested id.
	h.runner.observeEvent(native("other-provider", "anthropic", "requested-alias", "", 1), obs, wt, QueuedWork{}, nil)
	if obs.finalProvider != "other-provider" || obs.finalModel != "requested-alias" || obs.providerAttempts != 4 {
		t.Fatalf("obs = %+v; want other-provider/requested-alias/4 after the second call", obs)
	}

	// A synthetic aggregate fallback with a DISTINCT configured identity
	// (the span processor's terminal-cost synthesis carries cfg
	// System/Model) changes nothing: without the skip guard it would
	// overwrite the served report with the requested id.
	h.runner.observeEvent(agent.LlmCallEvent{
		System: "configured-system", Model: "configured-model",
		ResponseModelProvider: "configured-provider",
		InputTokens:           100, UsageSource: agent.LlmUsageAggregate, Synthetic: true,
	}, obs, wt, QueuedWork{}, nil)
	if obs.finalProvider != "other-provider" || obs.finalModel != "requested-alias" || obs.providerAttempts != 4 {
		t.Fatalf("obs = %+v; want the synthetic fallback to leave the report untouched", obs)
	}

	// A gateway-routed codex call carries the harness constant as System
	// ("openai") with no response-provider evidence: it must not yield a
	// serving provider. Same for a span-fallback configured system with no
	// response identity.
	h.runner.observeEvent(agent.LlmCallEvent{
		System: "openai", Model: "requested-model",
		InputTokens: 5, OutputTokens: 3,
		UsageSource: agent.LlmUsageProvider, TurnCompleted: true, Attempts: 1,
	}, obs, wt, QueuedWork{}, nil)
	if obs.finalProvider != "other-provider" || obs.providerAttempts != 5 {
		t.Fatalf("obs = %+v; want a System-only gateway call to leave finalProvider untouched while counting its attempt", obs)
	}
	// The System-only call DID update the served model to its requested
	// id (last-non-empty-wins on the model axis); restore the pinned
	// report for the assertions below.
	if obs.finalModel != "requested-model" {
		t.Fatalf("obs.finalModel = %q; want requested-model (System-only call falls back to Model)", obs.finalModel)
	}
	h.runner.observeEvent(native("other-provider", "anthropic", "requested-alias", "", 1), obs, wt, QueuedWork{}, nil)
	if obs.finalProvider != "other-provider" || obs.finalModel != "requested-alias" || obs.providerAttempts != 6 {
		t.Fatalf("obs = %+v; want other-provider/requested-alias/6 after restoring", obs)
	}

	// An empty serving identity never clobbers a recorded one, and an
	// unreported attempt count adds nothing.
	h.runner.observeEvent(agent.LlmCallEvent{
		InputTokens: 1, UsageSource: agent.LlmUsageProvider, TurnCompleted: true,
	}, obs, wt, QueuedWork{}, nil)
	if obs.finalProvider != "other-provider" || obs.finalModel != "requested-alias" || obs.providerAttempts != 6 {
		t.Fatalf("obs = %+v; want an empty call to leave the report untouched", obs)
	}

	res := &Result{}
	obs.applyTo(res, agent.ProviderStub)
	if res.FinalProvider != "other-provider" || res.FinalModel != "requested-alias" {
		t.Errorf("envelope serving report = %q/%q; want other-provider/requested-alias (last wins)",
			res.FinalProvider, res.FinalModel)
	}
	if res.ProviderAttempts != 0 {
		t.Errorf("res.ProviderAttempts = %d; want 0 (attempt totals ride the per-stream accumulation, not applyTo)", res.ProviderAttempts)
	}

	// applyTo's non-empty guards are pinned: a later stream with no call
	// (an injected turn, a tail recovery) keeps the earlier report instead
	// of wiping it.
	empty := &streamObservation{}
	empty.applyTo(res, agent.ProviderStub)
	if res.FinalProvider != "other-provider" || res.FinalModel != "requested-alias" {
		t.Errorf("envelope after empty applyTo = %q/%q; want the earlier report kept", res.FinalProvider, res.FinalModel)
	}

	// The session record mirrors the observed report next to the admitted
	// binding axes, which stay as dispatched (empty here).
	st, err := h.runner.store.Read(wt)
	if err != nil {
		t.Fatalf("store.Read: %v", err)
	}
	if st.FinalProvider != "other-provider" || st.FinalModel != "requested-alias" || st.ProviderAttempts != 6 {
		t.Errorf("state serving report = %+v; want other-provider/requested-alias/6", st)
	}
	if st.ModelProvider != "" || st.ModelAuthor != "" || st.EndpointOperator != "" || st.Protocol != "" {
		t.Errorf("admitted axes mutated by the serving mirror: %+v", st)
	}
}

// TestConsumeEvents_ServingAttemptsAccumulate pins the only line that
// delivers serving attempts to the wire: consumeEvents meters each
// stream's attempt sum onto the envelope via its per-stream defer, across
// streams (an injected turn counts exactly once alongside the first turn).
func TestConsumeEvents_ServingAttemptsAccumulate(t *testing.T) {
	t.Parallel()
	r := minimalRunner(t)
	wpath := t.TempDir()
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-SERVING-ATTEMPTS")}
	res := &Result{}

	call := func(provider string, attempts int) agent.LlmCallEvent {
		return agent.LlmCallEvent{
			ResponseModelProvider: provider, Model: "requested-model",
			InputTokens: 5, OutputTokens: 3,
			UsageSource: agent.LlmUsageProvider, TurnCompleted: true, Attempts: attempts,
		}
	}
	drain := func(events ...agent.Event) streamObservation {
		ch := make(chan agent.Event, len(events)+1)
		for _, ev := range events {
			ch <- ev
		}
		ch <- agent.ResultEvent{Success: true}
		close(ch)
		enforcer := NewBudgetEnforcer(nil, time.Now())
		obs, err := r.consumeEvents(context.Background(), &fakeHandle{events: ch}, wpath, qw, res, enforcer, noopSink{}, nil, nil)
		if err != nil {
			t.Fatalf("consumeEvents: %v", err)
		}
		return obs
	}

	// Stream one: two native calls (3 + 1 attempts).
	obs1 := drain(call("served-provider", 3), call("served-provider", 1))
	if obs1.providerAttempts != 4 {
		t.Fatalf("stream one attempts = %d; want 4", obs1.providerAttempts)
	}
	if res.ProviderAttempts != 4 {
		t.Fatalf("res.ProviderAttempts after stream one = %d; want 4 (per-stream defer)", res.ProviderAttempts)
	}
	obs1.applyTo(res, agent.ProviderStub)
	if res.FinalProvider != "served-provider" {
		t.Fatalf("res.FinalProvider = %q; want served-provider after applyTo", res.FinalProvider)
	}

	// Stream two (an injected turn): one more call with two attempts.
	obs2 := drain(call("served-provider", 2))
	if obs2.providerAttempts != 2 {
		t.Fatalf("stream two attempts = %d; want 2", obs2.providerAttempts)
	}
	if res.ProviderAttempts != 6 {
		t.Fatalf("res.ProviderAttempts after stream two = %d; want 6 (3+1+2 across streams)", res.ProviderAttempts)
	}
	obs2.applyTo(res, agent.ProviderStub)
	if res.FinalProvider != "served-provider" || res.ProviderAttempts != 6 {
		t.Fatalf("envelope = %q/%d; want served-provider/6", res.FinalProvider, res.ProviderAttempts)
	}

	// The session record accumulated the same session total by delta.
	st, err := r.store.Read(wpath)
	if err != nil {
		t.Fatalf("store.Read: %v", err)
	}
	if st.ProviderAttempts != 6 {
		t.Errorf("state ProviderAttempts = %d; want 6 (session total across streams)", st.ProviderAttempts)
	}
	if st.FinalProvider != "served-provider" {
		t.Errorf("state FinalProvider = %q; want served-provider", st.FinalProvider)
	}

	// Stream three: a fresh stream whose first call carries no
	// response-provider evidence (a gateway harness constant as System).
	// The mirror keeps the earlier provider instead of clearing it,
	// while still counting the call's attempt toward the session total.
	obs3 := drain(agent.LlmCallEvent{
		System: "openai", Model: "requested-model",
		InputTokens: 5, OutputTokens: 3,
		UsageSource: agent.LlmUsageProvider, TurnCompleted: true, Attempts: 1,
	})
	if obs3.finalProvider != "" {
		t.Fatalf("stream three provider = %q; want empty (System alone is never evidence)", obs3.finalProvider)
	}
	if res.ProviderAttempts != 7 {
		t.Fatalf("res.ProviderAttempts after stream three = %d; want 7", res.ProviderAttempts)
	}
	obs3.applyTo(res, agent.ProviderStub)
	if res.FinalProvider != "served-provider" {
		t.Fatalf("envelope provider after empty stream = %q; want served-provider kept", res.FinalProvider)
	}
	st, err = r.store.Read(wpath)
	if err != nil {
		t.Fatalf("store.Read: %v", err)
	}
	if st.FinalProvider != "served-provider" || st.ProviderAttempts != 7 {
		t.Errorf("state after empty stream = %+v; want provider kept and attempts 7", st)
	}
}

// spawnCountingStubProvider wraps the stub harness and counts Spawn calls so
// a test can prove the provider never started.
type spawnCountingStubProvider struct {
	agent.Provider
	spawns *atomic.Int64
}

func (p *spawnCountingStubProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.spawns.Add(1)
	return p.Provider.Spawn(ctx, spec)
}

// refusingExecutionSecurityReporter answers every pre-spawn report
// submission with a refusal naming the below-stamp dimensions.
type refusingExecutionSecurityReporter struct {
	submissions *atomic.Int64
}

func (r *refusingExecutionSecurityReporter) SubmitExecutionSecurityReport(_ context.Context, _ ExecutionSecurityReportSubmission) (ExecutionSecurityReportAcceptance, error) {
	r.submissions.Add(1)
	return ExecutionSecurityReportAcceptance{
		ContractVersion: ExecutionSecurityReportContractVersion,
		Decision:        "refused",
		Code:            "report_below_stamp",
		Dimensions:      []agent.ExecutionSecurityDimension{agent.ExecutionSecurityNetwork},
	}, nil
}

// acceptingExecutionSecurityReporter answers every pre-spawn report
// submission with an acceptance.
type acceptingExecutionSecurityReporter struct {
	submissions *atomic.Int64
}

func (r *acceptingExecutionSecurityReporter) SubmitExecutionSecurityReport(_ context.Context, _ ExecutionSecurityReportSubmission) (ExecutionSecurityReportAcceptance, error) {
	r.submissions.Add(1)
	return ExecutionSecurityReportAcceptance{
		ContractVersion: ExecutionSecurityReportContractVersion,
		Decision:        "accepted",
	}, nil
}

// TestLoop_ExecutionSecurityReportWaitsForAcceptance is the revert-RED guard
// for the pre-spawn report gate (loop step 6c): a stamped run whose dispatch
// target advertises the report contract submits what it will enforce through
// the preflight registration call before spawn. A refused submission ends
// with a typed refusal and zero spawns; an accepted one spawns as before.
// Skipping the wait — posting without reading the acceptance, or not posting
// at all — turns the refused case red because the provider starts.
func TestLoop_ExecutionSecurityReportWaitsForAcceptance(t *testing.T) {
	stamped := func(h *runnerHarness, identifier string) QueuedWork {
		qw := h.queuedWork(identifier)
		qw.ExecutionSecurity = executionSecuritySection(nil)
		qw.Capabilities = map[string]bool{CapabilityExecutionSecurityReportIngest: true}
		return qw
	}

	t.Run("refused report means no spawn and a typed refusal", func(t *testing.T) {
		h := newRunnerHarness(t)
		var spawns, submissions atomic.Int64
		inner, err := stub.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		if err := reg.Register(&spawnCountingStubProvider{Provider: inner, spawns: &spawns}); err != nil {
			t.Fatal(err)
		}
		h.runner.registry = reg
		h.runner.executionSecurityReporterOverride = &refusingExecutionSecurityReporter{submissions: &submissions}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := h.runner.Run(ctx, stamped(h, "LOOP-REPORT-REFUSED"))
		if agent.ExecutionSecurityErrorCode(err) != agent.ExecutionSecurityReceiptUnmet {
			t.Fatalf("err = %v, want execution_security_receipt_unmet", err)
		}
		if res.Status != "failed" || res.FailureMode != FailureExecutionSecurity {
			t.Fatalf("result status=%q mode=%q error=%q, want failed/execution-security", res.Status, res.FailureMode, res.Error)
		}
		if res.ExecutionSecurityRefusal == nil || res.ExecutionSecurityRefusal.Code != agent.ExecutionSecurityReceiptUnmet {
			t.Fatalf("typed refusal = %+v, want execution_security_receipt_unmet", res.ExecutionSecurityRefusal)
		}
		if got := len(res.ExecutionSecurityRefusal.Dimensions); got != 1 || res.ExecutionSecurityRefusal.Dimensions[0] != agent.ExecutionSecurityNetwork {
			t.Fatalf("refusal dimensions = %+v, want the refused network dimension", res.ExecutionSecurityRefusal.Dimensions)
		}
		if submissions.Load() != 1 {
			t.Fatalf("submissions = %d, want exactly one pre-spawn post", submissions.Load())
		}
		if spawns.Load() != 0 {
			t.Fatalf("spawns = %d, want zero: a refused report must not start the agent", spawns.Load())
		}
		if res.ProviderSessionID != "" {
			t.Fatalf("provider session %q started after a refused report", res.ProviderSessionID)
		}
	})

	t.Run("accepted report spawns as before", func(t *testing.T) {
		h := newRunnerHarness(t)
		var spawns, submissions atomic.Int64
		inner, err := stub.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		if err := reg.Register(&spawnCountingStubProvider{Provider: inner, spawns: &spawns}); err != nil {
			t.Fatal(err)
		}
		h.runner.registry = reg
		h.runner.executionSecurityReporterOverride = &acceptingExecutionSecurityReporter{submissions: &submissions}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := h.runner.Run(ctx, stamped(h, "LOOP-REPORT-ACCEPTED"))
		if err != nil || res.Status != "completed" {
			t.Fatalf("status=%q err=%v (%s), want completed", res.Status, err, res.Error)
		}
		if submissions.Load() != 1 {
			t.Fatalf("submissions = %d, want exactly one pre-spawn post", submissions.Load())
		}
		if spawns.Load() != 1 {
			t.Fatalf("spawns = %d, want one: an accepted report spawns as before", spawns.Load())
		}
		if res.ExecutionSecurity == nil {
			t.Fatal("accepted run recorded no report on the result")
		}
	})

	t.Run("unstamped work spawns without a submission", func(t *testing.T) {
		h := newRunnerHarness(t)
		var spawns, submissions atomic.Int64
		inner, err := stub.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		if err := reg.Register(&spawnCountingStubProvider{Provider: inner, spawns: &spawns}); err != nil {
			t.Fatal(err)
		}
		h.runner.registry = reg
		h.runner.executionSecurityReporterOverride = &acceptingExecutionSecurityReporter{submissions: &submissions}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := h.runner.Run(ctx, h.queuedWork("LOOP-REPORT-UNSTAMPED"))
		if err != nil || res.Status != "completed" {
			t.Fatalf("status=%q err=%v (%s), want completed", res.Status, err, res.Error)
		}
		if submissions.Load() != 0 {
			t.Fatalf("submissions = %d, want zero: unstamped work has no report to send", submissions.Load())
		}
		if spawns.Load() != 1 {
			t.Fatalf("spawns = %d, want one", spawns.Load())
		}
	})

	// Stamped work dispatched without the report capability spawns without a
	// submission: a controller that has not shipped the report contract must
	// never see it, and must never refuse the session for it. Dropping the
	// capability check in step 6c turns this case red because the run posts.
	t.Run("stamped work without the report capability spawns without a submission", func(t *testing.T) {
		h := newRunnerHarness(t)
		var spawns, submissions atomic.Int64
		inner, err := stub.New()
		if err != nil {
			t.Fatal(err)
		}
		reg := NewRegistry()
		if err := reg.Register(&spawnCountingStubProvider{Provider: inner, spawns: &spawns}); err != nil {
			t.Fatal(err)
		}
		h.runner.registry = reg
		h.runner.executionSecurityReporterOverride = &acceptingExecutionSecurityReporter{submissions: &submissions}

		qw := h.queuedWork("LOOP-REPORT-NO-CAPABILITY")
		qw.ExecutionSecurity = executionSecuritySection(nil)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := h.runner.Run(ctx, qw)
		if err != nil || res.Status != "completed" {
			t.Fatalf("status=%q err=%v (%s), want completed", res.Status, err, res.Error)
		}
		if submissions.Load() != 0 {
			t.Fatalf("submissions = %d, want zero: no capability means no report post", submissions.Load())
		}
		if spawns.Load() != 1 {
			t.Fatalf("spawns = %d, want one", spawns.Load())
		}
	})

	// Stamped local-transport work with the report capability spawns without
	// a submission: local-transport work has no controller registration
	// endpoint, so without the exclusion it would post to the local daemon
	// and be refused. Dropping the local-transport check in step 6c turns
	// this case red because the run posts.
	t.Run("stamped local-transport work with the capability spawns without a submission", func(t *testing.T) {
		h := newRunnerHarness(t)
		var spawns, submissions atomic.Int64
		inner, err := stub.New()
		if err != nil {
			t.Fatal(err)
		}
		localReg, err := NewRegistryWithOptions(RegistryOptions{RuntimeTransportMode: RuntimeTransportLocal})
		if err != nil {
			t.Fatal(err)
		}
		if err := localReg.Register(&spawnCountingStubProvider{Provider: inner, spawns: &spawns}); err != nil {
			t.Fatal(err)
		}
		h.runner.registry = localReg
		h.runner.executionSecurityReporterOverride = &acceptingExecutionSecurityReporter{submissions: &submissions}

		qw := h.queuedWork("LOOP-REPORT-LOCAL")
		qw.ExecutionSecurity = executionSecuritySection(nil)
		qw.Capabilities = map[string]bool{CapabilityExecutionSecurityReportIngest: true}
		// Local transport carries no controller runtime credentials.
		qw.AuthToken = ""

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := h.runner.Run(ctx, qw)
		if err != nil || res.Status != "completed" {
			t.Fatalf("status=%q err=%v (%s), want completed", res.Status, err, res.Error)
		}
		if submissions.Load() != 0 {
			t.Fatalf("submissions = %d, want zero: local-transport work has no registration endpoint", submissions.Load())
		}
		if spawns.Load() != 1 {
			t.Fatalf("spawns = %d, want one", spawns.Load())
		}
	})
}

// depsPromptCapturingProvider wraps the stub harness and records the
// delivered Spec so Run-level dependency-preparation tests can assert on
// the prompt plan the provider actually received.
type depsPromptCapturingProvider struct {
	agent.HarnessProvider
	spec   agent.Spec
	spawns int
}

func (p *depsPromptCapturingProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.spec = spec
	p.spawns++
	return p.HarnessProvider.Spawn(ctx, spec)
}

// shellDepsProvider reports the shell name so the runner takes the
// bare-shell lane (no harness protocol), while still delegating to the
// stub harness for manifest and spawn behavior.
type shellDepsProvider struct {
	agent.HarnessProvider
	spec agent.Spec
}

func (p *shellDepsProvider) Name() agent.ProviderName { return agent.ProviderShell }

func depsDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (p *shellDepsProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	p.spec = spec
	return p.HarnessProvider.Spawn(ctx, spec)
}

// seedManifestsWithBin commits a pnpm lockfile, a go.mod, and a leftover
// node_modules/.bin directory so the Run-level stale-.bin test proves the
// installer still runs instead of reading the leftover as ready.
func seedManifestsWithBin(t *testing.T, bareRepo string) {
	t.Helper()
	work := t.TempDir()
	runGitFixture(t, "", "clone", "-q", bareRepo, work)
	writeFile(t, work, "pnpm-lock.yaml", "lockfileVersion: 9.0\n")
	writeFile(t, work, "go.mod", "module example.test/seed\n\ngo 1.24\n")
	writeFile(t, work, filepath.Join("node_modules", ".bin", "keep"), "")
	for _, args := range [][]string{
		{"-C", work, "config", "user.email", "test@example.com"},
		{"-C", work, "config", "user.name", "test"},
		{"-C", work, "add", "pnpm-lock.yaml", "go.mod", filepath.Join("node_modules", ".bin", "keep")},
		{"-C", work, "commit", "-q", "-m", "seed dependency manifests with stale bin"},
		{"-C", work, "push", "-q", "origin", "HEAD:main"},
	} {
		runGitFixture(t, "", args...)
	}
}

func depsCapturingHarness(t *testing.T, h *runnerHarness, provider *depsPromptCapturingProvider) {
	t.Helper()
	inner, err := stub.New()
	if err != nil {
		t.Fatal(err)
	}
	provider.HarnessProvider = inner.(agent.HarnessProvider)
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatal(err)
	}
	h.runner.registry = reg
}

// TestRun_DependencyPreparationReadyAddsNoRecovery drives the production
// Run path with a lockfile plus a pre-existing empty/partial .bin: the
// installer still runs before spawn, the terminal result reports ready,
// and no recovery instruction is added. Restoring the .bin skip or
// removing the Run call leaves zero install calls or no ready report and
// fails it (AC-1 RED).
func TestRun_DependencyPreparationReadyAddsNoRecovery(t *testing.T) {
	h := newRunnerHarness(t)
	h.runner.logger = depsDiscardLogger()
	provider := &depsPromptCapturingProvider{}
	depsCapturingHarness(t, h, provider)
	execer := &recordingDepsExecer{}
	h.runner.depsExecer = execer
	seedManifestsWithBin(t, h.bareRepo)

	qw := h.queuedWork("DEPS-READY-RUN")
	qw.ExecutionSecurity = executionSecuritySection(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run status=%q err=%v (%s); want completed", res.Status, err, res.Error)
	}
	if calls := execer.recorded(); len(calls) != 2 {
		t.Fatalf("install calls = %d; want pnpm then go despite .bin", len(calls))
	}
	if res.DependencyPreparation == nil || res.DependencyPreparation.Status != agent.DependencyPreparationReady {
		t.Fatalf("DependencyPreparation = %+v; want ready", res.DependencyPreparation)
	}
	if provider.spec.PromptPlan == nil || provider.spec.PromptPlan.HarnessProtocol == nil {
		t.Fatalf("prompt plan missing harness protocol: %+v", provider.spec.PromptPlan)
	}
	if strings.Contains(provider.spec.PromptPlan.HarnessProtocol.Text, "Dependency preparation did not complete") {
		t.Fatalf("ready preparation must add no recovery instruction: %q", provider.spec.PromptPlan.HarnessProtocol.Text)
	}
	if provider.spec.PromptPlan.UserPrompt.Text == "" || !strings.Contains(provider.spec.PromptPlan.UserPrompt.Text, "This is a test issue body.") {
		t.Fatalf("original user task lost in prompt plan: %+v", provider.spec.PromptPlan.UserPrompt)
	}
}

// TestRun_DependencyPreparationFailureHandsRecovery drives Run with a
// failing installer: best-effort spawn still completes, the terminal
// result reports failed, and the delivered prompt plan carries the
// recovery instruction while preserving the original task. Discarding
// the outcome or omitting the protocol injection fails it (AC-2 RED).
func TestRun_DependencyPreparationFailureHandsRecovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		execer   *recordingDepsExecer
		wantStat string
	}{
		{name: "nonzero exit", execer: &recordingDepsExecer{exitCode: 1}, wantStat: agent.DependencyPreparationFailed},
		{name: "start error", execer: &recordingDepsExecer{err: errors.New("SENTINEL-start-error")}, wantStat: agent.DependencyPreparationFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			h.runner.logger = depsDiscardLogger()
			provider := &depsPromptCapturingProvider{}
			depsCapturingHarness(t, h, provider)
			h.runner.depsExecer = tc.execer
			seedManifestsInBareRepo(t, h.bareRepo)

			qw := h.queuedWork("DEPS-FAIL-" + tc.name)
			qw.ExecutionSecurity = executionSecuritySection(nil)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := h.runner.Run(ctx, qw)
			if err != nil || res.Status != "completed" {
				t.Fatalf("Run status=%q err=%v (%s); want completed despite install failure", res.Status, err, res.Error)
			}
			if res.DependencyPreparation == nil || res.DependencyPreparation.Status != tc.wantStat {
				t.Fatalf("DependencyPreparation = %+v; want %q", res.DependencyPreparation, tc.wantStat)
			}
			if provider.spec.PromptPlan == nil || provider.spec.PromptPlan.HarnessProtocol == nil {
				t.Fatalf("prompt plan missing harness protocol: %+v", provider.spec.PromptPlan)
			}
			text := provider.spec.PromptPlan.HarnessProtocol.Text
			for _, want := range []string{"Dependency preparation did not complete", "Dependencies are not ready.", "report blocked with the reason"} {
				if !strings.Contains(text, want) {
					t.Errorf("recovery instruction missing %q: %q", want, text)
				}
			}
			if !strings.Contains(provider.spec.PromptPlan.UserPrompt.Text, "This is a test issue body.") {
				t.Errorf("original user task lost in prompt plan: %+v", provider.spec.PromptPlan.UserPrompt)
			}
			if strings.Contains(text, "SENTINEL-start-error") {
				t.Errorf("recovery prompt leaks raw installer error: %q", text)
			}
			if res.DependencyPreparation != nil {
				raw, _ := json.Marshal(res.DependencyPreparation)
				if strings.Contains(string(raw), "SENTINEL-start-error") {
					t.Errorf("preparation report leaks raw installer error: %s", raw)
				}
			}
		})
	}
}

// TestRun_DependencyPreparationTimeoutHandsRecovery drives Run with a
// wedged installer under a short step timeout: the bounded cancellation
// still fires, best-effort spawn completes, and the handoff reports
// timed_out with the recovery instruction.
func TestRun_DependencyPreparationTimeoutHandsRecovery(t *testing.T) {
	h := newRunnerHarness(t)
	h.runner.logger = depsDiscardLogger()
	provider := &depsPromptCapturingProvider{}
	depsCapturingHarness(t, h, provider)
	blocking := &contextBlockingExecer{}
	h.runner.depsExecer = blocking
	h.runner.depsInstallTimeout = 100 * time.Millisecond
	seedManifestsInBareRepo(t, h.bareRepo)

	qw := h.queuedWork("DEPS-TIMEOUT-RUN")
	qw.ExecutionSecurity = executionSecuritySection(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run status=%q err=%v (%s); want completed despite timeout", res.Status, err, res.Error)
	}
	if res.DependencyPreparation == nil || res.DependencyPreparation.Status != agent.DependencyPreparationTimedOut {
		t.Fatalf("DependencyPreparation = %+v; want timed_out", res.DependencyPreparation)
	}
	if provider.spec.PromptPlan == nil || provider.spec.PromptPlan.HarnessProtocol == nil {
		t.Fatalf("prompt plan missing harness protocol: %+v", provider.spec.PromptPlan)
	}
	if !strings.Contains(provider.spec.PromptPlan.HarnessProtocol.Text, "Dependency preparation did not complete") {
		t.Fatalf("timed_out preparation must hand recovery: %q", provider.spec.PromptPlan.HarnessProtocol.Text)
	}
	if !strings.Contains(provider.spec.PromptPlan.HarnessProtocol.Text, agent.DependencyPreparationTimedOut) {
		t.Errorf("recovery handoff missing bounded status timed_out: %q", provider.spec.PromptPlan.HarnessProtocol.Text)
	}
}

// TestRun_DependencyPreparationSkipsAddNoBypass drives Run under a
// restricted execution-security gate: no installer runs, the report is
// the skipped outcome, and no bypass instruction is delivered.
// Removing the authority guard fails it (AC-3 RED).
func TestRun_DependencyPreparationSkipsAddNoBypass(t *testing.T) {
	h := newRunnerHarness(t)
	h.runner.logger = depsDiscardLogger()
	inner, err := stub.New()
	if err != nil {
		t.Fatal(err)
	}
	capturing := &depsPromptCapturingProvider{HarnessProvider: inner.(agent.HarnessProvider)}
	// The stub alone cannot render a restricted network level; wrap the
	// capturing harness with the restricted-level manifest so the run
	// reaches the install gate instead of refusing at pre-spawn.
	gated := &depsSecurityProvider{HarnessProvider: capturing, dimension: agent.ExecutionSecurityNetwork, level: agent.NetworkLogged}
	reg := NewRegistry()
	if err := reg.Register(gated); err != nil {
		t.Fatal(err)
	}
	h.runner.registry = reg
	provider := capturing
	execer := &recordingDepsExecer{}
	h.runner.depsExecer = execer
	seedManifestsInBareRepo(t, h.bareRepo)

	qw := h.queuedWork("DEPS-SKIP-RUN")
	qw.ExecutionSecurity = executionSecuritySection(nil)
	qw.ExecutionSecurity.Levels.Network = agent.NetworkLogged
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run status=%q err=%v (%s); want completed", res.Status, err, res.Error)
	}
	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none under restricted execution", calls)
	}
	if res.DependencyPreparation == nil || res.DependencyPreparation.Status != agent.DependencyPreparationSkippedRestricted {
		t.Fatalf("DependencyPreparation = %+v; want skipped_restricted", res.DependencyPreparation)
	}
	if provider.spec.PromptPlan != nil && provider.spec.PromptPlan.HarnessProtocol != nil &&
		strings.Contains(provider.spec.PromptPlan.HarnessProtocol.Text, "Dependency preparation did not complete") {
		t.Fatalf("skipped preparation must not hand recovery: %q", provider.spec.PromptPlan.HarnessProtocol.Text)
	}
}

// TestRun_ShellGetsEvidenceOnly proves the shell lane carries the bounded
// report as evidence but never executable recovery prose: a failing
// install still completes, the result reports failed, and the shell spec
// has no harness protocol to project instructions onto.
func TestRun_ShellGetsEvidenceOnly(t *testing.T) {
	h := newRunnerHarness(t)
	h.runner.logger = depsDiscardLogger()
	inner, err := stub.New()
	if err != nil {
		t.Fatal(err)
	}
	shell := &shellDepsProvider{HarnessProvider: inner.(agent.HarnessProvider)}
	reg := NewRegistry()
	if err := reg.Register(shell); err != nil {
		t.Fatal(err)
	}
	h.runner.registry = reg
	h.runner.depsExecer = &recordingDepsExecer{exitCode: 1}
	seedManifestsInBareRepo(t, h.bareRepo)

	qw := h.queuedWork("DEPS-SHELL-RUN")
	qw.ExecutionSecurity = executionSecuritySection(nil)
	qw.ResolvedProfile.Provider = agent.ProviderShell
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run status=%q err=%v (%s); want completed", res.Status, err, res.Error)
	}
	if res.DependencyPreparation == nil || res.DependencyPreparation.Status != agent.DependencyPreparationFailed {
		t.Fatalf("DependencyPreparation = %+v; want failed evidence on shell run", res.DependencyPreparation)
	}
	if shell.spec.PromptPlan != nil && shell.spec.PromptPlan.HarnessProtocol != nil {
		t.Fatalf("shell prompt plan must carry no harness protocol: %+v", shell.spec.PromptPlan.HarnessProtocol)
	}
}

// TestRun_NoManifestOmitsPreparation pins the byte-compatible path: a
// worktree with no manifests omits the report and leaves the protocol
// unchanged.
func TestRun_NoManifestOmitsPreparation(t *testing.T) {
	h := newRunnerHarness(t)
	h.runner.logger = depsDiscardLogger()
	provider := &depsPromptCapturingProvider{}
	depsCapturingHarness(t, h, provider)
	execer := &recordingDepsExecer{}
	h.runner.depsExecer = execer
	// No seedManifestsInBareRepo: the bare repo has no manifests.
	qw := h.queuedWork("DEPS-NOMANIFEST-RUN")
	qw.ExecutionSecurity = executionSecuritySection(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run status=%q err=%v (%s); want completed", res.Status, err, res.Error)
	}
	if calls := execer.recorded(); len(calls) != 0 {
		t.Fatalf("install calls = %v; want none with no manifests", calls)
	}
	if res.DependencyPreparation != nil {
		t.Fatalf("DependencyPreparation = %+v; want nil (omitted)", res.DependencyPreparation)
	}
	if provider.spec.PromptPlan != nil && provider.spec.PromptPlan.HarnessProtocol != nil &&
		strings.Contains(provider.spec.PromptPlan.HarnessProtocol.Text, "Dependency preparation did not complete") {
		t.Fatalf("no-manifest run must not hand recovery: %q", provider.spec.PromptPlan.HarnessProtocol.Text)
	}
}
