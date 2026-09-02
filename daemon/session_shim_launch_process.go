package daemon

import (
	"log/slog"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// shimLaunchProcess is the launched-but-not-yet-adopted worker process, as the
// discovery wait and its abandon path see it.
//
// Two verbs, and both of them REAP, because the daemon is still that process's
// parent at the OS level. startShimProcess releases the exec.Cmd handle (§D1 —
// the daemon must not be the thing that has to reap a shim in order to be
// replaceable), but release only disowns the Go-side handle: until this daemon
// exits and the process is reparented, an exited child that nobody waits on is a
// defunct entry. The measured incident left exactly that behind.
//
// It is an interface only so a test can script a process's liveness without
// racing a real one; production always resolves to the OS-backed
// newShimLaunchProcess.
type shimLaunchProcess interface {
	// Alive reports whether the launched process is still running, and REAPS it
	// when it has exited — the two are one observation, never two steps a caller
	// could pair wrongly. A process that exited reads as not alive exactly once
	// it has been reaped, so "not alive" always implies "no defunct entry".
	Alive() (bool, error)
	// StopAndReap terminates the launched process group and reaps the direct
	// child. It is idempotent: a process that already exited is not an error,
	// because every abandon path may reach it after the process died on its own.
	StopAndReap() error
}

// shimLiveDiscoveryExtensionFactor derives the LIVE discovery bound from the
// ordinary launch timeout: 4 × launchTimeout(), which is 120s at the shipped
// 30s default.
//
// Why derived rather than a second free-standing constant: the two bounds
// answer the same question about the same launch, so an embedder that
// configures a tighter or looser LaunchTimeout must move both together — a
// fixed 120s alongside a 5s launch timeout would be a 24× extension nobody
// chose, and alongside a 5-minute one it would be no extension at all.
//
// Why 4×: defaultShimLaunchTimeout is already sized for an ordinary harness
// cold start on an unloaded host. What was measured is the same cold start under
// CONCURRENT launch load — four interactive launches inside two minutes, where
// one worker's bootstrap had not published its discovery record 31s after spawn
// while the identical launch shape adopted in ~30s when it ran alone. A small
// multiple of the ordinary budget is what that shape needs; it is not open-ended,
// because a launch that will never publish must still fail the accept rather
// than hold a capacity slot forever.
//
// The extension is only ever spent while the launched process is ALIVE. A pid
// that dies ends the wait on the spot, so this bound costs nothing on the
// failure shape it does not cover.
const shimLiveDiscoveryExtensionFactor = 4

// liveDiscoveryTimeout is the TOTAL discovery budget for a launch whose process
// stays alive, measured from the start of the discovery wait — not an extra
// budget added after launchTimeout() expires.
func (c SessionShimConfig) liveDiscoveryTimeout() time.Duration {
	return shimLiveDiscoveryExtensionFactor * c.launchTimeout()
}

// shimLaunchProcessControl resolves the control for a freshly launched process,
// honouring the test seam when one is installed.
func (d *Daemon) shimLaunchProcessControl(started sessionshim.ProcessIdentity) shimLaunchProcess {
	d.shims.mu.RLock()
	factory := d.shims.launchProcess
	d.shims.mu.RUnlock()
	if factory != nil {
		return factory(started)
	}
	return newShimLaunchProcess(started)
}

// stopAbandonedShimLaunch stops the worker this daemon launched and then walked
// away from, so "the spawn was aborted" is TRUE about this host when the accept
// error is returned.
//
// # WHY THIS IS NOT OPTIONAL, AND WHY IT IS THE PROCESS HALF ONLY
//
// This is the same obligation releaseAmbiguousLaunchSessionShim discharges after
// an outcome-unknown commit, arrived at one step earlier in the launch. There,
// three things had to be true: the lineage published, the harness stopped, and
// the stop's terminal proof consumed. Here only the middle one exists, and that
// is a fact about where in the launch this happens, not a relaxation:
//
//   - NOTHING IS PUBLISHED, so there is nothing to publish. The launch never
//     reached completeSessionShimAdoption, so the control plane holds no
//     per-session adoption record for this lineage, no batch can be refused for
//     omitting it, and quarantining it would need a ShimID this launch never
//     learned — the discovery record that would have carried it is the very
//     thing that never appeared.
//   - NO CONTROLLER EXISTS, so Controller.Stop — the generation-fenced verb the
//     ambiguous release uses — cannot be sent. A shim that never published a
//     record has no socket to dial. The process group IS the only handle, so the
//     stop is the process-group termination the ambiguous path falls back to
//     when its own controller is nil.
//   - NO TERMINAL PROOF IS OWED, because nothing durable ever attested this
//     lineage. The reap below is the whole proof, and it is synchronous: waitpid
//     on our own child, not a tombstone another process has to publish.
//
// # WHY NOTHING ELSE WOULD EVER STOP IT
//
// The launch never reached trackLaunchedShim, so the session is not in
// d.shims.adopted and no adopted-set pass, startup adoption pass, or quarantine
// reconciliation can ever find it. And a worker that never published a discovery
// record has not reached sessionshim.Start either, so its own bounded orphan
// clock — the escape hatch §D10 relies on for a launch this daemon cannot
// identify — was never armed. Measured live: the abandoned worker went on to run
// its entire prompt un-adopted, send its messages, exit on its own terms, and
// end as a defunct entry under the daemon that spawned it.
//
// Best-effort by contract: the accept fails either way. A stop that cannot be
// completed is logged loudly with the pid, because that is the one case where an
// operator has something left to do.
func (d *Daemon) stopAbandonedShimLaunch(
	id sessionshim.Identity,
	started sessionshim.ProcessIdentity,
	process shimLaunchProcess,
	causeErr error,
) {
	if process == nil {
		return
	}
	if err := process.StopAndReap(); err != nil {
		slog.Error("session shim: could not stop the worker this launch abandoned; it may keep running un-adopted "+
			"(shim-discovery-deadline-2026-09-02)",
			"session", id.String(), "pid", started.PID, "discoveryError", causeErr, "error", err)
		return
	}
	slog.Warn("session shim: stopped and reaped the worker this launch abandoned, so the aborted spawn is true about this host "+
		"(shim-discovery-deadline-2026-09-02)",
		"session", id.String(), "pid", started.PID, "discoveryError", causeErr)
}
