package seatbudget

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Read-back: the limits a live seat scope actually carries.
//
// A daemon that adopts a shim-owned seat reports the limits it reads back
// from the seat's own cgroup, falling back to the secret-free launch record
// the launching daemon wrote, and never its own current configuration. The
// read goes through `systemctl show` against the seat's scope unit. Two facts
// about that output shape the parser, both observed on a real systemd 255:
//
//   - `systemctl show` exits 0 for a unit that does not exist and prints the
//     requested properties at their defaults (`infinity`, `[not set]`). Only
//     LoadState tells a collected or never-created scope (`not-found`) apart
//     from a live scope that caps nothing, so the read requires
//     LoadState=loaded.
//   - USec properties render as human timespans, not integers: a 200% quota
//     is `CPUQuotaPerSecUSec=2s`, 150% is `1.500000s`, 50% is `500ms`, 6400%
//     is `1min 4s`. The parser reads systemd's timespan form.
//
// Anything the parser cannot read is ok=false — the caller falls back to the
// launch record rather than reporting numbers it did not observe.

// SeatLimits are the limits a seat's cgroup actually carries. CPUs is the
// whole-core share derived from the CPU quota (rounded up, so a fractional
// quota never reads as no quota); MemoryMB is the memory ceiling in
// mebibytes; IOWeight is the cgroup v2 IO weight. Zero means that controller
// carries no limit.
type SeatLimits struct {
	CPUs     int
	MemoryMB int
	IOWeight int
}

// HasLimits reports whether the scope caps CPU or memory — the two limits a
// seat budget enforces. An IO weight alone is a share, not a cap.
func (l SeatLimits) HasLimits() bool { return l.CPUs > 0 || l.MemoryMB > 0 }

// seatScopeNamePattern is the exact shape ScopeNameForIncarnation renders: a
// 32-hex-digit digest and a decimal epoch.
var seatScopeNamePattern = regexp.MustCompile(`^donmai-seat-[0-9a-f]{32}-[0-9]{1,20}\.scope$`)

// ValidScopeName reports whether scope is a seat scope unit this package
// names. The read-back refuses anything else before it reaches systemctl, so
// a scope string from a corrupted or hostile launch record can never query
// an unrelated unit and have that unit's limits attributed to the seat.
func ValidScopeName(scope string) bool { return seatScopeNamePattern.MatchString(scope) }

// ReadSeatLimits reads the limits a live transient scope carries. userScope
// selects the bus, the same decision the launch used: a scope created with
// --user is only visible with --user. ok=false means the scope could not be
// observed — off Linux, a name this package did not render, an unreachable
// bus, a scope that is not loaded (collected, or never created), or output
// the parser cannot read.
func ReadSeatLimits(scope string, userScope bool) (SeatLimits, bool) {
	return readSeatLimitsWithRunner(scope, userScope, runSystemctlShow, runtime.GOOS)
}

// seatShowProperties is the fixed property list the read requests. LoadState
// is what proves the scope exists; the other three are the limits.
const seatShowProperties = "LoadState,CPUQuotaPerSecUSec,MemoryMax,IOWeight"

