package orchestrator_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient/orchestrator"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/linear"
	stub "github.com/RenseiAI/donmai/provider/harness/stub"
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

func (m *mockLinear) ListIssues(_ context.Context, _ map[string]any, _ int, _ string) (*linear.IssueListResult, error) {
	return nil, nil
}

func (m *mockLinear) ListBacklogIssues(_ context.Context, _ string, _ []string, _ bool) ([]linear.Issue, error) {
	return nil, nil
}

func (m *mockLinear) GetIssueComments(_ context.Context, _ string) ([]linear.Comment, error) {
	return nil, nil
}

func (m *mockLinear) GetIssueRelations(_ context.Context, _ string) (*linear.RelationsResult, error) {
	return nil, nil
}

func (m *mockLinear) ListWorkflowStates(_ context.Context, _ string) (map[string]string, error) {
	return nil, nil
}

func (m *mockLinear) ListLabels(_ context.Context) (map[string]string, error) {
	return nil, nil
}

func (m *mockLinear) ListLabelsForTeam(_ context.Context, _ string) (map[string]string, error) {
	return nil, nil
}

func (m *mockLinear) ListTeams(_ context.Context) ([]linear.Team, error) {
	return nil, nil
}

func (m *mockLinear) ListProjects(_ context.Context, _ string) ([]linear.Project, error) {
	return nil, nil
}

func (m *mockLinear) GetTeamByName(_ context.Context, _ string) (*linear.Team, error) {
	return nil, nil
}

func (m *mockLinear) GetProjectByName(_ context.Context, _ string) (*linear.Project, error) {
	return nil, nil
}

func (m *mockLinear) GetProjectByNameInTeam(_ context.Context, _, _ string) (*linear.Project, error) {
	return nil, nil
}

func (m *mockLinear) GetUserByNameOrEmail(_ context.Context, _ string) (*linear.User, error) {
	return nil, nil
}

func (m *mockLinear) GetViewer(_ context.Context) (*linear.User, error) {
	return nil, nil
}

func (m *mockLinear) CreateIssue(_ context.Context, _ linear.CreateIssueInput) (*linear.Issue, error) {
	return nil, nil
}

func (m *mockLinear) UpdateIssue(_ context.Context, _ string, _ linear.UpdateIssueInput) (*linear.Issue, error) {
	return nil, nil
}

func (m *mockLinear) CreateIssueLabel(_ context.Context, _, _ string) (*linear.Label, error) {
	return nil, nil
}

func (m *mockLinear) AddIssueLabel(_ context.Context, _, _ string) (*linear.Issue, error) {
	return nil, nil
}

func (m *mockLinear) CreateComment(_ context.Context, _, _ string) (*linear.Comment, error) {
	return nil, nil
}

func (m *mockLinear) CreateRelation(_ context.Context, _, _, _ string) (string, bool, error) {
	return "", false, nil
}

func (m *mockLinear) CreateDocument(_ context.Context, _ linear.CreateDocumentInput) (*linear.DocumentCreateResult, error) {
	return nil, nil
}

