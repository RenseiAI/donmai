//go:build unix

package runner

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed testdata/signal-guard-pkill.sh
var signalGuardPkillScript string

//go:embed testdata/signal-guard-killall.sh
var signalGuardKillallScript string

// signalGuardEnv names the environment variables the runner-provided
// pkill/killall guard scripts read. The runner stamps them per session;
// the guard refuses any pattern kill whose target is neither inside the
// session's worktree (or workarea root) nor a member of the session's
// recorded process group.
const (
	// signalGuardWorktreeEnv carries the session worktree path.
	signalGuardWorktreeEnv = "DONMAI_SESSION_WORKTREE"
	// signalGuardWorkareaRootEnv carries the session workarea root.
	signalGuardWorkareaRootEnv = "DONMAI_SESSION_WORKAREA_ROOT"
	// signalGuardProcessGroupEnv carries the session's recorded
	// process-group id. The runner records it from the spawned harness
	// process; empty means the group leg is unavailable.
	signalGuardProcessGroupEnv = "DONMAI_SESSION_PGID"
	// signalGuardEventLogEnv carries the path of the append-only event
	// log the guard writes each refusal to. The runner drains it into
	// typed activity events after the session ends.
	signalGuardEventLogEnv = "DONMAI_SIGNAL_GUARD_EVENT_LOG"
	// signalGuardDirName is the session-owned directory inside the
	// worktree that holds the guard scripts and the refusal log.
	signalGuardDirName = ".agent/signal-guard"
)

// signalGuardLayout is the installed per-session guard: the directory
// holding the scripts plus the paths the session env must carry.
type signalGuardLayout struct {
	// dir is the guard directory inside the session worktree.
	dir string
	// eventLog is the refusal log the guard appends to.
	eventLog string
	// pkill and killall are the guard script paths prepended to PATH.
	pkill   string
	killall string
}

// installSignalGuard writes the runner-provided pkill/killall guard
// scripts into the session worktree and returns the layout the session
// env must carry. The scripts ship with the runner binary (go:embed),
// so they survive upgrades with no hand-installed step. Best-effort:
// an install failure returns an error and the caller falls back to an
// unguarded PATH rather than failing the session.
func installSignalGuard(worktreePath string) (signalGuardLayout, error) {
	var layout signalGuardLayout
	if worktreePath == "" {
		return layout, fmt.Errorf("runner: signal guard needs a worktree path")
	}
	dir := filepath.Join(worktreePath, signalGuardDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: the guard dir holds session-executed shims; PATH traversal needs o+rx like any bin dir.
		return layout, fmt.Errorf("runner: create signal guard dir: %w", err)
	}
	layout = signalGuardLayout{
		dir:      dir,
		eventLog: filepath.Join(dir, "refusals.log"),
		pkill:    filepath.Join(dir, "pkill"),
		killall:  filepath.Join(dir, "killall"),
	}
	scripts := map[string]string{
		layout.pkill:   signalGuardPkillScript,
		layout.killall: signalGuardKillallScript,
	}
	for path, body := range scripts {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // G306: guard scripts are session-executed shims; they must carry the exec bit.
			return signalGuardLayout{}, fmt.Errorf("runner: write signal guard script: %w", err)
		}
	}
	return layout, nil
}

// signalGuardEnv overlays the guard layout onto the session env: the
// guard directory leads PATH so the session's pkill/killall resolve to
// the scoped guards, and the scope variables name the worktree, the
// workarea root, and (when known) the recorded process group. A nil env
// is allocated. Callers pass pgid <= 0 when the harness process has not
// been observed yet; the runner refreshes the variable once
// OnProcessSpawned fires.
func signalGuardEnv(env map[string]string, layout signalGuardLayout, workareaRoot string, pgid int) map[string]string {
	if env == nil {
		env = map[string]string{}
	}
	if layout.dir != "" {
		if prior := env["PATH"]; prior != "" {
			env["PATH"] = layout.dir + string(os.PathListSeparator) + prior
		} else if host := os.Getenv("PATH"); host != "" {
			env["PATH"] = layout.dir + string(os.PathListSeparator) + host
		} else {
			env["PATH"] = layout.dir
		}
		// The guard directory is <worktree>/.agent/signal-guard, so the
		// worktree is two levels up from either installed script path.
		env[signalGuardWorktreeEnv] = filepath.Dir(filepath.Dir(filepath.Dir(layout.pkill)))
		if workareaRoot != "" {
			env[signalGuardWorkareaRootEnv] = workareaRoot
		}
		env[signalGuardEventLogEnv] = layout.eventLog
		if pgid > 0 {
			env[signalGuardProcessGroupEnv] = fmt.Sprintf("%d", pgid)
		}
	}
	return env
}
