package pi

import (
	"os"
	"os/exec"
	"testing"

	"github.com/RenseiAI/donmai/runtime/harnessstate"
)

// gitInit initializes a real git repository at dir: a confined mutable leaf
// must own its .git directory, so both live confinement proofs (the macOS
// file and the Linux file) build their worlds on one. Portable across the
// two: the helper lives here so neither proof duplicates it.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "-c", "init.defaultBranch=main", "init", "--quiet", dir) //nolint:gosec // G204: test fixture; dir is a test temp path.
	cmd.Env = harnessstate.GitLocationNeutralEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}
}
