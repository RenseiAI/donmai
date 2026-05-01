package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/prompt"
	"github.com/RenseiAI/agentfactory-tui/result"
	"github.com/RenseiAI/agentfactory-tui/runtime/worktree"
)

// minimalRunner returns a Runner wired to a no-op platform mock, an
// in-memory worktree manager, and the stub provider. Used by tests
// that exercise individual loop helpers (steering decisions, backstop)
// without spinning up a full Run.
func minimalRunner(t *testing.T) *Runner {
	t.Helper()
	srv := mockPlatformServer(t)
	t.Cleanup(srv.Close)

	wtParent := t.TempDir()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "test-worker",
		AuthToken:   "token",
		HTTPClient:  srv.Client(),
		BaseDelay:   1, // 1ns — effectively no sleep between retries
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	reg := NewRegistry()
	r, err := New(Options{
		Registry:        reg,
		WorktreeManager: wtm,
		Poster:          poster,
		HTTPClient:      srv.Client(),
		// MaxSessionDuration negative disables timeout; some tests
		// run uninterruptable behaviors (hang-then-timeout) and need
		// caller-controlled cancellation.
		MaxSessionDuration: -1,
		SkipBackstop:       true,
		SkipSteering:       true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// mockPlatformServer returns an httptest.Server that accepts every
// /api/sessions/<id>/{completion,status,lock-refresh} call and
// responds 200 OK with `{"refreshed":true}` (for lock-refresh). The
// returned URL is suitable for both result.Poster and heartbeat.Pulser.
func mockPlatformServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// All endpoints accept POST and return JSON.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	return srv
}

// gitInit initialises a fresh git repo at dir with a single committed
// file so subsequent backstop / push operations have a base.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
		{"config", "commit.gpgsign", "false"},
	} {
		//nolint:gosec // G204: test fixture, args are hard-coded literals.
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// Seed an initial commit on main so future commits aren't on a
	// detached-HEAD or empty branch.
	writeFile(t, dir, "README.md", "# test repo\n")
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-m", "initial"},
	} {
		//nolint:gosec // G204: test fixture, args are hard-coded literals.
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// writeFile writes content to dir/relPath, creating parents as needed.
func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// checkout creates+switches to the named branch.
func checkout(t *testing.T, dir, branch string) {
	t.Helper()
	//nolint:gosec // G204: test fixture, branch comes from test caller.
	cmd := exec.Command("git", "checkout", "-b", branch)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b %s: %v\n%s", branch, err, out)
	}
}

// queuedWorkBase returns a minimal but dispatchable QueuedWork for
// tests that don't care about the prompt / repository fields.
func queuedWorkBase(identifier string) prompt.QueuedWork {
	return prompt.QueuedWork{
		SessionID:       "test-session-" + identifier,
		IssueID:         "issue-uuid-" + identifier,
		IssueIdentifier: identifier,
		WorkType:        "development",
		ProjectName:     "TestProject",
		OrganizationID:  "org_test",
		Body:            "This is a test issue body.",
		Title:           "Test issue " + identifier,
	}
}

// agentResultWithPR returns an agent.Result with a PR URL for table
// tests that need a "completed" envelope.
func agentResultWithPR(prURL string) agent.Result {
	return agent.Result{
		Status:         "completed",
		PullRequestURL: prURL,
	}
}

// agentResultWithFailure returns an agent.Result classified as failed
// with the supplied FailureMode.
func agentResultWithFailure(mode string) agent.Result {
	return agent.Result{
		Status:      "failed",
		FailureMode: mode,
	}
}

// withCtx returns a cancellable context with a 30s deadline. Used so
// tests don't hang forever on a misbehaving fixture.
func withCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(context.Background())
}
