package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestNewNarrationNudgeState_ResolvesBounds pins the option values: zero is
// the default (one nudge), a positive limit is honoured, and a negative
// limit disables the nudge.
func TestNewNarrationNudgeState_ResolvesBounds(t *testing.T) {
	cases := []struct {
		name      string
		limit     int
		wantLimit int
	}{
		{name: "default", limit: 0, wantLimit: DefaultNarrationNudgeLimit},
		{name: "explicit", limit: 2, wantLimit: 2},
		{name: "disabled", limit: -1, wantLimit: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{narrationNudgeLimit: tc.limit}
			s := r.newNarrationNudgeState()
			if s.limit != tc.wantLimit {
				t.Fatalf("limit = %d; want %d", s.limit, tc.wantLimit)
			}
			if tc.wantLimit == 0 && s.canNudge() {
				t.Fatalf("disabled nudge reports it can nudge")
			}
			if tc.wantLimit > 0 && !s.canNudge() {
				t.Fatalf("enabled nudge reports it cannot nudge")
			}
		})
	}
	if DefaultNarrationNudgeLimit != 1 {
		t.Fatalf("DefaultNarrationNudgeLimit = %d; want 1", DefaultNarrationNudgeLimit)
	}
}

// TestNarrationNudgePrompt pins the prompt wording and the goal restatement:
// the fixed narration text plus the session goal when one exists, and the
// bare prompt when none does.
func TestNarrationNudgePrompt(t *testing.T) {
	if !strings.Contains(narrationNudgePrompt, "You ended your turn with a progress note.") {
		t.Fatalf("narrationNudgePrompt = %q; want it to name the progress note", narrationNudgePrompt)
	}
	if !strings.Contains(narrationNudgePrompt, "commit, push, and report the result") {
		t.Fatalf("narrationNudgePrompt = %q; want it to ask for commit, push and report", narrationNudgePrompt)
	}
	qw := QueuedWork{}
	qw.IssueIdentifier = "TEST-1"
	qw.Title = "Fix it"
	qw.Body = "Do the thing."
	got := narrationNudgePromptFor(qw)
	if !strings.HasPrefix(got, narrationNudgePrompt) {
		t.Fatalf("prompt = %q; want it to start with the fixed narration text", got)
	}
	if !strings.Contains(got, "TEST-1") {
		t.Fatalf("prompt = %q; want it to carry the goal restatement", got)
	}
	empty := QueuedWork{}
	if got := narrationNudgePromptFor(empty); got != narrationNudgePrompt {
		t.Fatalf("empty-goal prompt = %q; want the bare prompt %q", got, narrationNudgePrompt)
	}
}

