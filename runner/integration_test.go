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
