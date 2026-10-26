package confinement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/ptyhost"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// Launcher starts argv the way one session mode's spawn path starts a
// harness, waits for it to exit and returns its exit code. env holds
// KEY=VALUE overrides applied on top of the launcher's base environment.
type Launcher func(ctx context.Context, argv, env []string, dir string) (int, error)

// DefaultLaunchers returns the production spawn paths: a plain child in its
// own process group for headless sessions, and the PTY host for interactive
// ones.
func DefaultLaunchers() map[agent.PromptSessionMode]Launcher {
	return map[agent.PromptSessionMode]Launcher{
		agent.PromptModeAutonomous:      HeadlessLauncher(),
		agent.PromptModeHumanControlled: InteractiveLauncher(),
	}
}

// headlessWaitDelay backstops a canceled headless launch: after the group
// kill below, Wait gives captured output this long to drain before it
// forcibly closes the pipes. It only ever caps a stuck wait — a healthy
// run whose output drains returns without touching it.
const headlessWaitDelay = 5 * time.Second

// HeadlessLauncher starts argv as a plain child with piped output in its own
// process group, as the headless harness spawn does. The launcher's base
// (this process's environment) is stripped of engine re-exec controls
// before the trusted overrides are layered on top, so an inherited
// confinement, embedder-registered, or *_PROBE copy never selects a
// re-exec role for the child; only the producer-owned copies in env
// (Plan.Environment plus the self-test probe marker) reach it.
func HeadlessLauncher() Launcher {
	return func(ctx context.Context, argv, env []string, dir string) (int, error) {
		if len(argv) == 0 {
			return -1, errors.New("confinement: empty command")
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: the confined argv the caller built.
		cmd.Dir = dir
		cmd.Env = overlayEnv(runtimeenv.FilterEngineControlEnv(os.Environ()), env)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// Cancel kills the whole process group, not just the direct child:
		// the child may have forked (a shell wrapper, a daemonizing
		// interface), and a surviving grandchild holding the captured
		// stdout/stderr pipes open would wedge Wait in awaitGoroutines
		// forever — the stall an unserved probe must fail fast instead
		// of. This mirrors the production headless spawn's group kill.
		cmd.Cancel = func() error { return killHeadlessGroup(cmd) }
		// Backstop for a grandchild that escaped the group (setsid): Wait
		// can never outlive this after a cancel, so stopping an unserved
		// probe always terminates.
		cmd.WaitDelay = headlessWaitDelay
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		err := cmd.Run()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		if err != nil {
			return -1, fmt.Errorf("confinement: headless launch: %w", err)
		}
		return 0, nil
	}
}

// killHeadlessGroup SIGKILLs the process group the headless child leads
// (Setpgid above makes the child its own group leader). A group that is
// already gone reads as success; any other kill failure is reported so the
// WaitDelay backstop above is the only silence.
func killHeadlessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// InteractiveLauncher starts argv under a pseudo-terminal through the PTY
// host, as the interactive harness spawn inside the session shim does.
func InteractiveLauncher() Launcher {
	return func(ctx context.Context, argv, env []string, dir string) (int, error) {
		sess, err := ptyhost.Spawn(ptyhost.Spec{Command: argv, Env: env, Cwd: dir})
		if err != nil {
			return -1, fmt.Errorf("confinement: interactive launch: %w", err)
		}
		select {
		case <-sess.Done():
		case <-ctx.Done():
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = sess.Stop(stopCtx)
			cancel()
			<-sess.Done()
		}
		exit, _ := sess.Exit()
		if ctx.Err() != nil {
			return -1, fmt.Errorf("confinement: interactive launch: %w", ctx.Err())
		}
		return int(exit.ExitCode), nil //nolint:gosec // G115: exit codes are small.
	}
}

// overlayEnv applies KEY=VALUE overrides on top of base, last wins. Engine
// re-exec controls are stripped from the base layer before the overlay, so
// only the producer-owned copies in overrides (Plan.Environment plus the
// self-test probe marker) reach the child. Overrides are trusted and kept
// verbatim: filtering them would strip the boundary's own stage policy.
func overlayEnv(base, overrides []string) []string {
	base = runtimeenv.FilterEngineControlEnv(base)
	index := map[string]int{}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range append(append([]string{}, base...), overrides...) {
		key, _, _ := strings.Cut(kv, "=")
		if i, ok := index[key]; ok {
			out[i] = kv
			continue
		}
		index[key] = len(out)
		out = append(out, kv)
	}
	return out
}
