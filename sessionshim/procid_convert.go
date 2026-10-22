package sessionshim

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// nanoPerSec is the integer spelling of 1e9 for Unix-nanosecond math. The
// float literal 1e9 would truncate in mixed constant expressions.
const nanoPerSec = 1_000_000_000

// legacyTicksStartedAtThreshold separates the two Linux ProcessStartedAt
// units: values below it are raw /proc starttime clock ticks since boot
// (written before the Unix-nanosecond conversion), values at or above it are
// Unix nanoseconds. Ticks at 100 Hz stay under 1e12 for three centuries of
// uptime, while real Unix nanoseconds exceed 1e18; 1e15 matches the
// wall-clock floor the platform contract pins, so no genuine value of either
// unit falls on the wrong side.
const legacyTicksStartedAtThreshold = 1_000_000_000_000_000

// linuxStartTimeSkewToleranceUnixNano bounds how far apart two Unix-nanosecond
// start-time readings for the SAME Linux process may drift and still compare
// equal. The kernel prints /proc/stat btime truncated to whole seconds and
// recomputes it from the wall clock on every read, so a wall-clock step
// (NTP/chrony step, manual date change, resume) that crosses a second boundary
// moves every later conversion by the step even though the process never
// restarted. The tolerance keeps a live shim verifiable across such steps.
// It is an anti-reuse window: two DIFFERENT incarnations of one pid whose
// start times fall inside it compare equal. Long-lived shims start far apart
// in time, so the window only matters for pids recycled within two seconds.
const linuxStartTimeSkewToleranceUnixNano = 2 * nanoPerSec

// withinLinuxStartTimeTolerance reports whether two Unix-nanosecond start
// times name the same instant modulo wall-clock skew: their absolute distance
// is at most linuxStartTimeSkewToleranceUnixNano. Both inputs must already be
// Unix nanoseconds (convert legacy ticks first); raw tick counts use exact
// equality elsewhere because they never skew.
func withinLinuxStartTimeTolerance(a, b int64) bool {
	if a > b {
		return a-b <= linuxStartTimeSkewToleranceUnixNano
	}
	return b-a <= linuxStartTimeSkewToleranceUnixNano
}

// isLegacyTicksStartedAt reports whether a stored ProcessStartedAt uses the
// pre-conversion Linux unit (clock ticks since boot) rather than Unix
// nanoseconds. Only Linux readers ever stored ticks; other platforms were
// always nanoseconds, so only the Linux comparison path consults this.

// parseProcPIDStatStartTicks extracts field 22 (starttime) from the raw bytes
// of /proc/<pid>/stat: the process start time in clock ticks since boot.
//
// comm (field 2) is parenthesised and may itself contain spaces and
// parentheses, so fields are counted from the LAST ')'. The returned value is
// raw ticks-since-boot, not wall clock: callers convert it with
// procStartTimeUnixNano before storing it in a ProcessIdentity, whose
// StartedAt is Unix nanoseconds on every platform.
func parseProcPIDStatStartTicks(statData []byte, pid int) (int64, error) {
	commEnd := strings.LastIndexByte(string(statData), ')')
	if commEnd < 0 || commEnd+2 >= len(statData) {
		return 0, fmt.Errorf("sessionshim: malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(statData[commEnd+2:]))
	// After comm, field 3 (state) is index 0, so starttime (field 22) is index 19.
	const startTimeIndex = 19
	if len(fields) <= startTimeIndex {
		return 0, fmt.Errorf("sessionshim: /proc/%d/stat has %d fields after comm, need > %d", pid, len(fields), startTimeIndex)
	}
	ticks, err := strconv.ParseInt(fields[startTimeIndex], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sessionshim: parse /proc/%d/stat starttime: %w", pid, err)
	}
	if ticks <= 0 {
		// Zero would be indistinguishable from "unknown" in a Record, whose
		// validation requires a positive value; bias to 1 tick.
		ticks = 1
	}
	return ticks, nil
}

// parseProcStatBootTimeUnixNano extracts the boot wall-clock time from the raw
// bytes of /proc/stat (the "btime <seconds>" line) as Unix nanoseconds.
//
// A missing or malformed btime line is an error, never a zero fallback: a
// zero boot time would shift every converted start time to near-epoch and let
// a stale record compare equal to a live process.
func parseProcStatBootTimeUnixNano(procStatData []byte) (int64, error) {
	for _, line := range strings.Split(string(procStatData), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "btime" {
			continue
		}
		if len(fields) != 2 {
			return 0, fmt.Errorf("sessionshim: malformed btime line in /proc/stat: %q", line)
		}
		secs, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("sessionshim: parse /proc/stat btime: %w", err)
		}
		if secs <= 0 {
			return 0, fmt.Errorf("sessionshim: /proc/stat btime %d is not positive", secs)
		}
		if secs > math.MaxInt64/nanoPerSec {
			return 0, fmt.Errorf("sessionshim: /proc/stat btime %d overflows Unix nanoseconds", secs)
		}
		return secs * nanoPerSec, nil
	}
	return 0, fmt.Errorf("sessionshim: /proc/stat has no btime line")
}

// coerceLegacyTicksStartedAt resolves a stored ProcessStartedAt to Unix
// nanoseconds for comparison: tick-magnitude values convert through the
// given tick rate and boot wall-clock, nanosecond-magnitude values pass
// through unchanged. Fail-closed on every malformed input, exactly like
// procStartTimeUnixNano: a stored value that cannot convert is an error,
// never a guess the caller could compare equal to a live process.
func coerceLegacyTicksStartedAt(storedTicks, clockTicksPerSecond, bootTimeUnixNano int64) (int64, error) {
	if storedTicks >= legacyTicksStartedAtThreshold {
		return storedTicks, nil
	}
	return procStartTimeUnixNano(storedTicks, clockTicksPerSecond, bootTimeUnixNano)
}

// procStartTimeUnixNano converts a /proc starttime (clock ticks since boot)
// to Unix nanoseconds: boot time plus ticks scaled by the clock-tick rate.
//
// The division is split into whole seconds and remainder so ticks*1e9 never
// overflows int64 for realistic uptimes. Every input is validated fail-closed:
// a non-positive tick count, tick rate, or boot time is an error, as is a
// result that would overflow int64.
func procStartTimeUnixNano(ticks, clockTicksPerSecond, bootTimeUnixNano int64) (int64, error) {
	if ticks <= 0 {
		return 0, fmt.Errorf("sessionshim: starttime ticks %d is not positive", ticks)
	}
	if clockTicksPerSecond <= 0 {
		return 0, fmt.Errorf("sessionshim: clock ticks per second %d is not positive", clockTicksPerSecond)
	}
	if bootTimeUnixNano <= 0 {
		return 0, fmt.Errorf("sessionshim: boot time %d is not positive", bootTimeUnixNano)
	}
	secs := ticks / clockTicksPerSecond
	rem := ticks % clockTicksPerSecond
	if secs > (math.MaxInt64-bootTimeUnixNano)/nanoPerSec {
		return 0, fmt.Errorf("sessionshim: starttime ticks %d overflows Unix nanoseconds", ticks)
	}
	if rem > math.MaxInt64/nanoPerSec {
		return 0, fmt.Errorf("sessionshim: starttime ticks %d overflows Unix nanoseconds", ticks)
	}
	return bootTimeUnixNano + secs*nanoPerSec + rem*nanoPerSec/clockTicksPerSecond, nil
}
