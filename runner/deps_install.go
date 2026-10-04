package runner

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/RenseiAI/donmai/internal/kit"
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
// seat starts fast.
const pnpmDepsInstallCommand = "pnpm install --prefer-offline --frozen-lockfile"

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
//     Runner's DepsExecer (production: shellExecer, the same local
//     in-box executor the kit provisioner uses).
func (r *Runner) installSessionDependencies(ctx context.Context, qw QueuedWork, wpath string, readOnlyCheckout bool) {
	if readOnlyCheckout {
		r.logger.Debug("dependency install skipped: selected repository is read-only",
			"sessionId", qw.SessionID)
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
		execer = shellExecer{baseEnv: buildSessionEnv(qw)}
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
	code, err := execer.Exec(ctx, dir, command, nil)
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
