package runner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/provider/stub"
	"github.com/RenseiAI/agentfactory-tui/result"
	"github.com/RenseiAI/agentfactory-tui/runtime/worktree"
)

// runnerHarness bundles the collaborators a Run-level test needs.
// Tests that exercise full Run() build one via newRunnerHarness so
// the wire-up cost is paid once.
type runnerHarness struct {
	runner   *Runner
	server   *httptest.Server
	bareRepo string
}

// newRunnerHarness wires a Runner against the stub provider and a
// fresh bare-repo backed WorktreeManager so end-to-end Run() can
// exercise real git operations.
func newRunnerHarness(t *testing.T) *runnerHarness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	srv := mockPlatformServer(t)
	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()

	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	reg := NewRegistry()
	p, _ := stub.New()
	if err := reg.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := New(Options{
		Registry:               reg,
		WorktreeManager:        wtm,
		Poster:                 poster,
		HTTPClient:             srv.Client(),
		SkipBackstop:           true,
		SkipSteering:           true,
		PreserveWorktreeAlways: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(srv.Close)
	return &runnerHarness{runner: r, server: srv, bareRepo: bareRepo}
}

// queuedWork returns a runner-shaped QueuedWork pointing at the
// harness's bare repo and platform mock URL.
func (h *runnerHarness) queuedWork(identifier string) QueuedWork {
	qw := QueuedWork{
		QueuedWork:  queuedWorkBase(identifier),
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		PlatformURL: h.server.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}
	qw.Repository = h.bareRepo
	return qw
}

// TestRun_HappyPath_StubProvider exercises the full Run() against the
// stub provider in BehaviorSucceedWithPR mode. Asserts the terminal
// Result has Status=completed and the synthetic cost data the stub
// emits in its terminal Result.
func TestRun_HappyPath_StubProvider(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-HAPPY-1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil {
		t.Fatal("Run returned nil result")
	}
	if res.Status != "completed" {
		t.Errorf("Status = %q; want completed (FailureMode=%q, Error=%q)",
			res.Status, res.FailureMode, res.Error)
	}
	if res.ProviderName != agent.ProviderStub {
		t.Errorf("ProviderName = %q; want stub", res.ProviderName)
	}
	if res.ProviderSessionID == "" {
		t.Error("expected ProviderSessionID from InitEvent capture")
	}
	if res.SessionID != qw.SessionID {
		t.Errorf("SessionID = %q; want %q", res.SessionID, qw.SessionID)
	}
	if res.Cost == nil {
		t.Error("expected Cost from terminal ResultEvent")
	}
	if res.WorktreePath == "" {
		t.Error("expected WorktreePath populated by Provision")
	}
	if res.WorkResult != "passed" {
		t.Errorf("expected WorkResult=passed (stub emits WORK_RESULT:passed); got %q", res.WorkResult)
	}
}

// TestRun_UnknownProvider_FailsFast confirms the runner classifies a
// missing provider as FailureProviderResolve.
func TestRun_UnknownProvider_FailsFast(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-PROV-1")
	qw.ResolvedProfile.Provider = agent.ProviderName("nonexistent")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := h.runner.Run(ctx, qw)
	if !errors.Is(err, agent.ErrNoProvider) {
		t.Fatalf("err = %v; want ErrNoProvider", err)
	}
	if res.FailureMode != FailureProviderResolve {
		t.Errorf("FailureMode = %q; want %q", res.FailureMode, FailureProviderResolve)
	}
}

// TestRun_ValidationFailure rejects a QueuedWork that is missing the
// required PlatformURL.
func TestRun_ValidationFailure(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-VAL-1")
	qw.PlatformURL = ""

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := h.runner.Run(ctx, qw)
	if err == nil {
		t.Fatal("expected error from Run with empty PlatformURL")
	}
	if res.FailureMode != FailurePromptRender {
		t.Errorf("FailureMode = %q; want %q", res.FailureMode, FailurePromptRender)
	}
}

// TestRun_PostsToPlatform confirms the runner calls /completion and
// /status on the platform mock after a successful run.
func TestRun_PostsToPlatform(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	var completionHits, statusHits, refreshHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/completion"):
			completionHits.Add(1)
		case strings.Contains(r.URL.Path, "/status"):
			statusHits.Add(1)
		case strings.Contains(r.URL.Path, "/lock-refresh"):
			refreshHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatalf("NewPoster: %v", err)
	}
	reg := NewRegistry()
	p, _ := stub.New()
	_ = reg.Register(p)
	r, err := New(Options{
		Registry:        reg,
		WorktreeManager: wtm,
		Poster:          poster,
		HTTPClient:      srv.Client(),
		SkipBackstop:    true,
		SkipSteering:    true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-POST-1"),
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		PlatformURL: srv.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
		},
	}
	qw.Repository = bareRepo

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("Status = %q; want completed", res.Status)
	}
	if completionHits.Load() == 0 {
		t.Errorf("expected /completion call; got 0")
	}
	if statusHits.Load() == 0 {
		t.Errorf("expected /status call; got 0")
	}
}

// makeBareRepo creates a bare git repo with a single commit on main
// and returns its absolute path. Used as the source for clone-based
// worktree provisioning.
func makeBareRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	work := t.TempDir()
	gitInit(t, work)
	bare := t.TempDir()
	//nolint:gosec // G204: test fixture, args are hard-coded literals.
	cmd := exec.Command("git", "clone", "--bare", work, filepath.Join(bare, "repo.git"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v\n%s", err, out)
	}
	return filepath.Join(bare, "repo.git")
}
