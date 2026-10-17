package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/internal/kit"
	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// DefaultDepsInstallTimeout bounds the whole post-acquire dependency
// install step (every ecosystem command below shares one deadline).
// Installing from a warm cache usually takes seconds; five minutes
// leaves headroom for a cold cache while keeping a wedged install
// from eating the session budget. Override per Runner via
// Options.DepsInstallTimeout.
const DefaultDepsInstallTimeout = 5 * time.Minute

// pnpmDepsInstallCommand installs a pnpm worktree's dependencies from
// its lockfile. --frozen-lockfile refuses to resolve or mutate the
// lockfile (the agent must see the committed tree, not a fresh
// resolve); --prefer-offline favours the shared store/cache so a warm
// seat starts fast. Lifecycle scripts and .pnpmfile code are disabled: this
// preparation runs outside the harness's confinement.
const pnpmDepsInstallCommand = "pnpm install --prefer-offline --frozen-lockfile --ignore-scripts --ignore-pnpmfile"

// goDepsInstallCommand pre-populates a Go worktree's module cache so
// the agent's first build does not pay the download.
const goDepsInstallCommand = "go mod download"

// installSessionDependencies installs the acquired worktree's
// dependencies before the agent spawns. Best-effort by contract: any
// failure is logged and the run continues with whatever is on disk,
// never failing the session.
//
//   - A pnpm-lock.yaml triggers the pnpm command above, skipped when
//     node_modules/.bin already exists (the kit post_acquire hook or
//     a previous run already installed).
//   - A go.mod triggers the go command above.
//   - pnpm runs before go when a worktree carries both.
//   - readOnlyCheckout (the selected repository is read-only) skips
//     the step: the runner may not write into that checkout.
//   - Every command shares one timeout and runs in wpath via the
//     Runner's DepsExecer (production: direct argv execution with a
//     minimal environment and process-group cancellation).
func (r *Runner) installSessionDependencies(ctx context.Context, qw QueuedWork, wpath string, readOnlyCheckout bool) {
	if readOnlyCheckout {
		r.logger.Debug("dependency install skipped: selected repository is read-only",
			"sessionId", qw.SessionID)
		return
	}
	if !depsInstallPermitted(qw) {
		r.logger.Debug("dependency install skipped: execution security requires confinement", "sessionId", qw.SessionID)
		return
	}
	if wpath == "" {
		return
	}
	timeout := r.depsInstallTimeout
	if timeout == 0 {
		timeout = DefaultDepsInstallTimeout
	}
	installCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		installCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var execer kit.Execer = r.depsExecer
	if execer == nil {
		execer = depsInstallExecer{}
	}
	if _, err := os.Stat(filepath.Join(wpath, "pnpm-lock.yaml")); err == nil {
		if _, err := os.Stat(filepath.Join(wpath, "node_modules", ".bin")); err != nil {
			r.runDepsInstallCommand(installCtx, qw, execer, wpath, pnpmDepsInstallCommand)
		} else {
			r.logger.Info("dependency install skipped: node_modules already present",
				"sessionId", qw.SessionID, "dir", wpath)
		}
	}
	if _, err := os.Stat(filepath.Join(wpath, "go.mod")); err == nil {
		r.runDepsInstallCommand(installCtx, qw, execer, wpath, goDepsInstallCommand)
	}
}

// runDepsInstallCommand runs one dependency install command and logs
// the outcome. A non-zero exit or exec error is a warning, never a
// session failure — the agent starts regardless.
func (r *Runner) runDepsInstallCommand(ctx context.Context, qw QueuedWork, execer kit.Execer, dir, command string) {
	r.logger.Info("installing repository dependencies",
		"sessionId", qw.SessionID, "dir", dir, "command", command)
	code, err := execer.Exec(ctx, dir, command, depsInstallEnv(qw))
	switch {
	case err != nil:
		r.logger.Warn("repository dependency install failed; continuing without installed dependencies",
			"sessionId", qw.SessionID, "command", command, "err", err)
	case code != 0:
		r.logger.Warn("repository dependency install failed; continuing without installed dependencies",
			"sessionId", qw.SessionID, "command", command, "exit", code)
	default:
		r.logger.Info("repository dependency install complete",
			"sessionId", qw.SessionID, "command", command)
	}
}

// The installer runs outside harness confinement. Only the weakest levels
// permit its host reads, cache writes and unmediated network access. Absent
// stamps keep the existing wire-compatible index-zero interpretation.
func depsInstallPermitted(qw QueuedWork) bool {
	return agent.EffectiveExecutionSecurityLevels(qw.ExecutionSecurity) == agent.IndexZeroExecutionSecurityLevels()
}

// depsInstallEnv admits tool lookup, home and cache locations only. Session
// authentication and arbitrary inherited variables never reach repository
// configuration (including .npmrc variable interpolation).
func depsInstallEnv(qw QueuedWork) map[string]string {
	env := make(map[string]string)
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "XDG_CACHE_HOME", "GOMODCACHE", "GOCACHE", "GOPATH", "PNPM_HOME"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	return applySeatBudget(env, qw.SeatBudget)
}

const (
	depsInstallOutputLimit = 16 * 1024
	depsInstallKillGrace   = time.Second
)

// depsInstallOutput bounds memory even when a failing tool floods its pipes.
// stdout and stderr share this writer, so os/exec serializes their writes.
type depsInstallOutput struct{ data []byte }

func (b *depsInstallOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := depsInstallOutputLimit - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
	}
	b.data = append(b.data, p...)
	return n, nil
}

type depsInstallExecer struct{}

func (depsInstallExecer) Exec(ctx context.Context, dir, command string, env map[string]string) (int, error) {
	// This seam accepts exactly the two runner-authored commands, never shell
	// syntax or arbitrary commands derived from repository content.
	var cmd *exec.Cmd
	switch command {
	case pnpmDepsInstallCommand:
		cmd = exec.CommandContext(ctx, "pnpm", "install", "--prefer-offline", "--frozen-lockfile", "--ignore-scripts", "--ignore-pnpmfile")
	case goDepsInstallCommand:
		cmd = exec.CommandContext(ctx, "go", "mod", "download")
	default:
		return -1, errors.New("dependency install: unsupported command")
	}

	cmd.Dir = dir
	cmd.Env = runtimeenv.ComposeChildEnv(nil, env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancel runs while the leader is still unreaped. Its PID is also the
	// process group ID; looking it up after Wait would miss surviving children.
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	// A descendant can escape the group while retaining an output pipe.
	// Bound that wait as well as the direct child's lifetime.
	cmd.WaitDelay = depsInstallKillGrace
	var output depsInstallOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	code := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return code, fmt.Errorf("dependency install: %w; output: %s", err, output.data)
}
