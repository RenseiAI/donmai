package seatbudget

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Mode is the enforcement posture a seat budget reports. The zero value is
// unset, which resolves to ModeNone: a host that never configured a budget
// reports no budget rather than a posture it did not earn.
type Mode string

const (
	// ModeEnforced means the seat's process tree is confined by the OS:
	// cgroups v2 limits on Linux. A CPU-burning child cannot exceed the
	// quota.
	ModeEnforced Mode = "enforced"
	// ModeBestEffort means the seat carries cooperative caps the OS does
	// not enforce: worker-cap environment on macOS, composed with the
	// installed service process-priority mode. A tool that ignores its
	// knob can still saturate the host.
	ModeBestEffort Mode = "best-effort"
	// ModeNone means no budget applies to the seat: budgeting is disabled
	// on this host, or the seat predates the budget.
	ModeNone Mode = "none"
)

// ParseMode normalises operator input. Empty reads as ModeNone: budgeting
// stays off unless the operator asks for it. "auto" resolves per OS at
// budget-build time (enforced where the OS can enforce, best-effort
// elsewhere); it is stored verbatim and never silently rewritten.
func ParseMode(raw string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(raw))) {
	case "", ModeNone:
		return ModeNone, nil
	case ModeEnforced, ModeBestEffort:
		return Mode(strings.ToLower(strings.TrimSpace(raw))), nil
	case "auto":
		return "auto", nil
	default:
		return "", fmt.Errorf("unsupported seat budget mode %q (want auto, enforced, best-effort or none)", raw)
	}
}

// Budget is one seat's resolved resource budget: the share of the host one
// session's process tree may use.
//
// CPUs is a whole-core count: the number of logical CPUs the seat may use.
// On Linux it renders as both the CPU set size and the CPU quota; on macOS
// it renders as the worker-cap environment (GOMAXPROCS and friends).
// MemoryMB is the seat's memory ceiling in mebibytes; zero means no memory
// limit. IOWeight is the cgroup v2 IO weight (1-10000); zero means the
// backend default (no explicit weight).
type Budget struct {
	// CPUs is the whole-core seat share. Always >= 1 on a resolved budget.
	CPUs int `json:"cpus"`
	// MemoryMB is the seat memory ceiling in mebibytes. Zero means no cap.
	MemoryMB int `json:"memoryMb,omitempty"`
	// IOWeight is the cgroup v2 IO weight. Zero means backend default.
	IOWeight int `json:"ioWeight,omitempty"`
	// Mode is the configured mode ("auto", "enforced", "best-effort",
	// "none"). Stored verbatim; EffectiveMode resolves it per OS.
	Mode Mode `json:"mode,omitempty"`
}

