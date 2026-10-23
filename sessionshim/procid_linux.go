//go:build linux

package sessionshim

import (
	"fmt"
	"os"
	"strings"
)

// processStartTime reads field 22 (starttime) of /proc/<pid>/stat and reports
// it as Unix nanoseconds, matching the ProcessIdentity contract (StartedAt is
// Unix nanoseconds on every platform) and the darwin implementation.
//
// The kernel reports starttime in clock ticks since boot, so the ticks are
// scaled by the tick rate and added to the boot wall-clock from /proc/stat's
// btime line. The Unix-nanosecond value is the contract and display unit;
// equality never uses it on Linux (see processIdentityMatches), so wall-clock
// steps that move btime cannot alias or split an incarnation.
var linuxBootTimeReader = readLinuxBootTimeUnixNano

// linuxBootIDReader reports the boot ID from
// /proc/sys/kernel/random/boot_id. Tests stub it to simulate a reboot;
// production always reads the kernel file.
var linuxBootIDReader = readLinuxBootID

func processStartTime(pid int) (int64, error) {
	ticks, err := readLinuxStartTicks(pid)
	if err != nil {
		return 0, err
	}
	bootUnixNano, err := linuxBootTimeReader()
	if err != nil {
		return 0, err
	}
	started, err := procStartTimeUnixNano(ticks, linuxClockTicksPerSecond, bootUnixNano)
	if err != nil {
		return 0, err
	}
	return started, nil
}

// processIdentityExtra reports the Linux drift-free key for pid: raw
// field-22 ticks-since-boot plus the boot ID. Both ignore the wall clock
// (ticks count since boot, the boot ID is a per-boot UUID), so comparing
// them exactly identifies an incarnation across NTP steps, manual date
// changes, suspend/resume and leap seconds.
func processIdentityExtra(pid int) (int64, string, error) {
	ticks, err := readLinuxStartTicks(pid)
	if err != nil {
		return 0, "", err
	}
	bootID, err := linuxBootIDReader()
	if err != nil {
		return 0, "", err
	}
	return ticks, bootID, nil
}

// validateStoredProcessIdentity fails closed for stored values that cannot
// name a Linux process: a tick-magnitude value that overflows Unix
// nanoseconds (corrupt, never a real uptime), or a nanosecond-magnitude
// value without the drift-free key (a record that predates the key or is
// otherwise incomplete). Legacy tick records and complete new records pass.
func validateStoredProcessIdentity(p ProcessIdentity) error {
	if p.StartTicks > 0 && p.BootID != "" {
		return nil
	}
	if p.StartTicks != 0 || p.BootID != "" {
		return fmt.Errorf("sessionshim: incomplete Linux process identity pid=%d", p.PID)
	}
	if p.StartedAt < legacyTicksStartedAtThreshold {
		bootUnixNano, err := linuxBootTimeReader()
		if err != nil {
			return err
		}
		if _, err := coerceLegacyTicksStartedAt(p.StartedAt, linuxClockTicksPerSecond, bootUnixNano); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("sessionshim: Linux process identity pid=%d is missing the drift-free key", p.PID)
}

// processIdentityMatches reports whether two identities name the same Linux
// process incarnation, without touching the wall clock. It delegates to the
// platform-independent decision so the equality is unit-testable everywhere.
func processIdentityMatches(a, b ProcessIdentity) (bool, error) {
	return linuxProcessIdentityEqual(a, b)
}

// readLinuxStartTicks reads raw field-22 ticks-since-boot for pid.
func readLinuxStartTicks(pid int) (int64, error) {
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
	return ticks, nil
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

// readLinuxBootID reports the boot ID from
// /proc/sys/kernel/random/boot_id. The value is a per-boot UUID: stable
// across wall-clock steps, different after every reboot, exactly the
// cross-boot discriminator raw ticks lack.
func readLinuxBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("sessionshim: read boot_id: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("sessionshim: boot_id is empty")
	}
	return id, nil
}