// TestIsNarrationOnlyStop pins the decision table on its own.
func TestIsNarrationOnlyStop(t *testing.T) {
	clean := streamObservation{terminalSuccess: true, terminalEvent: &agent.ResultEvent{Success: true}, lastAssistantText: "Mapping the fix."}
	with := func(mutate func(*streamObservation)) streamObservation {
		o := clean
		mutate(&o)
		return o
	}
	mkRes := func() Result { return Result{} }
	cases := []struct {
		name       string
		workType   string
		res        Result
		session    streamObservation
		turn       streamObservation
		reportedPR bool
		want       bool
	}{
		{name: "narration-only development stop", workType: "development", res: mkRes(), turn: clean, want: true},
		{name: "review work is never a narration stop", workType: "qa", res: mkRes(), turn: clean, want: false},
		{name: "research work is never a narration stop", workType: "research", res: mkRes(), turn: clean, want: false},
		{name: "verified pull request is not narration", workType: "development", res: func() Result { r := mkRes(); r.PullRequestURL = followUpPR; return r }(), turn: clean, want: false},
		{name: "reported pull request is not narration", workType: "development", res: mkRes(), turn: clean, reportedPR: true, want: false},
		{name: "manifest is not narration", workType: "development", res: func() Result { r := mkRes(); r.Manifest = &TurnManifest{Verdict: "passed"}; return r }(), turn: clean, want: false},
		{name: "work result is not narration", workType: "development", res: func() Result { r := mkRes(); r.WorkResult = "passed"; return r }(), turn: clean, want: false},
		{name: "blocked session is not narration", workType: "development", res: mkRes(), session: streamObservation{blocked: true}, turn: clean, want: false},
		{name: "blocked turn is not narration", workType: "development", res: mkRes(), turn: with(func(o *streamObservation) { o.blocked = true }), want: false},
		{name: "verdict turn is not narration", workType: "development", res: mkRes(), turn: with(func(o *streamObservation) { o.workResult = "passed" }), want: false},
		{name: "failed envelope is not narration", workType: "development", res: func() Result { r := mkRes(); r.FailureMode = FailureProviderError; r.Status = "failed"; return r }(), turn: clean, want: false},
		{name: "crash is not narration", workType: "development", res: mkRes(), turn: with(func(o *streamObservation) { o.errorEvent = &agent.ErrorEvent{Message: "boom"} }), want: false},
		{name: "no terminal is not narration", workType: "development", res: mkRes(), turn: streamObservation{lastAssistantText: "Mapping the fix."}, want: false},
		{name: "unsuccessful terminal is not narration", workType: "development", res: mkRes(), turn: with(func(o *streamObservation) { o.terminalSuccess = false }), want: false},
		{name: "provider error is not narration", workType: "development", res: mkRes(), turn: with(func(o *streamObservation) { o.providerError = "503" }), want: false},
		{name: "empty text is not narration", workType: "development", res: mkRes(), turn: with(func(o *streamObservation) { o.lastAssistantText = "  " }), want: false},
		{name: "last message with a tool call is not narration", workType: "development", res: mkRes(), turn: with(func(o *streamObservation) { o.toolAfterLastAssistant = true }), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qw := QueuedWork{}
			qw.WorkType = tc.workType
			res := tc.res
			if got := isNarrationOnlyStop(qw, &res, tc.session, tc.turn, tc.reportedPR); got != tc.want {
				t.Fatalf("isNarrationOnlyStop = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestObserveEvent_ToolAfterLastAssistant pins the ordering the narration
// check reads: a tool call after the latest assistant text marks that
// message as carrying a tool call, and a later assistant message clears it.
func TestObserveEvent_ToolAfterLastAssistant(t *testing.T) {
	r := minimalRunner(t)
	dir := t.TempDir()
	var obs streamObservation
	r.observeEvent(agent.ToolUseEvent{ToolName: "bash"}, &obs, dir, QueuedWork{}, nil)
	if obs.toolAfterLastAssistant {
		t.Fatalf("tool before any text must not mark the (absent) last message")
	}
	r.observeEvent(agent.AssistantTextEvent{Text: "Mapping the fix."}, &obs, dir, QueuedWork{}, nil)
	if obs.toolAfterLastAssistant {
		t.Fatalf("assistant text with no tool after it must read as narration-only")
	}
	r.observeEvent(agent.ToolUseEvent{ToolName: "bash"}, &obs, dir, QueuedWork{}, nil)
	if !obs.toolAfterLastAssistant {
		t.Fatalf("tool after the last text must mark that message as carrying a tool call")
	}
	r.observeEvent(agent.AssistantTextEvent{Text: "Still mapping."}, &obs, dir, QueuedWork{}, nil)
	if obs.toolAfterLastAssistant {
		t.Fatalf("a later assistant message must clear the tool-after flag")
	}
}

// runNarrationScenario runs one development session with the narration nudge
// enabled (limit 1 unless overridden) on the session repository.
func runNarrationScenario(t *testing.T, narrationLimit int, turns ...verdictScriptTurn) (*Result, *verdictScriptProvider) {
	t.Helper()
	if narrationLimit == 0 {
		narrationLimit = 1
	}
	return runScriptedSession(t, scriptedSession{
		workType:       "development",
		repository:     followUpRepository,
		pulls:          map[int]string{7: pullAtSessionCommit},
		narrationLimit: narrationLimit,
		turns:          turns,
	})
}

func wantNarrationPrompt(t *testing.T, prompts []string, count int) {
	t.Helper()
	if len(prompts) != count {
		t.Fatalf("prompts = %q; want %d", prompts, count)
	}
	for i, p := range prompts {
		base := p
		if idx := strings.Index(p, "\n\nThe goal is"); idx >= 0 {
			base = p[:idx]
		}
		if base != narrationNudgePrompt {
			t.Fatalf("prompt %d = %q; want the narration nudge %q", i, p, narrationNudgePrompt)
		}
	}
}

// TestRun_NarrationOnlyStopNudgedThenCommits is the reported pattern: a
// development turn that ends on a narration-only message with no new commit
// gets the narration nudge, and the follow-up turn that reports the pull
// request completes the session.
func TestRun_NarrationOnlyStopNudgedThenCommits(t *testing.T) {
	res, provider := runNarrationScenario(t, 1,
		verdictScriptTurn{toolCalls: 2, text: "Mapping the fix."},
		verdictScriptTurn{text: "Opened " + followUpPR},
	)
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	wantNarrationPrompt(t, provider.prompts, 1)
	if res.TurnContinuations == nil || res.TurnContinuations.NarrationNudges != 1 {
		t.Fatalf("TurnContinuations = %+v; want one narration nudge", res.TurnContinuations)
	}
	if res.TurnContinuations.Exhausted {
		t.Fatalf("TurnContinuations = %+v; a completed nudge must not read as exhausted", *res.TurnContinuations)
	}
}

// TestRun_NarrationOnlySecondStopFails pins the bound: a second
// narration-only stop after the single nudge fails the session with the
// bound named, sending nothing more.
func TestRun_NarrationOnlySecondStopFails(t *testing.T) {
	res, provider := runNarrationScenario(t, 1,
		verdictScriptTurn{toolCalls: 3, text: "Mapping the fix."},
		verdictScriptTurn{toolCalls: 2, text: "Still mapping the fix."},
		verdictScriptTurn{text: "never reached"},
	)
	if res.Status != "failed" || res.FailureMode != FailureContinuationsUnproductive {
		t.Fatalf("Status=%q FailureMode=%q (%s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureContinuationsUnproductive)
	}
	if !strings.Contains(res.Error, "narration") {
		t.Fatalf("Error = %q; want it to name the narration nudge", res.Error)
	}
	wantNarrationPrompt(t, provider.prompts, 1)
	if res.TurnContinuations == nil || res.TurnContinuations.NarrationNudges != 1 || !res.TurnContinuations.Exhausted {
		t.Fatalf("TurnContinuations = %+v; want one exhausted narration nudge", res.TurnContinuations)
	}
	if res.SteeringTriggered {
		t.Fatalf("SteeringTriggered = true; a narration stop gets the narration nudge, not the pull request nudge")
	}
}

// TestRun_NarrationBlockedNeverNudged pins that an explicit blocked result
// is never nudged: the legitimate decline ends as agent-blocked with no
// narration prompt.
func TestRun_NarrationBlockedNeverNudged(t *testing.T) {
	res, provider := runNarrationScenario(t, 1,
		verdictScriptTurn{text: "The task needs an engine hook.\nWORK_RESULT: blocked\nAGENT_BLOCKED: need an engine hook"},
		verdictScriptTurn{text: "never reached"},
	)
	if res.FailureMode != FailureAgentBlocked {
		t.Fatalf("FailureMode = %q; want %s", res.FailureMode, FailureAgentBlocked)
	}
	if len(provider.prompts) != 0 {
		t.Fatalf("prompts = %q; a blocked result must never be nudged", provider.prompts)
	}
	if res.TurnContinuations != nil && res.TurnContinuations.NarrationNudges != 0 {
		t.Fatalf("TurnContinuations = %+v; a blocked result must not record a narration nudge", *res.TurnContinuations)
	}
}

// TestPostNarrationNudgeActivity pins that the nudge is recorded as a
// session activity with the fixed marker.
func TestPostNarrationNudgeActivity(t *testing.T) {
	r := minimalRunner(t)
	rec := &recordingSink{}
	r.postNarrationNudgeActivity(context.Background(), rec)
	if len(rec.events) != 1 {
		t.Fatalf("activity events = %d; want 1", len(rec.events))
	}
	sys, ok := rec.events[0].(agent.SystemEvent)
	if !ok || sys.Subtype != narrationNudgeActivitySubtype {
		t.Fatalf("activity = %#v; want a SystemEvent with subtype %q", rec.events[0], narrationNudgeActivitySubtype)
	}
}

// TestSessionHasNewCommit pins the commit check: an untouched checkout reads
// as no new commit, a checkout with a new commit reads as having one, and an
// unreadable checkout fails closed as having one (no nudge on unknown state).
func TestSessionHasNewCommit(t *testing.T) {
	r := minimalRunner(t)
	ctx := context.Background()
	dir := t.TempDir()
	gitInit(t, dir)
	base := gitRun(t, dir, "rev-parse", "HEAD")
	res := &Result{}
	res.rescueTargets = []rescueTarget{{path: dir, base: base}}
	if r.sessionHasNewCommit(ctx, res, dir) {
		t.Fatalf("untouched checkout reads as having a new commit")
	}
	writeFile(t, dir, "fix.txt", "work\n")
	if out, err := runGit(ctx, dir, gitIdentity{Name: "test", Email: "test@example.com"}, "add", "fix.txt"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := runGit(ctx, dir, gitIdentity{Name: "test", Email: "test@example.com"}, "commit", "-q", "-m", "work"); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	if !r.sessionHasNewCommit(ctx, res, dir) {
		t.Fatalf("checkout with a new commit reads as having none")
	}
	// An unreadable checkout fails closed as having a commit, so the nudge
	// is skipped rather than sent on unknown state.
	unreadable := &Result{}
	unreadable.rescueTargets = []rescueTarget{{path: t.TempDir(), base: base}}
	if !r.sessionHasNewCommit(ctx, unreadable, unreadable.rescueTargets[0].path) {
		t.Fatalf("unreadable checkout must fail closed as having a commit")
	}
	if r.sessionHasNewCommit(ctx, &Result{}, dir) {
		t.Fatalf("session with no rescue target must read as having no commit")
	}
}
