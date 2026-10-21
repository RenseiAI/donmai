//go:build unix

package runner

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed signalguard/signal-guard.sh
var signalGuardScript string

// signalGuardEnv names the environment variables the runner-provided
// pkill/killall guard reads. The runner stamps them per session; the
// guard refuses any pattern kill whose target is neither inside the
// session's worktree (or workarea root) nor a member of the session's
// recorded process group.
const (
	// signalGuardWorktreeEnv carries the session worktree path.
	signalGuardWorktreeEnv = "DONMAI_SESSION_WORKTREE"
	// signalGuardWorkareaRootEnv carries the session workarea root.
	signalGuardWorkareaRootEnv = "DONMAI_SESSION_WORKAREA_ROOT"
	// signalGuardProcessGroupEnv carries the session's recorded
	// process-group id. The runner records it from the spawned harness
	// pid into the guard directory's session.pgid file; the variable
	// remains as an explicit override the guard prefers when set.
	// Empty disables the group leg.
	signalGuardProcessGroupEnv = "DONMAI_SESSION_PGID"
	// signalGuardDirEnv carries the runner-owned guard directory so the
	// guard can read the recorded process-group file at exec time.
	signalGuardDirEnv = "DONMAI_SIGNAL_GUARD_DIR"
	// signalGuardEventLogEnv carries the path of the append-only event
	// log the guard writes each refusal to. The runner drains it into
	// typed activity events after the session ends.
	signalGuardEventLogEnv = "DONMAI_SIGNAL_GUARD_EVENT_LOG"
	// signalGuardPGIDFileName is the file in the guard directory that
	// carries the session's recorded process-group id, written by the
	// runner when the harness pid is observed.
	signalGuardPGIDFileName = "session.pgid"
)

// signalGuardLayout is the installed per-session guard: a runner-owned
// directory outside any repository checkout holding the pkill/killall
// shims plus the paths the session env must carry.
type signalGuardLayout struct {
	// dir is the runner-owned guard directory (a fresh temporary
	// directory, never inside the session checkout, so no checkout file
	// can shadow a host tool through the session PATH and the guard
	// install can never modify a read-only repository or pollute the
	// session's scratch commits).
	dir string
	// eventLog is the refusal log the guard appends to.
	eventLog string
	// pgidFile carries the session's recorded process-group id.
	pgidFile string
	// pkill and killall are the guard script installed under both names.
	pkill   string
	killall string
}

// installSignalGuard writes the runner-provided pkill/killall guard into
// a fresh runner-owned directory outside any checkout and returns the
// layout the session env must carry. The guard ships with the runner
// binary (go:embed), so it survives upgrades with no hand-installed step.
// The caller removes layout.dir when the session ends.
func installSignalGuard() (signalGuardLayout, error) {
	dir, err := os.MkdirTemp("", "donmai-signal-guard-*")
	if err != nil {
		return signalGuardLayout{}, fmt.Errorf("runner: create signal guard dir: %w", err)
	}
	// Owner-only: the agent runs as the same OS user, so this is not a
	// privilege boundary, but nothing else on the host needs access.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory, not a file: owner-only search/exec for the session guard dir.
		_ = os.RemoveAll(dir)
		return signalGuardLayout{}, fmt.Errorf("runner: secure signal guard dir: %w", err)
	}
	layout := signalGuardLayout{
		dir:      dir,
		eventLog: filepath.Join(dir, "refusals.log"),
		pgidFile: filepath.Join(dir, signalGuardPGIDFileName),
		pkill:    filepath.Join(dir, "pkill"),
		killall:  filepath.Join(dir, "killall"),
	}
	for _, path := range []string{layout.pkill, layout.killall} {
		if err := os.WriteFile(path, []byte(signalGuardScript), 0o755); err != nil { //nolint:gosec // G306: guard shims are session-executed; they must carry the exec bit.
			_ = os.RemoveAll(dir)
			return signalGuardLayout{}, fmt.Errorf("runner: write signal guard script: %w", err)
		}
	}
	return layout, nil
}

// signalGuardEnv overlays the guard layout onto the session env: the
// runner-owned guard directory leads PATH so the session's pkill/killall
// resolve to the scoped guards, and the scope variables name the session
// worktree and the workarea root. The process-group leg is delivered via
// the guard directory's session.pgid file (written when the harness pid
// is observed), not via the env snapshot below, because the harness has
// not spawned yet when this env is composed.
func signalGuardEnv(env map[string]string, layout signalGuardLayout, worktree string, workareaRoot string) map[string]string {
	if env == nil {
		env = map[string]string{}
	}
	if layout.dir == "" || worktree == "" {
		return env
	}
	if prior := env["PATH"]; prior != "" {
		env["PATH"] = layout.dir + string(os.PathListSeparator) + prior
	} else if host := os.Getenv("PATH"); host != "" {
		env["PATH"] = layout.dir + string(os.PathListSeparator) + host
	} else {
		env["PATH"] = layout.dir
	}
	env[signalGuardWorktreeEnv] = worktree
	if workareaRoot != "" {
		env[signalGuardWorkareaRootEnv] = workareaRoot
	}
	env[signalGuardDirEnv] = layout.dir
	env[signalGuardEventLogEnv] = layout.eventLog
	return env
}
