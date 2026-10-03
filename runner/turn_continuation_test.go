package runner

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// runContinuationScenario runs one development session on a repository where
// followUpPR really is the session's pull request (see runScripted), with the
// given continuation limit (0 = default).
func runContinuationScenario(t *testing.T, limit int, turns ...verdictScriptTurn) (*Result, []string) {
	t.Helper()
	res, provider := runScriptedSession(t, scriptedSession{
		workType:          "development",
		repository:        followUpRepository,
		pulls:             map[int]string{7: pullAtSessionCommit},
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
	if res.Status != "failed" || res.FailureMode != FailureContinuationsUnproductive {
		t.Fatalf("Status=%q FailureMode=%q; want failed/%s", res.Status, res.FailureMode, FailureContinuationsUnproductive)
	}
	if !strings.Contains(res.Error, "after 2 consecutive continuation prompts whose turns made no tool call (2 continuation prompts in total)") {
		t.Errorf("Error = %q; want it to name the 2 unproductive continuations", res.Error)
	}
	if res.SteeringTriggered || res.PullRequestURL != "" {
		t.Errorf("SteeringTriggered=%v PullRequestURL=%q; unfinished work gets no nudge and no pull request", res.SteeringTriggered, res.PullRequestURL)
	}
	wantPrompts(t, prompts, continuePrompt, continuePrompt)
	wantContinuations(t, res, 2, 0, true)
}

// TestRun_ContinuationBoundsFollowProgress pins the progress bound: a turn
// that made a tool call is productive and resets the streak of unproductive
// continuations, so an agent that ends its turns early but keeps working is
// continued past TurnContinuationLimit; only that many CONSECUTIVE
// continuations with no tool call fail the session, and the total ceiling
// fails it with its own failure mode. Provider-error retries keep their own
// count, which progress does not reset.
func TestRun_ContinuationBoundsFollowProgress(t *testing.T) {
	productive := func(text string) verdictScriptTurn { return verdictScriptTurn{toolCalls: 3, text: text} }
	idle := func(text string) verdictScriptTurn { return verdictScriptTurn{text: text} }
	repeat := func(n int, turn func(i int) verdictScriptTurn) []verdictScriptTurn {
		turns := make([]verdictScriptTurn, n)
		for i := range turns {
			turns[i] = turn(i)
		}
		return turns
	}
	alternate := func(i int) verdictScriptTurn {
		if i%2 == 0 {
			return productive("Next I will run the tests.")
		}
		return idle("Next I will run the tests.")
	}
	done := verdictScriptTurn{text: "Opened " + followUpPR}
	neverReached := verdictScriptTurn{text: "never reached"}
	cases := []struct {
		name        string
		limit       int
		ceiling     int
		turns       []verdictScriptTurn
		wantMode    string // "" = completed with followUpPR
		wantErr     string
		wantPrompts []string
		want        agent.TurnContinuations
	}{
		{
			name:        "productive early stops continue past the limit",
			turns:       append(repeat(6, func(int) verdictScriptTurn { return productive("Next I will run the tests.") }), done),
			wantPrompts: slices.Repeat([]string{continuePrompt}, 6),
			want:        agent.TurnContinuations{Continued: 6, Limit: DefaultTurnContinuationLimit, Ceiling: DefaultTurnContinuationCeiling},
		},
		{
			name:        "three consecutive unproductive continuations fail",
			turns:       []verdictScriptTurn{productive("Plan: edit."), productive("Plan: test."), idle("Plan: push."), idle("Plan: push."), idle("Plan: push."), neverReached},
			wantMode:    FailureContinuationsUnproductive,
			wantErr:     "after 3 consecutive continuation prompts whose turns made no tool call (4 continuation prompts in total)",
			wantPrompts: slices.Repeat([]string{continuePrompt}, 4),
			want:        agent.TurnContinuations{Continued: 4, Unproductive: 3, Limit: DefaultTurnContinuationLimit, Ceiling: DefaultTurnContinuationCeiling, Exhausted: true},
		},
		{
			name:        "a productive turn resets the unproductive streak",
			turns:       []verdictScriptTurn{idle("a"), idle("b"), idle("c"), productive("d"), idle("e"), idle("f"), done},
			wantPrompts: slices.Repeat([]string{continuePrompt}, 6),
			want:        agent.TurnContinuations{Continued: 6, Unproductive: 2, Limit: DefaultTurnContinuationLimit, Ceiling: DefaultTurnContinuationCeiling},
		},
		{
			name:        "alternating productive and unproductive stops finish the work",
			turns:       append(repeat(7, func(i int) verdictScriptTurn { return alternate(i + 1) }), done),
			wantPrompts: slices.Repeat([]string{continuePrompt}, 7),
			want:        agent.TurnContinuations{Continued: 7, Unproductive: 1, Limit: DefaultTurnContinuationLimit, Ceiling: DefaultTurnContinuationCeiling},
		},
		{
			name:        "alternating productive and unproductive stops run to the ceiling",
			limit:       2,
			ceiling:     6,
			turns:       append(repeat(7, alternate), neverReached),
			wantMode:    FailureContinuationsCeiling,
			wantErr:     "after 6 continuation prompts, the ceiling (0 in a row made no tool call)",
			wantPrompts: slices.Repeat([]string{continuePrompt}, 6),
			want:        agent.TurnContinuations{Continued: 6, Limit: 2, Ceiling: 6, Exhausted: true},
		},
		{
			name:        "productive early stops fail at the ceiling",
			ceiling:     4,
			turns:       append(repeat(5, func(int) verdictScriptTurn { return productive("Next I will run the tests.") }), neverReached),
			wantMode:    FailureContinuationsCeiling,
			wantErr:     "after 4 continuation prompts, the ceiling",
			wantPrompts: slices.Repeat([]string{continuePrompt}, 4),
			want:        agent.TurnContinuations{Continued: 4, Limit: DefaultTurnContinuationLimit, Ceiling: 4, Exhausted: true},
		},
		{
			name:        "a productive retry resets the unproductive streak",
			turns:       []verdictScriptTurn{idle("a"), idle("b"), idle("c"), {text: "d", providerError: "504 Gateway Timeout"}, productive("e"), done},
			wantPrompts: []string{continuePrompt, continuePrompt, continuePrompt, retryPrompt, continuePrompt},
			want:        agent.TurnContinuations{Continued: 4, Retried: 1, Limit: DefaultTurnContinuationLimit, Ceiling: DefaultTurnContinuationCeiling},
		},
		{
			name: "productive provider-error turns keep the retry bound",
			turns: append(repeat(4, func(int) verdictScriptTurn {
				return verdictScriptTurn{toolCalls: 3, text: "Working.", providerError: "503 Service Unavailable"}
			}), neverReached),
			wantMode:    FailureProviderError,
			wantErr:     "after 3 retries: 503 Service Unavailable",
			wantPrompts: slices.Repeat([]string{retryPrompt}, 3),
			want:        agent.TurnContinuations{Retried: 3, Limit: DefaultTurnContinuationLimit, Ceiling: DefaultTurnContinuationCeiling, Exhausted: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, provider := runScriptedSession(t, scriptedSession{
				workType:            "development",
				repository:          followUpRepository,
				pulls:               map[int]string{7: pullAtSessionCommit},
				continuationLimit:   tc.limit,
				continuationCeiling: tc.ceiling,
				turns:               tc.turns,
			})
			if tc.wantMode == "" {
				if res.Status != "completed" || res.PullRequestURL != followUpPR {
					t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
				}
			} else {
				if res.Status != "failed" || res.FailureMode != tc.wantMode {
					t.Fatalf("Status=%q FailureMode=%q (%s); want failed/%s", res.Status, res.FailureMode, res.Error, tc.wantMode)
				}
				if !strings.Contains(res.Error, tc.wantErr) {
					t.Errorf("Error = %q; want it to contain %q", res.Error, tc.wantErr)
				}
				if res.PullRequestURL != "" {
					t.Errorf("PullRequestURL = %q; unfinished work gets no pull request", res.PullRequestURL)
				}
			}
			if res.SteeringTriggered {
				t.Errorf("SteeringTriggered = true; an early stop is continued, not nudged")
			}
			wantPrompts(t, provider.prompts, tc.wantPrompts...)
			if res.TurnContinuations == nil || *res.TurnContinuations != tc.want {
				t.Fatalf("TurnContinuations = %+v; want %+v", res.TurnContinuations, tc.want)
			}
		})
	}
}

// TestNewTurnFollowUps_ResolvesBounds pins the option values: zero is each
// default, a negative limit disables continuations and retries, and a
// negative ceiling removes the ceiling.
func TestNewTurnFollowUps_ResolvesBounds(t *testing.T) {
	cases := []struct {
		name                         string
		limit, ceiling               int
		wantLimit, wantCeilingResult int
	}{
		{name: "defaults", wantLimit: DefaultTurnContinuationLimit, wantCeilingResult: DefaultTurnContinuationCeiling},
		{name: "explicit", limit: 5, ceiling: 20, wantLimit: 5, wantCeilingResult: 20},
		{name: "continuations disabled", limit: -1, wantLimit: 0, wantCeilingResult: DefaultTurnContinuationCeiling},
		{name: "no ceiling", ceiling: -1, wantLimit: DefaultTurnContinuationLimit, wantCeilingResult: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{turnContinuationLimit: tc.limit, turnContinuationCeiling: tc.ceiling}
			f := r.newTurnFollowUps()
			if f.limit != tc.wantLimit || f.ceiling != tc.wantCeilingResult {
				t.Fatalf("limit=%d ceiling=%d; want %d/%d", f.limit, f.ceiling, tc.wantLimit, tc.wantCeilingResult)
			}
		})
	}
}

// TestTurnFollowUps_NoCeilingKeepsContinuingProductiveTurns pins that with
// the ceiling removed, only the unproductive streak bounds continuations:
// productive early stops are continued well past the default ceiling, and
// the limit-th consecutive unproductive continuation turn still fails.
func TestTurnFollowUps_NoCeilingKeepsContinuingProductiveTurns(t *testing.T) {
	f := (&Runner{turnContinuationCeiling: -1}).newTurnFollowUps()
	// endings are the productivity of each turn that stopped early: the
	// session's first turn, then each continuation turn in order.
	endings := slices.Repeat([]bool{true}, 2*DefaultTurnContinuationCeiling)
	endings = append(endings, slices.Repeat([]bool{false}, DefaultTurnContinuationLimit)...)
	for i, productive := range endings {
		step := f.next(turnStoppedEarly, productive, true, false)
		if i == len(endings)-1 {
			if step != tailExhausted || f.bound != boundUnproductive || f.unproductive != DefaultTurnContinuationLimit {
				t.Fatalf("last stop: step=%d bound=%d unproductive=%d; want tailExhausted by the unproductive bound at %d", step, f.bound, f.unproductive, DefaultTurnContinuationLimit)
			}
			break
		}
		if step != tailContinue {
			t.Fatalf("stop %d (productive=%v): step = %d; want tailContinue", i, productive, step)
		}
		f.continued++
	}
}

// TestRun_ProviderErrorNotRetryableEndsAtOnce pins the fail-fast path: a
// turn that ends on a provider error the provider marks as not retryable
// (a 400 for invalid parameters) ends right away with the error recorded —
// no retry prompt, no pull request nudge.
func TestRun_ProviderErrorNotRetryableEndsAtOnce(t *testing.T) {
	res, prompts := runContinuationScenario(t, 0,
		verdictScriptTurn{text: "Working.", providerError: "400 The request contains invalid parameters"},
		verdictScriptTurn{text: "never reached"},
	)
	if res.Status != "failed" || res.FailureMode != FailureProviderError {
		t.Fatalf("Status=%q FailureMode=%q; want failed/%s", res.Status, res.FailureMode, FailureProviderError)
	}
	if !strings.Contains(res.Error, "not retryable") || !strings.Contains(res.Error, "400 The request contains invalid parameters") {
		t.Errorf("Error = %q; want the not-retryable reason and the provider error", res.Error)
	}
	if len(prompts) != 0 {
		t.Errorf("prompts = %q; want no retry for a non-retryable provider error", prompts)
	}
	if res.SteeringTriggered {
		t.Errorf("SteeringTriggered = true; a non-retryable provider error is failed, not nudged")
	}
	wantContinuations(t, res, 0, 0, true)
}

// TestRun_ProviderErrorRetryableStatusesKeepBoundedRetries pins that 503
// and 429 provider errors keep today's bounded retries.
func TestRun_ProviderErrorRetryableStatusesKeepBoundedRetries(t *testing.T) {
	for _, status := range []string{"503 Service Unavailable", "429 Too Many Requests"} {
		t.Run(status, func(t *testing.T) {
			res, prompts := runContinuationScenario(t, 1,
				verdictScriptTurn{text: "Working.", providerError: status},
				verdictScriptTurn{providerError: status},
				verdictScriptTurn{text: "never reached"},
			)
			if res.Status != "failed" || res.FailureMode != FailureProviderError {
				t.Fatalf("Status=%q FailureMode=%q; want failed/%s", res.Status, res.FailureMode, FailureProviderError)
			}
			if !strings.Contains(res.Error, "after 1 retries") || !strings.Contains(res.Error, status) {
				t.Errorf("Error = %q; want the retry count and the provider error", res.Error)
			}
			wantPrompts(t, prompts, retryPrompt)
			wantContinuations(t, res, 0, 1, true)
		})
	}
}

// providerErrorThenSuccess runs a development session whose first turn ends
// on the provider error text and whose retry delivers the work.
func providerErrorThenSuccess(t *testing.T, providerError string) (*Result, []string) {
	t.Helper()
	return runContinuationScenario(t, 0,
		verdictScriptTurn{text: "Working.", providerError: providerError},
		verdictScriptTurn{text: "Opened " + followUpPR + "\nWORK_RESULT: passed"},
		verdictScriptTurn{text: "never reached"},
	)
}

// wantRetriedToCompletion asserts the session retried its provider error
// once and then completed with its pull request.
func wantRetriedToCompletion(t *testing.T, res *Result, prompts []string) {
	t.Helper()
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s after one retry",
			res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	wantPrompts(t, prompts, retryPrompt)
	wantContinuations(t, res, 0, 1, false)
}

// TestRun_NetworkErrorsWithNumbersStayRetryable pins that a status is never
// read from a port, an address or a number in prose: each of these network
// errors is retried, and the retry completes the work.
func TestRun_NetworkErrorsWithNumbersStayRetryable(t *testing.T) {
	for _, text := range []string{
		"connect ETIMEDOUT 104.18.32.47:443",
		"connect ECONNREFUSED 10.0.0.12:443",
		"dial tcp 10.0.0.12:443: i/o timeout",
		"timeout after 400 seconds",
		"stream reset after 404 bytes of the response body",
	} {
		t.Run(text, func(t *testing.T) {
			res, prompts := providerErrorThenSuccess(t, text)
			wantRetriedToCompletion(t, res, prompts)
		})
	}
}

// TestRun_RetryableStatusesThenSuccessComplete pins the statuses the policy
// retries: 408 and 409 (and 429 and 5xx) are retried, and the retry
// completes the work.
func TestRun_RetryableStatusesThenSuccessComplete(t *testing.T) {
	for _, text := range []string{
		"408 Request Timeout",
		"409 Conflict",
		"429 Too Many Requests",
		"HTTP 503 Service Unavailable",
	} {
		t.Run(text, func(t *testing.T) {
			res, prompts := providerErrorThenSuccess(t, text)
			wantRetriedToCompletion(t, res, prompts)
		})
	}
}

// TestRun_ContextOverflowStaysRetryable pins that a context overflow is
// retried even though the provider answers it with a 400 (or the harness
// marks it not retryable): the harness compacts its context on the next user
// message, so the runner's retry prompt is a new attempt that can succeed.
func TestRun_ContextOverflowStaysRetryable(t *testing.T) {
	for _, text := range []string{
		`400 {"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 213456 tokens > 200000 maximum"}}`,
		"400 This model's maximum context length is 128000 tokens. However, your messages resulted in 131072 tokens.",
		"400 prompt is too long" + agent.ProviderErrorNotRetryableSuffix,
	} {
		t.Run(text, func(t *testing.T) {
			res, prompts := providerErrorThenSuccess(t, text)
			wantRetriedToCompletion(t, res, prompts)
		})
	}
}

// TestRun_VerdictWinsOverANonRetryableProviderError pins the fatal path's
// limits. A turn that gave its own verdict before a provider error a retry
// cannot fix is classified by that verdict: research ends completed and
// passed, and development without a pull request gets the pull request
// nudge, as before the policy existed. Work the runner never retries (here
// research without a verdict) ends as it did before, too: the fatal path
// only replaces a retry the runner would have sent.
func TestRun_VerdictWinsOverANonRetryableProviderError(t *testing.T) {
	const invalid = "400 The request contains invalid parameters"
	cases := []struct {
		name           string
		workType       string
		turns          []verdictScriptTurn
		wantWorkResult string
		wantPR         string
		wantSteering   bool
		wantPrompts    int
	}{
		{
			name:           "research verdict, then a non-retryable 400",
			workType:       "research",
			turns:          []verdictScriptTurn{{text: "Findings written to the report.\nWORK_RESULT: passed", providerError: invalid}, {text: "never reached"}},
			wantWorkResult: "passed",
		},
		{
			name:     "research without a verdict, then a non-retryable 400",
			workType: "research",
			turns:    []verdictScriptTurn{{text: "Reading the sources.", providerError: invalid}, {text: "never reached"}},
		},
		{
			name:           "development verdict without a pull request, then a non-retryable 400",
			workType:       "development",
			turns:          []verdictScriptTurn{{text: "Implemented.\nWORK_RESULT: passed", providerError: invalid}, {text: "Opened " + followUpPR}, {text: "never reached"}},
			wantWorkResult: "passed",
			wantPR:         followUpPR,
			wantSteering:   true,
			wantPrompts:    1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := scriptedSession{workType: tc.workType, turns: tc.turns}
			if tc.workType == "development" {
				cfg.repository = followUpRepository
				cfg.pulls = map[int]string{7: pullAtSessionCommit}
			}
			res, provider := runScriptedSession(t, cfg)
			if res.Status != "completed" || res.FailureMode != "" {
				t.Fatalf("Status=%q FailureMode=%q (%s); want completed", res.Status, res.FailureMode, res.Error)
			}
			if res.WorkResult != tc.wantWorkResult {
				t.Errorf("WorkResult = %q; want %q", res.WorkResult, tc.wantWorkResult)
			}
			if res.PullRequestURL != tc.wantPR {
				t.Errorf("PullRequestURL = %q; want %q", res.PullRequestURL, tc.wantPR)
			}
			if res.SteeringTriggered != tc.wantSteering {
				t.Errorf("SteeringTriggered = %v; want %v", res.SteeringTriggered, tc.wantSteering)
			}
			if len(provider.prompts) != tc.wantPrompts {
				t.Errorf("follow-up prompts = %q; want %d", provider.prompts, tc.wantPrompts)
			}
			for _, prompt := range provider.prompts {
				if prompt == retryPrompt {
					t.Errorf("sent a retry for a provider error a retry cannot fix")
				}
			}
			wantContinuations(t, res, 0, 0, false)
		})
	}
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
// reported a pull request on the session's repository that the remote could
// not be read to confirm did not stop early: it gets the pull request nudge
// (and then the backstop), as before, never a continuation — so a remote
// outage cannot turn a finished session into a continuation loop.
func TestRun_UnconfirmedOwnPullRequestIsNudgedNotContinued(t *testing.T) {
	const unconfirmed = "https://github.com/example/repo/pull/99"
	outage := func(ctx context.Context, worktreePath string, refs ...string) (map[string]string, error) {
		if slices.Contains(refs, "refs/pull/99/head") {
			return nil, errors.New("remote unreachable")
		}
		return originRefs(ctx, worktreePath, refs...)
	}
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "development",
		repository: followUpRepository,
		pulls:      map[int]string{7: pullAtSessionCommit},
		lookup:     outage,
		turns: []verdictScriptTurn{
			{text: "Opened " + unconfirmed},
			{text: "Opened " + followUpPR},
		},
	})
	prompts := provider.prompts
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

