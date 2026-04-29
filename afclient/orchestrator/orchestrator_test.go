package orchestrator_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/afclient/orchestrator"
	"github.com/RenseiAI/agentfactory-tui/internal/linear"
)

// ── Fixtures ──────────────────────────────────────────────────────────────────

func makeIssue(id, identifier, title, projectName string) linear.Issue {
	iss := linear.Issue{
		ID:         id,
		Identifier: identifier,
		Title:      title,
	}
	iss.Project.Name = projectName
	iss.State.Name = "Backlog"
	return iss
}

// mockLinear satisfies the linear.Linear interface.
type mockLinear struct {
	issues   []linear.Issue
	singleFn func(ctx context.Context, id string) (*linear.Issue, error)
}

func (m *mockLinear) ListIssuesByProject(_ context.Context, _ string, _ []string) ([]linear.Issue, error) {
	return m.issues, nil
}

func (m *mockLinear) GetIssue(ctx context.Context, id string) (*linear.Issue, error) {
	if m.singleFn != nil {
		return m.singleFn(ctx, id)
	}
	for _, iss := range m.issues {
		if iss.ID == id || iss.Identifier == id {
			return &iss, nil
		}
	}
	return nil, errors.New("not found")
}

func (m *mockLinear) ListSubIssues(_ context.Context, _ string) ([]linear.Issue, error) {
	return nil, nil
}

// mockDispatcher records dispatches and returns a canned result.
type mockDispatcher struct {
	dispatched []*orchestrator.AgentDispatch
	err        error
}

func (d *mockDispatcher) Dispatch(_ context.Context, issue linear.Issue, _ orchestrator.Config) (*orchestrator.AgentDispatch, error) {
	ad := &orchestrator.AgentDispatch{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Title:      issue.Title,
		Project:    issue.Project.Name,
		Status:     orchestrator.DispatchCompleted,
	}
	d.dispatched = append(d.dispatched, ad)
	return ad, d.err
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// writeConfig creates .agentfactory/config.yaml under dir.
func writeConfig(t *testing.T, dir, content string) {
	t.Helper()
	afDir := filepath.Join(dir, ".agentfactory")
	if err := os.MkdirAll(afDir, 0o750); err != nil { //nolint:gosec
		t.Fatalf("mkdir .agentfactory: %v", err)
	}
	path := filepath.Join(afDir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil { // #nosec G306
		t.Fatalf("write config.yaml: %v", err)
	}
}

// isGitRepo reports whether dir is inside a git repository.
func isGitRepo(dir string) bool {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	return cmd.Run() == nil
}

// initGitRepo initialises a git repo in dir so ValidateGitRemote can run.
func initGitRepo(t *testing.T, dir, remoteURL string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"remote", "add", "origin", remoteURL},
	} {
		cmd := exec.Command("git", args...) // #nosec G204 -- test invokes a controlled fake binary
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// newTestOrchestrator builds an Orchestrator wired to mock dependencies.
func newTestOrchestrator(t *testing.T, cfg orchestrator.Config, lin linear.Linear, disp orchestrator.Dispatcher) *orchestrator.Orchestrator {
	t.Helper()
	o, err := orchestrator.NewForTest(cfg, lin)
	if err != nil {
		t.Fatalf("NewForTest: %v", err)
	}
	o.WithDispatcher(disp)
	return o
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestRunSingle_DryRun(t *testing.T) {
	dir := t.TempDir()
	issue := makeIssue("id-1", "REN-1", "Implement feature", "MyProject")
	lin := &mockLinear{issues: []linear.Issue{issue}}
	lin.singleFn = func(_ context.Context, _ string) (*linear.Issue, error) { return &issue, nil }
	disp := &mockDispatcher{}

	cfg := orchestrator.Config{
		LinearAPIKey: "test-key",
		Single:       "REN-1",
		DryRun:       true,
		GitRoot:      dir,
	}

	o := newTestOrchestrator(t, cfg, lin, disp)
	result, err := o.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Dispatched) != 1 {
		t.Fatalf("expected 1 dispatched entry, got %d", len(result.Dispatched))
	}
	if result.Dispatched[0].Status != orchestrator.DispatchSkipped {
		t.Errorf("expected DispatchSkipped, got %s", result.Dispatched[0].Status)
	}
	if len(disp.dispatched) != 0 {
		t.Error("expected no real dispatch in dry-run mode")
	}
}

func TestRunSingle_Dispatch(t *testing.T) {
	dir := t.TempDir()
	issue := makeIssue("id-1", "REN-1", "Implement feature", "MyProject")
	lin := &mockLinear{issues: []linear.Issue{issue}}
	lin.singleFn = func(_ context.Context, _ string) (*linear.Issue, error) { return &issue, nil }
	disp := &mockDispatcher{}

	cfg := orchestrator.Config{
		LinearAPIKey: "test-key",
		Single:       "REN-1",
		DryRun:       false,
		GitRoot:      dir,
	}

	o := newTestOrchestrator(t, cfg, lin, disp)
	result, err := o.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Dispatched) != 1 {
		t.Fatalf("expected 1 dispatched entry, got %d", len(result.Dispatched))
	}
	if result.Dispatched[0].Status != orchestrator.DispatchCompleted {
		t.Errorf("expected DispatchCompleted, got %s", result.Dispatched[0].Status)
	}
	if len(disp.dispatched) != 1 {
		t.Errorf("expected 1 real dispatch, got %d", len(disp.dispatched))
	}
}

func TestRunBacklog_DryRun(t *testing.T) {
	dir := t.TempDir()
	issues := []linear.Issue{
		makeIssue("id-1", "REN-1", "Issue one", "Alpha"),
		makeIssue("id-2", "REN-2", "Issue two", "Alpha"),
	}
	lin := &mockLinear{issues: issues}
	disp := &mockDispatcher{}

	cfg := orchestrator.Config{
		LinearAPIKey: "test-key",
		Project:      "Alpha",
		DryRun:       true,
		Max:          3,
		GitRoot:      dir,
	}

	o := newTestOrchestrator(t, cfg, lin, disp)
	result, err := o.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Dispatched) != 2 {
		t.Fatalf("expected 2 dispatched entries, got %d", len(result.Dispatched))
	}
	for _, d := range result.Dispatched {
		if d.Status != orchestrator.DispatchSkipped {
			t.Errorf("expected DispatchSkipped, got %s for %s", d.Status, d.Identifier)
		}
	}
	if len(disp.dispatched) != 0 {
		t.Error("expected no real dispatch in dry-run mode")
	}
}

