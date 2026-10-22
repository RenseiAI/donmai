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

// isLegacyTicksStartedAt reports whether a stored ProcessStartedAt uses the
// pre-conversion Linux unit (clock ticks since boot) rather than Unix
// nanoseconds. Only Linux readers ever stored ticks; other platforms were
// always nanoseconds, so only the Linux comparison path consults this.
func isLegacyTicksValue(v int64) bool { return v < legacyTicksStartedAtThreshold }

// linuxProcessIdentityEqual reports whether two Linux identities name the
// same process incarnation, without touching the wall clock. Complete
// identities (raw ticks plus boot ID) compare exactly; legacy
// tick-magnitude records compare ticks exactly as the pre-conversion code
// did. Nanosecond values without the key cannot be verified drift-free and
// never match here. It is pure and platform-independent so the decision is
// unit-testable everywhere; the Linux comparison path calls it, and other
// platforms keep exact Unix-nanosecond equality.
func linuxProcessIdentityEqual(a, b ProcessIdentity) (bool, error) {
	aHasKey := a.StartTicks > 0 && a.BootID != ""
	bHasKey := b.StartTicks > 0 && b.BootID != ""
	if aHasKey && bHasKey {
		return a.StartTicks == b.StartTicks && a.BootID == b.BootID, nil
	}
	if aHasKey && !bHasKey {
		if isLegacyTicksValue(b.StartedAt) {
			return a.StartTicks == b.StartedAt, nil
		}
		return false, fmt.Errorf("sessionshim: Linux process identity is missing the drift-free key")
	}
	if bHasKey && !aHasKey {
		if isLegacyTicksValue(a.StartedAt) {
			return b.StartTicks == a.StartedAt, nil
		}
		return false, fmt.Errorf("sessionshim: Linux process identity is missing the drift-free key")
	}
	if !aHasKey && !bHasKey {
		// Both sides predate the key (two legacy records, or two fabricated
		// test identities): fall back to exact start-time equality so old
		// records and existing tests keep working. Production Linux
		// identities always carry the key; mixed key/no-key comparisons
		// above stay fail-closed.
		return a.StartedAt == b.StartedAt, nil
	}
	return false, fmt.Errorf("sessionshim: Linux process identity is missing the drift-free key")
}

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