// TestRun_OtherOwnRepositoryPullRequestThenEarlyStopIsContinued pins that an
// own-repository pull request confirmed to be someone else's does not count as
// the session reporting its pull request: a turn that cites one and stops
// early is continued, not nudged.
func TestRun_OtherOwnRepositoryPullRequestThenEarlyStopIsContinued(t *testing.T) {
	const other = "https://github.com/example/repo/pull/100"
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "development",
		repository: followUpRepository,
		pulls:      map[int]string{7: pullAtSessionCommit, 100: pullAtOtherCommit},
		turns: []verdictScriptTurn{
			{text: "Read " + other + " for context. Now implementing."},
			{text: "Opened " + followUpPR},
		},
	})
	if res.SteeringTriggered {
		t.Errorf("SteeringTriggered = true; a turn citing someone else's pull request stopped early")
	}
	wantPrompts(t, provider.prompts, continuePrompt)
	wantContinuations(t, res, 1, 0, false)
	if res.PullRequestURL != followUpPR {
		t.Errorf("PullRequestURL = %q; want %q", res.PullRequestURL, followUpPR)
	}
}

// TestRun_ContinuationExhaustedStillRunsTheBackstop pins that a session whose
// turn never finished still gets the backstop's open-PR attempt, whichever
// continuation bound it ran out of: the work is committed and pushed to the
// session branch and a pull request is opened, while the session stays failed
// with the failure mode of that bound.
func TestRun_ContinuationExhaustedStillRunsTheBackstop(t *testing.T) {
	cases := []struct {
		name     string
		limit    int
		ceiling  int
		turns    []verdictScriptTurn
		wantMode string
	}{
		{
			name:  "unproductive bound",
			limit: 1,
			turns: []verdictScriptTurn{
				{files: map[string]string{"feature.txt": "half done\n"}, text: "Working on it."},
				{text: "Still working."},
				{text: "never reached"},
			},
			wantMode: FailureContinuationsUnproductive,
		},
		{
			name:    "ceiling",
			ceiling: 1,
			turns: []verdictScriptTurn{
				{files: map[string]string{"feature.txt": "half done\n"}, toolCalls: 2, text: "Working on it."},
				{toolCalls: 2, text: "Still working."},
				{text: "never reached"},
			},
			wantMode: FailureContinuationsCeiling,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const opened = "https://github.com/example/repo/pull/12"
			ghArgs := stubGhRecordingArgs(t, opened)
			res, provider := runScriptedSession(t, scriptedSession{
				workType:            "development",
				repository:          followUpRepository,
				backstop:            true,
				continuationLimit:   tc.limit,
				continuationCeiling: tc.ceiling,
				turns:               tc.turns,
			})
			if res.Status != "failed" || res.FailureMode != tc.wantMode {
				t.Fatalf("Status=%q FailureMode=%q; want failed/%s", res.Status, res.FailureMode, tc.wantMode)
			}
			if res.SteeringTriggered {
				t.Errorf("SteeringTriggered = true; unfinished work gets no pull request nudge")
			}
			wantPrompts(t, provider.prompts, continuePrompt)
			if res.BackstopReport == nil || !res.BackstopReport.Pushed || !res.BackstopReport.PRCreated {
				t.Fatalf("BackstopReport = %+v; want the work pushed and a pull request opened", res.BackstopReport)
			}
			if res.PullRequestURL != opened {
				t.Errorf("PullRequestURL = %q; want the backstop's %q", res.PullRequestURL, opened)
			}
			if !strings.Contains(readFile(t, ghArgs), "--head") {
				t.Errorf("gh pr create was not asked to open the session branch's pull request")
			}
		})
	}
}