func TestRunBacklog_RepoMismatch(t *testing.T) {
	dir := t.TempDir()
	if !isGitRepo(dir) {
		initGitRepo(t, dir, "https://github.com/org/other-repo.git")
	}

	issue := makeIssue("id-1", "REN-1", "Implement feature", "Alpha")
	lin := &mockLinear{issues: []linear.Issue{issue}}
	disp := &mockDispatcher{}

	cfg := orchestrator.Config{
		LinearAPIKey: "test-key",
		Project:      "Alpha",
		Repository:   "github.com/org/my-repo",
		DryRun:       false,
		GitRoot:      dir,
	}

	o := newTestOrchestrator(t, cfg, lin, disp)
	_, err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected repo mismatch error, got nil")
	}
}

func TestValidateGitRemote_Match(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, "https://github.com/org/my-repo.git")

	if err := orchestrator.ValidateGitRemote("github.com/org/my-repo", dir); err != nil {
		t.Errorf("unexpected mismatch error: %v", err)
	}
}

func TestValidateGitRemote_SSHFormat(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, "git@github.com:org/my-repo.git")

	if err := orchestrator.ValidateGitRemote("github.com/org/my-repo", dir); err != nil {
		t.Errorf("SSH remote should match: %v", err)
	}
}

func TestValidateGitRemote_Mismatch(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir, "https://github.com/org/other-repo.git")

	err := orchestrator.ValidateGitRemote("github.com/org/my-repo", dir)
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
}

func TestRunSingle_AllowlistEnforced(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `apiVersion: v1
kind: RepositoryConfig
allowedProjects:
  - AllowedProject
`)
	issue := makeIssue("id-1", "REN-1", "Issue title", "NotAllowed")
	lin := &mockLinear{issues: []linear.Issue{issue}}
	lin.singleFn = func(_ context.Context, _ string) (*linear.Issue, error) { return &issue, nil }
	disp := &mockDispatcher{}

	cfg := orchestrator.Config{
		LinearAPIKey: "test-key",
		Single:       "REN-1",
		DryRun:       false,
		GitRoot:      dir,
	}

	o := newTestOrchestrator(t, cfg, lin, disp)
	_, err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected allowlist enforcement error, got nil")
	}
}
