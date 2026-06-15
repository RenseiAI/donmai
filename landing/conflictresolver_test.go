package landing

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func makeConflictContext(overrides func(*ConflictContext)) ConflictContext {
	c := ConflictContext{
		RepoPath:      "/repo",
		WorktreePath:  "/repo/.worktrees/wt-1",
		SourceBranch:  "feature/x",
		TargetBranch:  "main",
		Proposal:      42,
		IssueID:       "TASK-1",
		ConflictFiles: []string{"src/index.ts", "src/utils.ts"},
	}
	if overrides != nil {
		overrides(&c)
	}
	return c
}

// markerRouter routes grep conflict-marker checks per file (true = has markers),
// optionally fails `git rebase --continue`, and serves a `git diff` body.
func markerRouter(fileHasMarkers map[string]bool, rebaseErr error, diffOut string) func(string, []string) (string, error) {
	return func(name string, args []string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		switch {
		case name == "grep":
			// The last arg is the file being checked.
			file := args[len(args)-1]
			if fileHasMarkers[file] {
				return "3", nil // count > 0 ⇒ has markers
			}
			// grep exits non-zero when no matches ⇒ no markers.
			return "", errors.New("grep: exit code 1")
		case strings.Contains(line, "rebase --continue"):
			if rebaseErr != nil {
				return "", rebaseErr
			}
			return "", nil
		case strings.Contains(line, "git diff"):
			if diffOut != "" {
				return diffOut, nil
			}
			return "diff --git a/file\n+change", nil
		default:
			return "", nil // git add etc. succeed
		}
	}
}

func TestConflictResolver_Resolve_MergirafDisabled_GoesToEscalation(t *testing.T) {
	fr := &fakeRunner{reply: func(string, []string) (string, error) { return "", nil }}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: false, EscalationStrategy: EscalationNotify}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(nil))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if res.Method != methodEscalation {
		t.Errorf("Method = %q, want %q", res.Method, methodEscalation)
	}
	if res.EscalationAction != EscalationNotify {
		t.Errorf("EscalationAction = %q, want %q", res.EscalationAction, EscalationNotify)
	}
	for _, line := range fr.commandLines() {
		if strings.HasPrefix(line, "grep") {
			t.Errorf("mergiraf disabled but grep was called: %q", line)
		}
	}
}

func TestConflictResolver_Resolve_AllFilesResolved(t *testing.T) {
	fr := &fakeRunner{reply: markerRouter(map[string]bool{
		"src/index.ts": false,
		"src/utils.ts": false,
	}, nil, "")}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: true, EscalationStrategy: EscalationNotify}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(nil))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if res.Status != ResolutionResolved {
		t.Errorf("Status = %q, want %q", res.Status, ResolutionResolved)
	}
	if res.Method != methodMergiraf {
		t.Errorf("Method = %q, want %q", res.Method, methodMergiraf)
	}
	if !equalStrings(res.ResolvedFiles, []string{"src/index.ts", "src/utils.ts"}) {
		t.Errorf("ResolvedFiles = %v, want [src/index.ts src/utils.ts]", res.ResolvedFiles)
	}
	if res.UnresolvedFiles != nil {
		t.Errorf("UnresolvedFiles = %v, want nil", res.UnresolvedFiles)
	}

	lines := fr.commandLines()
	if n := countPrefix(lines, "git add"); n != 2 {
		t.Errorf("git add calls = %d, want 2", n)
	}
	if n := countContains(lines, "rebase --continue"); n != 1 {
		t.Errorf("rebase --continue calls = %d, want 1", n)
	}
	// GIT_EDITOR=true must be set on the rebase --continue call.
	if !rebaseHasEditorEnv(fr) {
		t.Error("rebase --continue did not carry GIT_EDITOR=true")
	}
}

