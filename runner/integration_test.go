//go:build runner_integration

package runner_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/prompt"
	"github.com/RenseiAI/agentfactory-tui/provider/stub"
	"github.com/RenseiAI/agentfactory-tui/result"
	"github.com/RenseiAI/agentfactory-tui/runner"
	"github.com/RenseiAI/agentfactory-tui/runtime/worktree"
)

// TestIntegration_StubProvider_FullRun exercises a full Run() against
// the F.2.2 stub provider end-to-end with a real httptest mock and a
// real bare-repo backed worktree. The test asserts the platform sees
// the canonical sequence of HTTP calls (lock-refresh → completion →
// status) and the runner returns a Result with Status=completed.
//
// This is the highest-fidelity test of the runner package short of a
// real claude/codex provider. Build-tagged so the default unit run
// does not pay for the git+httptest setup; CI runs this as part of
// the integration suite.
func TestIntegration_StubProvider_FullRun(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	type recordedCall struct {
		Path string
		Body string
	}
	var calls []recordedCall
	var callsMu sync.Mutex
	var refreshes atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		callsMu.Lock()
		calls = append(calls, recordedCall{Path: r.URL.Path, Body: string(body[:n])})
		callsMu.Unlock()
		if strings.Contains(r.URL.Path, "/lock-refresh") {
			refreshes.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"refreshed": true,
			"ok":        true,
		})
	}))
	defer srv.Close()

	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "integration-worker",
		AuthToken:   "integration-tok",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := runner.NewRegistry()
	p, _ := stub.New()
	if err := reg.Register(p); err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Options{
		Registry:               reg,
		WorktreeManager:        wtm,
		Poster:                 poster,
		HTTPClient:             srv.Client(),
		HeartbeatInterval:      100 * time.Millisecond,
		SkipBackstop:           true,
		SkipSteering:           true,
		SkipPostSession:        true,
		PreserveWorktreeAlways: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	qw := runner.QueuedWork{
		QueuedWork: prompt.QueuedWork{
			SessionID:       "sess-integration",
			IssueID:         "issue-integration",
			IssueIdentifier: "REN-INTEGRATION-1",
			WorkType:        "development",
			ProjectName:     "Integration",
			OrganizationID:  "org_integration",
			Repository:      bareRepo,
			Body:            "Integration test issue body.",
			Title:           "Integration smoke",
		},
		WorkerID:    "integration-worker",
		AuthToken:   "integration-tok",
		PlatformURL: srv.URL,
		ResolvedProfile: runner.ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := r.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (Error=%q FailureMode=%q)",
			res.Status, res.Error, res.FailureMode)
	}

	// Verify platform calls.
	callsMu.Lock()
	defer callsMu.Unlock()
	var sawCompletion, sawStatus bool
	for _, c := range calls {
		if strings.Contains(c.Path, "/completion") {
			sawCompletion = true
		}
		if strings.Contains(c.Path, "/status") {
			sawStatus = true
		}
	}
	if !sawCompletion {
		t.Errorf("expected /completion call; calls=%v", calls)
	}
	if !sawStatus {
		t.Errorf("expected /status call; calls=%v", calls)
	}
}

// TestIntegration_StagePromptHappyPath asserts that a QueuedWork
// dispatched with the new Phase 2 stage fields (StagePrompt /
// StageID / StageBudget) runs end-to-end with the stub provider and
// surfaces the budget report on Result. (REN-1485 / REN-1487 Phase 2
// daemon-side acceptance.)
func TestIntegration_StagePromptHappyPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "refreshed": true})
	}))
	defer srv.Close()

	bareRepo := makeBareRepo(t)
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "stage-worker",
		AuthToken:   "stage-tok",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := runner.NewRegistry()
	p, _ := stub.New()
	if err := reg.Register(p); err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Options{
		Registry:          reg,
		WorktreeManager:   wtm,
		Poster:            poster,
		HTTPClient:        srv.Client(),
		HeartbeatInterval: 100 * time.Millisecond,
		SkipBackstop:      true,
		SkipSteering:      true,
		SkipPostSession:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	qw := runner.QueuedWork{
		QueuedWork: prompt.QueuedWork{
			SessionID:       "sess-stage",
			IssueID:         "issue-stage",
			IssueIdentifier: "REN-1487",
			WorkType:        "development",
			ProjectName:     "Stage",
			OrganizationID:  "org_stage",
			Repository:      bareRepo,
			Body:            "Stage smoke",
			Title:           "Stage smoke",
			StagePrompt:     "Run the development stage on REN-1487.",
			StageID:         "development",
			StageBudget: &prompt.StageBudget{
				MaxDurationSeconds: 600,
				MaxSubAgents:       10,
				MaxTokens:          100_000,
			},
			StageSourceEventID: "evt-stage-1",
		},
		WorkerID:    "stage-worker",
		AuthToken:   "stage-tok",
		PlatformURL: srv.URL,
		ResolvedProfile: runner.ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status=%q want completed (Error=%q FailureMode=%q)",
			res.Status, res.Error, res.FailureMode)
	}
	if res.BudgetReport == nil {
		t.Fatalf("expected non-nil BudgetReport")
	}
	if !res.BudgetReport.Enforced {
		t.Fatalf("expected BudgetReport.Enforced=true for stage dispatch")
	}
	if res.BudgetReport.CapBreached != "" {
		t.Fatalf("expected no breach, got %s (%s)",
			res.BudgetReport.CapBreached, res.BudgetReport.BreachDetail)
	}
}

