package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/workarea"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// layoutHarness is a runner wired against an explicit worktree parent so a test
// can assert the exact on-disk workarea shape a session produces.
type layoutHarness struct {
	runner   *Runner
	wtParent string
	url      string
}

func newLayoutHarness(t *testing.T, provider agent.Provider) *layoutHarness {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	srv := mockPlatformServer(t)
	t.Cleanup(srv.Close)
	wtParent := t.TempDir()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL, WorkerID: "worker-1", AuthToken: "tok",
		HTTPClient: srv.Client(), BaseDelay: 1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r, err := New(Options{
		Registry:               reg,
		WorktreeManager:        wtm,
		Poster:                 poster,
		HTTPClient:             srv.Client(),
		MaxSessionDuration:     -1,
		SkipBackstop:           true,
		SkipSteering:           true,
		SkipPostSession:        true,
		PreserveWorktreeAlways: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &layoutHarness{runner: r, wtParent: wtParent, url: srv.URL}
}

func (h *layoutHarness) work(t *testing.T, sessionID, repo string, provider agent.ProviderName) QueuedWork {
	t.Helper()
	qw := QueuedWork{
		QueuedWork:      queuedWorkBase("REN-LAYOUT-1"),
		WorkerID:        "worker-1",
		AuthToken:       "tok",
		PlatformURL:     h.url,
		ResolvedProfile: ResolvedProfile{Provider: provider},
	}
	qw.SessionID = sessionID
	qw.Repository = repo
	return qw
}

// assertSessionOwnedLayout is the shared session-owned shape assertion: the session
// owns <worktree-parent>/<sessionID>/ and the SELECTED repository is a leaf
// inside it, named canonically from the repository URL.
func assertSessionOwnedLayout(t *testing.T, wtParent, sessionID, repo, gotRepoPath, gotRoot string) {
	t.Helper()
	wantRoot := filepath.Join(wtParent, sessionID)
	wantRepo := filepath.Join(wantRoot, workarea.RepositoryLeaf(repo))
	if gotRoot != wantRoot {
		t.Errorf("workareaRoot = %q; want %q", gotRoot, wantRoot)
	}
	if gotRepoPath != wantRepo {
		t.Errorf("repository worktree = %q; want %q", gotRepoPath, wantRepo)
	}
	if gotRepoPath == gotRoot {
		t.Errorf("repository path and workarea root are the same value %q — the two concepts must stay distinct", gotRoot)
	}
	if !strings.HasPrefix(gotRepoPath, wantRoot+string(filepath.Separator)) {
		t.Errorf("repository %q is not inside the session root %q", gotRepoPath, wantRoot)
	}
}

// TestRun_HeadlessProvisionsSessionOwnedWorkarea is the headless half of the
// interactive/headless parity V16 control.
//
// RED without the nesting change: WorktreePath is <parent>/<sessionID> and
// WorkareaRoot is empty.
func TestRun_HeadlessProvisionsSessionOwnedWorkarea(t *testing.T) {
	p, _ := stub.New()
	h := newLayoutHarness(t, p)
	repo := makeBareRepo(t)
	const sessionID = "sess-headless-layout"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, h.work(t, sessionID, repo, agent.ProviderStub))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q (Error=%q)", res.Status, res.Error)
	}
	assertSessionOwnedLayout(t, h.wtParent, sessionID, repo, res.WorktreePath, res.WorkareaRoot)
}