// TestRun_WorkThatOwesNoPullRequestIsNotContinued pins the gate on the
// continuation: a QA turn that ends WITH a verdict is not continued (its
// contract is a verdict, not a pull request).
func TestRun_WorkThatOwesNoPullRequestIsNotContinued(t *testing.T) {
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "qa",
		repository: followUpRepository,
		turns: []verdictScriptTurn{
			{text: "Findings look good.\nWORK_RESULT: passed\nREVIEW_VERDICT: APPROVE"},
			{text: "never reached"},
		},
	})
	wantPrompts(t, provider.prompts)
	wantContinuations(t, res, 0, 0, false)
}

// TestRun_ReviewTurnWithoutVerdictIsContinued replays the reported failure:
// a review turn that ends on a one-line progress note — no manifest, no
// work-result marker, no review verdict — gets the continuation prompt
// (bounded like implement work), and the final message holds a verdict that
// also appears as the structured field on the turn result.
func TestRun_ReviewTurnWithoutVerdictIsContinued(t *testing.T) {
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "qa",
		repository: followUpRepository,
		turns: []verdictScriptTurn{
			{text: "Proving hook cache-token revert produces GREEN, confirming B1 blocks the change..."},
			{text: "Findings complete.\nWORK_RESULT: passed\nREVIEW_VERDICT: REQUEST_CHANGES"},
		},
	})
	wantPrompts(t, provider.prompts, continueReviewPrompt)
	wantContinuations(t, res, 1, 0, false)
	if res.WorkResult != "passed" {
		t.Fatalf("WorkResult = %q; want passed", res.WorkResult)
	}
	if res.ReviewVerdict != "REQUEST_CHANGES" {
		t.Fatalf("ReviewVerdict = %q; want REQUEST_CHANGES", res.ReviewVerdict)
	}
	if !strings.Contains(res.Summary, "REVIEW_VERDICT: REQUEST_CHANGES") {
		t.Fatalf("Summary = %q; want it to hold the final review verdict", res.Summary)
	}
}