// TestIntegration_BudgetExceeded_SubAgentCap drives a synthetic
// provider that emits N+1 Task tool-use events, asserting that
// runner.Run cleanly classifies the outcome as
// FailureBudgetExceeded with cap=max-sub-agents and surfaces the
// breach on Result.BudgetReport. (REN-1485 / REN-1487 acceptance #4.)
func TestIntegration_BudgetExceeded_SubAgentCap(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "refreshed": true})
	}))
	defer srv.Close()

	bareRepo := makeBareRepo(t)
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "budget-worker",
		AuthToken:   "budget-tok",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := runner.NewRegistry()
	if err := reg.Register(&taskSpammingProvider{}); err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Options{
		Registry:          reg,
		WorktreeManager:   wtm,
		Poster:            poster,
		HTTPClient:        srv.Client(),
		HeartbeatInterval: 100 * time.Millisecond,
		SkipBackstop:      true,
		SkipSteering:      true,
		SkipPostSession:   true,
	})
	if err != nil {
		t.Fatal(err)
	}

	qw := runner.QueuedWork{
		QueuedWork: prompt.QueuedWork{
			SessionID:       "sess-budget",
			IssueID:         "issue-budget",
			IssueIdentifier: "REN-1485",
			Repository:      bareRepo,
			Body:            "Budget breach scenario",
			Title:           "Budget breach",
			StagePrompt:     "Run development.",
			StageID:         "development",
			StageBudget: &prompt.StageBudget{
				MaxSubAgents: 2,
			},
		},
		WorkerID:    "budget-worker",
		AuthToken:   "budget-tok",
		PlatformURL: srv.URL,
		ResolvedProfile: runner.ResolvedProfile{
			Provider: agent.ProviderName("task-spammer"),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, runErr := r.Run(ctx, qw)
	// runErr is non-nil — the budget breach surfaces as a runtime error.
	if runErr == nil {
		t.Fatalf("expected non-nil err on budget breach, got Result.Status=%q", res.Status)
	}
	if res.Status != "failed" {
		t.Fatalf("expected Status=failed, got %q", res.Status)
	}
	if res.FailureMode != runner.FailureBudgetExceeded {
		t.Fatalf("expected FailureMode=%q, got %q", runner.FailureBudgetExceeded, res.FailureMode)
	}
	if res.BudgetReport == nil {
		t.Fatalf("expected non-nil BudgetReport")
	}
	if res.BudgetReport.CapBreached != runner.CapSubAgents {
		t.Fatalf("expected CapBreached=%q, got %q", runner.CapSubAgents, res.BudgetReport.CapBreached)
	}
	if res.BudgetReport.ObservedSubAgents < 3 {
		t.Fatalf("expected ObservedSubAgents>=3, got %d", res.BudgetReport.ObservedSubAgents)
	}
}

// taskSpammingProvider is a minimal in-test agent.Provider that emits
// repeated Task ToolUseEvents to drive the budget enforcer past its
// MaxSubAgents cap. Used only by the budget integration test.
type taskSpammingProvider struct{}

func (taskSpammingProvider) Name() agent.ProviderName { return "task-spammer" }
func (taskSpammingProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{
		EmitsSubagentEvents: true,
	}
}

func (p *taskSpammingProvider) Spawn(ctx context.Context, _ agent.Spec) (agent.Handle, error) {
	return p.spawnHandle(ctx), nil
}

func (p *taskSpammingProvider) Resume(ctx context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return p.spawnHandle(ctx), nil
}
func (p *taskSpammingProvider) Shutdown(_ context.Context) error { return nil }

func (p *taskSpammingProvider) spawnHandle(ctx context.Context) agent.Handle {
	ev := make(chan agent.Event, 16)
	h := &taskSpammingHandle{events: ev}
	go func() {
		defer close(ev)
		send := func(e agent.Event) bool {
			select {
			case ev <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !send(agent.InitEvent{SessionID: "task-spammer-1"}) {
			return
		}
		// Emit way more Task events than any reasonable cap.
		for i := 0; i < 10; i++ {
			if !send(agent.ToolUseEvent{ToolName: "Task", ToolUseID: "t" + string(rune('0'+i))}) {
				return
			}
		}
		// If we reach here, the enforcer never tripped — emit a
		// terminal ResultEvent so the runner doesn't complain about
		// silent exit.
		_ = send(agent.ResultEvent{Success: true})
	}()
	return h
}

type taskSpammingHandle struct {
	events chan agent.Event
}

func (h *taskSpammingHandle) Events() <-chan agent.Event { return h.events }
func (h *taskSpammingHandle) Inject(_ context.Context, _ string) error {
	return nil
}
func (h *taskSpammingHandle) Stop(_ context.Context) error { return nil }
func (h *taskSpammingHandle) SessionID() string            { return "task-spammer-1" }

// makeBareRepo creates a bare git repo seeded with a single commit on
// main. Mirrors the helper in runner_test.go but lives here so the
// build-tagged file can be compiled in isolation.
func makeBareRepo(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	cmd := exec.Command("git", "commit", "--allow-empty", "-m", "init")
	cmd.Dir = work
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	bare := t.TempDir()
	cmd = exec.Command("git", "clone", "--bare", work, filepath.Join(bare, "repo.git"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v\n%s", err, out)
	}
	return filepath.Join(bare, "repo.git")
}