func (m *mockLinear) DeleteRelation(_ context.Context, _ string) error {
	return nil
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

// writeConfig creates .donmai/config.yaml under dir.
func writeConfig(t *testing.T, dir, content string) {
	t.Helper()
	afDir := filepath.Join(dir, ".donmai")
	if err := os.MkdirAll(afDir, 0o750); err != nil { //nolint:gosec
		t.Fatalf("mkdir .donmai: %v", err)
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
	issue := makeIssue("id-1", "ENG-1", "Implement feature", "MyProject")
	lin := &mockLinear{issues: []linear.Issue{issue}}
	lin.singleFn = func(_ context.Context, _ string) (*linear.Issue, error) { return &issue, nil }
	disp := &mockDispatcher{}

	cfg := orchestrator.Config{
		LinearAPIKey: "test-key",
		Single:       "ENG-1",
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
	issue := makeIssue("id-1", "ENG-1", "Implement feature", "MyProject")
	lin := &mockLinear{issues: []linear.Issue{issue}}
	lin.singleFn = func(_ context.Context, _ string) (*linear.Issue, error) { return &issue, nil }
	disp := &mockDispatcher{}

	cfg := orchestrator.Config{
		LinearAPIKey: "test-key",
		Single:       "ENG-1",
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
		makeIssue("id-1", "ENG-1", "Issue one", "Alpha"),
		makeIssue("id-2", "ENG-2", "Issue two", "Alpha"),
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

	issue := makeIssue("id-1", "ENG-1", "Implement feature", "Alpha")
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

type changingRemoteDispatcher struct {
	root  string
	calls atomic.Int64
}

func (d *changingRemoteDispatcher) Dispatch(ctx context.Context, issue linear.Issue, _ orchestrator.Config) (*orchestrator.AgentDispatch, error) {
	if d.calls.Add(1) == 1 {
		cmd := exec.CommandContext(ctx, "git", "remote", "set-url", "origin", "https://example.invalid/other/repo.git")
		cmd.Dir = d.root
		if output, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("change fixture origin: %w: %s", err, output)
		}
	}

	return &orchestrator.AgentDispatch{
		IssueID:    issue.ID,
		Identifier: issue.Identifier,
		Status:     orchestrator.DispatchCompleted,
	}, nil
}

func TestRunBacklog_RevalidatesRepositoryBeforeEachDispatch(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir, "https://example.invalid/expected/repo.git")
	issues := []linear.Issue{
		makeIssue("id-1", "ENG-1", "Issue one", "Alpha"),
		makeIssue("id-2", "ENG-2", "Issue two", "Alpha"),
		makeIssue("id-3", "ENG-3", "Issue three", "Alpha"),
	}
	lin := &mockLinear{issues: issues}
	disp := &changingRemoteDispatcher{root: dir}
	cfg := orchestrator.Config{
		Project:    "Alpha",
		Repository: "example.invalid/expected/repo",
		Max:        1,
		GitRoot:    dir,
	}

	o := newTestOrchestrator(t, cfg, lin, disp)
	result, err := o.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls := disp.calls.Load(); calls != 1 {
		t.Fatalf("dispatch calls = %d, want 1 after the remote changes", calls)
	}
	if len(result.Dispatched) != 1 || result.Dispatched[0].Identifier != "ENG-1" {
		t.Fatalf("dispatched = %v, want only ENG-1", result.Dispatched)
	}
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0].Error(), "repository mismatch") {
		t.Fatalf("errors = %v, want one repository mismatch", result.Errors)
	}
}

func TestRunBacklog_MatchingRepositoryDispatchesAllIssues(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")

	dir := t.TempDir()
	initGitRepo(t, dir, "https://example.invalid/expected/repo.git")
	issues := []linear.Issue{
		makeIssue("id-1", "ENG-1", "Issue one", "Alpha"),
		makeIssue("id-2", "ENG-2", "Issue two", "Alpha"),
		makeIssue("id-3", "ENG-3", "Issue three", "Alpha"),
	}
	lin := &mockLinear{issues: issues}
	disp := &mockDispatcher{}
	cfg := orchestrator.Config{
		Project:    "Alpha",
		Repository: "example.invalid/expected/repo",
		Max:        1,
		GitRoot:    dir,
	}

	o := newTestOrchestrator(t, cfg, lin, disp)
	result, err := o.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("errors = %v, want none", result.Errors)
	}
	if len(result.Dispatched) != len(issues) || len(disp.dispatched) != len(issues) {
		t.Fatalf("dispatched = %d result entries, %d dispatcher calls; want %d", len(result.Dispatched), len(disp.dispatched), len(issues))
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
	issue := makeIssue("id-1", "ENG-1", "Issue title", "NotAllowed")
	lin := &mockLinear{issues: []linear.Issue{issue}}
	lin.singleFn = func(_ context.Context, _ string) (*linear.Issue, error) { return &issue, nil }
	disp := &mockDispatcher{}

	cfg := orchestrator.Config{
		LinearAPIKey: "test-key",
		Single:       "ENG-1",
		DryRun:       false,
		GitRoot:      dir,
	}

	o := newTestOrchestrator(t, cfg, lin, disp)
	_, err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected allowlist enforcement error, got nil")
	}
}

// ── Native backlog lifecycle controls ───────────────────────────────────────

// concurrencyFactory tracks the maximum concurrent NewProvider calls to prove
// the backlog run respects its --max bound.
type concurrencyFactory struct {
	current atomic.Int64
	peak    atomic.Int64
	delay   time.Duration
}

