package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
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
		SkipPostSession:        true,
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
	// Correlation-key capture for the orchestration-owned durable CI
	// wait (ADR-2026-06-10-durable-ci-wait.md): the envelope must carry
	// the worktree's post-backstop head commit so the terminal status
	// post can correlate CI completion events to this session.
	if res.CommitSHA == "" {
		t.Fatal("expected CommitSHA captured at envelope-build time")
	}
	headOut, gitErr := runGit(context.Background(), res.WorktreePath, gitIdentity{}, "rev-parse", "HEAD")
	if gitErr != nil {
		t.Fatalf("rev-parse worktree HEAD: %v", gitErr)
	}
	if want := strings.TrimSpace(headOut); res.CommitSHA != want {
		t.Errorf("CommitSHA = %q; want worktree HEAD %q", res.CommitSHA, want)
	}
}

func TestRun_RepositoryFreeUsesEmptyWorkarea(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REPOSITORY-FREE")
	qw.Repository = ""
	qw.Ref = "main"
	qw.Branch = "existing-branch-metadata"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (FailureMode=%q, Error=%q)", res.Status, res.FailureMode, res.Error)
	}
	if res.WorktreePath == "" || res.WorkareaRoot != res.WorktreePath {
		t.Fatalf("repository-free paths = root %q cwd %q; want one flat workarea", res.WorkareaRoot, res.WorktreePath)
	}
	if _, err := os.Stat(filepath.Join(res.WorktreePath, ".git")); !os.IsNotExist(err) {
		t.Fatalf("repository-free workarea contains git metadata: %v", err)
	}
	if res.CommitSHA != "" {
		t.Fatalf("repository-free CommitSHA = %q, want empty", res.CommitSHA)
	}
}

