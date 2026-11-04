package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
	"github.com/RenseiAI/donmai/stagewire"
)

// TestScanMarkerClose pins the WORK_RESULT:close marker: a reason
// (duplicate, obsolete, canceled) and, for a duplicate, an optional
// identifier. It reads as a decline ("blocked") on every legacy path and
// carries the structured close request beside it.
func TestScanMarkerClose(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		text        string
		wantVerdict string
		wantReason  string
		wantClose   *WorkClose
	}{
		{"duplicate with target", "Done.\nWORK_RESULT:close duplicate PRJ-12", "blocked", "close as duplicate of PRJ-12", &WorkClose{Reason: CloseReasonDuplicate, DuplicateOf: "PRJ-12"}},
		{"duplicate without target", "WORK_RESULT: close duplicate", "blocked", "close as duplicate", &WorkClose{Reason: CloseReasonDuplicate}},
		{"obsolete", "WORK_RESULT: close obsolete", "blocked", "close as obsolete", &WorkClose{Reason: CloseReasonObsolete}},
		{"cancelled spelling, comment fence", "<!-- WORK_RESULT: close cancelled -->", "blocked", "close as canceled", &WorkClose{Reason: CloseReasonCanceled}},
		{"colon after close, upper case", "WORK_RESULT: CLOSE: Duplicate PRJ-9", "blocked", "close as duplicate of PRJ-9", &WorkClose{Reason: CloseReasonDuplicate, DuplicateOf: "PRJ-9"}},
		{"target only kept for duplicates", "WORK_RESULT: close obsolete PRJ-9", "blocked", "close as obsolete", &WorkClose{Reason: CloseReasonObsolete}},
		{"reason does not cross the line", "WORK_RESULT: close\nduplicate PRJ-1", "", "", nil},
		{"unknown reason is no marker", "WORK_RESULT: close maybe", "", "", nil},
		{"missing reason is no marker", "WORK_RESULT: close", "", "", nil},
		{"quoted in prose is ignored", "I will not emit WORK_RESULT: close duplicate PRJ-1 here", "", "", nil},
		{"first marker wins", "WORK_RESULT: close obsolete\nWORK_RESULT: passed", "blocked", "close as obsolete", &WorkClose{Reason: CloseReasonObsolete}},
		{"earlier passed wins over a later close", "WORK_RESULT: passed\nWORK_RESULT: close obsolete", "passed", "", nil},
		{"closed is not close", "WORK_RESULT: closed duplicate", "", "", nil},
		{"plain blocked is unchanged", "WORK_RESULT: blocked\nAGENT_BLOCKED: no access", "blocked", "no access", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdict, reason, closeReq := scanMarker(tc.text)
			if verdict != tc.wantVerdict || reason != tc.wantReason || !reflect.DeepEqual(closeReq, tc.wantClose) {
				t.Fatalf("scanMarker = %q, %q, %+v; want %q, %q, %+v", verdict, reason, closeReq, tc.wantVerdict, tc.wantReason, tc.wantClose)
			}
			if v, r := scanVerdict(tc.text); v != tc.wantVerdict || r != tc.wantReason {
				t.Fatalf("scanVerdict = %q, %q; want %q, %q", v, r, tc.wantVerdict, tc.wantReason)
			}
		})
	}
}

// TestCloseMarkerReachesTheResult pins the plumbing: the stream observation
// keeps the close request, a later plain verdict clears it, and the blocked
// fork stamps it onto the result.
func TestCloseMarkerReachesTheResult(t *testing.T) {
	t.Parallel()
	r := &Runner{}
	var obs streamObservation
	r.observeEvent(agent.AssistantTextEvent{Text: "WORK_RESULT: close duplicate PRJ-4"}, &obs, "", QueuedWork{}, nil)
	if !obs.blocked || obs.close == nil || obs.close.DuplicateOf != "PRJ-4" {
		t.Fatalf("observation = %+v; want a blocked close", obs)
	}
	res := &Result{}
	if !classifyBlocked(res, obs, false) || res.WorkClose == nil || res.WorkClose.Reason != CloseReasonDuplicate {
		t.Fatalf("classifyBlocked left WorkClose = %+v", res.WorkClose)
	}
	if res.FailureMode != FailureAgentBlocked || !strings.Contains(res.Error, "duplicate of PRJ-4") {
		t.Fatalf("legacy classification = %q, %q; want an agent-blocked decline naming the close", res.FailureMode, res.Error)
	}
	r.observeEvent(agent.AssistantTextEvent{Text: "WORK_RESULT: passed"}, &obs, "", QueuedWork{}, nil)
	if obs.blocked || obs.close != nil {
		t.Fatalf("a later passed verdict kept the close: %+v", obs)
	}
}