// TestRun_ReviewTurnWithoutVerdictIsContinuedOnReadOnlyCheckout pins the
// 'works on read-only checkouts' half of the continuation change: a review
// turn that ends with no verdict is still continued when the selected
// repository is read-only (the harness only provisions a mutable checkout,
// where the mutable-checkout arm of tailRecoverable would continue it
// anyway). The run uses a session-root-v1 declaration selecting a read-only
// repository, so the continuation must come from the review-work arm.
func TestRun_ReviewTurnWithoutVerdictIsContinuedOnReadOnlyCheckout(t *testing.T) {
	bare := githubRepositoryFixture(t, followUpRepository)
	setPullRef(t, bare, 7, pullAtSessionCommit)
	res, provider := runScriptedSession(t, scriptedSession{
		workType: "qa",
		declaration: &workarea.RepositoryDeclarationV1{
			Protocol: workarea.ProtocolSessionRootV1,
			Repositories: []workarea.DeclaredRepositoryV1{
				{Source: workarea.RepositorySource{Repository: followUpRepository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryReadOnly},
			},
		},
		turns: []verdictScriptTurn{
			{text: "Still reading the diff; no verdict yet."},
			{text: "Findings complete.\nWORK_RESULT: passed\nREVIEW_VERDICT: APPROVE"},
		},
	})
	wantPrompts(t, provider.prompts, continueReviewPrompt)
	wantContinuations(t, res, 1, 0, false)
	if res.WorkResult != "passed" {
		t.Fatalf("WorkResult = %q; want passed", res.WorkResult)
	}
	if res.ReviewVerdict != "APPROVE" {
		t.Fatalf("ReviewVerdict = %q; want APPROVE", res.ReviewVerdict)
	}
}

// TestWaitRetryBackoff_Cancellable pins that the wait before a provider-error
// retry ends as soon as the session's context does.
func TestWaitRetryBackoff_Cancellable(t *testing.T) {
	r := minimalRunner(t)
	r.providerRetryBackoff = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	if err := r.waitRetryBackoff(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitRetryBackoff = %v; want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("waitRetryBackoff returned after %s; want it to end with the context", elapsed)
	}
	r.providerRetryBackoff = func(int) time.Duration { return time.Millisecond }
	if err := r.waitRetryBackoff(context.Background(), 1); err != nil {
		t.Fatalf("waitRetryBackoff with a live context = %v; want nil", err)
	}
	if got := defaultProviderRetryBackoff(3); got != 30*time.Second {
		t.Errorf("defaultProviderRetryBackoff(3) = %s; want 30s", got)
	}
}

func TestSessionPullRequestVerifier_ReportsOwnRepository(t *testing.T) {
	v := &sessionPullRequestVerifier{repository: "acme/widgets", outcomes: map[string]candidateOutcome{
		"https://github.com/acme/widgets/pull/5": candidateRefused,
		"https://github.com/acme/widgets/pull/6": candidateUnconfirmed,
	}}
	turn := func(text string) streamObservation {
		return streamObservation{pullRequestCandidates: appendPullRequestCandidates(nil, text)}
	}
	if !v.reportsOwnRepository(turn("see https://github.com/Acme/Widgets/pull/3")) {
		t.Error("an unchecked own-repository URL (any case) is a reported pull request")
	}
	if !v.reportsOwnRepository(turn("see https://github.com/acme/widgets/pull/6")) {
		t.Error("an own-repository URL the remote could not confirm is a reported pull request")
	}
	if v.reportsOwnRepository(turn("see https://github.com/acme/widgets/pull/5")) {
		t.Error("an own-repository URL confirmed to be someone else's is not")
	}
	if v.reportsOwnRepository(turn("see " + quotedExamplePR)) {
		t.Error("the quoted example on another repository is not")
	}
	v.accepted = "https://github.com/acme/widgets/pull/8"
	v.outcomes[v.accepted] = candidateAccepted
	if v.reportsOwnRepository(turn("opened " + v.accepted)) {
		t.Error("the accepted pull request is judged from the envelope, not reported again")
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
		reviewWork bool
		want       turnEnding
	}{
		{name: "clean end with nothing", turn: clean, want: turnStoppedEarly},
		{name: "provider error", turn: with(func(o *streamObservation) { o.providerError = "503" }), want: turnProviderError},
		{name: "non-retryable provider error ends at once", turn: with(func(o *streamObservation) {
			o.providerError = "400 The request contains invalid parameters"
			o.providerErrorNotRetryable = true
		}), want: turnProviderErrorFatal},
		{name: "a verdict in the turn wins over a non-retryable provider error", turn: with(func(o *streamObservation) {
			o.workResult = "passed"
			o.providerError = "400 The request contains invalid parameters"
			o.providerErrorNotRetryable = true
		}), want: turnFinished},
		{name: "a retryable provider error is retried even after a verdict", turn: with(func(o *streamObservation) {
			o.workResult = "passed"
			o.providerError = "503"
		}), want: turnProviderError},
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
		{name: "structured review verdict ends a review turn", res: func() Result { r := Result{}; r.ReviewVerdict = "APPROVE"; return r }(), turn: clean, reviewWork: true, want: turnFinished},
		{name: "review verdict marker ends a review turn", turn: with(func(o *streamObservation) { o.reviewVerdict = "REQUEST_CHANGES" }), reviewWork: true, want: turnFinished},
		{name: "review verdict does not end implement work", turn: with(func(o *streamObservation) { o.reviewVerdict = "APPROVE" }), want: turnStoppedEarly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.res
			if got := classifyTurnEnding(&res, tc.session, tc.turn, tc.reportedPR, tc.reviewWork); got != tc.want {
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
	nonRetryableEvent := agent.SystemEvent{Subtype: agent.SystemSubtypeProviderError, Message: "400 invalid parameters" + agent.ProviderErrorNotRetryableSuffix}
	providerErr := func(text string) agent.Event {
		return agent.SystemEvent{Subtype: agent.SystemSubtypeProviderError, Message: text}
	}
	cases := []struct {
		name             string
		events           []agent.Event
		want             string
		wantNotRetryable bool
	}{
		{name: "error ends the stream", events: []agent.Event{agent.AssistantTextEvent{Text: "partial"}, errEvent}, want: "503 Service Unavailable"},
		{name: "assistant text after the error", events: []agent.Event{errEvent, agent.AssistantTextEvent{Text: "recovered"}}},
		{name: "tool call after the error", events: []agent.Event{errEvent, agent.ToolUseEvent{ToolName: "bash"}}},
		{name: "other system events keep it", events: []agent.Event{errEvent, agent.SystemEvent{Subtype: "auto_retry_end"}}, want: "503 Service Unavailable"},
		{name: "non-retryable marker strips the suffix and keeps the text", events: []agent.Event{nonRetryableEvent}, want: "400 invalid parameters", wantNotRetryable: true},
		{name: "non-retryable 400 by status", events: []agent.Event{providerErr("400 invalid parameters")}, want: "400 invalid parameters", wantNotRetryable: true},
		{name: "recovery clears a non-retryable error", events: []agent.Event{nonRetryableEvent, agent.AssistantTextEvent{Text: "recovered"}}},
		{name: "429 stays retryable", events: []agent.Event{providerErr("429 Too Many Requests")}, want: "429 Too Many Requests"},
		{name: "a port is not a status", events: []agent.Event{providerErr("connect ECONNREFUSED 10.0.0.12:443")}, want: "connect ECONNREFUSED 10.0.0.12:443"},
		{name: "a context overflow stays retryable even when marked", events: []agent.Event{providerErr("400 prompt is too long" + agent.ProviderErrorNotRetryableSuffix)}, want: "400 prompt is too long"},
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
			if obs.providerErrorNotRetryable != tc.wantNotRetryable {
				t.Fatalf("providerErrorNotRetryable = %v; want %v", obs.providerErrorNotRetryable, tc.wantNotRetryable)
			}
		})
	}
}

// scriptedDraft is a draft read that answers from states in order — one
// per read, the last repeating — and records the URLs it was asked about.
type scriptedDraft struct {
	mu     sync.Mutex
	states []bool
	urls   []string
}

func (d *scriptedDraft) lookup(_ context.Context, _ string, url string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.urls = append(d.urls, url)
	i := min(len(d.urls), len(d.states)) - 1
	return d.states[i], nil
}

func (d *scriptedDraft) reads() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.urls)
}