func (f *concurrencyFactory) NewProvider(_ context.Context, _ string) (agent.Provider, error) {
	cur := f.current.Add(1)
	for {
		peak := f.peak.Load()
		if cur <= peak || f.peak.CompareAndSwap(peak, cur) {
			break
		}
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	p, err := stub.New(stub.WithDefaultBehavior(stub.BehaviorSucceedWithPR))
	f.current.Add(-1)
	return p, err
}

func TestRunBacklog_NativeRespectsConcurrencyLimit(t *testing.T) {
	dir := t.TempDir()
	issues := []linear.Issue{
		makeIssue("id-1", "ENG-1", "Issue one", "Alpha"),
		makeIssue("id-2", "ENG-2", "Issue two", "Alpha"),
		makeIssue("id-3", "ENG-3", "Issue three", "Alpha"),
		makeIssue("id-4", "ENG-4", "Issue four", "Alpha"),
		makeIssue("id-5", "ENG-5", "Issue five", "Alpha"),
		makeIssue("id-6", "ENG-6", "Issue six", "Alpha"),
	}
	lin := &mockLinear{issues: issues}
	fac := &concurrencyFactory{delay: 100 * time.Millisecond}

	cfg := orchestrator.Config{
		LinearAPIKey:    "test-key",
		Project:         "Alpha",
		Max:             2,
		GitRoot:         dir,
		ProviderFactory: fac,
	}

	o := newTestOrchestrator(t, cfg, lin, &mockDispatcher{})
	o.WithProviderFactory(fac)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := o.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Dispatched) != len(issues) {
		t.Fatalf("expected %d dispatched entries, got %d", len(issues), len(result.Dispatched))
	}
	for _, d := range result.Dispatched {
		if d.Status != orchestrator.DispatchCompleted {
			t.Errorf("expected DispatchCompleted, got %s for %s", d.Status, d.Identifier)
		}
		if !d.CompletedAt.After(d.StartedAt) && !d.CompletedAt.Equal(d.StartedAt) {
			t.Errorf("expected CompletedAt to be set for %s", d.Identifier)
		}
	}
	if len(result.Errors) != 0 {
		t.Fatalf("expected no errors, got %v", result.Errors)
	}
	if peak := fac.peak.Load(); peak > 2 {
		t.Fatalf("peak provider concurrency = %d, want <= 2", peak)
	}
	if peak := fac.peak.Load(); peak < 1 {
		t.Fatal("expected at least one provider construction")
	}
}

func TestRunSingle_NativeFailureRecorded(t *testing.T) {
	dir := t.TempDir()
	issue := makeIssue("id-1", "ENG-1", "Clone breaks", "MyProject")
	lin := &mockLinear{issues: []linear.Issue{issue}}
	lin.singleFn = func(_ context.Context, _ string) (*linear.Issue, error) { return &issue, nil }

	cfg := orchestrator.Config{
		LinearAPIKey: "test-key",
		Single:       "ENG-1",
		GitRoot:      dir,
	}

	o := newTestOrchestrator(t, cfg, lin, &mockDispatcher{})
	o.WithProviderFactory(&stubBehaviorFactory{behavior: stub.BehaviorFailOnClone})
	result, err := o.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Dispatched) != 1 {
		t.Fatalf("expected 1 dispatched entry, got %d", len(result.Dispatched))
	}
	if result.Dispatched[0].Status != orchestrator.DispatchFailed {
		t.Fatalf("Status = %q, want failed", result.Dispatched[0].Status)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected 1 recorded error, got %d", len(result.Errors))
	}
}

func TestRunBacklog_UnknownHarnessFailsFast(t *testing.T) {
	dir := t.TempDir()
	issues := []linear.Issue{makeIssue("id-1", "ENG-1", "Issue one", "Alpha")}
	lin := &mockLinear{issues: issues}

	cfg := orchestrator.Config{
		LinearAPIKey: "test-key",
		Project:      "Alpha",
		GitRoot:      dir,
		Harness:      "nope",
	}

	o := newTestOrchestrator(t, cfg, lin, &mockDispatcher{})
	_, err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected unknown-harness error, got nil")
	}
}

// stubBehaviorFactory builds stub providers with one fixed behavior.
type stubBehaviorFactory struct {
	behavior stub.Behavior
}

func (f *stubBehaviorFactory) NewProvider(_ context.Context, _ string) (agent.Provider, error) {
	return stub.New(stub.WithDefaultBehavior(f.behavior))
}
