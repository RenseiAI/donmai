package opencode

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestOpenCodeConfigBoundary_SecretOutsideWorktreeAndGitClean(t *testing.T) {
	const secretSentinel = "opencode-platform-bearer-must-stay-outside-worktree"
	repo := t.TempDir()
	cmd := testGitCommand(t, "init", "-q", repo)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	tempRoot := t.TempDir()
	boundary, err := newOpenCodeConfigBoundary(tempRoot, agent.Spec{
		Cwd: repo,
		MCPServers: []agent.MCPServerConfig{{
			Name: "platform", Type: "http", URL: "https://example.invalid/mcp",
			Headers: map[string]string{"Authorization": "Bearer " + secretSentinel},
		}},
	})
	if err != nil {
		t.Fatalf("new config boundary: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })

	rel, err := filepath.Rel(repo, boundary.home)
	if err != nil {
		t.Fatalf("relative config path: %v", err)
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("owned config boundary %q is inside worktree %q", boundary.home, repo)
	}
	data, err := os.ReadFile(boundary.configPath)
	if err != nil {
		t.Fatalf("read owned config: %v", err)
	}
	if !strings.Contains(string(data), secretSentinel) {
		t.Fatal("owned config did not preserve the remote MCP bearer semantics")
	}
	status := testGitCommand(t, "-C", repo, "status", "--porcelain", "--untracked-files=all")
	output, err := status.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if len(output) != 0 {
		t.Fatalf("owned config created worktree residue: %q", output)
	}
	if strings.Contains(boundary.home, secretSentinel) || strings.Contains(boundary.configPath, secretSentinel) {
		t.Fatal("secret entered the owned config path")
	}

	if err := boundary.remove(); err != nil {
		t.Fatalf("remove config boundary: %v", err)
	}
	if _, err := os.Stat(boundary.home); !os.IsNotExist(err) {
		t.Fatalf("config boundary survived cleanup: %v", err)
	}
}

func testGitCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found: %v", err)
	}
	//nolint:gosec // G204: test-only git binary resolved via LookPath; arguments are controlled fixtures.
	return exec.Command(gitBin, args...)
}

func TestOpenCodeConfigBoundary_DoesNotFollowConfigSymlink(t *testing.T) {
	root := t.TempDir()
	boundary, err := newOpenCodeConfigBoundary(root, agent.Spec{})
	if err != nil {
		t.Fatalf("new config boundary: %v", err)
	}
	target := filepath.Join(root, "must-survive")
	const body = "not-owned"
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Remove(boundary.configPath); err != nil {
		t.Fatalf("remove config fixture: %v", err)
	}
	if err := os.Symlink(target, boundary.configPath); err != nil {
		t.Fatalf("symlink config fixture: %v", err)
	}
	if err := boundary.validate(); err == nil {
		t.Fatal("symlinked config passed validation")
	}
	if err := boundary.remove(); err != nil {
		t.Fatalf("remove config boundary: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != body {
		t.Fatalf("cleanup followed config symlink: body=%q err=%v", got, err)
	}
}

func TestOpenCodeConfigBoundary_RemovesReplacedHomeSymlinkOnly(t *testing.T) {
	root := t.TempDir()
	boundary, err := newOpenCodeConfigBoundary(root, agent.Spec{})
	if err != nil {
		t.Fatalf("new config boundary: %v", err)
	}
	target := filepath.Join(root, "external-directory")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	marker := filepath.Join(target, "must-survive")
	if err := os.WriteFile(marker, []byte("not-owned"), 0o600); err != nil {
		t.Fatalf("write target marker: %v", err)
	}
	if err := os.RemoveAll(boundary.home); err != nil {
		t.Fatalf("remove owned fixture: %v", err)
	}
	if err := os.Symlink(target, boundary.home); err != nil {
		t.Fatalf("replace home with symlink: %v", err)
	}
	if err := boundary.remove(); err != nil {
		t.Fatalf("remove replaced home link: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cleanup followed replaced home symlink: %v", err)
	}
	if _, err := os.Lstat(boundary.home); !os.IsNotExist(err) {
		t.Fatalf("replaced home symlink survived cleanup: %v", err)
	}
}

func TestOpenCodeServerResource_ConcurrentCloseIsIdempotent(t *testing.T) {
	boundary, err := newOpenCodeConfigBoundary(t.TempDir(), agent.Spec{})
	if err != nil {
		t.Fatalf("new config boundary: %v", err)
	}
	resource := &openCodeServerResource{config: boundary}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- resource.close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent close: %v", err)
		}
	}
	if _, err := os.Stat(boundary.home); !os.IsNotExist(err) {
		t.Fatalf("config boundary survived concurrent close: %v", err)
	}
}

func TestOpenCodeConfigBoundary_SessionsSharingCwdStayIsolated(t *testing.T) {
	repo := t.TempDir()
	tempRoot := t.TempDir()
	spec := agent.Spec{Cwd: repo}
	first, err := newOpenCodeConfigBoundary(tempRoot, spec)
	if err != nil {
		t.Fatalf("new first config boundary: %v", err)
	}
	second, err := newOpenCodeConfigBoundary(tempRoot, spec)
	if err != nil {
		_ = first.remove()
		t.Fatalf("new second config boundary: %v", err)
	}
	t.Cleanup(func() {
		_ = first.remove()
		_ = second.remove()
	})
	if first.home == second.home || first.configPath == second.configPath {
		t.Fatalf("sessions sharing cwd reused config boundary: first=%q second=%q", first.home, second.home)
	}

	p := &Provider{}
	firstResource := &openCodeServerResource{config: first}
	secondResource := &openCodeServerResource{config: second}
	if err := p.registerResource(firstResource); err != nil {
		t.Fatalf("register first resource: %v", err)
	}
	if err := p.registerResource(secondResource); err != nil {
		t.Fatalf("register second resource: %v", err)
	}
	if err := p.releaseResource(firstResource); err != nil {
		t.Fatalf("release first resource: %v", err)
	}
	if _, err := os.Stat(first.home); !os.IsNotExist(err) {
		t.Fatalf("first config survived release: %v", err)
	}
	if _, err := os.Stat(second.configPath); err != nil {
		t.Fatalf("releasing first config disturbed second: %v", err)
	}
	if err := p.releaseResource(secondResource); err != nil {
		t.Fatalf("release second resource: %v", err)
	}
	if _, err := os.Stat(second.home); !os.IsNotExist(err) {
		t.Fatalf("second config survived release: %v", err)
	}
}

func TestOpenCodeConfigBoundary_ParentSubstitutionFailurePersists(t *testing.T) {
	boundary, err := newOpenCodeConfigBoundary(t.TempDir(), agent.Spec{})
	if err != nil {
		t.Fatalf("new config boundary: %v", err)
	}
	otherInfo, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatalf("stat replacement identity: %v", err)
	}
	boundary.parentInfo = otherInfo
	first := boundary.remove()
	if first == nil || !strings.Contains(first.Error(), "parent identity changed") {
		t.Fatalf("first cleanup error = %v, want parent identity refusal", first)
	}
	if second := boundary.remove(); second == nil || second.Error() != first.Error() {
		t.Fatalf("second cleanup error = %v, want persistent %v", second, first)
	}
	resource := &openCodeServerResource{config: boundary}
	if err := resource.close(); !errors.Is(err, errOpenCodeConfigCleanup) {
		t.Fatalf("resource cleanup error = %v, want bounded sentinel", err)
	}
}
