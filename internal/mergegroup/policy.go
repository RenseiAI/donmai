// Package mergegroup records and enforces the merge-queue flake policy.
// Decision: no-retry. Merge-group test jobs run once and never retry.
package mergegroup

import (
	"fmt"
	"regexp"
	"strings"
)

// Decision is the recorded flake-policy choice.
// MaxAttempts is the hard attempt bound every event gets under no-retry.
const (
	Decision    = "no-retry"
	MaxAttempts = 1
)

// Event names mirror the workflow trigger names.
const (
	EventPush        = "push"
	EventPullRequest = "pull_request"
	EventMergeGroup  = "merge_group"
)

// Entry is one inventory row: the job that must keep its coverage.
type Entry struct {
	Job     string
	Snippet string
}

// Inventory lists every merge-group test job and the coverage that must stay inside it.
var Inventory = []Entry{
	{Job: "test", Snippet: "go test -race -coverprofile=coverage.out"},
	{Job: "macos-confinement", Snippet: "live_darwin_test.go"},
	{Job: "linux-confinement", Snippet: "live_linux_test.go"},
}

// Marker is the verbatim decision line the verified workflow must carry.
const Marker = "flake-policy-decision: no-retry"

// MaxAttemptsFor returns the attempt bound for an event.
func MaxAttemptsFor(event string) int {
	_ = event
	return MaxAttempts
}

// AllowsRetry reports whether an event may retry a failed attempt.
func AllowsRetry(event string) bool {
	_ = event
	return false
}

// Resolve applies the no-retry bound to an injected exit sequence.
func Resolve(exits []int) (succeeded bool, attempts int) {
	if len(exits) == 0 {
		return false, 0
	}
	return exits[0] == 0, 1
}

var (
	maxAttemptsRe = regexp.MustCompile(`(?i)max-attempts\s*:`)
	retryActionRe = regexp.MustCompile(`(?i)uses\s*:\s*\S*retr(y|ies)`)
	jobHeaderRe   = regexp.MustCompile(`^  [A-Za-z0-9_.-]+:`)
	shellLoopRe   = regexp.MustCompile(`(?i)\bfor\s+\S+\s+in\b[^\n]*\bdo\b|\bwhile\b[^\n]*\bdo\b|\buntil\b[^\n]*\bdo\b`)
)

func jobBlock(text, job string) (string, bool) {
	lines := strings.Split(text, "\n")
	header := "  " + job + ":"
	start := -1
	for i, line := range lines {
		if line == header || strings.HasPrefix(line, header+" ") || strings.HasPrefix(line, header+"\t") {
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

func stripComments(text string) string {
	kept := make([]string, 0, len(text)/40+1)
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func activeTrigger(code string) bool {
	lines := strings.Split(code, "\n")
	on, jobs := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case "on:":
			on = i
		case "jobs:":
			jobs = i
		}
		if on >= 0 && jobs > on {
			break
		}
	}
	if on < 0 {
		return false
	}
	if jobs < 0 {
		jobs = len(lines)
	}
	for _, l := range lines[on+1 : jobs] {
		t := strings.TrimSpace(l)
		if t == "merge_group:" || strings.HasPrefix(t, "merge_group: ") || strings.HasPrefix(t, "merge_group:\t") {
			return true
		}
	}
	return false
}

func hasContinueOnError(code string) bool {
	for _, l := range strings.Split(code, "\n") {
		ll := strings.ToLower(l)
		i := strings.Index(ll, "continue-on-error")
		if i < 0 {
			continue
		}
		rest := ll[i+len("continue-on-error"):]
		j := strings.Index(rest, ":")
		if j < 0 {
			continue
		}
		v := strings.TrimSpace(rest[j+1:])
		v = strings.Trim(v, `"' `+"\t")
		if v == "false" {
			continue
		}
		return true
	}
	return false
}

func stepChunks(code string) []string {
	var chunks []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			chunks = append(chunks, strings.Join(cur, "\n"))
		}
		cur = nil
	}
	for _, l := range strings.Split(code, "\n") {
		if strings.HasPrefix(l, "      - ") {
			flush()
		}
		cur = append(cur, l)
	}
	flush()
	return chunks
}

func snippetActive(block, snippet string) bool {
	for _, s := range stepChunks(block) {
		if !strings.Contains(s, "run:") || !strings.Contains(s, snippet) {
			continue
		}
		for _, l := range strings.Split(s, "\n") {
			if !strings.Contains(l, snippet) {
				continue
			}
			ll := strings.ToLower(l)
			if strings.Contains(ll, "echo") || strings.Contains(ll, "printf") {
				continue
			}
			return true
		}
	}
	return false
}

func hasShellRetry(code string) bool {
	for _, s := range stepChunks(code) {
		if !strings.Contains(s, "go test") {
			continue
		}
		if shellLoopRe.MatchString(s) {
			return true
		}
		if strings.Count(s, "go test") > 1 {
			return true
		}
		if strings.Contains(s, "||") {
			return true
		}
		ll := strings.ToLower(s)
		if strings.Contains(ll, "retry") || strings.Contains(ll, "attempt") {
			if strings.Contains(s, "go test") {
				return true
			}
		}
		if strings.Contains(s, "exit 0") {
			return true
		}
	}
	return false
}

// ValidateWorkflow verifies the documented decision and required coverage.
func ValidateWorkflow(text string) error {
	if !strings.Contains(text, Marker) {
		return fmt.Errorf("missing decision marker %q", Marker)
	}
	code := stripComments(text)
	if !activeTrigger(code) {
		return fmt.Errorf("workflow must keep the active merge_group trigger")
	}
	for _, e := range Inventory {
		block, ok := jobBlock(code, e.Job)
		if !ok {
			return fmt.Errorf("inventory job %q is missing", e.Job)
		}
		if !snippetActive(block, e.Snippet) {
			return fmt.Errorf("inventory job %q lost its active coverage %q", e.Job, e.Snippet)
		}
	}
	if hasContinueOnError(code) {
		return fmt.Errorf("retry forbidden: workflow adds continue-on-error")
	}
	if maxAttemptsRe.FindString(code) != "" {
		return fmt.Errorf("retry forbidden: workflow adds max-attempts")
	}
	if retryActionRe.FindString(code) != "" {
		return fmt.Errorf("retry forbidden: workflow adds a retry action")
	}
	if hasShellRetry(code) {
		return fmt.Errorf("retry forbidden: workflow adds a shell retry around tests")
	}
	return nil
}
