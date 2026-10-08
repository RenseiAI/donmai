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
			plan, err := confinePiSession(spec, layout, c, reads, "")
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

// TestPiConfinement_OverrideTokenDeniedWithEnvStripped drives the
// production entry point (Provider.confineSession, the same call both
// spawn lanes make) with the token-file override set: the live token is
// minted at the override path, while the worker's environment — like every
// spawner-produced worker environment — has the override stripped. The
// daemon-stated path (Options.ControlTokenPath, threaded from the
// accept-time session detail) must still deny the live token: from inside
// the seat the overridden token does not read and its directory does not
// list, in both read scopes, while the seat's own leaf reads and writes.
//
// RED proof: construct the provider without ControlTokenPath (or resolve
// the deny from the stripped environment) and the overridden token reads.
func TestPiConfinement_OverrideTokenDeniedWithEnvStripped(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "pdo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	statehome.SetBaseHome(base)
	t.Cleanup(statehome.ResetForTest)
	// The host sets the override; the worker never sees it (the spawner
	// strips it from every worker environment).
	overrideDir := filepath.Join(base, "host-state")
	token := filepath.Join(overrideDir, "control-token")
	if err := os.MkdirAll(overrideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(runtimeenv.ControlTokenPathEnv, token)
	if err := os.WriteFile(token, []byte("sentinel-override-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worktrees := filepath.Join(base, "worktrees")
	w := liveWorld{root: filepath.Join(worktrees, "s1"), outside: filepath.Join(base, "outside")}
	w.mut = filepath.Join(w.root, "mut")
	w.ro = filepath.Join(w.root, "ro")
	for _, dir := range []string{w.mut, w.ro, w.outside, filepath.Join(w.root, ".workarea")} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	gitInit(t, w.mut)
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
	// The worker as the spawner hands it over: the override stripped, the
	// stated path threaded from the daemon in-process (never through env).
	t.Setenv(runtimeenv.ControlTokenPathEnv, "")
	p := &Provider{binary: bin, opts: Options{
		RequireConfinement: true,
		ControlTokenPath:   token,
		confinementDirs:    liveConfinementDirs(t),
	}}
	if got := os.Getenv(runtimeenv.ControlTokenPathEnv); got != "" {
		t.Fatalf("worker environment carries the override %q; the test would prove nothing", got)
	}
	for _, reads := range []piReadScope{{}, {level: agent.FileReadWorkarea}} {
		name := string(reads.level)
		if name == "" {
			name = "open"
		}
		t.Run(name, func(t *testing.T) {
			plan, err := p.confineSession(spec, layout, c)
			if err != nil {
				t.Fatalf("a worktree with a stated override token was refused: %v", err)
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
			// The live token at the override path is refused: a read
			// must fail (or return none of the sentinel) and the
			// override directory must not list.
			if code, out := run("/bin/cat", token); code == 0 && strings.Contains(out, "sentinel-override-token") {
				t.Errorf("confined cat of the overridden token: exit %d: %q; want EPERM", code, out)
			} else if code == 0 && !strings.Contains(out, "Operation not permitted") && !strings.Contains(out, "No such file") {
				t.Errorf("confined cat of the overridden token: exit %d: %q; want EPERM", code, out)
			}
			if code, out := run("/bin/ls", overrideDir); code == 0 || !strings.Contains(out, "Operation not permitted") {
				t.Errorf("confined ls of the override directory: exit %d: %q; want EPERM", code, out)
			}
			// The seat's own leaf still reads and writes.
			own := filepath.Join(w.mut, "own-override-"+name+".txt")
			if code, out := run("/bin/sh", "-c", `echo own > "`+own+`" && /bin/cat "`+own+`" && /bin/ls "`+w.mut+`" >/dev/null`); code != 0 || !strings.Contains(out, "own") {
				t.Errorf("confined work in the seat's own leaf: exit %d: %q", code, out)
			}
		})
	}
	if raw, err := os.ReadFile(token); err != nil || string(raw) != "sentinel-override-token\n" { //nolint:gosec // G304: the test's own sentinel.
		t.Errorf("the token changed: %q %v", raw, err)
	}
}