// Report is the per-seat budget evidence host status and the session result
// carry. Mode names the posture the seat actually ran under; Values carries
// the numbers; Detail is a short human line (for example which cgroup path
// or which env knobs).
type Report struct {
	Mode     Mode   `json:"mode"`
	CPUs     int    `json:"cpus,omitempty"`
	MemoryMB int    `json:"memoryMb,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// EffectiveMode resolves the configured mode for the running OS. "auto"
// means enforced where the OS can enforce (Linux cgroups v2) and
// best-effort elsewhere (macOS cooperative caps). An explicit mode is
// honoured verbatim except that "enforced" on an OS with no enforcement
// backend degrades to best-effort rather than failing the spawn: the seat
// still runs, and the report says what it got.
func (b Budget) EffectiveMode() Mode {
	return EffectiveModeFor(b.Mode, runtime.GOOS)
}

// EffectiveModeFor is EffectiveMode parametrised by GOOS so tests can pin
// both platforms on one host.
func EffectiveModeFor(mode Mode, goos string) Mode {
	switch mode {
	case ModeEnforced:
		if goos == "linux" {
			return ModeEnforced
		}
		return ModeBestEffort
	case ModeBestEffort:
		return ModeBestEffort
	case "auto":
		if goos == "linux" {
			return ModeEnforced
		}
		return ModeBestEffort
	default:
		return ModeNone
	}
}

// Validate reports whether the authored budget is well-formed. Zero Budget
// is valid and means budgeting is off.
func (b Budget) Validate() error {
	if b.CPUs < 0 {
		return fmt.Errorf("seat budget cpus must be >= 0 (got %d)", b.CPUs)
	}
	if b.MemoryMB < 0 {
		return fmt.Errorf("seat budget memory must be >= 0 (got %d)", b.MemoryMB)
	}
	if b.IOWeight < 0 || b.IOWeight > 10000 {
		return fmt.Errorf("seat budget ioWeight must be 0-10000 (got %d)", b.IOWeight)
	}
	if _, err := ParseMode(string(b.Mode)); err != nil {
		return err
	}
	return nil
}

// Disabled reports whether the budget carries no constraint: mode none, or
// mode unset with no numeric limits.
func (b Budget) Disabled() bool {
	if b.Mode == ModeNone {
		return true
	}
	return b.Mode == "" && b.CPUs <= 0 && b.MemoryMB <= 0
}

// Resolve fills an authored (possibly partial) budget against host capacity
// and returns the per-seat budget the daemon applies. maxSeats is the
// daemon's max concurrent seat count; hostCPUs and hostMemoryMB are the
// detected host totals. Any authored field wins over the derived default;
// un-authored fields divide the host by the seat count.
//
// The single-seat rule: when maxSeats <= 1 the derived budget is the whole
// host (minus nothing — the system reservation already lives in the
// capacity block), so the defaults never throttle a lone seat.
func Resolve(authored Budget, maxSeats, hostCPUs, hostMemoryMB int) Budget {
	out := authored
	if out.CPUs <= 0 {
		out.CPUs = defaultCPUs(hostCPUs, maxSeats)
	}
	if out.MemoryMB <= 0 && hostMemoryMB > 0 && maxSeats > 1 {
		out.MemoryMB = hostMemoryMB / maxSeats
	}
	if out.Mode == "" {
		out.Mode = "auto"
	}
	return out
}

// defaultCPUs divides host cores by the seat count with a floor of one.
// A single seat keeps every core.
func defaultCPUs(hostCPUs, maxSeats int) int {
	if hostCPUs <= 0 {
		hostCPUs = runtime.NumCPU()
	}
	if maxSeats <= 1 {
		return hostCPUs
	}
	n := hostCPUs / maxSeats
	if n < 1 {
		n = 1
	}
	return n
}

// WorkerCapEnv renders the cooperative worker caps for a seat: the
// environment entries that keep core-fanning tools to cpus workers. Every
// entry is a knob the named tool actually reads (see the per-key doc
// below); unknown or tool-specific spellings are deliberately absent — an
// env entry a tool ignores is worse than none, because the report would
// claim a cap that does nothing.
//
// Keys are stable and sorted: GOMAXPROCS caps the Go runtime (go test,
// go build); MAKEFLAGS -j caps make; CMAKE_BUILD_PARALLEL_LEVEL caps
// cmake --build; NINJAFLAGS caps ninja; CARGO_BUILD_JOBS caps cargo;
// the *_NUM_WORKERS / *_MAX_WORKERS entries cap the JS runners that read
// them (vitest workers, Next/Turbopack build workers).
func WorkerCapEnv(cpus int) map[string]string {
	if cpus < 1 {
		cpus = 1
	}
	n := strconv.Itoa(cpus)
	return map[string]string{
		"GOMAXPROCS":                 n,
		"MAKEFLAGS":                  "-j" + n,
		"CMAKE_BUILD_PARALLEL_LEVEL": n,
		"NINJAFLAGS":                 "-j" + n,
		"CARGO_BUILD_JOBS":           n,
		"VITEST_MAX_WORKERS":         n,
		"NEXT_BUILD_WORKERS":         n,
	}
}

// WorkerCapKeys lists the variable names WorkerCapEnv sets, sorted. Tests
// and the macOS composer share this list so a new knob cannot be added to
// one without the other.
func WorkerCapKeys() []string {
	env := WorkerCapEnv(1)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
