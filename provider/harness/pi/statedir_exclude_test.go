package pi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runtime/harnessstate"
)

// TestMaterializeExtension_LeavesCheckoutClean is the pi-side control for the
// motive half of the state-dir fix. Materializing a session's state into a git
// checkout must not make that checkout look dirty: `?? .pi/` in `git status`
// is what made a live session's storage read as deletable junk.
//
// This exercises the INTERACTIVE-seat path specifically — a checkout the
// workarea provision step never touched, which is where the 2026-08-29 loss
// happened. runtime/harnessstate carries the provision-side control.
//
// RED (with the harnessstate.EnsureExcluded call removed from
// materializeExtension):
//
//	git status --porcelain = "?? .pi/"
//
// GREEN: empty.
func TestMaterializeExtension_LeavesCheckoutClean(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "test"},
		{"add", "README.md"},
		{"commit", "--quiet", "-m", "seed"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test fixture; dir is t.TempDir() and args are literals.
		cmd.Env = harnessstate.GitLocationNeutralEnv(os.Environ())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	if _, err := materializeExtension(dir); err != nil {
		t.Fatalf("materializeExtension: %v", err)
	}

	status := exec.Command("git", "-C", dir, "status", "--porcelain") //nolint:gosec // G204: test fixture; dir is t.TempDir().
	status.Env = harnessstate.GitLocationNeutralEnv(os.Environ())
	out, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("checkout is dirty after materializing session state:\n%s", out)
	}
	// The state dir really is there — a clean status because nothing was
	// created would be a false pass.
	if _, err := os.Stat(filepath.Join(dir, piStateDir)); err != nil {
		t.Fatalf("state dir missing after materialize: %v", err)
	}
	// And the harness's own dir is one the shared table knows about, so the
	// backstop excludes it too.
	if !harnessstate.IsStateDir(piStateDir) {
		t.Fatalf("%s is not declared in runtime/harnessstate — the backstop would still stage it", piStateDir)
	}
	// Nothing tracked was touched: the project's own .gitignore is not ours to
	// edit.
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf(".gitignore was created in the checkout (err=%v) — the exclude belongs in git's own exclude file", err)
	}
}
