package daemon

import (
	"runtime"

	"github.com/RenseiAI/donmai/daemon/seatbudget"
)

// SeatBudget is the spawner-visible per-seat resource budget: the resolved
// share one session's process tree may use. It mirrors seatbudget.Budget
// without importing the enforcement package into the spawner options'
// public shape more than once — the daemon converts at the boundary.
type SeatBudget struct {
	// CPUs is the whole-core seat share.
	CPUs int
	// MemoryMB is the seat memory ceiling in mebibytes. Zero means no cap.
	MemoryMB int
	// IOWeight is the cgroup v2 IO weight. Zero means backend default.
	IOWeight int
	// Mode is auto, enforced, best-effort or none. Empty reads as none
	// (budgeting off).
	Mode string
}

// toSeatBudget converts spawner options into the enforcement budget.
func (b SeatBudget) toSeatBudget() seatbudget.Budget {
	return seatbudget.Budget{
		CPUs:     b.CPUs,
		MemoryMB: b.MemoryMB,
		IOWeight: b.IOWeight,
		Mode:     seatbudget.Mode(b.Mode),
	}
}

// seatBudgetForSpawn resolves the effective seat budget for one spawn: the
// spawner's configured budget with its mode resolved for this OS. ok=false
// means budgeting is off — the seat spawns exactly as before. The share
// lives behind its own RWMutex (not the spawn mutex) because SetSeatBudget
// swaps it from the config watcher while AcceptWork/spawn run unlocked;
// an unlocked struct read there races the write under -race.
func (s *WorkerSpawner) seatBudgetForSpawn() (seatbudget.Budget, bool) {
	s.seatBudgetMu.RLock()
	defer s.seatBudgetMu.RUnlock()
	return s.seatBudgetForSpawnLocked()
}

// seatBudgetForSpawnLocked is seatBudgetForSpawn for callers that hold the
// seat-budget lock (read or write). Numeric limits clamp at the boundary:
// the config path validates, but an embedder composing a spawner directly
// bypasses it — a negative memory cap or IO weight must read as uncapped
// (the renderers already drop non-positive values) rather than travel
// into scope argv or reports as a negative number.
func (s *WorkerSpawner) seatBudgetForSpawnLocked() (seatbudget.Budget, bool) {
	b := s.seatBudget.toSeatBudget()
	if b.Disabled() {
		return seatbudget.Budget{}, false
	}
	if b.CPUs < 1 {
		b.CPUs = 1
	}
	if b.MemoryMB < 0 {
		b.MemoryMB = 0
	}
	if b.IOWeight < 0 {
		b.IOWeight = 0
	}
	return b, true
}

// applySeatBudgetToEnv overlays the seat's cooperative worker caps onto an
// already-composed seat environment. On Linux the hard limits come from the
// cgroup placement (applySeatBudgetToCmd); the env caps ride along so tools
// size their fan-out to the share instead of discovering the quota by
// hitting it. On macOS the env caps ARE the budget. Base-operator values
// win: ComposeEnv never overrides an explicitly set knob.
func applySeatBudgetToEnv(env []string, b seatbudget.Budget, ok bool) []string {
	if !ok {
		return env
	}
	return seatbudget.ComposeEnv(env, b.CPUs)
}

// applySeatBudgetToCmd wraps the seat command in its Linux cgroup placement.
// The wrap applies when the seat ASKED for hard enforcement (mode enforced
// or auto on Linux) and the systemd placement is available. "auto" on a
// non-Linux host, or an explicit best-effort mode, never wraps: the seat
// runs on cooperative caps and the report says best-effort. A seat that
// asked for enforcement but has no backend runs unwrapped and the report
// says none with the missing backend named — the spawn must not fail for
// want of a hierarchy, and the report must never claim a confinement the
// seat did not get. name scopes the transient unit per session. userScope
// selects the systemd bus (--user for a per-user daemon service).
func applySeatBudgetToCmd(command []string, sessionID string, b seatbudget.Budget, ok bool, placement seatbudget.Placement) []string {
	return applySeatBudgetToCmdForBus(command, sessionID, b, ok, placement, false)
}

// applySeatBudgetToCmdForBus is applySeatBudgetToCmd parametrised by the
// systemd bus. Production resolves userScope from the live bus probe;
// tests pin both spellings on any host.
func applySeatBudgetToCmdForBus(command []string, sessionID string, b seatbudget.Budget, ok bool, placement seatbudget.Placement, userScope bool) []string {
	return applySeatBudgetToCmdForBusGOOS(command, sessionID, b, ok, placement, userScope, runtime.GOOS)
}

// applySeatBudgetToCmdForBusGOOS is applySeatBudgetToCmdForBus parametrised
// by GOOS: the default `auto` mode confines on Linux and degrades elsewhere,
// so the wrap decision differs per OS. Production passes runtime.GOOS;
// tests pin both platforms on any host (the auto-mode wrap is the
// documented default and must stay pinned where it confines).
func applySeatBudgetToCmdForBusGOOS(command []string, sessionID string, b seatbudget.Budget, ok bool, placement seatbudget.Placement, userScope bool, goos string) []string {
	if !ok || len(command) == 0 {
		return command
	}
	if !wantsEnforcementForGOOS(b, goos) {
		return command
	}
	if placement != seatbudget.PlacementSystemd {
		return command
	}
	prefix := seatbudget.SystemdScopeArgsForBus(seatbudget.ScopeName(sessionID), b, userScope)
	return append(prefix, command...)
}

