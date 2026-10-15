//go:build !unix

package runner

// signalGuardEnv names the environment variables the runner-provided
// pkill/killall guard scripts read. Stubbed on platforms without
// process groups; the guards only install on unix.
const (
	signalGuardWorktreeEnv     = "DONMAI_SESSION_WORKTREE"
	signalGuardWorkareaRootEnv = "DONMAI_SESSION_WORKAREA_ROOT"
	signalGuardProcessGroupEnv = "DONMAI_SESSION_PGID"
	signalGuardEventLogEnv     = "DONMAI_SIGNAL_GUARD_EVENT_LOG"
	signalGuardDirName         = ".agent/signal-guard"
)

type signalGuardLayout struct {
	dir      string
	eventLog string
	pkill    string
	killall  string
}

func installSignalGuard(worktreePath string) (signalGuardLayout, error) {
	return signalGuardLayout{}, nil
}

func signalGuardEnv(env map[string]string, layout signalGuardLayout, workareaRoot string, pgid int) map[string]string {
	return env
}
