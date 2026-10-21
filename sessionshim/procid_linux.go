//go:build linux

package sessionshim

import (
	"fmt"
	"os"
)

// processStartTime reads field 22 (starttime) of /proc/<pid>/stat and reports
// it as Unix nanoseconds, matching the ProcessIdentity contract (StartedAt is
// Unix nanoseconds on every platform) and the darwin implementation.
//
// The kernel reports starttime in clock ticks since boot, so the ticks are
// scaled by the tick rate and added to the boot wall-clock from /proc/stat's
// btime line. Reporting raw ticks would keep same-boot equality working but
// would alias across reboots (ticks-since-boot restarts at zero) and would
// disagree in magnitude with the Unix-nanosecond values stored in records.
func processStartTime(pid int) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, os.ErrNotExist
		}
		return 0, fmt.Errorf("sessionshim: read /proc/%d/stat: %w", pid, err)
	}
	ticks, err := parseProcPIDStatStartTicks(data, pid)
	if err != nil {
		return 0, err
	}
	bootUnixNano, err := readLinuxBootTimeUnixNano()
	if err != nil {
		return 0, err
	}
	started, err := procStartTimeUnixNano(ticks, linuxClockTicksPerSecond, bootUnixNano)
	if err != nil {
		return 0, err
	}
	return started, nil
}

// canonicalRecordStartTime resolves a stored ProcessStartedAt to the Unix
// nanoseconds processStartTime reports, so Alive compares like with like.
// Tick-magnitude records predate the conversion and scale through the current
// boot; nanosecond records pass through. A stored value that cannot convert
// is an error (Adopt quarantines), never a stale classification that would
// release a claim on a live shim.
func canonicalRecordStartTime(stored int64) (int64, error) {
	if stored >= legacyTicksStartedAtThreshold {
		return stored, nil
	}
	bootUnixNano, err := readLinuxBootTimeUnixNano()
	if err != nil {
		return 0, err
	}
	return procStartTimeUnixNano(stored, linuxClockTicksPerSecond, bootUnixNano)
}

// linuxClockTicksPerSecond is the kernel's USER_HZ tick rate used by
// /proc/<pid>/stat field 22. It is 100 on every Linux architecture this
// binary targets; Go offers no libc-free sysconf(_SC_CLK_TCK) wrapper, so the
// rate is pinned here and the scaling itself stays unit-tested through
// procStartTimeUnixNano's clockTicksPerSecond parameter.
const linuxClockTicksPerSecond = 100

// readLinuxBootTimeUnixNano reports the boot wall-clock from /proc/stat.
func readLinuxBootTimeUnixNano() (int64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, fmt.Errorf("sessionshim: read /proc/stat: %w", err)
	}
	bootUnixNano, err := parseProcStatBootTimeUnixNano(data)
	if err != nil {
		return 0, err
	}
	return bootUnixNano, nil
}
