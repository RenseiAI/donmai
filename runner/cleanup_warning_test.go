package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/testexec"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

// A completed turn followed by a cleanup warning stays completed: the warning
// is observability, never a failure, and it never triggers a backstop that
// would replace the agent's own pull request.
func TestRun_SuccessWithCleanupWarningStaysCompleted(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	hp, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider does not satisfy HarnessProvider")
	}
	ch := make(chan agent.Event, 8)
	ch <- agent.InitEvent{SessionID: "cleanup-warning"}
	ch <- agent.AssistantTextEvent{Text: "done\nWORK_RESULT: passed\n"}
	ch <- agent.SystemEvent{
		Subtype: "codex_warning",
		Message: "codex isolated MCP configuration cleanup failed after the turn completed; the app-server was stopped and this provider refuses new sessions: clear isolated MCP config: isolated config activation verification failed",
	}
	ch <- agent.ResultEvent{Success: true}
	close(ch)
	provider := &evidenceProvider{HarnessProvider: hp, spawnHandle: &outputCapHandle{events: ch}}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	qw := h.queuedWork("CLEANUP-WARNING")
	qw.WorkType = WorkTypeQAStr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (mode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	if res.FailureMode != "" {
		t.Fatalf("FailureMode = %q; want empty (warning is not a failure)", res.FailureMode)
	}
	if res.WorkResult != "passed" {
		t.Fatalf("WorkResult = %q; want passed", res.WorkResult)
	}
	if res.BackstopReport != nil {
		t.Fatalf("BackstopReport = %+v; want nil (no backstop on a completed run)", res.BackstopReport)
	}
}

// When the agent already opened its own pull request, the backstop recovers
// that URL and never edits the title or description: a recording gh stub
// fails the test if `pr edit` is ever invoked.
func TestRunBackstop_KeepsAgentPRTitleAndBody(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	remote := t.TempDir()
	//nolint:gosec // G204: test fixture, args are hard-coded literals.
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	repo := t.TempDir()
	gitInit(t, repo)
	//nolint:gosec // G204: test fixture, args are hard-coded literals.
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", remote).CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", out, err)
	}
	checkout(t, repo, "feature/agent-pr")
	writeFile(t, repo, "src/fix.go", "package fix\n")

	const wantURL = "https://github.com/example/project/pull/99"
	bin := t.TempDir()
	callsFile := filepath.Join(bin, "calls")
	script := `#!/bin/sh
dir="${0%/*}"
printf '%s\n' "$@" >> "$dir/calls"
if [ "$1" = "repo" ]; then
  printf 'PUBLIC\n'
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "create" ]; then
  printf 'a pull request for branch "feature/agent-pr" into branch "main" already exists:\n` + wantURL + `\n' 1>&2
  exit 1
fi
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  printf '{"number":99,"url":"` + wantURL + `","baseRefName":"main","headRefName":"feature/agent-pr","isDraft":false}\n'
  exit 0
fi
printf 'unexpected gh call: %s\n' "$*" 1>&2
exit 1
`
	if err := testexec.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r := minimalRunner(t)
	res := &Result{}
	res.WorktreePath = repo
	report := r.runBackstop(context.Background(), QueuedWork{
		QueuedWork: queuedWorkBase("AGENT-PR"),
	}, "feature/agent-pr", res, nil)
	if report.Diagnostics != "" {
		t.Fatalf("diagnostics = %q, want empty on already-exists recovery", report.Diagnostics)
	}
	if !report.Pushed {
		t.Error("Pushed = false; want true")
	}
	if report.PRURL != wantURL {
		t.Errorf("PRURL = %q; want the agent's %q", report.PRURL, wantURL)
	}
	raw, err := os.ReadFile(callsFile)
	if err != nil {
		t.Fatalf("read gh calls: %v", err)
	}
	calls := string(raw)
	if !strings.Contains(calls, "pr\ncreate\n") && !strings.Contains(calls, "create") {
		t.Errorf("gh calls = %q; want a pr create call", calls)
	}
	if strings.Contains(calls, "edit") {
		t.Errorf("gh calls = %q; want no pr edit (agent title/body must be kept)", calls)
	}
}
