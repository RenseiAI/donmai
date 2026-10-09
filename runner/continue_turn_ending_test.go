package runner

import (
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestRun_ContinueModeSiblingPullRequestURLStillContinues pins the early
// stop of a continued pull request whose turn output also quotes a sibling
// pull request on the same repository: the first turn reads the continued
// pull request (gh pr view, whose comments link an earlier attempt) and a
// later turn lists pull requests (gh pr list, naming another number), each
// committing nothing and ending with a progress note. Every such turn is
// sent the no-new-commit continuation, as a turn without the sibling URL
// is, instead of skipping every follow-up and failing at the delivery gate
// with no new commit.
func TestRun_ContinueModeSiblingPullRequestURLStillContinues(t *testing.T) {
	const continued = "https://github.com/example/repo/pull/12"
	view := "title: Fix the thing\nurl: " + continued + "\n--\nreviewer: the earlier attempt was " + followUpPR + "; keep its tests."
	first := verdictScriptTurn{events: []agent.Event{
		agent.ToolUseEvent{ToolName: "bash", ToolUseID: "view", Input: map[string]any{"command": "gh pr view 12 --comments"}},
		agent.ToolResultEvent{ToolName: "bash", ToolUseID: "view", Content: view},
		agent.AssistantTextEvent{Text: "Recovered the earlier commit; now mapping the diff against the acceptance criteria."},
	}}
	note := verdictScriptTurn{text: "Still mapping the diff."}
	list := verdictScriptTurn{events: []agent.Event{
		agent.ToolUseEvent{ToolName: "bash", ToolUseID: "list", Input: map[string]any{"command": "gh pr list --repo example/repo --limit 5"}},
		agent.ToolResultEvent{ToolName: "bash", ToolUseID: "list", Content: "12 Fix the thing\n15 Another attempt\n" + followUpPR + "\n"},
		agent.AssistantTextEvent{Text: "Still mapping the diff."},
	}}
	// The first two turns carry tool calls (productive), so they are
	// continued without touching the unproductive streak; the trailing
	// text-only notes then exhaust it, exactly as text-only turns do.
	const wantContinued = DefaultTurnContinuationLimit + 1
	res, provider := runScriptedSession(t, scriptedSession{
		workType:       "development",
		repository:     followUpRepository,
		continueNumber: 12,
		turns: append(
			[]verdictScriptTurn{first, list},
			append(slices.Repeat([]verdictScriptTurn{note}, DefaultTurnContinuationLimit+1), verdictScriptTurn{text: "never reached"})...,
		),
	})
	wantPrompts(t, provider.prompts, slices.Repeat([]string{continueNoNewCommitPrompt}, wantContinued)...)
	wantContinuations(t, res, wantContinued, 0, true)
	if res.Status != "failed" || res.FailureMode != FailureContinuationsUnproductive {
		t.Fatalf("Status=%q FailureMode=%q (%s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureContinuationsUnproductive)
	}
	if !strings.HasSuffix(res.Error, "the pull request does not deliver the work: no new commit") {
		t.Errorf("Error = %q; want it to end with the reason, no new commit", res.Error)
	}
}

// TestSessionPullRequestVerifier_PinnedPullRequestReportsNoOther pins the
// scope of that rule: only a rework or continued run whose pull request is
// accepted ignores the other URLs a turn carried.
func TestSessionPullRequestVerifier_PinnedPullRequestReportsNoOther(t *testing.T) {
	const accepted = "https://github.com/acme/widgets/pull/8"
	turn := streamObservation{pullRequestCandidates: appendPullRequestCandidates(nil,
		"opened "+accepted+"; see https://github.com/acme/widgets/pull/3 and https://github.com/acme/widgets/pull/6")}
	newVerifier := func(startHead, acceptedURL string) *sessionPullRequestVerifier {
		return &sessionPullRequestVerifier{repository: "acme/widgets", startHead: startHead, accepted: acceptedURL, outcomes: map[string]candidateOutcome{
			accepted:                                 candidateAccepted,
			"https://github.com/acme/widgets/pull/6": candidateUnconfirmed,
		}}
	}
	if newVerifier(strings.Repeat("a", 40), accepted).reportsOwnRepository(turn) {
		t.Error("a rework whose pull request is accepted reported another URL; want it judged by its own pull request")
	}
	if !newVerifier(strings.Repeat("a", 40), "").reportsOwnRepository(turn) {
		t.Error("a rework with no accepted pull request must still count an unchecked own-repository URL")
	}
	if !newVerifier("", accepted).reportsOwnRepository(turn) {
		t.Error("a session that opened its own pull request must still count an unchecked own-repository URL")
	}
}