// TestRun_InteractiveLayoutMatchesHeadless is the parity V16 control
// An interactive session provisions the SAME
// session-owned layout as a headless one and launches the harness with CWD at
// the selected repository, not at the session root.
//
// RED without the nesting change: Spec.Cwd is <parent>/<sessionID>, which both
// fails the leaf assertion and equals the workarea root.
func TestRun_InteractiveLayoutMatchesHeadless(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	provider := &promptCaptureInteractiveProvider{
		name:     agent.ProviderClaude,
		caps:     (&claude.Provider{}).Capabilities(),
		manifest: (&claude.Provider{}).Manifest(),
	}
	h := newLayoutHarness(t, provider)
	repo := makeBareRepo(t)
	const sessionID = "sess-interactive-layout"

	qw := h.work(t, sessionID, repo, agent.ProviderClaude)
	qw.Mode = prompt.InteractiveRunMode
	qw.InitialPrompt = "seed"
	qw.CodeIntel = &prompt.CodeIntelWork{Repo: repo}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run result=%+v err=%v", res, err)
	}

	assertSessionOwnedLayout(t, h.wtParent, sessionID, repo, res.WorktreePath, res.WorkareaRoot)
	// The harness CWD is the selected repository, never the session root.
	if provider.raw.Cwd != res.WorktreePath {
		t.Errorf("interactive Spec.Cwd = %q; want the selected repository %q", provider.raw.Cwd, res.WorktreePath)
	}
	if provider.raw.Cwd == res.WorkareaRoot {
		t.Errorf("interactive harness launched at the workarea root %q instead of the selected repository", res.WorkareaRoot)
	}

	// Code-intel indexes the SELECTED repository, never the session root — a
	// root-scoped index would sweep in every read-only context repository the
	// session materialized beside it.
	assertCodeIntelRoot(t, provider.raw.MCPServers, res.WorktreePath, res.WorkareaRoot)
}

// assertCodeIntelRoot finds the in-box code-intel MCP entry and asserts its
// --root argument is the repository worktree.
func assertCodeIntelRoot(t *testing.T, servers []agent.MCPServerConfig, wantRoot, workareaRoot string) {
	t.Helper()
	for _, srv := range servers {
		for i, arg := range srv.Args {
			if arg != "--root" || i+1 >= len(srv.Args) {
				continue
			}
			got := srv.Args[i+1]
			if got != wantRoot {
				t.Errorf("code-intel --root = %q; want the selected repository %q", got, wantRoot)
			}
			if got == workareaRoot {
				t.Errorf("code-intel rooted at the session workarea root %q; it would index sibling context repos", workareaRoot)
			}
			return
		}
	}
	t.Error("no code-intel MCP server with a --root argument was wired")
}

// TestRun_SelectedRepositoryPinIsHonored is the repository-pin V16 control
// When the work item names a repository that is
// NOT the project primary, the provisioned worktree's leaf is derived from the
// pinned repository and its origin remote points at the pin — never at the
// primary.
//
// RED without the nesting change: both repositories provision to the identical
// flat path <parent>/<sessionID>, so the leaf carries no repository identity at
// all and the two sessions are indistinguishable on disk.
func TestRun_SelectedRepositoryPinIsHonored(t *testing.T) {
	p, _ := stub.New()
	h := newLayoutHarness(t, p)

	primary := makeNamedBareRepo(t, "acme-primary.git")
	secondary := makeNamedBareRepo(t, "acme-secondary.git")
	if workarea.RepositoryLeaf(primary) == workarea.RepositoryLeaf(secondary) {
		t.Fatalf("fixture repos share a leaf %q; the pin assertion would be vacuous",
			workarea.RepositoryLeaf(primary))
	}
	const sessionID = "sess-pinned-secondary"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, h.work(t, sessionID, secondary, agent.ProviderStub))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("Status = %q (Error=%q)", res.Status, res.Error)
	}

	assertSessionOwnedLayout(t, h.wtParent, sessionID, secondary, res.WorktreePath, res.WorkareaRoot)
	if leaf := filepath.Base(res.WorktreePath); leaf == workarea.RepositoryLeaf(primary) {
		t.Errorf("pinned session silently fell back to the primary repository leaf %q", leaf)
	}

	origin, gitErr := runGit(ctx, res.WorktreePath, gitIdentity{}, "remote", "get-url", "origin")
	if gitErr != nil {
		t.Fatalf("git remote get-url origin: %v (%s)", gitErr, origin)
	}
	if got := strings.TrimSpace(origin); got != secondary {
		t.Errorf("origin = %q; want the pinned repository %q", got, secondary)
	}
}

// makeNamedBareRepo is makeBareRepo with a caller-chosen bare-repo basename, so
// a test can distinguish two repositories by their derived workarea leaf.
func makeNamedBareRepo(t *testing.T, name string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	work := t.TempDir()
	gitInit(t, work)
	bare := filepath.Join(t.TempDir(), name)
	//nolint:gosec // G204: test fixture, name comes from test callers.
	cmd := exec.Command("git", "clone", "--bare", work, bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare: %v\n%s", err, out)
	}
	return bare
}
