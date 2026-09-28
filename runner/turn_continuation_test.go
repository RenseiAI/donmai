package runner

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// runContinuationScenario runs one development session on a repository where
// followUpPR really is the session's pull request (see runScripted), with the
// given continuation limit (0 = default).
func runContinuationScenario(t *testing.T, limit int, turns ...verdictScriptTurn) (*Result, []string) {
	t.Helper()
	res, provider := runScriptedSession(t, scriptedSession{
		workType: "development",
		github:   "example/repo",
		lookup: &fakePullRequestLookup{heads: map[string]pullRequestHead{
			followUpPR: {Number: 7, URL: followUpPR, HeadRefName: scriptedSessionBranch},
		}},
		continuationLimit: limit,
		turns:             turns,
	})
	return res, provider.prompts
}

func wantContinuations(t *testing.T, res *Result, continued, retried int, exhausted bool) {
	t.Helper()
	got := res.TurnContinuations
	if continued == 0 && retried == 0 && !exhausted {
		if got != nil {
			t.Fatalf("TurnContinuations = %+v; want none", *got)
		}
		return
	}
	if got == nil {
		t.Fatalf("TurnContinuations = nil; want continued=%d retried=%d exhausted=%v", continued, retried, exhausted)
	}
	if got.Continued != continued || got.Retried != retried || got.Exhausted != exhausted {
		t.Fatalf("TurnContinuations = %+v; want continued=%d retried=%d exhausted=%v", *got, continued, retried, exhausted)
	}
}

func wantPrompts(t *testing.T, prompts []string, want ...string) {
	t.Helper()
	if len(prompts) != len(want) {
		t.Fatalf("follow-up prompts = %d %q; want %d", len(prompts), prompts, len(want))
	}
	for i := range want {
		if prompts[i] != want[i] {
			t.Errorf("prompt %d = %q; want %q", i, prompts[i], want[i])
		}
	}
}

// TestRun_EarlyStopIsContinuedUntilTheWorkIsDone is the pi pattern: turns that
// end on a one-line progress note with no result are continued, not nudged to
// open a pull request, until a turn delivers the pull request.
func TestRun_EarlyStopIsContinuedUntilTheWorkIsDone(t *testing.T) {
	res, prompts := runContinuationScenario(t, 0,
		verdictScriptTurn{text: "Now let me run the tests."},
		verdictScriptTurn{text: "Tests pass. Next I will push."},
		verdictScriptTurn{text: "Opened " + followUpPR + "\nWORK_RESULT: passed"},
	)
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	if res.SteeringTriggered {
		t.Errorf("SteeringTriggered = true; an early stop is continued, not nudged to open a pull request")
	}
	wantPrompts(t, prompts, continuePrompt, continuePrompt)
	wantContinuations(t, res, 2, 0, false)
	if res.TurnContinuations.Limit != DefaultTurnContinuationLimit {
		t.Errorf("Limit = %d; want the default %d", res.TurnContinuations.Limit, DefaultTurnContinuationLimit)
	}
}

// TestRun_ProviderErrorIsRetriedNotNudged pins that a turn that ended on a
// model provider error gets the retry prompt, and neither the continuation nor
// the pull request nudge.
func TestRun_ProviderErrorIsRetriedNotNudged(t *testing.T) {
	res, prompts := runContinuationScenario(t, 0,
		verdictScriptTurn{text: "Checking the build.", providerError: "504 Gateway Timeout"},
		verdictScriptTurn{text: "Opened " + followUpPR},
	)
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	if res.SteeringTriggered {
		t.Errorf("SteeringTriggered = true; a provider error is retried, not nudged")
	}
	wantPrompts(t, prompts, retryPrompt)
	wantContinuations(t, res, 0, 1, false)
}

