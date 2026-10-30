// Package mergegroup records and enforces the merge-queue flake policy.
//
// Decision: no-retry. The merge-group test jobs run once and never retry.
// Causal repairs already address the known flakes: the test job is pinned to
// a green runner image, the confinement jobs refuse to pass when their live
// tests skip, and the suite pipes through a skip summary so a quietly
// skipping test cannot stay green. A retry would rerun a deterministic
// failure, consume merge-queue capacity, and hide the signal the queue
// exists to provide. Required coverage stays in its own job; relocating it
// requires updating the inventory below and the workflow text together.
package mergegroup

import (
	"fmt"
	"regexp"
	"strings"
)

// Decision is the recorded flake-policy choice. The workflow check fails
// when the verified workflow stops carrying this marker.
const Decision = "no-retry"

// MaxAttempts is the hard attempt bound every event gets under no-retry.
const MaxAttempts = 1

// Event names mirror the workflow trigger names.
const (
	EventPush        = "push"
	EventPullRequest = "pull_request"
	EventMergeGroup  = "merge_group"
)

// Entry is one inventory row: the job that must keep its coverage.
type Entry struct {
	// Job is the workflow job name holding the coverage.
	Job string
	// Snippet is a verbatim fragment of the coverage step the workflow
	// must keep inside Job. Relocating the step without updating the
	// inventory fails the check.
	Snippet string
}

// Inventory lists every merge-group test job and the coverage that must
// stay inside it.
var Inventory = []Entry{
	{Job: "test", Snippet: "go test -race -coverprofile=coverage.out"},
	{Job: "macos-confinement", Snippet: "live_darwin_test.go"},
	{Job: "linux-confinement", Snippet: "live_linux_test.go"},
}

// Marker is the verbatim decision line the verified workflow must carry.
const Marker = "flake-policy-decision: no-retry"

// MaxAttemptsFor returns the attempt bound for an event. Under no-retry
// every event, including merge_group, runs once: no event gains retries.
func MaxAttemptsFor(event string) int {
	_ = event
	return MaxAttempts
}

// AllowsRetry reports whether an event may retry a failed attempt.
// Under no-retry it is always false.
func AllowsRetry(event string) bool {
	_ = event
	return false
}

// Resolve applies the no-retry bound to an injected exit sequence (0 is
// pass, anything else is fail). It consumes at most one entry, so a
// fail/pass flake still reports failure instead of being masked by a
// second attempt.
func Resolve(exits []int) (succeeded bool, attempts int) {
	if len(exits) == 0 {
		return false, 0
	}
	return exits[0] == 0, 1
}

var (
	continueOnErrorRe = regexp.MustCompile(`(?i)continue-on-error\s*:\s*true`)
	maxAttemptsRe     = regexp.MustCompile(`(?i)max-attempts\s*:`)
	retryActionRe     = regexp.MustCompile(`(?i)uses\s*:\s*\S*retr(y|ies)`)
	jobHeaderRe       = regexp.MustCompile(`^  [A-Za-z0-9_.-]+:`)
)

// jobBlock returns one job's text; coverage must live inside its own job.
func jobBlock(text, job string) (string, bool) {
	lines := strings.Split(text, "\n")
	header := "  " + job + ":"
	start := -1
	for i, line := range lines {
		if line == header || strings.HasPrefix(line, header+" ") {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if jobHeaderRe.MatchString(lines[i]) {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

// stripComments drops full-line and trailing `#` comments so prose about
// retries in comments never reads as a retry construct.
func stripComments(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// ValidateWorkflow verifies the documented decision and the required
// coverage for every inventory entry. It returns nil only when the
// workflow carries the decision marker, keeps the merge_group trigger,
// keeps each snippet, and adds no retry construct.
func ValidateWorkflow(text string) error {
	if !strings.Contains(text, Marker) {
		return fmt.Errorf("missing decision marker %q", Marker)
	}
	if !strings.Contains(text, "merge_group:") {
		return fmt.Errorf("workflow must keep the merge_group trigger")
	}
	for _, e := range Inventory {
		block, ok := jobBlock(text, e.Job)
		if !ok {
			return fmt.Errorf("inventory job %q is missing", e.Job)
		}
		if !strings.Contains(block, e.Snippet) {
			return fmt.Errorf("inventory job %q lost its coverage %q", e.Job, e.Snippet)
		}
	}
	code := stripComments(text)
	if continueOnErrorRe.FindString(code) != "" {
		return fmt.Errorf("retry forbidden: workflow adds continue-on-error")
	}
	if maxAttemptsRe.FindString(code) != "" {
		return fmt.Errorf("retry forbidden: workflow adds max-attempts")
	}
	if retryActionRe.FindString(code) != "" {
		return fmt.Errorf("retry forbidden: workflow adds a retry action")
	}
	return nil
}