func stageTransitionsFixture() *stagewire.Transitions {
	return &stagewire.Transitions{
		Start:   &stagewire.Transition{StageKey: "in-progress", WriteStatus: "state-start"},
		Success: &stagewire.Transition{StageKey: "review", WriteStatus: "state-success"},
		Failure: &stagewire.Transition{StageKey: "rework", WriteStatus: "state-failure"},
		Blocked: &stagewire.Transition{StageKey: "icebox", WriteStatus: "state-blocked"},
		Close:   &stagewire.Transition{StageKey: "closed", WriteStatus: "state-close"},
	}
}

// TestClassifyStageOutcome pins the outcome classification of a finished
// run against its completion contract.
func TestClassifyStageOutcome(t *testing.T) {
	t.Parallel()
	prContract := stagewire.Fields{Card: &stagewire.Card{ID: "c", Contract: &stagewire.Contract{PR: true, WorkResult: true}}}
	reviewContract := stagewire.Fields{Card: &stagewire.Card{ID: "c", Contract: &stagewire.Contract{Verdict: true, WorkResult: true}}}
	noContract := stagewire.Fields{Card: &stagewire.Card{ID: "c", Contract: &stagewire.Contract{}}}
	for _, tc := range []struct {
		name         string
		fields       stagewire.Fields
		deferFailure bool
		res          Result
		wantOutcome  stagewire.Outcome
		wantReason   string
		wantComment  bool
	}{
		{"passed review", reviewContract, false, resultOf("completed", "", "passed", ""), stagewire.OutcomeSuccess, "passed", false},
		{"failed review", reviewContract, false, resultOf("completed", "", "failed", ""), stagewire.OutcomeFailure, "failed", false},
		{"request changes is a failure", reviewContract, false, withVerdict(resultOf("completed", "", "passed", ""), "REQUEST_CHANGES"), stagewire.OutcomeFailure, "failed", false},
		{"no verdict stays and comments", reviewContract, false, resultOf("completed", "", "", ""), "", "no-verdict", true},
		{"unknown verdict stays and comments", reviewContract, false, resultOf("completed", "", "unknown", ""), "", "no-verdict", true},
		{"pull request delivered", prContract, false, resultOf("completed", "", "passed", "https://github.com/o/r/pull/1"), stagewire.OutcomeSuccess, "passed", false},
		{"pull request missing is a failure", prContract, false, resultOf("completed", "", "passed", ""), stagewire.OutcomeFailure, "no-pull-request", false},
		{"nothing owed, completed", noContract, false, resultOf("completed", "", "", ""), stagewire.OutcomeSuccess, "completed", false},
		{"failed session", prContract, false, resultOf("failed", FailureSpawn, "", ""), stagewire.OutcomeFailure, FailureSpawn, false},
		{"blocked", prContract, false, resultOf("failed", FailureAgentBlocked, "", ""), stagewire.OutcomeBlocked, "blocked", false},
		{"close", prContract, false, closed(resultOf("failed", FailureAgentBlocked, "", "")), stagewire.OutcomeClose, "close", true},
		{"operator cancel stays and comments", prContract, false, resultOf("failed", FailureOperatorCancelled, "", ""), "", "operator-cancelled", true},
		{"timeout stays and comments", prContract, false, resultOf("failed", FailureTimeout, "", ""), "", "timeout", true},
		{"lost ownership stays silently", prContract, false, resultOf("failed", FailureLostOwnership, "", ""), "", "lost-ownership", false},
		{"deferred provider error stays", prContract, true, resultOf("failed", FailureProviderError, "", ""), "", "deferred-provider-error", false},
		{"provider error after retries is a failure", prContract, false, resultOf("failed", FailureProviderError, "", ""), stagewire.OutcomeFailure, FailureProviderError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			qw := QueuedWork{QueuedWork: prompt.QueuedWork{WorkType: "development", Fields: tc.fields}, DeferFailureTransition: tc.deferFailure}
			res := tc.res
			got := classifyStageOutcome(qw, &res)
			if got.outcome != tc.wantOutcome || got.reason != tc.wantReason || (got.comment != "") != tc.wantComment {
				t.Fatalf("decision = %+v; want outcome %q reason %q comment %v", got, tc.wantOutcome, tc.wantReason, tc.wantComment)
			}
		})
	}
}