func TestConflictResolver_Resolve_PartialResolutionEscalates(t *testing.T) {
	fr := &fakeRunner{reply: markerRouter(map[string]bool{
		"src/index.ts": false, // resolved
		"src/utils.ts": true,  // unresolved
	}, nil, "")}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: true, EscalationStrategy: EscalationNotify}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(nil))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if res.Status != ResolutionEscalated {
		t.Errorf("Status = %q, want %q", res.Status, ResolutionEscalated)
	}
	if res.Method != methodEscalation {
		t.Errorf("Method = %q, want %q", res.Method, methodEscalation)
	}
	if res.EscalationAction != EscalationNotify {
		t.Errorf("EscalationAction = %q, want %q", res.EscalationAction, EscalationNotify)
	}
	// Only the unresolved file is forwarded.
	if !equalStrings(res.UnresolvedFiles, []string{"src/utils.ts"}) {
		t.Errorf("UnresolvedFiles = %v, want [src/utils.ts]", res.UnresolvedFiles)
	}
}

func TestConflictResolver_Resolve_NoneResolved_Parks(t *testing.T) {
	fr := &fakeRunner{reply: markerRouter(map[string]bool{
		"src/index.ts": true,
		"src/utils.ts": true,
	}, nil, "")}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: true, EscalationStrategy: EscalationPark}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(nil))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if res.Status != ResolutionParked {
		t.Errorf("Status = %q, want %q", res.Status, ResolutionParked)
	}
	if res.EscalationAction != EscalationPark {
		t.Errorf("EscalationAction = %q, want %q", res.EscalationAction, EscalationPark)
	}
	if !equalStrings(res.UnresolvedFiles, []string{"src/index.ts", "src/utils.ts"}) {
		t.Errorf("UnresolvedFiles = %v, want both", res.UnresolvedFiles)
	}
}

func TestConflictResolver_Resolve_RebaseContinueFailure(t *testing.T) {
	fr := &fakeRunner{reply: markerRouter(map[string]bool{
		"src/index.ts": false,
		"src/utils.ts": false,
	}, errors.New("rebase failed: could not apply commit"), "")}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: true, EscalationStrategy: EscalationNotify}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(nil))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	// mergiraf reports escalated (rebase failed) → resolve falls through to notify.
	if res.Status != ResolutionEscalated {
		t.Errorf("Status = %q, want %q", res.Status, ResolutionEscalated)
	}
	if res.Method != methodEscalation {
		t.Errorf("Method = %q, want %q", res.Method, methodEscalation)
	}
	if res.EscalationAction != EscalationNotify {
		t.Errorf("EscalationAction = %q, want %q", res.EscalationAction, EscalationNotify)
	}
	// The full original set is forwarded after a rebase --continue failure.
	if !equalStrings(res.UnresolvedFiles, []string{"src/index.ts", "src/utils.ts"}) {
		t.Errorf("UnresolvedFiles = %v, want both", res.UnresolvedFiles)
	}
}

func TestConflictResolver_Escalation_Reassign_IncludesDiff(t *testing.T) {
	diff := "diff --git a/src/index.ts\n+++ b/src/index.ts\n@@ conflict @@"
	fr := &fakeRunner{reply: markerRouter(nil, nil, diff)}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: false, EscalationStrategy: EscalationReassign}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(nil))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if res.Status != ResolutionEscalated || res.Method != methodEscalation || res.EscalationAction != EscalationReassign {
		t.Fatalf("got status=%q method=%q action=%q", res.Status, res.Method, res.EscalationAction)
	}
	for _, want := range []string{"TASK-1", "PR #42", "Agent should resolve and re-submit", "Diff:", diff} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message missing %q; message=%q", want, res.Message)
		}
	}
}

