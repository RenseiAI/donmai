package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// seat starts fast. --ignore-scripts blocks install/preinstall/
// postinstall lifecycle hooks and --ignore-pnpmfile blocks .pnpmfile.cjs
// hooks: a repository checkout is untrusted content, and the install
// step must fetch files, never execute them.
const pnpmDepsInstallCommand = "pnpm install --prefer-offline --frozen-lockfile --ignore-scripts --ignore-pnpmfile"

// goDepsInstallCommand is the display form of goDepsInstallArgv, logged
// and asserted in tests.
const goDepsInstallCommand = "go mod download"

// pnpmDepsInstallArgv installs a pnpm worktree's dependencies from its
// lockfile as a direct-exec argument vector: argv[0] is the binary
// resolved on the install env's PATH and the step never runs a shell, so
// a repository file (a .npmrc registry line, a packageManager field, a
// .pnpmfile.cjs hook, an install script) can only feed ARGUMENTS to the
// binary, never a command of its own. Scripts stay disabled separately
// via the --ignore-scripts / --ignore-pnpmfile flags below.
var pnpmDepsInstallArgv = depsInstallArgv{"pnpm", "install", "--prefer-offline", "--frozen-lockfile", "--ignore-scripts", "--ignore-pnpmfile"}

// goDepsInstallArgv pre-populates a Go worktree's module cache as a
// direct-exec argument vector. `go mod download` fetches modules without
// executing repository code.
var goDepsInstallArgv = depsInstallArgv{"go", "mod", "download"}

// depsInstallArgv is the direct-exec argument vector for one dependency
// install command.
type depsInstallArgv []string

// depsInstallStep binds one install command to the display line the
// runner logs and the test fake asserts command, directory and order
// on, so the runLoop order test keeps proving the install ran before
// spawn.
type depsInstallStep struct {
	// argv is the direct-exec argument vector; Command renders it for
	// the log line and the order assertion.
	argv depsInstallArgv
}

// Command renders the step's argument vector as a shell-style command
// line for the log line and the order assertion.
func (s depsInstallStep) Command() string {
	return strings.Join(s.argv, " ")
}

// pnpmDepsInstallStep is the display/argv binding for the pnpm step.
var pnpmDepsInstallStep = depsInstallStep{argv: pnpmDepsInstallArgv}

// goDepsInstallStep is the display/argv binding for the go step.
var goDepsInstallStep = depsInstallStep{argv: goDepsInstallArgv}

// depsInstallOutputLimit bounds the captured stdout/stderr kept per
// install command for the failure log. Installs are chatty (tarballs,
// progress lines); the tail is what diagnoses a failure.
const depsInstallOutputLimit = 8 << 10

// depsInstallKillGrace is the cooperative window between SIGTERM and
// SIGKILL when the shared install deadline expires: the install
// binary's own cleanup (store unlock, partial-file removal) runs first,
// then the whole group is reaped so a TERM-ignoring child tree cannot
// hold the step — and the session slot — past its budget.
const depsInstallKillGrace = 5 * time.Second