// reworkBranch is the existing branch a scripted rework run continues; the
// session's pull request (followUpPR) has it as its head.
const reworkBranch = "feature/rework"

// TestRun_DraftPullRequestIsContinued is the reported failure: the brief asked
// for a draft pull request early, and the turn ended on a progress note with
// that draft as its only result. The turn is continued with a prompt that
// names the draft, and once the pull request is ready the work is delivered.
func TestRun_DraftPullRequestIsContinued(t *testing.T) {
	draft := &scriptedDraft{states: []bool{true, false}}
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "development",
		repository: followUpRepository,
		pulls:      map[int]string{7: pullAtSessionCommit},
		draft:      draft.lookup,
		turns: []verdictScriptTurn{
			{toolCalls: 2, text: "Opened draft " + followUpPR + " to keep the work safe. Next I will write the tests."},
			{toolCalls: 2, text: "Tests written; " + followUpPR + " is ready for review."},
		},
	})
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	wantPrompts(t, provider.prompts, continueDraftPrompt)
	wantContinuations(t, res, 1, 0, false)
	if res.SteeringTriggered {
		t.Errorf("SteeringTriggered = true; a draft is continued, not nudged to open a pull request")
	}
	if got := draft.reads(); len(got) != 2 || got[0] != followUpPR || got[1] != followUpPR {
		t.Errorf("draft reads = %q; want the session's pull request read after each turn", got)
	}
}