// TestRun_ContinuationLimitReachedFailsWithTheCount pins the bound: after the
// limit of continuations the turn is still unfinished, so the session fails
// with its own failure mode, records the count, and sends nothing more.
func TestRun_ContinuationLimitReachedFailsWithTheCount(t *testing.T) {
	res, prompts := runContinuationScenario(t, 2,
		verdictScriptTurn{text: "Looking around."},
		verdictScriptTurn{text: "Still looking."},
		verdictScriptTurn{text: "Almost there."},
		verdictScriptTurn{text: "Opened " + followUpPR + " (never reached)"},
	)
	if res.Status != "failed" || res.FailureMode != FailureContinuationsExhausted {
		t.Fatalf("Status=%q FailureMode=%q; want failed/%s", res.Status, res.FailureMode, FailureContinuationsExhausted)
	}
	if !strings.Contains(res.Error, "after 2 continuation prompts") {
		t.Errorf("Error = %q; want it to name the 2 continuations", res.Error)
	}
	if res.SteeringTriggered || res.PullRequestURL != "" {
		t.Errorf("SteeringTriggered=%v PullRequestURL=%q; unfinished work gets no nudge and no pull request", res.SteeringTriggered, res.PullRequestURL)
	}
	wantPrompts(t, prompts, continuePrompt, continuePrompt)
	wantContinuations(t, res, 2, 0, true)
}

// TestRun_ProviderErrorRetriesExhaustedFailAsProviderError pins the retry
// bound: a turn that still ends on a provider error after the limit fails as a
// provider error, with the count and the provider's text.
func TestRun_ProviderErrorRetriesExhaustedFailAsProviderError(t *testing.T) {
	res, prompts := runContinuationScenario(t, 1,
		verdictScriptTurn{text: "Working.", providerError: "503 Service Unavailable"},
		verdictScriptTurn{providerError: "503 Service Unavailable"},
		verdictScriptTurn{text: "never reached"},
	)
	if res.Status != "failed" || res.FailureMode != FailureProviderError {
		t.Fatalf("Status=%q FailureMode=%q; want failed/%s", res.Status, res.FailureMode, FailureProviderError)
	}
	if !strings.Contains(res.Error, "after 1 retries") || !strings.Contains(res.Error, "503 Service Unavailable") {
		t.Errorf("Error = %q; want the retry count and the provider error", res.Error)
	}
	wantPrompts(t, prompts, retryPrompt)
	wantContinuations(t, res, 0, 1, true)
}

// TestRun_ExplicitVerdictsAreNotContinued pins that a blocked verdict — in
// either spelling or as a manifest — is never continued, and that a turn which
// gave a passed verdict without a pull request gets the pull request nudge,
// not a continuation.
func TestRun_ExplicitVerdictsAreNotContinued(t *testing.T) {
	cases := []struct {
		name         string
		first        verdictScriptTurn
		wantMode     string
		wantSteering bool
	}{
		{name: "WORK_RESULT blocked", first: verdictScriptTurn{text: "The spec is ambiguous.\nWORK_RESULT: blocked\nAGENT_BLOCKED: need a product decision"}, wantMode: FailureAgentBlocked},
		{name: "AGENT_BLOCKED alone", first: verdictScriptTurn{text: "AGENT_BLOCKED: no access to the staging data"}, wantMode: FailureAgentBlocked},
		{name: "blocked manifest", first: verdictScriptTurn{manifest: `{"schemaVersion":1,"verdict":"blocked","blockedReason":"need a decision"}`, text: "Need a decision."}, wantMode: FailureAgentBlocked},
		{name: "passed verdict without a pull request is nudged", first: verdictScriptTurn{text: "All done.\nWORK_RESULT: passed"}, wantSteering: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, prompts := runContinuationScenario(t, 0, tc.first, verdictScriptTurn{text: "Opened " + followUpPR})
			if res.FailureMode != tc.wantMode {
				t.Errorf("FailureMode = %q; want %q", res.FailureMode, tc.wantMode)
			}
			if res.SteeringTriggered != tc.wantSteering {
				t.Errorf("SteeringTriggered = %v; want %v", res.SteeringTriggered, tc.wantSteering)
			}
			for _, prompt := range prompts {
				if prompt == continuePrompt || prompt == retryPrompt {
					t.Errorf("sent %q after an explicit verdict", prompt)
				}
			}
			wantContinuations(t, res, 0, 0, false)
		})
	}
}

