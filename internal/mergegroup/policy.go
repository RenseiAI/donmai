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
	// Deny-by-default rerun guards. Any job- or step-level `if:` that
	// runs on failure()/always() is a rerun gate however it is spelled,
	// and any `set +` disarms errexit wherever it sits. Both are judged
	// on the raw lines, not on whether a test command shares the chunk.
	rerunCondRe     = regexp.MustCompile(`(?i)^\s*if\s*:.*(failure\(\)|always\(\))`)
	setPlusRe       = regexp.MustCompile(`set\s+\+`)
	jobHeaderRe     = regexp.MustCompile(`^  [A-Za-z0-9_.-]+:`)
	shellLoopRe     = regexp.MustCompile(`(?i)\bfor\s+\S+\s+in\b[^\n]*\bdo\b|\bwhile\b[^\n]*\bdo\b|\buntil\b[^\n]*\bdo\b`)
	semicolonTrueRe = regexp.MustCompile(`(?i);\s*(/bin/true|/usr/bin/true|true)\b`)
	semicolonNoopRe = regexp.MustCompile(`;\s*:(?:\s|;|$)`)
	andTrueRe       = regexp.MustCompile(`(?i)&&\s*(/bin/true|/usr/bin/true|true)\b`)
	exitZeroRe      = regexp.MustCompile(`(?i)\bexit\s+0\b`)
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
		// Split on job headers as well as steps so a chunk never spans
		// two jobs. Without this, a header line (e.g. max-attempts)
		// glues to the previous job's go test and trips the wrong guard.
		if jobHeaderRe.MatchString(l) {
			flush()
		} else if strings.HasPrefix(l, "      - ") {
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

// stripQuoted removes single- and double-quoted segments so a -run
// 'a|b' filter alternation is not mistaken for a shell pipe.
func stripQuoted(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var q byte
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !in && (c == '\'' || c == '"') {
			in = true
			q = c
			continue
		}
		if in && c == q {
			in = false
			continue
		}
		if in {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// chunkHasUnprotectedPipe reports a shell pipe in a test chunk without
// pipefail. The YAML `run: |` block marker is not a shell pipe, so the
// run header carrying only that marker is skipped; quoted -run filters
// are stripped before scanning.
func chunkHasUnprotectedPipe(s string) bool {
	if strings.Contains(strings.ToLower(s), "pipefail") {
		return false
	}
	for _, line := range strings.Split(s, "\n") {
		stripped := stripQuoted(line)
		trim := strings.TrimSpace(stripped)
		if strings.HasPrefix(trim, "run:") {
			after := strings.TrimSpace(strings.TrimPrefix(trim, "run:"))
			if after == "|" || after == "|-" || after == "|+" || after == "" {
				continue
			}
			tmp := strings.ReplaceAll(after, "||", "")
			if strings.Contains(tmp, "|") {
				return true
			}
			continue
		}
		tmp := strings.ReplaceAll(stripped, "||", "")
		if strings.Contains(tmp, "|") {
			return true
		}
	}
	return false
}

// hasConfinementRun requires the live test execution itself, not just the
// guard grep that names the live file. Deleting the go test invocation
// while keeping the grep must fail the check.
func hasConfinementRun(block string) bool {
	for _, s := range stepChunks(block) {
		if !strings.Contains(s, "run:") {
			continue
		}
		var kept []string
		for _, l := range strings.Split(s, "\n") {
			ll := strings.ToLower(l)
			if strings.Contains(ll, "echo") || strings.Contains(ll, "printf") {
				continue
			}
			kept = append(kept, l)
		}
		joined := strings.Join(kept, "\n")
		if strings.Contains(joined, "go test") && strings.Contains(joined, "runtime/confinement") {
			return true
		}
	}
	return false
}

func hasShellRetry(code string) bool {
	for _, s := range stepChunks(code) {
		hasGoTest := strings.Contains(s, "go test")
		hasMakeTest := strings.Contains(s, "make test")
		// Cross-step retry: a rerun-conditioned step re-executes the
		// suite, whether it spells `go test` or the `make test` wrapper
		// (which runs `go test -race ./...`). `cancelled()` does not run
		// after a failure, so it is not a retry.
		ll := strings.ToLower(s)
		if strings.Contains(ll, "failure()") || strings.Contains(ll, "always()") {
			if hasGoTest || hasMakeTest {
				return true
			}
		}
		if !hasGoTest && !hasMakeTest {
			continue
		}
		if shellLoopRe.MatchString(s) {
			return true
		}
		if hasGoTest && strings.Count(s, "go test") > 1 {
			return true
		}
		if strings.Contains(s, "||") {
			return true
		}
		if strings.Contains(ll, "retry") || strings.Contains(ll, "attempt") {
			return true
		}
		// Exit-zero masking in any whitespace or path spelling.
		if exitZeroRe.FindString(s) != "" {
			return true
		}
		// Exit-zero masking: "; true" / "; :" / "&& true" (and the
		// /bin/true and /usr/bin/true spellings) after the test run
		// always succeeds, concealing a deterministic failure.
		if semicolonTrueRe.FindString(s) != "" {
			return true
		}
		if semicolonNoopRe.FindString(s) != "" {
			return true
		}
		if andTrueRe.FindString(s) != "" {
			return true
		}
		// A single pipe without pipefail masks the test exit status
		// even under `set -e`.
		if chunkHasUnprotectedPipe(s) {
			return true
		}
		// Disarming errexit in a test chunk lets a trailing
		// `; true` or `; exit 0` mask a failure.
		if strings.Contains(ll, "set +e") || strings.Contains(ll, "set +o errexit") {
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
		if (e.Job == "macos-confinement" || e.Job == "linux-confinement") && !hasConfinementRun(block) {
			return fmt.Errorf("inventory job %q lost its live test execution", e.Job)
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
	for _, l := range strings.Split(code, "\n") {
		if rerunCondRe.MatchString(l) {
			return fmt.Errorf("retry forbidden: workflow adds a rerun condition")
		}
	}
	if setPlusRe.FindString(code) != "" {
		return fmt.Errorf("retry forbidden: workflow disarms errexit")
	}
	if hasShellRetry(code) {
		return fmt.Errorf("retry forbidden: workflow adds a shell retry around tests")
	}
	return nil
}