// TestRun_DraftPullRequestStillDraftIsNotDelivered pins the end of the draft
// path: a session whose pull request is still a draft when its continuations
// run out — or that cannot be continued at all — ends not delivered: failed,
// not passed, the receipt naming the draft.
func TestRun_DraftPullRequestStillDraftIsNotDelivered(t *testing.T) {
	stillDraft := verdictScriptTurn{text: "Draft " + followUpPR + " is open; still working on it."}
	cases := []struct {
		name        string
		limit       int
		wantErr     string
		wantPrompts []string
		want        agent.TurnContinuations
	}{
		{
			name:        "continuations run out",
			wantErr:     "the turn still ended unfinished after 3 consecutive continuation prompts whose turns made no tool call (3 continuation prompts in total): no turn-result manifest and no verdict, and the pull request does not deliver the work: pull request still draft",
			wantPrompts: slices.Repeat([]string{continueDraftPrompt}, DefaultTurnContinuationLimit),
			want:        agent.TurnContinuations{Continued: 3, Unproductive: 3, Limit: DefaultTurnContinuationLimit, Ceiling: DefaultTurnContinuationCeiling, Exhausted: true},
		},
		{
			name:    "continuations disabled",
			limit:   -1,
			wantErr: "the turn ended unfinished and this session takes no continuation prompt: no turn-result manifest and no verdict, and the pull request does not deliver the work: pull request still draft",
			want:    agent.TurnContinuations{Ceiling: DefaultTurnContinuationCeiling, Exhausted: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			draft := &scriptedDraft{states: []bool{true}}
			res, provider := runScriptedSession(t, scriptedSession{
				workType:          "development",
				repository:        followUpRepository,
				pulls:             map[int]string{7: pullAtSessionCommit},
				draft:             draft.lookup,
				continuationLimit: tc.limit,
				turns:             append(slices.Repeat([]verdictScriptTurn{stillDraft}, 4), verdictScriptTurn{text: "never reached"}),
			})
			if res.Status != "failed" || res.FailureMode != FailureContinuationsUnproductive {
				t.Fatalf("Status=%q FailureMode=%q (%s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureContinuationsUnproductive)
			}
			if res.WorkResult == "passed" {
				t.Errorf("WorkResult = passed; a draft pull request is not delivered work")
			}
			if res.Error != tc.wantErr {
				t.Errorf("Error = %q; want %q", res.Error, tc.wantErr)
			}
			if res.PullRequestURL != followUpPR {
				t.Errorf("PullRequestURL = %q; want the session's draft %s kept on the receipt", res.PullRequestURL, followUpPR)
			}
			if res.SteeringTriggered {
				t.Errorf("SteeringTriggered = true; a draft gets no pull request nudge")
			}
			wantPrompts(t, provider.prompts, tc.wantPrompts...)
			if res.TurnContinuations == nil || *res.TurnContinuations != tc.want {
				t.Fatalf("TurnContinuations = %+v; want %+v", res.TurnContinuations, tc.want)
			}
		})
	}
}

// TestRun_ReworkReadyPullRequestWithANewCommitIsAccepted pins the delivered
// rework: the run continues an existing pull request, pushes a new commit to
// it, and reports it ready. Its head differs from the head recorded at run
// start, it is not a draft, and the run completes without a continuation.
func TestRun_ReworkReadyPullRequestWithANewCommitIsAccepted(t *testing.T) {
	draft := &scriptedDraft{states: []bool{false}}
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "development",
		repository: followUpRepository,
		pulls:      map[int]string{7: pullAtSessionCommit},
		ref:        reworkBranch,
		draft:      draft.lookup,
		turns: []verdictScriptTurn{
			{
				files: map[string]string{"fix.txt": "review comments addressed\n"},
				push:  []string{"refs/heads/" + reworkBranch, "refs/pull/7/head"},
				text:  "Pushed the review fixes to " + followUpPR + ".",
			},
			{text: "never reached"},
		},
	})
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	wantPrompts(t, provider.prompts)
	wantContinuations(t, res, 0, 0, false)
	if got := draft.reads(); len(got) != 1 {
		t.Errorf("draft reads = %q; want the pull request's draft state read once, after the new commit was seen", got)
	}
}

