package runner

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/activity"
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
		name        string
		workType    string
		res         Result
		session     streamObservation
		turn        streamObservation
		reportedPR  bool
		undelivered string
		want        bool
	}{
		{name: "narration-only development stop", workType: "development", res: mkRes(), turn: clean, want: true},
		{name: "review work is never a narration stop", workType: "qa", res: mkRes(), turn: clean, want: false},
		{name: "research work is never a narration stop", workType: "research", res: mkRes(), turn: clean, want: false},
		{name: "verified pull request is not narration", workType: "development", res: func() Result { r := mkRes(); r.PullRequestURL = followUpPR; return r }(), turn: clean, want: false},
		{name: "continued pull request with no new commit is narration", workType: "development", res: func() Result { r := mkRes(); r.PullRequestURL = "https://github.com/example/repo/pull/12"; return r }(), turn: clean, undelivered: undeliveredNoNewCommit, want: true},
		{name: "continued pull request that delivers is not narration", workType: "development", res: func() Result { r := mkRes(); r.PullRequestURL = "https://github.com/example/repo/pull/12"; return r }(), turn: clean, undelivered: "", want: false},
		{name: "draft pull request keeps its own remedy", workType: "development", res: func() Result { r := mkRes(); r.PullRequestURL = followUpPR; return r }(), turn: clean, undelivered: undeliveredDraft, want: false},
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
			if got := isNarrationOnlyStop(qw, &res, tc.session, tc.turn, tc.reportedPR, tc.undelivered); got != tc.want {
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

// TestRun_NarrationOnlySecondProductiveStopContinues pins that the nudge is
// only a prompt swap: a second narration-only stop after the single nudge
// takes the generic continuation path and the existing continuation bounds
// decide exhaustion. Both turns here are productive (tool calls), so the
// session continues instead of failing.
func TestRun_NarrationOnlySecondProductiveStopContinues(t *testing.T) {
	res, provider := runNarrationScenario(t, 1,
		verdictScriptTurn{toolCalls: 3, text: "Mapping the fix."},
		verdictScriptTurn{toolCalls: 2, text: "Still mapping the fix."},
		verdictScriptTurn{text: "Opened " + followUpPR},
	)
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	if len(provider.prompts) != 2 {
		t.Fatalf("prompts = %q; want the narration nudge then the generic continuation", provider.prompts)
	}
	wantNarrationPrompt(t, provider.prompts[:1], 1)
	base := provider.prompts[1]
	if idx := strings.Index(base, "\n\nThe goal is"); idx >= 0 {
		base = base[:idx]
	}
	if base != continuePrompt {
		t.Fatalf("second prompt = %q; want the generic continuation %q", provider.prompts[1], continuePrompt)
	}
	if res.TurnContinuations == nil || res.TurnContinuations.NarrationNudges != 1 {
		t.Fatalf("TurnContinuations = %+v; want one narration nudge", res.TurnContinuations)
	}
	if res.TurnContinuations.Exhausted {
		t.Fatalf("TurnContinuations = %+v; a completed nudge must not read as exhausted", *res.TurnContinuations)
	}
	if res.SteeringTriggered {
		t.Fatalf("SteeringTriggered = true; a narration stop gets a continuation, not the pull request nudge")
	}
}

// TestRun_NarrationProductiveStopsContinuePastLimit runs the existing
// productive-continuation shape with the nudge at its production default:
// an agent that ends its turns early but keeps working is continued past
// the narration budget under the generic bounds, not failed after two turns.
func TestRun_NarrationProductiveStopsContinuePastLimit(t *testing.T) {
	turns := append(slices.Repeat([]verdictScriptTurn{{toolCalls: 3, text: "Next I will run the tests."}}, 6),
		verdictScriptTurn{text: "Opened " + followUpPR})
	res, provider := runNarrationScenario(t, 1, turns...)
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	if len(provider.prompts) != 6 {
		t.Fatalf("prompts = %d; want 6 continuations", len(provider.prompts))
	}
	wantNarrationPrompt(t, provider.prompts[:1], 1)
	for i, p := range provider.prompts[1:] {
		base := p
		if idx := strings.Index(p, "\n\nThe goal is"); idx >= 0 {
			base = p[:idx]
		}
		if base != continuePrompt {
			t.Fatalf("prompt %d = %q; want the generic continuation after the nudge", i+1, p)
		}
	}
	if res.TurnContinuations == nil || res.TurnContinuations.NarrationNudges != 1 || res.TurnContinuations.Continued != 6 {
		t.Fatalf("TurnContinuations = %+v; want 6 continuations with one narration nudge", res.TurnContinuations)
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
	if sys.Message != narrationNudgeActivityMessage {
		t.Fatalf("activity message = %q; want %q", sys.Message, narrationNudgeActivityMessage)
	}
}

// TestNarrationNudgeActivitySubtypePinsPoster ties the runner marker to the
// activity poster without duplicating the literal: the runner aliases the
// poster's exported constants, and the poster maps that subtype to the
// fixed context marker.
func TestNarrationNudgeActivitySubtypePinsPoster(t *testing.T) {
	if narrationNudgeActivitySubtype != activity.NarrationNudgeSubtype {
		t.Fatalf("runner subtype = %q; want the poster subtype %q", narrationNudgeActivitySubtype, activity.NarrationNudgeSubtype)
	}
	if narrationNudgeActivityMessage != activity.NarrationNudgeContent {
		t.Fatalf("runner message = %q; want the poster content %q", narrationNudgeActivityMessage, activity.NarrationNudgeContent)
	}
	if activity.NarrationNudgeContextKey != "narrationNudge" || activity.NarrationNudgeContextValue != "sent" {
		t.Fatalf("poster context = %q=%q; want narrationNudge=sent", activity.NarrationNudgeContextKey, activity.NarrationNudgeContextValue)
	}
}

// TestRun_NarrationNudgePostsActivity pins the marker end to end: a scripted
// development session that takes the narration nudge posts the activity to
// the platform, where the poster renders it as the fixed context marker.
func TestRun_NarrationNudgePostsActivity(t *testing.T) {
	platform := newRecordingPlatformServer(t)
	res, provider := runScriptedSession(t, scriptedSession{
		workType:       "development",
		repository:     followUpRepository,
		pulls:          map[int]string{7: pullAtSessionCommit},
		narrationLimit: 1,
		platform:       platform,
		turns: []verdictScriptTurn{
			{toolCalls: 2, text: "Mapping the fix."},
			{text: "Opened " + followUpPR},
		},
	})
	if res.Status != "completed" {
		t.Fatalf("Status=%q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	wantNarrationPrompt(t, provider.prompts, 1)
	// The asynchronous poster may not have flushed before Run returned:
	// poll the captured activity posts briefly.
	deadline := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			for _, body := range platform.activityPosts() {
				if strings.Contains(string(body), activity.NarrationNudgeContent) && strings.Contains(string(body), "narrationNudge") {
					close(deadline)
					return
				}
			}
		}
		close(deadline)
	}()
	<-deadline
	found := false
	for _, body := range platform.activityPosts() {
		s := string(body)
		if strings.Contains(s, activity.NarrationNudgeContent) && strings.Contains(s, "narrationNudge") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no narration-nudge activity among %d posts", len(platform.activityPosts()))
	}
}

// TestRun_NarrationContinuedPullRequestNudgedThenPushes pins the rework path:
// a continued pull request with no new commit counts as no pull request for
// the narration check, so the first narration-only stop gets the narration
// nudge (not the generic no-new-commit continuation) and the session
// completes after the follow-up turn pushes.
func TestRun_NarrationContinuedPullRequestNudgedThenPushes(t *testing.T) {
	res, provider := runScriptedSession(t, scriptedSession{
		workType:       "development",
		repository:     followUpRepository,
		narrationLimit: 1,
		continueNumber: 12,
		turns: []verdictScriptTurn{
			{text: "Mapping the fix."},
			{
				files: map[string]string{"fix.txt": "work\n"},
				push:  []string{"refs/heads/continued/pr-12", "refs/pull/12/head"},
				text:  "Pushed the fix.",
			},
		},
	})
	if res.Status != "completed" || res.PullRequestURL != "https://github.com/example/repo/pull/12" {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with the continued pull request", res.Status, res.PullRequestURL, res.FailureMode, res.Error)
	}
	wantNarrationPrompt(t, provider.prompts, 1)
	if res.TurnContinuations == nil || res.TurnContinuations.NarrationNudges != 1 {
		t.Fatalf("TurnContinuations = %+v; want one narration nudge", res.TurnContinuations)
	}
}

// TestRun_NarrationAfterLocalCommitGetsGenericContinuation pins the commit
// guard at the runner seam: a narration-only turn whose checkout already
// holds a commit of its own takes the generic continuation, never the
// narration nudge.
func TestRun_NarrationAfterLocalCommitGetsGenericContinuation(t *testing.T) {
	res, provider := runScriptedSession(t, scriptedSession{
		workType:       "development",
		repository:     followUpRepository,
		pulls:          map[int]string{7: pullAtSessionCommit},
		narrationLimit: 1,
		turns: []verdictScriptTurn{
			{
				files: map[string]string{"fix.txt": "work\n"},
				push:  []string{"refs/heads/" + scriptedSessionBranch},
				text:  "Mapping the fix.",
			},
			{
				files: map[string]string{"more.txt": "more\n"},
				push:  []string{"refs/heads/" + scriptedSessionBranch, "refs/pull/7/head"},
				text:  "Opened " + followUpPR,
			},
		},
	})
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	if len(provider.prompts) != 1 {
		t.Fatalf("prompts = %q; want one generic continuation", provider.prompts)
	}
	base := provider.prompts[0]
	if idx := strings.Index(base, "\n\nThe goal is"); idx >= 0 {
		base = base[:idx]
	}
	if base != continuePrompt {
		t.Fatalf("prompt = %q; want the generic continuation, not the narration nudge", provider.prompts[0])
	}
	if res.TurnContinuations != nil && res.TurnContinuations.NarrationNudges != 0 {
		t.Fatalf("TurnContinuations = %+v; a committed turn must not record a narration nudge", res.TurnContinuations)
	}
}

// TestRun_NarrationWrapUpDoesNotConsumeBudget pins that a wrap-up follow-up
// is not a narration nudge: when the token meter passes the wrap-up point,
// the narration candidate gets the wrap-up prompt, records nothing, and the
// next narration-only stop still gets its nudge.
func TestRun_NarrationWrapUpDoesNotConsumeBudget(t *testing.T) {
	res, provider := runScriptedSession(t, scriptedSession{
		workType:       "development",
		repository:     followUpRepository,
		pulls:          map[int]string{7: pullAtSessionCommit},
		narrationLimit: 1,
		budget:         tokenBudget(),
		turns: []verdictScriptTurn{
			{events: concat(modelCall("Mapping the fix.", 850))},
			{events: modelCall("Still mapping.", 10)},
			{events: modelCall("Opened "+followUpPR, 10)},
		},
	})
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	if len(provider.prompts) != 2 {
		t.Fatalf("prompts = %q; want wrap-up then narration", provider.prompts)
	}
	if provider.prompts[0] != wrapUpPrompt {
		t.Fatalf("first prompt = %q; want the wrap-up prompt, not the narration nudge", provider.prompts[0])
	}
	wantNarrationPrompt(t, provider.prompts[1:], 1)
	if res.TurnContinuations == nil || res.TurnContinuations.NarrationNudges != 1 {
		t.Fatalf("TurnContinuations = %+v; want exactly one narration nudge (the wrap-up is not one)", res.TurnContinuations)
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
	// An empty rescue base fails closed as having a commit: with no base to
	// compare against, the checkout never reads as having done nothing.
	emptyBase := &Result{}
	emptyBase.rescueTargets = []rescueTarget{{path: dir, base: "  "}}
	if !r.sessionHasNewCommit(ctx, emptyBase, dir) {
		t.Fatalf("empty rescue base must fail closed as having a commit")
	}
}
