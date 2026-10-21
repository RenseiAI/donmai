//go:build unix

package runner

import (
	"os"
	"path/filepath"
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

// recordSessionProcessGroup records the session's process group from the
// spawned harness pid: every harness spawns its child in its own process
// group, so the group id is the leader's pid (the getpgid of a Setpgid
// child is its own pid). The group id is delivered to the pkill/killall
// guards through the guard directory's session.pgid file, which the
// guards read at exec time as the second scope leg alongside the worktree:
// a session process running outside the worktree (a daemon child, a tmp
// tool) is still in scope via its group, while another session's
// identical command line is not. The best-effort state.json record keeps
// the group visible for forensics. A pid the group cannot be resolved for
// leaves the guards on the worktree leg alone.
func (r *Runner) recordSessionProcessGroup(statePath, sessionID string, pid int, guardDir string) {
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
	if guardDir != "" {
		if werr := os.WriteFile(filepath.Join(guardDir, signalGuardPGIDFileName), []byte(strconv.Itoa(pgid)+"\n"), 0o600); werr != nil { //nolint:gosec // G306: the pgid file is session-readable by design; the guard runs as the session user.
			r.logger.Warn("session process-group file write failed; guards use the worktree leg only",
				"sessionId", sessionID, "err", werr)
		}
	}
	if _, uerr := r.store.Update(statePath, func(s *state.State) error {
		s.SessionProcessGroup = pgid
		return nil
	}); uerr != nil {
		r.logger.Warn("state session-process-group update failed", "sessionId", sessionID, "err", uerr)
	}
}