// TestRun_ReworkWithoutANewCommitIsContinuedThenNotDelivered pins the
// undelivered rework: the run continues an existing pull request, and its
// turns report that pull request without pushing to it. Each such turn is
// continued with a prompt naming the missing commit, and when the
// continuations run out the run ends not delivered, the receipt saying
// "no new commit". The check reads only the git remote: gh fails every call
// here and is never asked.
func TestRun_ReworkWithoutANewCommitIsContinuedThenNotDelivered(t *testing.T) {
	ghCalls := sentinelGh(t)
	note := verdictScriptTurn{text: "Read the review on " + followUpPR + "; I will fix the comments next."}
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "development",
		repository: followUpRepository,
		pulls:      map[int]string{7: pullAtSessionCommit},
		ref:        reworkBranch,
		draft:      githubPullRequestDraft,
		turns:      append(slices.Repeat([]verdictScriptTurn{note}, 4), verdictScriptTurn{text: "never reached"}),
	})
	if res.Status != "failed" || res.FailureMode != FailureContinuationsUnproductive {
		t.Fatalf("Status=%q FailureMode=%q (%s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureContinuationsUnproductive)
	}
	if res.WorkResult == "passed" {
		t.Errorf("WorkResult = passed; a rework without a new commit is not delivered work")
	}
	if !strings.HasSuffix(res.Error, "the pull request does not deliver the work: no new commit") {
		t.Errorf("Error = %q; want it to end with the reason, no new commit", res.Error)
	}
	wantPrompts(t, provider.prompts, slices.Repeat([]string{continueNoNewCommitPrompt}, DefaultTurnContinuationLimit)...)
	wantContinuations(t, res, DefaultTurnContinuationLimit, 0, true)
	if _, err := os.Stat(ghCalls); !os.IsNotExist(err) {
		t.Errorf("gh was called (%s): the new-commit check must not need it", readFile(t, ghCalls))
	}
}

// TestRun_DraftStateUnknownWhenGhFailsKeepsTheSession pins the fallback: on a
// host where gh cannot answer (absent, unauthenticated, offline), the draft
// state is unknown, and a turn whose only result is the session's pull
// request ends as it did before the draft check — accepted, not continued.
func TestRun_DraftStateUnknownWhenGhFailsKeepsTheSession(t *testing.T) {
	ghCalls := sentinelGh(t)
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "development",
		repository: followUpRepository,
		pulls:      map[int]string{7: pullAtSessionCommit},
		draft:      githubPullRequestDraft,
		turns: []verdictScriptTurn{
			{text: "Opened " + followUpPR + "."},
			{text: "never reached"},
		},
	})
	if res.Status != "completed" || res.PullRequestURL != followUpPR {
		t.Fatalf("Status=%q PullRequestURL=%q (%s: %s); want completed with %s", res.Status, res.PullRequestURL, res.FailureMode, res.Error, followUpPR)
	}
	wantPrompts(t, provider.prompts)
	wantContinuations(t, res, 0, 0, false)
	if calls := readFile(t, ghCalls); !strings.Contains(calls, "pr view "+followUpPR+" --json "+ghPullRequestViewFields) {
		t.Errorf("gh calls = %q; want the draft read through gh pr view", calls)
	}
}

