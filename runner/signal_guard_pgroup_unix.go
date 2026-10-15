//go:build unix

package runner

import (
	"strconv"
	"syscall"

	"github.com/RenseiAI/donmai/runtime/state"
)

// chainOnProcessSpawned appends after to an existing OnProcessSpawned
// callback. Either side may be nil. The prepared-harness authority
// projection strips OnProcessSpawned before digesting (it is process
// state, not harness authority), so chaining here does not disturb
// receipt-bearing runs.
func chainOnProcessSpawned(existing func(pid int), after func(pid int)) func(pid int) {
	if existing == nil {
		return after
	}
	if after == nil {
		return existing
	}
	return func(pid int) {
		existing(pid)
		after(pid)
	}
}

// recordSessionProcessGroup records the session's process group from
// the spawned harness pid: every harness spawns its child in its own
// process group, so the group id is the leader's pid (the getpgid of a
// Setpgid child is its own pid). The group id refreshes the session
// env the pkill/killall guards read (the guards consult it as the
// second scope leg alongside the worktree) and is persisted to
// state.json for forensics. All legs are best-effort: a pid whose group
// cannot be resolved leaves the guards on the worktree leg alone.
func (r *Runner) recordSessionProcessGroup(statePath, sessionID string, pid int, specEnv map[string]string) {
	if pid <= 0 {
		return
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		r.logger.Warn("session process group unresolvable; guards use the worktree leg only",
			"sessionId", sessionID, "pid", pid, "err", err)
		return
	}
	if pgid <= 0 {
		return
	}
	if specEnv != nil {
		specEnv[signalGuardProcessGroupEnv] = strconv.Itoa(pgid)
	}
	if _, uerr := r.store.Update(statePath, func(s *state.State) error {
		s.SessionProcessGroup = pgid
		return nil
	}); uerr != nil {
		r.logger.Warn("state session-process-group update failed", "sessionId", sessionID, "err", uerr)
	}
}