// TestRun_EarlyStopAfterThePullRequestNudgeIsContinued is the reported
// failure: the nudge fires once, and a second early stop used to end the
// session unfinished. It now gets a continuation.
func TestRun_EarlyStopAfterThePullRequestNudgeIsContinued(t *testing.T) {
	res, prompts := runContinuationScenario(t, 0,
		verdictScriptTurn{text: "Implementation done.\nWORK_RESULT: passed"},
		verdictScriptTurn{text: "Committing now."},
		verdictScriptTurn{text: "Opened " + followUpPR},
	)
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	if !res.SteeringTriggered {
		t.Errorf("SteeringTriggered = false; the passed verdict without a pull request gets the nudge first")
	}
	if len(prompts) != 2 || prompts[1] != continuePrompt {
		t.Fatalf("prompts = %q; want the nudge, then one continuation", prompts)
	}
	wantContinuations(t, res, 1, 0, false)
}

// TestRun_UnconfirmedOwnPullRequestIsNudgedNotContinued pins that a turn which
// reported a pull request on the session's repository that could not be
// confirmed did not stop early: it gets the pull request nudge (and then the
// backstop), as before, never a continuation — so a lookup outage cannot turn
// a finished session into a continuation loop.
func TestRun_UnconfirmedOwnPullRequestIsNudgedNotContinued(t *testing.T) {
	const unconfirmed = "https://github.com/example/repo/pull/99"
	res, prompts := runContinuationScenario(t, 0,
		verdictScriptTurn{text: "Opened " + unconfirmed},
		verdictScriptTurn{text: "Opened " + followUpPR},
	)
	if !res.SteeringTriggered {
		t.Errorf("SteeringTriggered = false; want the pull request nudge")
	}
	for _, prompt := range prompts {
		if prompt == continuePrompt {
			t.Errorf("sent a continuation after the turn reported a pull request")
		}
	}
	if res.PullRequestURL != followUpPR {
		t.Errorf("PullRequestURL = %q; want %q", res.PullRequestURL, followUpPR)
	}
	wantContinuations(t, res, 0, 0, false)
}

func TestSessionPullRequestVerifier_ReportsOwnRepository(t *testing.T) {
	v := &sessionPullRequestVerifier{repository: "acme/widgets"}
	turn := func(text string) streamObservation {
		return streamObservation{pullRequestCandidates: appendPullRequestCandidates(nil, text)}
	}
	if !v.reportsOwnRepository(turn("see https://github.com/Acme/Widgets/pull/3")) {
		t.Error("an own-repository URL (any case) is a reported pull request")
	}
	if v.reportsOwnRepository(turn("see " + quotedExamplePR)) {
		t.Error("the quoted example on another repository is not")
	}
	if (&sessionPullRequestVerifier{}).reportsOwnRepository(turn("https://github.com/acme/widgets/pull/3")) {
		t.Error("a session with no GitHub repository reports none")
	}
	var none *sessionPullRequestVerifier
	if none.reportsOwnRepository(turn("https://github.com/acme/widgets/pull/3")) {
		t.Error("a nil verifier reports none")
	}
}

// TestRun_NegativeContinuationLimitKeepsTheSingleNudge pins the off switch: an
// early stop gets only the pull request nudge, as before.
func TestRun_NegativeContinuationLimitKeepsTheSingleNudge(t *testing.T) {
	res, prompts := runContinuationScenario(t, -1,
		verdictScriptTurn{text: "Now let me run the tests."},
		verdictScriptTurn{text: "Still running."},
		verdictScriptTurn{text: "never reached"},
	)
	if !res.SteeringTriggered || len(prompts) != 1 {
		t.Fatalf("SteeringTriggered=%v prompts=%d; want the single nudge", res.SteeringTriggered, len(prompts))
	}
	wantContinuations(t, res, 0, 0, false)
}