// TestRun_SessionVerdictEndsThePullRequestReRead pins that a verdict the
// agent gave in an earlier turn stands: a later turn without a marker — a
// memory inject or the pull request nudge — does not make the session's pull
// request the only result, so an intentional draft or a rework that needed no
// change completes as it did before the draft check, without a continuation.
func TestRun_SessionVerdictEndsThePullRequestReRead(t *testing.T) {
	note := verdictScriptTurn{text: "Noted; nothing else to do."}
	// Enough unmarked turns for a re-read to run the continuations out.
	notes := slices.Repeat([]verdictScriptTurn{note}, DefaultTurnContinuationLimit+1)
	cases := []struct {
		name         string
		ref          string
		inject       string
		draft        bool
		first        []verdictScriptTurn
		wantSteering bool
	}{
		{
			name:   "rework that needed no change, then a memory inject",
			ref:    reworkBranch,
			inject: "recall: this area was reworked last week",
			first:  []verdictScriptTurn{{text: "No change is needed: " + followUpPR + " already addresses the review.\nWORK_RESULT: passed"}},
		},
		{
			name:   "intentional draft with a verdict, then a memory inject",
			inject: "recall: design reviews happen on draft pull requests",
			draft:  true,
			first:  []verdictScriptTurn{{text: "Opened draft " + followUpPR + "; the task asks for it to stay a draft until the design review.\nWORK_RESULT: passed"}},
		},
		{
			name: "rework verdict, then the pull request nudge",
			ref:  reworkBranch,
			first: []verdictScriptTurn{
				{text: "The review comments are already addressed on the branch.\nWORK_RESULT: passed"},
				{text: "The pull request is " + followUpPR + "."},
			},
			wantSteering: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			draft := &scriptedDraft{states: []bool{tc.draft}}
			res, provider := runScriptedSession(t, scriptedSession{
				workType:   "development",
				repository: followUpRepository,
				pulls:      map[int]string{7: pullAtSessionCommit},
				ref:        tc.ref,
				inject:     tc.inject,
				draft:      draft.lookup,
				turns:      append(slices.Clone(tc.first), notes...),
			})
			if res.Status != "completed" || res.PullRequestURL != followUpPR || res.WorkResult != "passed" {
				t.Fatalf("Status=%q PullRequestURL=%q WorkResult=%q (%s: %s); want completed and passed with %s",
					res.Status, res.PullRequestURL, res.WorkResult, res.FailureMode, res.Error, followUpPR)
			}
			if res.SteeringTriggered != tc.wantSteering {
				t.Errorf("SteeringTriggered = %v; want %v", res.SteeringTriggered, tc.wantSteering)
			}
			for _, prompt := range provider.prompts {
				if prompt == continueDraftPrompt || prompt == continueNoNewCommitPrompt || prompt == continuePrompt {
					t.Errorf("sent the continuation %q after the session gave a verdict", prompt)
				}
			}
			wantContinuations(t, res, 0, 0, false)
			if got := draft.reads(); len(got) != 0 {
				t.Errorf("draft reads = %q; want none after a verdict", got)
			}
		})
	}
}

// TestRun_IntentionalDraftVerdictAfterTheDraftPromptCompletes pins the way
// out the draft prompt offers: a task that deliberately keeps its pull
// request a draft says so and reports its turn result, and the session
// completes instead of being continued to its bound.
func TestRun_IntentionalDraftVerdictAfterTheDraftPromptCompletes(t *testing.T) {
	for _, want := range []string{"deliberately needs the pull request to stay a draft", "report your turn result (WORK_RESULT)"} {
		if !strings.Contains(continueDraftPrompt, want) {
			t.Fatalf("continueDraftPrompt does not offer the intentional-draft way out (%q):\n%s", want, continueDraftPrompt)
		}
	}
	draft := &scriptedDraft{states: []bool{true}}
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "development",
		repository: followUpRepository,
		pulls:      map[int]string{7: pullAtSessionCommit},
		draft:      draft.lookup,
		turns: []verdictScriptTurn{
			{text: "Opened draft " + followUpPR + "; writing the design notes next."},
			{text: "The task asks for " + followUpPR + " to stay a draft until the design review.\nWORK_RESULT: passed"},
			{text: "never reached"},
		},
	})
	if res.Status != "completed" || res.PullRequestURL != followUpPR || res.WorkResult != "passed" {
		t.Fatalf("Status=%q PullRequestURL=%q WorkResult=%q (%s: %s); want completed and passed with %s",
			res.Status, res.PullRequestURL, res.WorkResult, res.FailureMode, res.Error, followUpPR)
	}
	wantPrompts(t, provider.prompts, continueDraftPrompt)
	wantContinuations(t, res, 1, 0, false)
}

// TestPullRequestIsTheOnlyResult pins when the session's pull request is
// re-read: only when, without it, the turn would have stopped early.
func TestPullRequestIsTheOnlyResult(t *testing.T) {
	clean := streamObservation{terminalSuccess: true, terminalEvent: &agent.ResultEvent{Success: true}}
	withPR := func(mutate func(*Result)) Result {
		r := Result{}
		r.PullRequestURL = followUpPR
		if mutate != nil {
			mutate(&r)
		}
		return r
	}
	cases := []struct {
		name       string
		res        Result
		turn       streamObservation
		reportedPR bool
		want       bool
	}{
		{name: "pull request and a progress note", res: withPR(nil), turn: clean, want: true},
		{name: "no pull request", turn: clean},
		{name: "turn-result manifest", res: withPR(func(r *Result) { r.Manifest = &TurnManifest{Verdict: "passed"} }), turn: clean},
		{name: "verdict in the turn", res: withPR(nil), turn: func() streamObservation { o := clean; o.workResult = "passed"; return o }()},
		{name: "another own-repository pull request reported", res: withPR(nil), turn: clean, reportedPR: true},
		{name: "provider error", res: withPR(nil), turn: func() streamObservation { o := clean; o.providerError = "503"; return o }()},
		{name: "runner-recorded failure", res: withPR(func(r *Result) { r.FailureMode = FailureProviderError }), turn: clean},
		{name: "verdict from an earlier turn", res: withPR(func(r *Result) { r.WorkResult = "passed" }), turn: clean},
		{name: "unknown verdict from an earlier turn", res: withPR(func(r *Result) { r.WorkResult = "unknown" }), turn: clean},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.res
			if got := pullRequestIsTheOnlyResult(&res, streamObservation{}, tc.turn, tc.reportedPR, false); got != tc.want {
				t.Fatalf("pullRequestIsTheOnlyResult = %v; want %v", got, tc.want)
			}
			if res.PullRequestURL != tc.res.PullRequestURL {
				t.Fatalf("PullRequestURL changed to %q", res.PullRequestURL)
			}
		})
	}
}
