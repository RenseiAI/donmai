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
// means budgeting is off — the seat spawns exactly as before.
func (s *WorkerSpawner) seatBudgetForSpawn() (seatbudget.Budget, bool) {
	b := s.opts.SeatBudget.toSeatBudget()
	if b.Disabled() {
		return seatbudget.Budget{}, false
	}
	if b.CPUs < 1 {
		b.CPUs = 1
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
// says none — the spawn must not fail for want of a hierarchy. name scopes
// the transient unit per session.
func applySeatBudgetToCmd(command []string, sessionID string, b seatbudget.Budget, ok bool, placement seatbudget.Placement) []string {
	if !ok || len(command) == 0 {
		return command
	}
	if !wantsEnforcement(b) {
		return command
	}
	if placement != seatbudget.PlacementSystemd {
		return command
	}
	prefix := seatbudget.SystemdScopeArgs(seatbudget.ScopeName(sessionID), b)
	return append(prefix, command...)
}

// wantsEnforcement reports whether the seat asked for hard OS enforcement:
// explicit enforced mode anywhere, or auto on Linux. Kept beside the wrap
// (not on Budget) because it answers the spawner's question — "wrap this
// command?" — while EffectiveMode answers the reporter's ("what did the
// seat get?").
func wantsEnforcement(b seatbudget.Budget) bool {
	if b.Mode == seatbudget.ModeEnforced {
		return true
	}
	return b.Mode == "auto" && runtime.GOOS == "linux"
}

// sessionSeatBudgetReport builds the per-session handle evidence for one
// spawn: the posture the seat actually runs under with the values. A
// disabled budget reports mode "none". A seat that asked for enforcement
// reports enforced only when the placement can confine it: systemd wrap, or
// cgroupfs (confinement without the scope wrapper). With no backend the
// seat runs unconfined and the report says none — never a posture the seat
// did not get. Best-effort covers explicit best-effort plus auto off Linux.
func sessionSeatBudgetReport(b seatbudget.Budget, ok bool, placement seatbudget.Placement) *SessionSeatBudget {
	if !ok {
		return &SessionSeatBudget{Mode: string(seatbudget.ModeNone)}
	}
	if wantsEnforcement(b) {
		rep := &SessionSeatBudget{
			Mode:     string(seatbudget.ModeEnforced),
			CPUs:     b.CPUs,
			MemoryMB: b.MemoryMB,
		}
		detail := "cpus " + seatbudget.CPUSet(b.CPUs) + ", quota " + seatbudget.CPUQuotaPercent(b.CPUs)
		if mem := seatbudget.MemoryBytes(b.MemoryMB); mem != "" {
			detail += ", memory " + mem + " bytes"
		}
		switch placement {
		case seatbudget.PlacementSystemd:
			rep.Detail = detail + " via transient systemd scope"
			return rep
		case seatbudget.PlacementCgroupFS:
			rep.Detail = detail + " via cgroupfs (scope wrap unavailable)"
			return rep
		default:
			return &SessionSeatBudget{Mode: string(seatbudget.ModeNone), Detail: "no enforcement backend on this host"}
		}
	}
	return &SessionSeatBudget{
		Mode:     string(seatbudget.ModeBestEffort),
		CPUs:     b.CPUs,
		MemoryMB: b.MemoryMB,
		Detail:   seatbudget.DescribeBestEffort(b.CPUs, len(seatbudget.WorkerCapKeys())),
	}
}

// wrapSeatCommandForTest is the production entry point's seam: tests drive
// AcceptWork (the real spawn path) and assert on the wrapped command via
// WorkerCommand capture, not this helper directly.