// TestClassifyTurnEnding pins the decision table on its own.
func TestClassifyTurnEnding(t *testing.T) {
	clean := streamObservation{terminalSuccess: true, terminalEvent: &agent.ResultEvent{Success: true}}
	with := func(mutate func(*streamObservation)) streamObservation {
		o := clean
		mutate(&o)
		return o
	}
	cases := []struct {
		name       string
		res        Result
		session    streamObservation
		turn       streamObservation
		reportedPR bool
		want       turnEnding
	}{
		{name: "clean end with nothing", turn: clean, want: turnStoppedEarly},
		{name: "provider error", turn: with(func(o *streamObservation) { o.providerError = "503" }), want: turnProviderError},
		{name: "verified pull request", res: func() Result { r := Result{}; r.PullRequestURL = followUpPR; return r }(), turn: clean, want: turnFinished},
		{name: "turn-result manifest", res: func() Result { r := Result{}; r.Manifest = &TurnManifest{Verdict: "passed"}; return r }(), turn: clean, want: turnFinished},
		{name: "an own-repository pull request that did not verify", turn: clean, reportedPR: true, want: turnFinished},
		{name: "blocked from any turn", session: streamObservation{blocked: true}, turn: clean, want: turnFinished},
		{name: "failed verdict", res: func() Result { r := Result{}; r.WorkResult = "failed"; return r }(), turn: clean, want: turnFinished},
		{name: "passed verdict in the turn", turn: with(func(o *streamObservation) { o.workResult = "passed" }), want: turnFinished},
		{name: "runner-recorded failure", res: func() Result { r := Result{}; r.FailureMode = FailureProviderError; return r }(), turn: clean, want: turnFinished},
		{name: "crash", turn: with(func(o *streamObservation) { o.errorEvent = &agent.ErrorEvent{Message: "boom"} }), want: turnFinished},
		{name: "no terminal", turn: streamObservation{}, want: turnFinished},
		{name: "unsuccessful terminal", turn: with(func(o *streamObservation) { o.terminalSuccess = false }), want: turnFinished},
		{name: "unsuccessful terminal on a provider error", turn: with(func(o *streamObservation) { o.terminalSuccess = false; o.providerError = "504" }), want: turnProviderError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.res
			if got := classifyTurnEnding(&res, tc.session, tc.turn, tc.reportedPR); got != tc.want {
				t.Fatalf("classifyTurnEnding = %d; want %d", got, tc.want)
			}
		})
	}
}

// TestObserveEvent_ProviderErrorClearsOnRecovery pins that the provider-error
// observation stands only when nothing the model produced came after it.
func TestObserveEvent_ProviderErrorClearsOnRecovery(t *testing.T) {
	r := minimalRunner(t)
	dir := t.TempDir()
	errEvent := agent.SystemEvent{Subtype: agent.SystemSubtypeProviderError, Message: "503 Service Unavailable"}
	cases := []struct {
		name   string
		events []agent.Event
		want   string
	}{
		{name: "error ends the stream", events: []agent.Event{agent.AssistantTextEvent{Text: "partial"}, errEvent}, want: "503 Service Unavailable"},
		{name: "assistant text after the error", events: []agent.Event{errEvent, agent.AssistantTextEvent{Text: "recovered"}}},
		{name: "tool call after the error", events: []agent.Event{errEvent, agent.ToolUseEvent{ToolName: "bash"}}},
		{name: "other system events keep it", events: []agent.Event{errEvent, agent.SystemEvent{Subtype: "auto_retry_end"}}, want: "503 Service Unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var obs streamObservation
			for _, ev := range tc.events {
				r.observeEvent(ev, &obs, dir, QueuedWork{})
			}
			if obs.providerError != tc.want {
				t.Fatalf("providerError = %q; want %q", obs.providerError, tc.want)
			}
		})
	}
}