func resultOf(status, failureMode, workResult, prURL string) Result {
	res := Result{}
	res.Status, res.FailureMode, res.WorkResult, res.PullRequestURL = status, failureMode, workResult, prURL
	return res
}

func withVerdict(res Result, verdict string) Result {
	res.ReviewVerdict = verdict
	return res
}

func closed(res Result) Result {
	res.WorkClose = &WorkClose{Reason: CloseReasonObsolete}
	return res
}

// stateWrites returns the stateIds of the captured updateIssue calls, and
// fails the test on any name-keyed updateIssueStatus call.
func stateWrites(t *testing.T, calls *[]proxyCall, mu *sync.Mutex) (states []string, comments int) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	for _, call := range *calls {
		switch call.Method {
		case "updateIssue":
			if len(call.Args) != 2 {
				t.Fatalf("updateIssue args = %v; want [issue, {stateId}]", call.Args)
			}
			fields, _ := call.Args[1].(map[string]any)
			if len(fields) != 1 {
				t.Fatalf("updateIssue args = %v; want [issue, {stateId}]", call.Args)
			}
			state, _ := fields["stateId"].(string)
			states = append(states, state)
		case "createComment":
			comments++
		default:
			t.Fatalf("unexpected tracker call %s %v (the name-keyed status tables must never be consulted)", call.Method, call.Args)
		}
	}
	return states, comments
}

// TestStageTransitionsApplied pins the status moves for a work item that
// carries transitions: start at run start, the matching outcome's target
// at the end, "stay" writes nothing, and a run without an outcome stays and
// comments.
func TestStageTransitionsApplied(t *testing.T) {
	t.Parallel()
	review := &stagewire.Card{ID: "c", Contract: &stagewire.Contract{Verdict: true, WorkResult: true}}
	for _, tc := range []struct {
		name         string
		transitions  *stagewire.Transitions
		res          Result
		wantStates   []string
		wantComments int
	}{
		{"success", stageTransitionsFixture(), resultOf("completed", "", "passed", ""), []string{"state-start", "state-success"}, 0},
		{"failure", stageTransitionsFixture(), resultOf("completed", "", "failed", ""), []string{"state-start", "state-failure"}, 0},
		{"blocked", stageTransitionsFixture(), resultOf("failed", FailureAgentBlocked, "", ""), []string{"state-start", "state-blocked"}, 0},
		{"close", stageTransitionsFixture(), closed(resultOf("failed", FailureAgentBlocked, "", "")), []string{"state-start", "state-close"}, 1},
		{"no verdict stays", stageTransitionsFixture(), resultOf("completed", "", "", ""), []string{"state-start"}, 1},
		{"operator cancel stays", stageTransitionsFixture(), resultOf("failed", FailureOperatorCancelled, "", ""), []string{"state-start"}, 1},
		{"stay target writes nothing", &stagewire.Transitions{Success: &stagewire.Transition{}}, resultOf("completed", "", "passed", ""), nil, 0},
		{"absent outcome stays, no start declared", &stagewire.Transitions{}, resultOf("completed", "", "failed", ""), nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, calls, mu := stubProxyServer(t)
			r := makePostSessionRunner(t, srv)
			qw := QueuedWork{QueuedWork: queuedWorkBase("STAGE-" + strings.ReplaceAll(tc.name, " ", "-"))}
			qw.WorkType = WorkTypeQAStr
			qw.Card = review
			qw.Transitions = tc.transitions

			start := r.applyStageStart(context.Background(), qw)
			res := tc.res
			r.finishStageTransition(context.Background(), qw, &res, start)

			states, comments := stateWrites(t, calls, mu)
			if !reflect.DeepEqual(states, tc.wantStates) || comments != tc.wantComments {
				t.Fatalf("state writes %v, comments %d; want %v, %d", states, comments, tc.wantStates, tc.wantComments)
			}
			if res.StageTransition == nil || res.StageStart == nil {
				t.Fatalf("records not stamped: start %+v end %+v", res.StageStart, res.StageTransition)
			}
			if len(res.PostSessionWarnings) != 0 {
				t.Fatalf("warnings = %v", res.PostSessionWarnings)
			}
		})
	}
}