func TestRun_RepositoryFreeDisablesVCRecovery(t *testing.T) {
	h := newRunnerHarness(t)
	h.runner.skipSteering = false
	h.runner.skipBackstop = false

	fakeBin := t.TempDir()
	markers := map[string]string{
		"git": filepath.Join(t.TempDir(), "git-command-ran"),
		"gh":  filepath.Join(t.TempDir(), "gh-command-ran"),
	}
	for name, marker := range markers {
		path := filepath.Join(fakeBin, name)
		command := []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$0 $*\" >> %q\nexit 97\n", marker))
		if err := os.WriteFile(path, command, 0o600); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // owner-only test fixture must be executable
			t.Fatalf("chmod fake %s: %v", name, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat fake %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("fake %s mode = %o, want 700", name, got)
		}
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	qw := h.queuedWork("REPOSITORY-FREE-RECOVERY")
	qw.Repository = ""
	qw.ResolvedProfile.ProviderConfig = map[string]any{
		"stub.behavior":          string(stub.BehaviorSlowTool),
		"stub.injectUnsupported": true,
		"stub.progressTicks":     0,
	}
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
		t.Errorf("repository-free Status = %q, want completed (FailureMode=%q, Error=%q)", res.Status, res.FailureMode, res.Error)
	}
	if res.PullRequestURL != "" {
		t.Errorf("repository-free PullRequestURL = %q, want empty", res.PullRequestURL)
	}
	if res.SteeringTriggered {
		t.Error("repository-free run triggered tail steering")
	}
	if res.SteeringResumeFallback {
		t.Error("repository-free run used tail steering resume fallback")
	}
	if res.BackstopReport != nil {
		t.Errorf("repository-free run produced backstop report: %+v", res.BackstopReport)
	}
	if res.CommitSHA != "" {
		t.Errorf("repository-free CommitSHA = %q, want empty", res.CommitSHA)
	}
	for name, marker := range markers {
		if body, err := os.ReadFile(marker); err == nil {
			t.Errorf("repository-free run invoked %s: %s", name, body)
		} else if !os.IsNotExist(err) {
			t.Errorf("read %s command marker: %v", name, err)
		}
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
		SkipPostSession: true,
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

// TestRun_CancellationStillPostsTerminalResult exercises the complete Run →
// result.Poster HTTP path after the run context has been cancelled. The
// terminal post must retain context values, use a fresh deadline, and deliver a
// stopped status instead of being short-circuited by the dead parent context.
func TestRun_CancellationStillPostsTerminalResult(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	type contextKey struct{}
	type contextObservation struct {
		err          error
		hasDeadline  bool
		deadlineLeft time.Duration
		value        any
	}

	started := make(chan struct{})
	var startedOnce sync.Once
	var completionHits atomic.Int64
	statusBodies := make(chan map[string]any, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/lock-refresh"):
			startedOnce.Do(func() { close(started) })
		case strings.HasSuffix(req.URL.Path, "/completion"):
			completionHits.Add(1)
		case strings.HasSuffix(req.URL.Path, "/status"):
			var body map[string]any
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Errorf("decode status request: %v", err)
			} else {
				statusBodies <- body
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	var observationMu sync.Mutex
	var observations []contextObservation
	key := contextKey{}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		HTTPClient:  srv.Client(),
		MaxAttempts: 1,
		BaseDelay:   0,
		CredentialProvider: func(ctx context.Context) (result.RuntimeCredentials, error) {
			deadline, hasDeadline := ctx.Deadline()
			observation := contextObservation{
				err:         ctx.Err(),
				hasDeadline: hasDeadline,
				value:       ctx.Value(key),
			}
			if hasDeadline {
				observation.deadlineLeft = time.Until(deadline)
			}
			observationMu.Lock()
			observations = append(observations, observation)
			observationMu.Unlock()
			return result.RuntimeCredentials{WorkerID: "worker-1", AuthToken: "tok"}, nil
		},
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}

	wtm, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	reg := NewRegistry()
	provider, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := New(Options{
		Registry:               reg,
		WorktreeManager:        wtm,
		Poster:                 poster,
		HTTPClient:             srv.Client(),
		SkipBackstop:           true,
		SkipSteering:           true,
		SkipPostSession:        true,
		PreserveWorktreeAlways: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("CANCEL-POST"),
		WorkerID:    "worker-1",
		AuthToken:   "tok",
		PlatformURL: srv.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
			ProviderConfig: map[string]any{
				"stub.behavior": string(stub.BehaviorHangThenTimeout),
			},
		},
	}
	qw.Mode = "interview"
	qw.Repository = makeBareRepo(t)

	parent := context.WithValue(context.Background(), key, "terminal-context-value")
	runCtx, cancel := context.WithCancel(parent)
	resultCh := make(chan struct {
		res *Result
		err error
	}, 1)
	go func() {
		res, runErr := r.Run(runCtx, qw)
		resultCh <- struct {
			res *Result
			err error
		}{res: res, err: runErr}
	}()

	select {
	case <-started:
		cancel()
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Run did not reach lock refresh before cancellation")
	}

	var got struct {
		res *Result
		err error
	}
	select {
	case got = <-resultCh:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", got.err)
	}
	if got.res == nil || got.res.Status != "stopped" {
		t.Fatalf("Run result = %#v, want status stopped", got.res)
	}
	if completionHits.Load() == 0 {
		t.Fatal("terminal /completion request was not received")
	}

	var terminalStatus map[string]any
	for len(statusBodies) > 0 {
		body := <-statusBodies
		if body["status"] == "stopped" {
			terminalStatus = body
		}
	}
	if terminalStatus == nil {
		t.Fatal("terminal /status request with status=stopped was not received")
	}

	observationMu.Lock()
	gotObservations := append([]contextObservation(nil), observations...)
	observationMu.Unlock()
	if len(gotObservations) < 2 {
		t.Fatalf("credential provider calls = %d, want at least completion + status", len(gotObservations))
	}
	for i, observation := range gotObservations {
		if observation.err != nil {
			t.Errorf("credential context %d error = %v, want live context", i, observation.err)
		}
		if !observation.hasDeadline {
			t.Errorf("credential context %d has no cleanup deadline", i)
		}
		if observation.deadlineLeft <= 0 || observation.deadlineLeft > terminalResultPostTimeout+time.Second {
			t.Errorf("credential context %d deadline remaining = %v, want (0, %v]", i, observation.deadlineLeft, terminalResultPostTimeout+time.Second)
		}
		if observation.value != "terminal-context-value" {
			t.Errorf("credential context %d value = %v, want preserved parent value", i, observation.value)
		}
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
