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
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// failingInteractiveProvider drives the production interactive lane to a
// caller-chosen exit: Spawn compiles the Spec against the exact production
// manifest, stages files the terminal session left uncommitted in the
// provisioned checkout, and returns a handle whose PTY surface has already
// exited. The runner then supervises the (already-dead) session through the
// same dispatchInteractive + post-Run teardown path a human's crashed shell
// takes.
type failingInteractiveProvider struct {
	manifest agent.HarnessManifest
	raw      agent.Spec
	// files are written into the provisioned checkout during Spawn, modelling
	// work the terminal session left uncommitted.
	files map[string]string
	// exitCode is the PTY child's terminal exit; nonzero fails the session.
	exitCode uint64
}

func (p *failingInteractiveProvider) Name() agent.ProviderName { return agent.ProviderClaude }
func (p *failingInteractiveProvider) Capabilities() agent.Capabilities {
	return (&claude.Provider{}).Capabilities()
}
func (p *failingInteractiveProvider) Manifest() agent.HarnessManifest { return p.manifest }

func (p *failingInteractiveProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	p.raw = spec
	if _, err := agent.PreparePrompt(spec, p.manifest); err != nil {
		return nil, err
	}
	for name, content := range p.files {
		full := filepath.Join(spec.Cwd, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return nil, err
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			return nil, err
		}
	}
	done := make(chan struct{})
	close(done)
	session := &recordingInteractiveSession{
		done:   done,
		exit:   attachwire.NewNormalExit(p.exitCode),
		exitOK: true,
	}
	return &testInteractiveHandle{
		Handle:  &fakeHandle{events: make(chan agent.Event)},
		session: session,
	}, nil
}

func (*failingInteractiveProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}
func (*failingInteractiveProvider) Shutdown(context.Context) error { return nil }

// runInteractiveSession runs one interactive-mode session end to end through
// the production Runner.Run entry point: real worktree provisioning against
// a local fixture repository, the interactive PTY dispatch lane, and the
// post-Run teardown. preserveAlways mirrors Options.PreserveWorktreeAlways
// (tests that inspect the worktree keep it; teardown cases clear it).
func runInteractiveSession(t *testing.T, provider *failingInteractiveProvider, sessionID string, rescueDir string, preserveAlways bool) *Result {
	t.Helper()
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")
	server := mockPlatformServer(t)
	t.Cleanup(server.Close)
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: server.URL, WorkerID: "worker-1", AuthToken: "token",
		HTTPClient: server.Client(), BaseDelay: 1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := New(Options{
		Registry:               registry,
		WorktreeManager:        manager,
		Poster:                 poster,
		HTTPClient:             server.Client(),
		PreserveWorktreeAlways: preserveAlways,
		MaxSessionDuration:     -1,
		SkipBackstop:           true,
		SkipSteering:           true,
		SkipPostSession:        true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rescueDir != "" {
		r.rescueDir = rescueDir
	}
	qw := QueuedWork{
		QueuedWork: prompt.QueuedWork{
			SessionID:       sessionID,
			IssueID:         "issue-id",
			IssueIdentifier: "ISSUE-INTERACTIVE",
			WorkType:        "development",
			Mode:            prompt.InteractiveRunMode,
			Repository:      makeBareRepo(t),
		},
		WorkerID:        "worker-1",
		AuthToken:       "token",
		PlatformURL:     server.URL,
		ResolvedProfile: ResolvedProfile{Provider: agent.ProviderClaude},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := r.Run(ctx, qw)
	if err != nil && res == nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// TestRun_FailedInteractiveSessionRescuesBeforeTeardown pins the failed
// interactive path end to end through Run: a terminal session whose shell
// exits nonzero with uncommitted work has that work archived to the rescue
// directory first, and only then is the worktree removed. Before the rescue
// covered interactive teardowns, the same session deleted the worktree with
// no patch — the unpublished work was irrecoverable.
func TestRun_FailedInteractiveSessionRescuesBeforeTeardown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	rescueDir := t.TempDir()
	provider := &failingInteractiveProvider{
		manifest: (&claude.Provider{}).Manifest(),
		files:    map[string]string{"work.txt": "uncommitted\n"},
		exitCode: 3,
	}
	res := runInteractiveSession(t, provider, "interactive-failed-rescue", rescueDir, false)
	if res.Status != "failed" {
		t.Fatalf("Status = %q (%s: %s); want failed", res.Status, res.FailureMode, res.Error)
	}
	if !strings.Contains(res.Error, "exit 3") {
		t.Fatalf("Error = %q; want the PTY exit detail", res.Error)
	}
	if res.WorktreePath == "" {
		t.Fatal("failed session has no worktree path")
	}
	// The rescue runs before the teardown: the patch carries the work the
	// crashed terminal session left uncommitted.
	patch := onlyRescuePatch(t, rescueDir)
	if body := readFile(t, patch); !strings.Contains(body, "work.txt") {
		t.Errorf("rescue patch does not carry the uncommitted work.txt")
	}
	if _, err := os.Stat(res.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree stat err = %v; want the failed session's worktree torn down", err)
	}
}

// TestRun_FailedInteractiveSessionKeepsAWorkareaWhoseWorkCannotBePreserved
// pins the fail-closed guard on the failed terminal path: a terminal
// session whose shell exits nonzero with uncommitted work keeps its
// worktree when the rescue directory cannot be written, through the same
// production Run entry point the teardown test above drives.
func TestRun_FailedInteractiveSessionKeepsAWorkareaWhoseWorkCannotBePreserved(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("a file where the rescue directory should go\n"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	provider := &failingInteractiveProvider{
		manifest: (&claude.Provider{}).Manifest(),
		files:    map[string]string{"work.txt": "uncommitted\n"},
		exitCode: 3,
	}
	res := runInteractiveSession(t, provider, "interactive-failed-keep", blocked, false)
	if res.Status != "failed" {
		t.Fatalf("Status = %q (%s: %s); want failed", res.Status, res.FailureMode, res.Error)
	}
	if got := readFile(t, filepath.Join(res.WorktreePath, "work.txt")); got != "uncommitted\n" {
		t.Fatalf("work.txt = %q; want the unpreserved work kept in place", got)
	}
}

// TestRun_CompletedInteractiveSessionKeepsUnpublishedWorkarea pins the other
// side of the same gate: a completed interactive session with uncommitted
// work keeps its worktree through the publication hold, and the rescue
// writes no duplicate patch for the held work.
func TestRun_CompletedInteractiveSessionKeepsUnpublishedWorkarea(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	rescueDir := t.TempDir()
	provider := &failingInteractiveProvider{
		manifest: (&claude.Provider{}).Manifest(),
		files:    map[string]string{"work.txt": "uncommitted\n"},
		exitCode: 0,
	}
	res := runInteractiveSession(t, provider, "11111111-1111-4111-8111-111111111111", rescueDir, false)
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	if got := readFile(t, filepath.Join(res.WorktreePath, "work.txt")); got != "uncommitted\n" {
		t.Fatalf("work.txt = %q; want the unpublished work kept in place", got)
	}
	if patches := rescuePatches(t, rescueDir); len(patches) != 0 {
		t.Fatalf("rescue patches = %v; want none — the publication hold owns the completed path", patches)
	}
}