func TestConflictResolver_Escalation_Reassign_TruncatesDiff(t *testing.T) {
	longDiff := strings.Repeat("x", 6000)
	fr := &fakeRunner{reply: markerRouter(nil, nil, longDiff)}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: false, EscalationStrategy: EscalationReassign}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(nil))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	idx := strings.Index(res.Message, "Diff:\n")
	if idx < 0 {
		t.Fatalf("message has no Diff: section; message=%q", res.Message)
	}
	diffPortion := res.Message[idx+len("Diff:\n"):]
	if len(diffPortion) > diffTruncateLimit {
		t.Errorf("diff portion length = %d, want <= %d", len(diffPortion), diffTruncateLimit)
	}
}

func TestConflictResolver_Escalation_Reassign_DiffFailure(t *testing.T) {
	fr := &fakeRunner{reply: func(name string, args []string) (string, error) {
		if name == "git" && len(args) > 0 && args[0] == "diff" {
			return "", errors.New("diff failed")
		}
		return "", nil
	}}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: false, EscalationStrategy: EscalationReassign}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(nil))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if res.EscalationAction != EscalationReassign {
		t.Errorf("EscalationAction = %q, want reassign", res.EscalationAction)
	}
	if !strings.Contains(res.Message, "(unable to generate diff)") {
		t.Errorf("message = %q, want it to contain the diff-failure placeholder", res.Message)
	}
}

func TestConflictResolver_Escalation_Notify_Message(t *testing.T) {
	fr := &fakeRunner{}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: false, EscalationStrategy: EscalationNotify}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(nil))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	for _, want := range []string{"Merge conflict on TASK-1 PR #42", "src/index.ts", "src/utils.ts"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message missing %q; message=%q", want, res.Message)
		}
	}
}

func TestConflictResolver_Escalation_Park_Message(t *testing.T) {
	fr := &fakeRunner{}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: false, EscalationStrategy: EscalationPark}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(nil))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if res.Status != ResolutionParked {
		t.Errorf("Status = %q, want parked", res.Status)
	}
	for _, want := range []string{"PR #42 parked", "auto-retry"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message missing %q; message=%q", want, res.Message)
		}
	}
}

func TestConflictResolver_UnknownStrategyDefaultsToNotify(t *testing.T) {
	fr := &fakeRunner{}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: false, EscalationStrategy: EscalationStrategy("bogus")}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(nil))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if res.EscalationAction != EscalationNotify {
		t.Errorf("EscalationAction = %q, want notify (default)", res.EscalationAction)
	}
}

func TestConflictResolver_EmptyConflictFiles(t *testing.T) {
	fr := &fakeRunner{}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: false, EscalationStrategy: EscalationNotify}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(func(c *ConflictContext) { c.ConflictFiles = nil }))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if res.Status != ResolutionEscalated {
		t.Errorf("Status = %q, want escalated", res.Status)
	}
	if len(res.UnresolvedFiles) != 0 {
		t.Errorf("UnresolvedFiles = %v, want empty", res.UnresolvedFiles)
	}
}

func TestConflictResolver_SingleFileResolved(t *testing.T) {
	fr := &fakeRunner{reply: markerRouter(map[string]bool{"src/index.ts": false}, nil, "")}
	r := &ConflictResolver{cfg: ConflictResolverConfig{MergirafEnabled: true, EscalationStrategy: EscalationNotify}, runner: fr}

	res, err := r.Resolve(context.Background(), makeConflictContext(func(c *ConflictContext) { c.ConflictFiles = []string{"src/index.ts"} }))
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if res.Status != ResolutionResolved {
		t.Errorf("Status = %q, want resolved", res.Status)
	}
	if !equalStrings(res.ResolvedFiles, []string{"src/index.ts"}) {
		t.Errorf("ResolvedFiles = %v, want [src/index.ts]", res.ResolvedFiles)
	}
}

// --- helpers ---

func countPrefix(lines []string, prefix string) int {
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func countContains(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func rebaseHasEditorEnv(fr *fakeRunner) bool {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	for _, c := range fr.calls {
		if strings.Contains(c.commandLine(), "rebase --continue") {
			for _, e := range c.extraEnv {
				if e == "GIT_EDITOR=true" {
					return true
				}
			}
		}
	}
	return false
}
