package daemon

import (
	"fmt"
	"runtime"
	"strconv"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/daemon/seatbudget"
)

// validateSeatBudget enforces the authored per-seat budget ranges. Zero
// value is valid and means budgeting is off.
func validateSeatBudget(b SeatBudgetConfig) error {
	return seatbudget.Budget{
		CPUs:     b.CPUs,
		MemoryMB: b.MemoryMB,
		IOWeight: b.IOWeight,
		Mode:     seatbudget.Mode(b.Mode),
	}.Validate()
}

// resolveSeatBudget returns the per-seat budget the daemon applies: the
// authored block with un-authored fields derived from host capacity. A
// zero authored block means budgeting is OFF — the daemon must not invent
// limits the operator never asked for. Resolve derives host shares only
// when the operator authored something (a mode, a CPU count, a memory cap
// or an IO weight).
//
// maxSeats is the daemon's max concurrent seat count; hostCPUs and
// hostMemoryMB are the detected host totals (0 = detect here).
func resolveSeatBudget(authored SeatBudgetConfig, maxSeats, hostCPUs, hostMemoryMB int) seatbudget.Budget {
	if authored == (SeatBudgetConfig{}) {
		return seatbudget.Budget{}
	}
	if hostCPUs <= 0 {
		hostCPUs = runtime.NumCPU()
	}
	if hostMemoryMB <= 0 {
		hostMemoryMB = probeMemTotalMB()
	}
	if maxSeats <= 0 {
		maxSeats = 1
	}
	return seatbudget.Resolve(seatbudget.Budget{
		CPUs:     authored.CPUs,
		MemoryMB: authored.MemoryMB,
		IOWeight: authored.IOWeight,
		Mode:     seatbudget.Mode(authored.Mode),
	}, maxSeats, hostCPUs, hostMemoryMB)
}

// seatBudgetReport builds the additive status/report budget block for one
// resolved budget: the posture the seat actually runs under plus the
// values. Disabled budgets report mode "none" with no values. A Linux
// seat that asked for enforcement but has no systemd backend reports
// mode "none" with the reason — never enforced for a seat that runs
// unwrapped.
func seatBudgetReport(b seatbudget.Budget) *afclient.SeatBudgetStatus {
	return seatBudgetReportFor(b, seatbudget.HostPlacement(), runtime.GOOS)
}

// seatBudgetReportFor is seatBudgetReport parametrised by placement and
// GOOS so tests pin the systemd-less Linux downgrade on any host. The mode
// resolves for the same GOOS (an enforced seat off Linux is already
// best-effort before the placement question arises).
func seatBudgetReportFor(b seatbudget.Budget, placement seatbudget.Placement, goos string) *afclient.SeatBudgetStatus {
	if b.Disabled() {
		return &afclient.SeatBudgetStatus{Mode: string(seatbudget.ModeNone)}
	}
	mode := seatbudget.EffectiveModeFor(b.Mode, goos)
	if mode == seatbudget.ModeEnforced && placement != seatbudget.PlacementSystemd {
		// Downgrade ONLY on Linux, where EffectiveMode promised enforced.
		// Off Linux the mode already resolved to best-effort (macOS
		// cooperative caps) and the report below renders the knob line;
		// collapsing that to none would erase a real best-effort budget.
		if goos == "linux" {
			return &afclient.SeatBudgetStatus{Mode: string(seatbudget.ModeNone), Detail: seatBudgetNoBackendDetailFor(placement)}
		}
	}
	return &afclient.SeatBudgetStatus{
		Mode:     string(mode),
		CPUs:     b.CPUs,
		MemoryMB: b.MemoryMB,
		Detail:   seatBudgetDetailForPlacement(b, goos, placement),
	}
}

// seatBudgetNoBackendDetail names the missing backend for a Linux seat
// that asked for enforcement on a host with no systemd: the report must
// name the reason rather than claim a confinement the seat did not get.
// The placement is the caller's live probe (never re-probed here) so the
// status path stays hermetic and host-independent under test.
func seatBudgetNoBackendDetailFor(placement seatbudget.Placement) string {
	if placement == seatbudget.PlacementCgroupFS {
		return "no usable systemd on this host (needs systemd " + strconv.Itoa(seatbudget.MinScopeSystemd) + " or newer): cgroupfs placement is not applied, seat runs unconfined"
	}
	return "no enforcement backend on this host"
}

// seatBudgetNoBackendDetail names the missing backend at the live
// placement.
//
//nolint:unused // kept as the named entry point beside seatBudgetNoBackendDetailFor
func seatBudgetNoBackendDetail() string {
	return seatBudgetNoBackendDetailFor(seatbudget.HostPlacement())
}

// seatBudgetDetailFor is the host-GOOS entry point. The published
// status path goes through seatBudgetReportFor; direct callers must pass
// the placement downgrade (or use seatBudgetReportFor) rather than
// rendering an enforced line for a seat that runs unconfined.
//
//nolint:unused // kept as the named entry point beside seatBudgetDetailForPlacement
func seatBudgetDetailFor(b seatbudget.Budget, goos string) string {
	return seatBudgetDetailForPlacement(b, goos, seatbudget.HostPlacement())
}

// seatBudgetDetailForPlacement is seatBudgetDetailFor with the placement
// injected: the CI runner has systemd while the systemd-less container
// does not, so reading the live host here makes the test host-dependent.
// Tests pin both placements on any host through this form.
func seatBudgetDetailForPlacement(b seatbudget.Budget, goos string, placement seatbudget.Placement) string {
	switch seatbudget.EffectiveModeFor(b.Mode, goos) {
	case seatbudget.ModeEnforced:
		detail := fmt.Sprintf("cpus %s, quota %s", seatbudget.CPUSet(b.CPUs), seatbudget.CPUQuotaPercent(b.CPUs))
		if mem := seatbudget.MemoryBytes(b.MemoryMB); mem != "" {
			detail += fmt.Sprintf(", memory %s bytes", mem)
		}
		switch placement {
		case seatbudget.PlacementSystemd:
			return detail + " via transient systemd scope"
		default:
			return detail + " (no enforcement backend on this host)"
		}
	case seatbudget.ModeBestEffort:
		return seatbudget.DescribeBestEffort(b.CPUs, len(seatbudget.WorkerCapKeys()))
	default:
		return ""
	}
}

// seatBudgetHandleReport builds the handle evidence for a session whose
// launch-time spec is gone: an adopted-at-startup shim, a quarantined
// lineage, or a held recovery projection. The posture is the daemon's
// CURRENT resolved share (the share a fresh launch would get), with the
// same systemd-less downgrade the launch paths apply — a disabled budget
// reports mode none with no values. Recovery surfaces never invent a
// per-session share; they re-report the host's current share.
func (d *Daemon) seatBudgetHandleReport() *SessionSeatBudget {
	var budget seatbudget.Budget
	var ok bool
	if d != nil {
		if spawner := d.Spawner(); spawner != nil {
			budget, ok = spawner.seatBudgetForSpawn()
		} else {
			budget = d.daemonSeatBudget()
			ok = !budget.Disabled()
		}
	}
	return sessionSeatBudgetReport(budget, ok, seatbudget.HostPlacement())
}

// daemonSeatBudget resolves the daemon's own configured budget against its
// live capacity. Nil config reads as budgeting off.
func (d *Daemon) daemonSeatBudget() seatbudget.Budget {
	cfg := d.Config()
	if cfg == nil {
		return seatbudget.Budget{}
	}
	return resolveSeatBudget(cfg.Capacity.SeatBudget, cfg.Capacity.MaxConcurrentSessions, 0, 0)
}
