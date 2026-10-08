//go:build darwin

package pi

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

// TestPiConfinement_WorktreeBeneathStateHomeDeniesTheToken drives the
// production entry point (confinePiSession) for the default host layout:
// the session's worktree lives beneath the daemon's state home, beside the
// control token. In both read scopes the seat is confined rather than
// refused, and from inside it the token does not read and neither the
// state home nor the worktrees directory lists, while the seat's own leaf
// reads and writes.
func TestPiConfinement_WorktreeBeneathStateHomeDeniesTheToken(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "pdp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	statehome.SetBaseHome(base)
	t.Cleanup(statehome.ResetForTest)
	t.Setenv(runtimeenv.ControlTokenPathEnv, "")
	state := statehome.StateDir("")
	token := filepath.Join(state, controlTokenLeaf)
	worktrees := filepath.Join(state, "worktrees")
	w := liveWorld{root: filepath.Join(worktrees, "s1"), outside: filepath.Join(base, "outside")}
	w.mut = filepath.Join(w.root, "mut")
	w.ro = filepath.Join(w.root, "ro")
	for _, dir := range []string{w.mut, w.ro, w.outside, filepath.Join(w.root, ".workarea")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	gitInit(t, w.mut)
	if err := os.WriteFile(token, []byte("sentinel-control-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := writeLiveHarness(t, w)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ensurePiConfiner(liveCtx(t), bin, *liveConfinementDirs(t), []string{exe})
	if err != nil {
		t.Fatalf("ensurePiConfiner: %v", err)
	}
	spec := w.spec()
	spec.RepositoryAuthority = w.authority()
	layout, err := materializeExtensionForSpec(spec, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, reads := range []piReadScope{{}, {level: agent.FileReadWorkarea}} {
		name := string(reads.level)
		if name == "" {
			name = "open"
		}
		t.Run(name, func(t *testing.T) {
			plan, err := confinePiSession(spec, layout, c, reads)
			if err != nil {
				t.Fatalf("a worktree beneath the state home was refused: %v", err)
			}
			t.Cleanup(func() { _ = plan.Release() })
			run := func(argv ...string) (int, string) {
				t.Helper()
				wrapped, err := plan.Command(argv)
				if err != nil {
					t.Fatalf("Command: %v", err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				cmd := exec.CommandContext(ctx, wrapped[0], wrapped[1:]...) //nolint:gosec // G204: the confined argv under test.
				cmd.Dir = w.mut
				cmd.Env = append(os.Environ(), plan.Environment()...)
				out, err := cmd.CombinedOutput()
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					return exitErr.ExitCode(), string(out)
				}
				if err != nil {
					t.Fatalf("run %v: %v", argv, err)
				}
				return 0, string(out)
			}
			for _, argv := range [][]string{{"/bin/cat", token}, {"/bin/ls", state}, {"/bin/ls", worktrees}} {
				if code, out := run(argv...); code == 0 || !strings.Contains(out, "Operation not permitted") {
					t.Errorf("confined %s %s: exit %d: %q; want EPERM", filepath.Base(argv[0]), filepath.Base(argv[1]), code, out)
				}
			}
			own := filepath.Join(w.mut, "own-"+name+".txt")
			if code, out := run("/bin/sh", "-c", `echo own > "`+own+`" && /bin/cat "`+own+`" && /bin/ls "`+w.mut+`" >/dev/null`); code != 0 || !strings.Contains(out, "own") {
				t.Errorf("confined work in the seat's own leaf: exit %d: %q", code, out)
			}
		})
	}
	if raw, err := os.ReadFile(token); err != nil || string(raw) != "sentinel-control-token\n" { //nolint:gosec // G304: the test's own sentinel.
		t.Errorf("the token changed: %q %v", raw, err)
	}
}