// wantsEnforcementForGOOS reports whether the seat asked for hard OS
// enforcement: explicit enforced mode anywhere, or auto on Linux. Kept
// beside the wrap (not on Budget) because it answers the spawner's
// question — "wrap this command?" — while EffectiveMode answers the
// reporter's ("what did the seat get?"). The GOOS parameter lets tests
// pin the default-mode matrix on any host: `auto` is the documented
// default and resolveSeatBudget stores it verbatim, so the branch that
// confines default-configured Linux seats (wrap) versus degrades them
// (best-effort report) needs its own pin — EffectiveModeFor does not
// answer the spawner's wrap question. Production passes runtime.GOOS.
func wantsEnforcementForGOOS(b seatbudget.Budget, goos string) bool {
	if b.Mode == seatbudget.ModeEnforced {
		return true
	}
	return b.Mode == "auto" && goos == "linux"
}

// sessionSeatBudgetReport builds the per-session handle evidence for one
// spawn: the posture the seat actually runs under with the values. A
// disabled budget reports mode "none". A seat that asked for enforcement
// reports enforced ONLY for the systemd wrap — the one placement that
// confines the seat. The quota (not core pinning) is the binding CPU
// limit: identical AllowedCPUs ranges across seats shared 2 cores of 6
// budgeted, and user services never get the cpuset controller delegated,
// so the scope carries quota+weight and the report says quota. A systemd-less Linux host (cgroupfs placement) runs
// the seat unconfined: the launcher performs no direct cgroupfs placement,
// so the report is mode "none" with the missing backend named, never
// enforced. With no backend at all the report is likewise none. Best-effort
// covers explicit best-effort plus auto off Linux.
func sessionSeatBudgetReport(b seatbudget.Budget, ok bool, placement seatbudget.Placement) *SessionSeatBudget {
	return sessionSeatBudgetReportForGOOS(b, ok, placement, runtime.GOOS)
}

// sessionSeatBudgetReportForGOOS is sessionSeatBudgetReport parametrised by
// GOOS: the default `auto` mode reports enforced on Linux (where it wraps)
// and best-effort elsewhere. Production passes runtime.GOOS; tests pin the
// wrap/report agreement on any host — a wrap without an enforced report
// (or vice versa) is the false-confinement shape this package exists to
// prevent.
func sessionSeatBudgetReportForGOOS(b seatbudget.Budget, ok bool, placement seatbudget.Placement, goos string) *SessionSeatBudget {
	if !ok {
		return &SessionSeatBudget{Mode: string(seatbudget.ModeNone)}
	}
	if wantsEnforcementForGOOS(b, goos) {
		detail := "cpus " + seatbudget.CPUSet(b.CPUs) + ", quota " + seatbudget.CPUQuotaPercent(b.CPUs)
		if mem := seatbudget.MemoryBytes(b.MemoryMB); mem != "" {
			detail += ", memory " + mem + " bytes"
		}
		if placement == seatbudget.PlacementSystemd {
			return &SessionSeatBudget{
				Mode:     string(seatbudget.ModeEnforced),
				CPUs:     b.CPUs,
				MemoryMB: b.MemoryMB,
				Detail:   detail + " via transient systemd scope",
			}
		}
		return &SessionSeatBudget{Mode: string(seatbudget.ModeNone), Detail: seatBudgetNoBackendDetailFor(placement)}
	}
	return &SessionSeatBudget{
		Mode:     string(seatbudget.ModeBestEffort),
		CPUs:     b.CPUs,
		MemoryMB: b.MemoryMB,
		Detail:   seatbudget.DescribeBestEffort(b.CPUs, len(seatbudget.WorkerCapKeys())),
	}
}

// shimSeatBudget resolves the spawner's budget for the shim launch path:
// the same effective budget the direct path wraps at spawn. ok=false means
// budgeting is off — the shim launches exactly as before. d may be nil in
// tests that construct a spawner without a daemon.
func (d *Daemon) shimSeatBudget() (seatbudget.Budget, bool) {
	if d == nil || d.spawner == nil {
		return seatbudget.Budget{}, false
	}
	return d.spawner.seatBudgetForSpawn()
}

// seatBudgetShimCommand wraps the shim worker command in the same Linux
// cgroup placement the direct path applies at spawn: an enforced seat on a
// systemd host launches inside the transient scope (on the user bus with
// --user for a per-user daemon service); anywhere else the command runs
// unwrapped and the handle report says what the seat got. The env caps
// (applied by spawnThroughShim before this daemon's launch entry) ride
// along on both paths so tools size their fan-out to the share.
func seatBudgetShimCommand(command []string, sessionID string, b seatbudget.Budget, ok bool, placement seatbudget.Placement, userScope bool) []string {
	return applySeatBudgetToCmdForBus(command, sessionID, b, ok, placement, userScope)
}

// wrapSeatCommandForTest is the production entry point's seam: tests drive
// AcceptWork (the real spawn path) and assert on the wrapped command via
// WorkerCommand capture, not this helper directly.
//
// seatBudgetLaunchPlacement and seatBudgetLaunchUserScope resolve the Linux
// enforcement placement for a fresh spawn. The direct spawn and the shim
// launch read these instead of probing inline so tests pin the systemd
// spelling on any host; they default to the live probes and a test that
// overrides them must restore the defaults (no parallel use while stubbed).
var seatBudgetLaunchPlacement = seatbudget.HostPlacement

var seatBudgetLaunchUserScope = seatbudget.UserScopeForBus