// runSystemctlShow runs `systemctl [--user] show <scope>` for the seat
// properties. The binary and property names are fixed; only the scope unit
// (validated against the digest shape) and the bus flag vary.
func runSystemctlShow(scope string, userScope bool) ([]byte, error) {
	args := []string{"show", scope, "--property=" + seatShowProperties}
	if userScope {
		args = append([]string{"--user"}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "systemctl", args...).Output() //nolint:gosec // G204: fixed binary and property names; the unit name is validated against the digest shape before it gets here
}

// readSeatLimitsWithRunner is ReadSeatLimits with the systemctl call and the
// platform injected so tests pin the gate and the parse on any host.
func readSeatLimitsWithRunner(scope string, userScope bool, run func(scope string, userScope bool) ([]byte, error), goos string) (SeatLimits, bool) {
	if goos != "linux" {
		return SeatLimits{}, false
	}
	if !ValidScopeName(scope) {
		return SeatLimits{}, false
	}
	out, err := run(scope, userScope)
	if err != nil {
		return SeatLimits{}, false
	}
	return parseSeatShow(out)
}

// parseSeatShow parses `systemctl show` output for the seat properties. The
// output order is not the requested order, so it parses by key. Every
// property must be present: a partial answer is not an observation.
func parseSeatShow(out []byte) (SeatLimits, bool) {
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if props["LoadState"] != "loaded" {
		return SeatLimits{}, false
	}
	quota, okQuota := props["CPUQuotaPerSecUSec"]
	memMax, okMem := props["MemoryMax"]
	ioWeight, okIO := props["IOWeight"]
	if !okQuota || !okMem || !okIO {
		return SeatLimits{}, false
	}
	var limits SeatLimits
	if quota != "infinity" {
		usec, err := parseSystemdTimespan(quota)
		if err != nil {
			return SeatLimits{}, false
		}
		// The quota is CPU time per one second of wall time: one core is
		// one second. Round up so a fractional share never reads as zero.
		limits.CPUs = int((usec + 999_999) / 1_000_000) //nolint:gosec // G115: bounded by parseSystemdTimespan's overflow check, then divided by 1e6
	}
	if memMax != "infinity" {
		bytes, err := strconv.ParseUint(memMax, 10, 64)
		if err != nil {
			return SeatLimits{}, false
		}
		limits.MemoryMB = int(bytes / (1024 * 1024)) //nolint:gosec // G115: a uint64 byte count divided by 2^20 fits an int on every 64-bit host the scope backend runs on
	}
	if ioWeight != "[not set]" {
		w, err := strconv.Atoi(ioWeight)
		if err != nil || w < 0 {
			return SeatLimits{}, false
		}
		limits.IOWeight = w
	}
	return limits, true
}

// systemdTimespanUnits are the suffixes systemd's format_timespan renders for
// spans a CPU quota can reach. Larger units (days and up) are refused: a
// quota that large is not a seat's, and refusing falls back to the record.
var systemdTimespanUnits = map[string]uint64{
	"us":  1,
	"μs":  1, // U+03BC, which newer systemd renders
	"µs":  1, // U+00B5
	"ms":  1_000,
	"s":   1_000_000,
	"min": 60 * 1_000_000,
	"h":   60 * 60 * 1_000_000,
}

// parseSystemdTimespan parses the span `systemctl show` renders for a USec
// property: space-separated <number><unit> terms ("1min 4s"), where a term
// may carry a decimal fraction ("1.500000s"), or a bare "0". It returns
// microseconds.
func parseSystemdTimespan(s string) (uint64, error) {
	if s == "0" {
		return 0, nil
	}
	terms := strings.Fields(s)
	if len(terms) == 0 {
		return 0, errors.New("seatbudget: empty timespan")
	}
	var total uint64
	for _, term := range terms {
		split := strings.IndexFunc(term, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
		if split <= 0 {
			return 0, fmt.Errorf("seatbudget: timespan term %q has no number or no unit", term)
		}
		number, unit := term[:split], term[split:]
		mult, ok := systemdTimespanUnits[unit]
		if !ok {
			return 0, fmt.Errorf("seatbudget: timespan term %q has unit %q", term, unit)
		}
		whole, frac, hasFrac := strings.Cut(number, ".")
		if whole == "" || (hasFrac && (frac == "" || len(frac) > 9 || strings.Contains(frac, "."))) {
			return 0, fmt.Errorf("seatbudget: timespan term %q is not a decimal", term)
		}
		w, err := strconv.ParseUint(whole, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("seatbudget: timespan term %q: %w", term, err)
		}
		if w > (math.MaxUint64-total)/mult {
			return 0, fmt.Errorf("seatbudget: timespan %q overflows", s)
		}
		total += w * mult
		if hasFrac {
			f, err := strconv.ParseUint(frac, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("seatbudget: timespan term %q: %w", term, err)
			}
			scale := uint64(1)
			for range frac {
				scale *= 10
			}
			// f < scale ≤ 1e9 and mult ≤ 3.6e9, so f*mult stays below 2^63.
			part := f * mult / scale
			if part > math.MaxUint64-total {
				return 0, fmt.Errorf("seatbudget: timespan %q overflows", s)
			}
			total += part
		}
	}
	return total, nil
}

// LaunchedSeat is what the launch gave one seat, as its launch record states:
// the posture (enforced, best-effort or none), the limits, and the scope unit
// that owns its cgroup. A zero Mode means no launch record exists.
type LaunchedSeat struct {
	Scope    string
	Mode     Mode
	CPUs     int
	MemoryMB int
	IOWeight int
}

// ReportSeatLimits renders the handle evidence for one shim-owned seat from
// the limits read back from its live scope and its launch record. It never
// consults the daemon's current configuration, which may have changed since
// the launch.
//
// liveOK=true means the scope is loaded and was read. A cgroup that carries
// limits reports them as enforced. One that carries none reports the launch
// record's best-effort caps when the launch gave only those, and none
// otherwise — never enforced numbers nothing enforces.
//
// liveOK=false means the scope could not be observed (collected, unreachable
// bus, off Linux, no scope), and the launch record's own posture stands in.
func ReportSeatLimits(seat LaunchedSeat, live SeatLimits, liveOK bool) Report {
	if liveOK {
		if live.HasLimits() {
			return enforcedSeatReport(seat.Scope, live.CPUs, live.MemoryMB, live.IOWeight)
		}
		if seat.Mode == ModeBestEffort {
			return bestEffortSeatReport(seat.CPUs, seat.MemoryMB)
		}
		return Report{Mode: ModeNone, Detail: "seat scope " + seat.Scope + " carries no limits"}
	}
	switch seat.Mode {
	case ModeEnforced:
		if seat.Scope != "" && (seat.CPUs > 0 || seat.MemoryMB > 0) {
			rep := enforcedSeatReport(seat.Scope, seat.CPUs, seat.MemoryMB, seat.IOWeight)
			rep.Detail += " (launch record; the scope could not be read back)"
			return rep
		}
	case ModeBestEffort:
		return bestEffortSeatReport(seat.CPUs, seat.MemoryMB)
	case "":
		return Report{Mode: ModeNone, Detail: "no seat launch record for this seat"}
	}
	if seat.Scope != "" {
		return Report{Mode: ModeNone, Detail: "seat scope " + seat.Scope + " carries no limits"}
	}
	return Report{Mode: ModeNone}
}

// enforcedSeatReport is the enforced evidence for limits a seat scope
// carries. A CPU share is named only when the scope holds a quota.
func enforcedSeatReport(scope string, cpus, memoryMB, ioWeight int) Report {
	var parts []string
	if cpus > 0 {
		parts = append(parts, "cpus "+CPUSet(cpus)+", quota "+CPUQuotaPercent(cpus))
	}
	if mem := MemoryBytes(memoryMB); mem != "" {
		parts = append(parts, "memory "+mem+" bytes")
	}
	if ioWeight > 0 {
		parts = append(parts, "io weight "+strconv.Itoa(ioWeight))
	}
	return Report{
		Mode:     ModeEnforced,
		CPUs:     cpus,
		MemoryMB: memoryMB,
		Detail:   strings.Join(parts, ", ") + " via transient systemd scope " + scope,
	}
}

// bestEffortSeatReport is the best-effort evidence for a seat whose budget is
// its cooperative worker caps, as the direct spawn path reports it.
func bestEffortSeatReport(cpus, memoryMB int) Report {
	return Report{
		Mode:     ModeBestEffort,
		CPUs:     cpus,
		MemoryMB: memoryMB,
		Detail:   DescribeBestEffort(cpus, len(WorkerCapKeys())),
	}
}