// depsInstallLevelsPermit reports whether the session's stamped levels
// permit running a repository-content install before spawn. The install
// is a network fetch into a checkout the runner may write, so it runs
// unless the stamp withholds either half: fileWrite at the workarea
// rung or stronger confines the writes to the session's own checkout
// (host withholds them), and any network rung but none (open, logged,
// or allow-list) leaves the registry reachable. Work without the
// section — index 0 on every dimension — runs it too: index 0 opens the
// same network and file surface the agent itself receives, so skipping
// there would leave the common case uninstalled while installing only
// the confined ones.
//
// The install's own hardening — no shell, no lifecycle scripts, the
// minimal env without credentials — is what covers the other
// dimensions, not the stamp: the step never needs the harness to
// render anything, so the permit check reads the LEVELS, never the
// harness manifest, and runs before the execution-security refusal
// below (loop.go step 6b), exactly like the kit toolchain provision it
// follows.
func depsInstallLevelsPermit(levels agent.ExecutionSecurityLevels) bool {
	fileWrite, ok := agent.ExecutionSecurityLevelIndex(agent.ExecutionSecurityFileWrite, levels.FileWrite)
	if !ok {
		return false
	}
	// Index 0 (host — the unstamped default) opens the same file
	// surface the agent itself receives, so the common case installs;
	// only the workarea rung confines the writes to the session's own
	// checkout while still permitting the step, and anything
	// off-ladder refuses.
	if fileWrite != 0 {
		workarea, _ := agent.ExecutionSecurityLevelIndex(agent.ExecutionSecurityFileWrite, agent.FileWriteWorkarea)
		if fileWrite != workarea {
			return false
		}
	}
	network, ok := agent.ExecutionSecurityLevelIndex(agent.ExecutionSecurityNetwork, levels.Network)
	if !ok {
		return false
	}
	// Any rung but none leaves the registry reachable: open and logged
	// ride the session's egress, allow-list through its channel. Only
	// none withholds the fetch.
	none, _ := agent.ExecutionSecurityLevelIndex(agent.ExecutionSecurityNetwork, agent.NetworkNone)
	return network != none
}

// depsInstallEnv builds the minimal environment one install command
// runs under. The install needs a toolchain lookup (PATH), a home for
// tool caches (HOME), the platform cache directories it shares with
// the harness child, and the seat's cooperative worker caps so its
// fan-out cannot escape the seat share. Everything else — worker and
// provider credentials, session identifiers, proxy and registry
// overrides — stays out: a repository file the install reads must
// never observe a bearer or steer the fetch. Callers pass the
// already-capped session env; only the allow-listed names cross the
// boundary.
func depsInstallEnv(home string, seatCaps, host map[string]string, extra map[string]string) map[string]string {
	out := make(map[string]string)
	// PATH wins over every fallback below: without it the install
	// binary is unresolvable.
	if seatCaps != nil {
		if path, ok := seatCaps["PATH"]; ok && path != "" {
			out["PATH"] = path
		}
	}
	if out["PATH"] == "" {
		if path := host["PATH"]; path != "" {
			out["PATH"] = path
		} else {
			out["PATH"] = "/usr/bin:/bin"
		}
	}
	if home != "" {
		out["HOME"] = home
	} else if h := host["HOME"]; h != "" {
		out["HOME"] = h
	}
	// Cache directories are shared with the harness child so a warm
	// store serves both; names outside this list are dropped even when
	// the session env carries them.
	for _, key := range []string{
		// Go module + build cache.
		"GOCACHE", "GOMODCACHE", "GOPATH", "GOTOOLCHAIN", "GOFLAGS", "GOENV",
		// pnpm store + npm cache.
		"PNPM_HOME", "PNPM_STORE_PATH", "npm_config_cache", "COREPACK_HOME",
		// Generic cache + temp dir.
		"XDG_CACHE_HOME", "TMPDIR", "TMP", "TEMP",
		// Locale keeps tool output parseable.
		"LC_ALL", "LANG",
	} {
		if extra != nil {
			if value, ok := extra[key]; ok && value != "" {
				out[key] = value
				continue
			}
		}
		if seatCaps != nil {
			if value, ok := seatCaps[key]; ok && value != "" {
				out[key] = value
				continue
			}
		}
		if value := host[key]; value != "" {
			out[key] = value
		}
	}
	// Seat worker caps (GOMAXPROCS, MAKEFLAGS, ...) ride along: the
	// install fans out like any other build.
	if seatCaps != nil {
		for key, value := range workerCapEnv(0) {
			if _, ok := out[key]; !ok {
				out[key] = value
			}
		}
		for _, key := range []string{"GOMAXPROCS", "MAKEFLAGS", "CMAKE_BUILD_PARALLEL_LEVEL", "NINJAFLAGS", "CARGO_BUILD_JOBS"} {
			if value, ok := seatCaps[key]; ok && value != "" {
				out[key] = value
			}
		}
	}
	return out
}

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
//   - The session's stamped levels gate the step (see
//     depsInstallLevelsPermit): the install is a network fetch into a
//     checkout the runner may write, so it runs only when the stamp
//     leaves the registry reachable and the workarea writable. The
//     gate reads the LEVELS, never the harness manifest, and runs
//     before the execution-security refusal (loop.go step 6b),
//     exactly like the kit toolchain provision it follows.
//   - Every command shares one timeout and runs in wpath without a
//     shell, under a minimal credential-free env, in its own process
//     group so expiry kills the whole tree. A failure captures the
//     bounded tail of the install output on the warning log and
//     continues.
//   - Production runs the hardened install execer (depsInstallExecer);
//     a test-injected DepsExecer (kit.Execer: command + dir + env)
//     asserts command, directory and order without running real
//     installs.
func (r *Runner) installSessionDependencies(ctx context.Context, qw QueuedWork, wpath string, readOnlyCheckout bool) {
	if readOnlyCheckout {
		r.logger.Debug("dependency install skipped: selected repository is read-only",
			"sessionId", qw.SessionID)
		return
	}
	if wpath == "" {
		return
	}
	levels := agent.EffectiveExecutionSecurityLevels(qw.ExecutionSecurity)
	if !depsInstallLevelsPermit(levels) {
		r.logger.Info("dependency install skipped: session levels do not permit the install",
			"sessionId", qw.SessionID,
			"fileWrite", levels.FileWrite,
			"network", levels.Network,
		)
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
		execer = depsInstallExecer{logger: r.logger}
	}
	if _, err := os.Stat(filepath.Join(wpath, "pnpm-lock.yaml")); err == nil {
		if _, err := os.Stat(filepath.Join(wpath, "node_modules", ".bin")); err != nil {
			r.runDepsInstallCommand(installCtx, qw, execer, wpath, pnpmDepsInstallStep)
		} else {
			r.logger.Info("dependency install skipped: node_modules already present",
				"sessionId", qw.SessionID, "dir", wpath)
		}
	}
	if _, err := os.Stat(filepath.Join(wpath, "go.mod")); err == nil {
		r.runDepsInstallCommand(installCtx, qw, execer, wpath, goDepsInstallStep)
	}
}