// TestStageTransitionsAbsentKeepLegacy pins that a work item without
// transitions never reaches the stage-transition path.
func TestStageTransitionsAbsentKeepLegacy(t *testing.T) {
	t.Parallel()
	srv, calls, mu := stubProxyServer(t)
	r := makePostSessionRunner(t, srv)
	qw := QueuedWork{QueuedWork: queuedWorkBase("STAGE-LEGACY")}
	qw.WorkType = WorkTypeQAStr
	if start := r.applyStageStart(context.Background(), qw); start != nil {
		t.Fatalf("start applied without transitions: %+v", start)
	}
	res := resultOf("completed", "", "passed", "")
	r.finishStageTransition(context.Background(), qw, &res, nil)
	mu.Lock()
	defer mu.Unlock()
	if len(*calls) != 0 || res.StageTransition != nil {
		t.Fatalf("stage path ran without transitions: calls %v record %+v", *calls, res.StageTransition)
	}
}

// TestRunStageTransitionsBypassNameTables runs a full session for a
// built-in name ("qa", whose legacy table moves a pass to a named status)
// with transitions and the legacy post-session block enabled. The tracker
// sees only the start and success state writes; the name-keyed
// updateIssueStatus is never called.
func TestRunStageTransitionsBypassNameTables(t *testing.T) {
	srv, calls, mu := stubProxyServer(t)
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL, WorkerID: "wkr-stage", AuthToken: "tok-stage", HTTPClient: srv.Client(), BaseDelay: 1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	reg := NewRegistry()
	provider, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := New(Options{
		Registry: reg, WorktreeManager: wtm, Poster: poster, HTTPClient: srv.Client(),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		SkipBackstop: true, SkipSteering: true, PreserveWorktreeAlways: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	qw := QueuedWork{
		QueuedWork:      queuedWorkBase("STAGE-RUN"),
		WorkerID:        "wkr-stage",
		AuthToken:       "tok-stage",
		PlatformURL:     srv.URL,
		ResolvedProfile: ResolvedProfile{Provider: agent.ProviderStub},
	}
	qw.WorkType = WorkTypeQAStr
	qw.Repository = makeBareRepo(t)
	qw.Stage = &stagewire.Stage{ID: "stg", Key: "verify", Name: "Verify", Role: stagewire.RoleReview}
	qw.Card = &stagewire.Card{ID: "card", Handle: "reviewer", Contract: &stagewire.Contract{Verdict: true, WorkResult: true}}
	qw.Transitions = stageTransitionsFixture()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := r.Run(ctx, qw)
	if res == nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" || res.WorkResult != "passed" {
		t.Fatalf("Status = %q, WorkResult = %q (err %v); want completed/passed", res.Status, res.WorkResult, err)
	}
	states, _ := stateWrites(t, calls, mu)
	if !reflect.DeepEqual(states, []string{"state-start", "state-success"}) {
		t.Fatalf("state writes = %v; want start then success", states)
	}
	if res.LinearStatusTransition != nil {
		t.Fatalf("legacy post-session ran: %+v", res.LinearStatusTransition)
	}
	if res.StageTransition == nil || !res.StageTransition.Succeeded || res.StageTransition.Outcome != string(stagewire.OutcomeSuccess) {
		t.Fatalf("stage transition record = %+v", res.StageTransition)
	}
}

// TestStageRetryPolicyGovernsMechanicalRetries pins that a dispatched
// retry policy sets the provider-error retry bound and backoff, and that
// its absence keeps today's bound and backoff.
func TestStageRetryPolicyGovernsMechanicalRetries(t *testing.T) {
	t.Parallel()
	r := &Runner{}
	for _, tc := range []struct {
		name   string
		policy *stagewire.Retry
		want   int
	}{
		{"absent keeps the configured bound", nil, DefaultTurnContinuationLimit},
		{"one retry", &stagewire.Retry{Retries: 1}, 1},
		{"zero retries", &stagewire.Retry{}, 0},
		{"five retries", &stagewire.Retry{Retries: 5}, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := r.newTurnFollowUps()
			f.applyRetryPolicy(tc.policy)
			retries := 0
			for f.next(turnProviderError, true, true, false) == tailRetry {
				retries++
				f.retried++
				if retries > 20 {
					t.Fatal("retries never exhausted")
				}
			}
			if retries != tc.want || f.bound != boundRetries {
				t.Fatalf("retried %d times (bound %d); want %d then the retry bound", retries, f.bound, tc.want)
			}
		})
	}
}

