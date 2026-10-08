package daemon

import (
	"runtime"

	"github.com/RenseiAI/donmai/daemon/seatbudget"
	"github.com/RenseiAI/donmai/sessionshim"
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
//
// NOTE: this is the DIRECT-child lane only. Shim launches never come here:
// they wrap through seatBudgetShimCommandForLaunch, which scopes EVERY shim
// launch (budget or not) in a digest-named unit and refuses to launch
// without a backend. The direct lane keeps its truncation-named, budget-only
// wrap so a non-shim seat spawns exactly as before.
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
// budgeting is off — the shim still launches inside its own scope (see
// seatBudgetShimCommand), just without limit properties. d may be nil in
// tests that construct a spawner without a daemon.
func (d *Daemon) shimSeatBudget() (seatbudget.Budget, bool) {
	if d == nil || d.spawner == nil {
		return seatbudget.Budget{}, false
	}
	return d.spawner.seatBudgetForSpawn()
}

// seatBudgetShimScopeName derives the transient scope unit name for one shim
// launch: a fixed-length digest of the lifecycle identity plus the launch's
// process epoch. Two launches of one session (original and relaunch after a
// restart) are different scopes; two sessions sharing a session-id prefix
// never alias. The epoch suffix keeps the incarnation greppable after the
// digest.
func seatBudgetShimScopeName(orgID, sessionID string, processEpoch uint64) string {
	return seatbudget.ScopeNameForIncarnation(orgID, sessionID, processEpoch)
}

// seatBudgetShimCommandForLaunch is seatBudgetShimCommand keyed by the full
// launch identity (organisation, session, process epoch) and GOOS so tests
// pin the digest naming and the platform gate on any host. Production passes
// runtime.GOOS.
func seatBudgetShimCommandForLaunch(command []string, orgID, sessionID string, processEpoch uint64, b seatbudget.Budget, ok bool, placement seatbudget.Placement, userScope bool, goos string) ([]string, bool) {
	if len(command) == 0 {
		return command, false
	}
	if goos != "linux" {
		return command, true
	}
	if placement != seatbudget.PlacementSystemd {
		return nil, false
	}
	if !ok {
		b = seatbudget.Budget{}
	}
	prefix := seatbudget.SystemdScopeArgsForBus(seatBudgetShimScopeName(orgID, sessionID, processEpoch), b, userScope)
	return append(prefix, command...), true
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

// seatBudgetLaunchedReport builds the handle evidence for a shim this daemon
// just launched: the seat's OWN facts (the scope unit and limits the launch
// stamped into the contract, carried here explicitly rather than re-read
// from this daemon's current configuration), reported through the live
// cgroup read-back with record fallback. scope carries the stamped scope
// unit; cpus/memoryMB/ioWeight the stamped limits. After the operator
// reconfigures the budget, the live seat still reports what it runs under —
// never what a fresh seat would get.
func seatBudgetLaunchedReport(scope string, cpus, memoryMB, ioWeight int) *SessionSeatBudget {
	live, liveOK := seatbudget.ReadSeatLimits(scope, seatBudgetLaunchUserScope())
	rep := seatbudget.ReportSeatLimits(scope, live, liveOK && scope != "", cpus, memoryMB, ioWeight)
	return &SessionSeatBudget{Mode: string(rep.Mode), CPUs: rep.CPUs, MemoryMB: rep.MemoryMB, Detail: rep.Detail}
}

// seatBudgetAdoptedReport builds the handle evidence for a shim this daemon
// adopted (at startup or after a restart): the limits read back from the
// seat's own cgroup through its scope, falling back to the launch record the
// shim republished into its discovery record. It never consults this
// daemon's current configuration: the operator may have reconfigured the
// host since the launch, and the handle must say what the seat runs under,
// not what a fresh seat would get. A nil controller (or a record with no
// seat facts at all) reports mode none with the reason named.
func (d *Daemon) seatBudgetAdoptedReport(ctrl *sessionshim.Controller) *SessionSeatBudget {
	if ctrl == nil {
		return &SessionSeatBudget{Mode: string(seatbudget.ModeNone), Detail: "seat scope unknown: adopted without a live controller"}
	}
	scope := ctrl.SeatScope()
	recordCPUs, recordMemoryMB, recordIOWeight := ctrl.SeatLimits()
	live, liveOK := seatbudget.ReadSeatLimits(scope, seatBudgetLaunchUserScope())
	rep := seatbudget.ReportSeatLimits(scope, live, liveOK && scope != "", recordCPUs, recordMemoryMB, recordIOWeight)
	return &SessionSeatBudget{Mode: string(rep.Mode), CPUs: rep.CPUs, MemoryMB: rep.MemoryMB, Detail: rep.Detail}
}

// seatBudgetQuarantinedReport builds the handle evidence for a quarantined
// lineage: no controller exists to read back from, but the discovery record
// still names the scope and the launched limits. Those stand in directly —
// never this daemon's current configuration.
func (d *Daemon) seatBudgetQuarantinedReport(q sessionshim.QuarantinedSession) *SessionSeatBudget {
	if d == nil {
		return &SessionSeatBudget{Mode: string(seatbudget.ModeNone)}
	}
	// The registry cannot be opened here: sessionShimHandles already holds
	// the adopted-set read lock, and sessionShimRegistry takes the write
	// lock. A quarantined lineage therefore reports its own launched
	// limits, which the quarantine record already carries from adoption —
	// never this daemon's current configuration.
	_ = d
	// The entry carries the launch's own seat facts (scope + limits), read
	// from the discovery record at quarantine time — never this daemon's
	// current configuration. A live cgroup read still takes precedence
	// where it answers; otherwise the record's numbers stand in.
	live, liveOK := seatbudget.ReadSeatLimits(q.SeatScope, seatBudgetLaunchUserScope())
	rep := seatbudget.ReportSeatLimits(q.SeatScope, live, liveOK && q.SeatScope != "", q.SeatCPUs, q.SeatMemoryMB, q.SeatIOWeight)
	return &SessionSeatBudget{Mode: string(rep.Mode), CPUs: rep.CPUs, MemoryMB: rep.MemoryMB, Detail: rep.Detail}
}

// shimLaunchSeatFacts reads the seat launch facts back from the discovery
// record: the scope unit and the limits the launch asked for. It is the
// secret-free launch record the slice requires — no bearer, no credential,
// only the scope name and the numbers. ok=false means no record exists
// (or it carries no facts), and the caller reports accordingly.
func (d *Daemon) shimLaunchSeatFacts(id sessionshim.Identity) (scope string, cpus, memoryMB, ioWeight int) {
	if d == nil {
		return "", 0, 0, 0
	}
	registry, err := d.sessionShimRegistry()
	if err != nil {
		return "", 0, 0, 0
	}
	rec, err := registry.Get(id)
	if err != nil {
		return "", 0, 0, 0
	}
	return rec.SeatScope, rec.SeatCPUs, rec.SeatMemoryMB, rec.SeatIOWeight
}
