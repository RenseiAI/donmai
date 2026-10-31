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
// A pnpm-lock.yaml triggers the pnpm command above; a go.mod triggers
// the go command above. pnpm runs before go when a worktree carries
// both. The pnpm command always runs for a detected lockfile, even when
// node_modules/.bin already exists: a leftover .bin directory proves
// nothing about the lockfile the session actually checked out, so a
// stale or partial install must not read as ready. readOnlyCheckout
// (the selected repository is read-only) skips the step: the runner may
// not write into that checkout. Every command shares one timeout and
// runs in wpath via the Runner's DepsExecer (production: direct argv
// execution with a minimal environment and process-group cancellation).
//
// Recovery handoff (explicit, not automatic repair): the returned
// *agent.DependencyPreparation is the bounded outcome the runner retains
// on the terminal result and, for failed/timed_out outcomes, renders as
// a short runner-authored recovery instruction in the delivered harness
// protocol (see injectDependencyPreparationPartial). There is no second
// install attempt with weaker policy and no additional outbound fetch:
// the handoff asks the model-driving harness to inspect and repair with
// its own permitted tools, and to report blocked when repair cannot
// finish. It reports attempted preparation, never a guarantee of agent
// recovery. Cancellation never authorizes more work: once the install
// context expires the step stops and reports timed_out.
func (r *Runner) installSessionDependencies(ctx context.Context, qw QueuedWork, wpath string, readOnlyCheckout bool) *agent.DependencyPreparation {
	ecosystems := detectedDependencyEcosystems(wpath)
	if len(ecosystems) == 0 {
		return nil
	}
	if readOnlyCheckout {
		r.logger.Debug("dependency install skipped: selected repository is read-only",
			"sessionId", qw.SessionID)
		return &agent.DependencyPreparation{Status: agent.DependencyPreparationSkippedReadOnly, Ecosystems: ecosystems}
	}
	if !depsInstallPermitted(qw) {
		r.logger.Debug("dependency install skipped: execution security requires confinement", "sessionId", qw.SessionID)
		return &agent.DependencyPreparation{Status: agent.DependencyPreparationSkippedRestricted, Ecosystems: ecosystems}
	}
	if wpath == "" {
		return nil
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
	// Cancellation never authorizes more work: an already-expired
	// install context reports timed_out without running anything.
	if installCtx.Err() != nil {
		r.logger.Warn("repository dependency install timed out; continuing without installed dependencies",
			"sessionId", qw.SessionID, "err", installCtx.Err())
		return &agent.DependencyPreparation{Status: agent.DependencyPreparationTimedOut, Ecosystems: ecosystems}
	}
	timedOut := false
	failed := false
	if hasDependencyEcosystem(ecosystems, "pnpm") {
		pnpmTimedOut, pnpmFailed := r.runDepsInstallCommand(installCtx, qw, execer, wpath, pnpmDepsInstallCommand)
		timedOut = timedOut || pnpmTimedOut
		failed = failed || pnpmFailed
		// Re-evaluate after the first ecosystem so a cancellation
		// mid-install never authorizes the second install: an expired
		// shared context reports timed_out below without starting go.
		if installCtx.Err() != nil {
			timedOut = true
		}
	}
	// Run go unless the shared context expired or the first command
	// already consumed the shared deadline. A failed (non-timeout)
	// pnpm install still attempts go — failure is per-ecosystem,
	// best-effort — but a timed-out first command starts no more work.
	goTimedOut, goFailed := false, false
	if hasDependencyEcosystem(ecosystems, "go") && !timedOut && installCtx.Err() == nil {
		goTimedOut, goFailed = r.runDepsInstallCommand(installCtx, qw, execer, wpath, goDepsInstallCommand)
	}
	// A context that expired between ecosystems authorizes no more
	// work and reports timed_out even when nothing else failed.
	if installCtx.Err() != nil {
		timedOut = timedOut || true
	}
	timedOut = timedOut || goTimedOut
	failed = failed || goFailed
	switch {
	case timedOut:
		return &agent.DependencyPreparation{Status: agent.DependencyPreparationTimedOut, Ecosystems: ecosystems}
	case failed:
		return &agent.DependencyPreparation{Status: agent.DependencyPreparationFailed, Ecosystems: ecosystems}
	default:
		r.logger.Info("repository dependency preparation ready",
			"sessionId", qw.SessionID, "dir", wpath)
		return &agent.DependencyPreparation{Status: agent.DependencyPreparationReady, Ecosystems: ecosystems}
	}
}

// detectedDependencyEcosystems names the installable ecosystems present
// in wpath, in install order (pnpm before go). Bounded to the two
// runner-authored commands; never derived from repository content beyond
// the two manifest file probes below.
func detectedDependencyEcosystems(wpath string) []string {
	if wpath == "" {
		return nil
	}
	var ecosystems []string
	if _, err := os.Stat(filepath.Join(wpath, "pnpm-lock.yaml")); err == nil {
		ecosystems = append(ecosystems, "pnpm")
	}
	if _, err := os.Stat(filepath.Join(wpath, "go.mod")); err == nil {
		ecosystems = append(ecosystems, "go")
	}
	return ecosystems
}

func hasDependencyEcosystem(ecosystems []string, name string) bool {
	for _, ecosystem := range ecosystems {
		if ecosystem == name {
			return true
		}
	}
	return false
}

// dependencyPreparationNeedsRecovery reports whether a preparation
// outcome must carry the runner-authored recovery handoff into the
// delivered harness protocol: only failed and timed_out outcomes do.
// Ready and both skipped outcomes add no instruction; nil (no detected
// ecosystem) adds none either.
func dependencyPreparationNeedsRecovery(prep *agent.DependencyPreparation) bool {
	if prep == nil {
		return false
	}
	return prep.Status == agent.DependencyPreparationFailed || prep.Status == agent.DependencyPreparationTimedOut
}

// injectDependencyPreparationPartial appends the recovery handoff to the
// harness protocol when prep needs it, preserving the existing protocol
// text and the caller's original task. Ready, skipped, and nil outcomes
// return the protocol unchanged so they add no instruction and no
// bypass. Shell delivery never reaches this helper through the loop's
// provider gate; autonomous shell remediation is out of scope.
func injectDependencyPreparationPartial(existing string, prep *agent.DependencyPreparation) string {
	if !dependencyPreparationNeedsRecovery(prep) {
		return existing
	}
	partial := "# Dependency preparation\nDependency preparation did not complete (status: " + prep.Status + "). Dependencies are not ready. Inspect and repair the checkout with your permitted tools before relying on dependency-based checks; do not assume dependencies are installed. If repair cannot finish, report blocked with the reason."
	if existing == "" {
		return partial
	}
	return existing + "\n\n" + partial
}

// runDepsInstallCommand runs one dependency install command and logs
// the outcome. A non-zero exit or exec error is a warning, never a
// session failure — the agent starts regardless. It reports the
// per-command outcome as (timedOut, failed): an expired install context
// (deadline or parent cancellation) is timed_out even when the execer
// also reports an error, so cancellation never reads as a plain failure.
// Raw command output and error text stay in the log; the returned pair
// carries no output bytes.
func (r *Runner) runDepsInstallCommand(ctx context.Context, qw QueuedWork, execer kit.Execer, dir, command string) (timedOut bool, failed bool) {
	r.logger.Info("installing repository dependencies",
		"sessionId", qw.SessionID, "dir", dir, "command", command)
	code, err := execer.Exec(ctx, dir, command, depsInstallEnv(qw))
	if ctx.Err() != nil {
		r.logger.Warn("repository dependency install timed out; continuing without installed dependencies",
			"sessionId", qw.SessionID, "command", command, "err", ctx.Err())
		return true, false
	}
	switch {
	case err != nil:
		r.logger.Warn("repository dependency install failed; continuing without installed dependencies",
			"sessionId", qw.SessionID, "command", command, "err", err)
		return false, true
	case code != 0:
		r.logger.Warn("repository dependency install failed; continuing without installed dependencies",
			"sessionId", qw.SessionID, "command", command, "exit", code)
		return false, true
	default:
		r.logger.Info("repository dependency install complete",
			"sessionId", qw.SessionID, "command", command)
		return false, false
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