// TestStageRetryPolicyBackoff pins that a dispatched retry policy's backoff
// replaces the configured one, in both directions.
func TestStageRetryPolicyBackoff(t *testing.T) {
	t.Parallel()
	slow := &Runner{providerRetryBackoff: func(int) time.Duration { return time.Hour }}
	start := time.Now()
	if err := slow.waitRetryBackoffFor(context.Background(), &stagewire.Retry{Retries: 2, BackoffSeconds: 0}, 1); err != nil {
		t.Fatalf("waitRetryBackoffFor: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("policy backoff 0 waited %s; the configured hour must not apply", elapsed)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := (&Runner{providerRetryBackoff: func(int) time.Duration { return 0 }}).waitRetryBackoffFor(ctx, &stagewire.Retry{BackoffSeconds: 3600}, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("policy backoff 3600s returned %v; want it to wait out the context", err)
	}
}

// flakySpawnProvider fails its first `failures` spawns, then delegates.
type flakySpawnProvider struct {
	agent.Provider
	failures int
	spawns   atomic.Int64
}

func (p *flakySpawnProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if int(p.spawns.Add(1)) <= p.failures {
		return nil, errors.New("spawn failed")
	}
	return p.Provider.Spawn(ctx, spec)
}

// TestStageRetryPolicyRetriesSpawn pins spawn retries: none without a
// policy, up to the policy's retries with one.
func TestStageRetryPolicyRetriesSpawn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		policy     *stagewire.Retry
		failures   int
		wantErr    bool
		wantSpawns int64
	}{
		{"no policy: one attempt", nil, 1, true, 1},
		{"recovers within the retries", &stagewire.Retry{Retries: 2}, 2, false, 3},
		{"exhausts the retries", &stagewire.Retry{Retries: 1}, 3, true, 2},
		{"zero retries: one attempt", &stagewire.Retry{}, 1, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner, err := stub.New()
			if err != nil {
				t.Fatal(err)
			}
			provider := &flakySpawnProvider{Provider: inner, failures: tc.failures}
			r := &Runner{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			qw := QueuedWork{QueuedWork: prompt.QueuedWork{Fields: stagewire.Fields{Retry: tc.policy}}}
			handle, err := r.spawnWithRetry(context.Background(), provider, agent.Spec{}, qw)
			if (err != nil) != tc.wantErr || provider.spawns.Load() != tc.wantSpawns {
				t.Fatalf("err %v after %d spawns; want error %v after %d", err, provider.spawns.Load(), tc.wantErr, tc.wantSpawns)
			}
			if handle != nil {
				_ = handle.Stop(context.Background())
			}
		})
	}
}
