package daemon

import (
	"fmt"
	"runtime"

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
	if b.Disabled() {
		return &afclient.SeatBudgetStatus{Mode: string(seatbudget.ModeNone)}
	}
	mode := b.EffectiveMode()
	if mode == seatbudget.ModeEnforced && seatbudget.HostPlacement() != seatbudget.PlacementSystemd {
		// Downgrade ONLY on Linux, where EffectiveMode promised enforced.
		// Off Linux the mode already resolved to best-effort (macOS
		// cooperative caps) and the report below renders the knob line;
		// collapsing that to none would erase a real best-effort budget.
		if runtime.GOOS == "linux" {
			return &afclient.SeatBudgetStatus{Mode: string(seatbudget.ModeNone), Detail: seatBudgetNoBackendDetail()}
		}
	}
	return &afclient.SeatBudgetStatus{
		Mode:     string(mode),
		CPUs:     b.CPUs,
		MemoryMB: b.MemoryMB,
		Detail:   seatBudgetDetail(b),
	}
}

// seatBudgetNoBackendDetail names the missing backend for a Linux seat
// that asked for enforcement on a host with no systemd: the report must
// name the reason rather than claim a confinement the seat did not get.
func seatBudgetNoBackendDetail() string {
	if seatbudget.HostPlacement() == seatbudget.PlacementCgroupFS {
		return "no systemd on this host: cgroupfs placement is not applied, seat runs unconfined"
	}
	return "no enforcement backend on this host"
}

// seatBudgetDetail renders the short human line behind a budget report:
// which placement enforces it on Linux, which knobs carry it on macOS.
// The enforced branch is reached only when the host placement can confine
// the seat (see seatBudgetReport); a systemd-less host never renders an
// enforced line.
func seatBudgetDetail(b seatbudget.Budget) string {
	switch b.EffectiveMode() {
	case seatbudget.ModeEnforced:
		detail := fmt.Sprintf("cpus %s, quota %s", seatbudget.CPUSet(b.CPUs), seatbudget.CPUQuotaPercent(b.CPUs))
		if mem := seatbudget.MemoryBytes(b.MemoryMB); mem != "" {
			detail += fmt.Sprintf(", memory %s bytes", mem)
		}
		switch seatbudget.HostPlacement() {
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

// daemonSeatBudget resolves the daemon's own configured budget against its
// live capacity. Nil config reads as budgeting off.
func (d *Daemon) daemonSeatBudget() seatbudget.Budget {
	cfg := d.Config()
	if cfg == nil {
		return seatbudget.Budget{}
	}
	return resolveSeatBudget(cfg.Capacity.SeatBudget, cfg.Capacity.MaxConcurrentSessions, 0, 0)
}
