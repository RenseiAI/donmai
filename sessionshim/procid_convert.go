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