// runDepsInstallCommand runs one dependency install step and logs the
// outcome. A non-zero exit or exec error captures the bounded tail of
// the command's output on a warning and continues — the agent starts
// regardless.
func (r *Runner) runDepsInstallCommand(ctx context.Context, qw QueuedWork, execer kit.Execer, dir string, step depsInstallStep) {
	command := step.Command()
	r.logger.Info("installing repository dependencies",
		"sessionId", qw.SessionID, "dir", dir, "command", command)
	// The install env is derived here — not inside the execer — so the
	// boundary is one auditable call site: the seat-capped session env
	// plus the daemon host env, allow-listed down to the minimal set.
	env := depsInstallEnv(homeDir(), cappedSessionEnv(qw), hostEnv(), nil)
	code, err := execer.Exec(ctx, dir, command, env)
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

// depsInstallExecer is the production kit.Execer for the dependency
// install step. It runs each install command directly (no shell), in
// its own process group, with a bounded captured output tail logged on
// failure. The env arrives per call from runDepsInstallCommand; the
// base env only supplies the daemon-host fallback for PATH/HOME/cache
// names the call env omits.
type depsInstallExecer struct {
	logger interface {
		Warn(msg string, args ...any)
	}
}

// Exec runs argv (encoded as a command line by the step) directly,
// without a shell: the first word is the binary resolved on the call
// env's PATH, the rest are its arguments. Runner-only attach controls
// are stripped from every env layer before the repository-content
// install starts. The command runs in its own process group so a
// context expiry kills the whole tree (SIGTERM for cleanup, then
// SIGKILL); its combined output is captured bounded, and the tail is
// logged on failure.
func (e depsInstallExecer) Exec(ctx context.Context, dir, command string, env map[string]string) (int, error) {
	argv, err := depsInstallDecodeArgv(command)
	if err != nil {
		return -1, err
	}
	merged := runtimeenv.ComposeChildEnv(os.Environ(), env)
	// Resolve the binary against the CALL env's PATH — not the daemon
	// host's — so the lookup sees what the install will run with.
	resolved := depsInstallLookPath(argv[0], callEnvPath(merged))
	if resolved == "" {
		return -1, &exec.Error{Name: argv[0], Err: exec.ErrNotFound}
	}
	// nolint:gosec // G204: argv is the runner-owned install vector, never repository content.
	cmd := exec.CommandContext(ctx, resolved, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = merged
	setNewProcessGroup(cmd)
	var output depsInstallBoundedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	runErr := cmd.Run()
	// A context expiry must reap the WHOLE tree: CommandContext signals
	// the leader alone, so grandchildren (a registry fetch, a
	// post-resolution helper) would otherwise outlive the step and
	// keep writing into the worktree after the session moved on.
	if ctx.Err() != nil {
		killDepsInstallGroup(cmd, depsInstallKillGrace)
	}
	if runErr != nil {
		tail := output.tail()
		if tail != "" && e.logger != nil {
			e.logger.Warn("repository dependency install output",
				"dir", dir, "command", command, "output", tail)
		}
		var exitErr *exec.ExitError
		if asErr := asExitError(runErr); asErr != nil {
			_ = exitErr
			return asErr.ExitCode(), nil
		}
		return -1, runErr
	}
	return 0, nil
}

// depsInstallDecodeArgv decodes a step command line back into its
// argument vector. Steps are rendered from fixed vectors with no
// quoting (single words joined by spaces), so the decode is an exact
// split; anything else is refused rather than guessed.
func depsInstallDecodeArgv(command string) (depsInstallArgv, error) {
	for _, known := range []depsInstallArgv{pnpmDepsInstallArgv, goDepsInstallArgv} {
		if command == strings.Join(known, " ") {
			return append(depsInstallArgv(nil), known...), nil
		}
	}
	return nil, &exec.Error{Name: command, Err: exec.ErrNotFound}
}

// callEnvPath returns the PATH entry of a composed KEY=VALUE env, or ""
// when the install env carries none (the fallback then applies). The
// composition appends the call env after the inherited parent and the
// child resolves duplicates last-wins, so the LAST PATH entry is the
// call env's — the one the install will run with.
func callEnvPath(composed []string) string {
	path := ""
	for _, entry := range composed {
		if strings.HasPrefix(entry, "PATH=") {
			path = strings.TrimPrefix(entry, "PATH=")
		}
	}
	return path
}

// depsInstallLookPath resolves binary against pathList (a PATH-style
// list) the way exec.LookPath does, without consulting the daemon
// process PATH. Empty entries mean the current directory and are
// skipped: the install must never resolve a repository-shipped binary
// from the worktree.
func depsInstallLookPath(binary, pathList string) string {
	if strings.ContainsRune(binary, '/') {
		return ""
	}
	for _, dir := range strings.Split(pathList, string(os.PathListSeparator)) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, binary)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&0o111 == 0 {
			continue
		}
		return candidate
	}
	return ""
}

// depsInstallBoundedBuffer is an io.Writer that keeps the last
// depsInstallOutputLimit bytes of a command's combined output.
type depsInstallBoundedBuffer struct {
	kept []byte
}

// Write implements io.Writer, retaining only the tail.
func (b *depsInstallBoundedBuffer) Write(data []byte) (int, error) {
	b.kept = append(b.kept, data...)
	if len(b.kept) > depsInstallOutputLimit {
		b.kept = append([]byte(nil), b.kept[len(b.kept)-depsInstallOutputLimit:]...)
	}
	return len(data), nil
}

// tail returns the retained output as a string.
func (b *depsInstallBoundedBuffer) tail() string {
	return string(b.kept)
}
