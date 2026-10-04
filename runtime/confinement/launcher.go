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

// HeadlessLauncher starts argv as a plain child with piped output in its own
// process group, as the headless harness spawn does.
func HeadlessLauncher() Launcher {
	return func(ctx context.Context, argv, env []string, dir string) (int, error) {
		if len(argv) == 0 {
			return -1, errors.New("confinement: empty command")
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: the confined argv the caller built.
		cmd.Dir = dir
		cmd.Env = overlayEnv(os.Environ(), env)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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

// overlayEnv applies KEY=VALUE overrides on top of base, last wins.
func overlayEnv(base, overrides []string) []string {
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
