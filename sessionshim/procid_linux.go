//go:build linux

package sessionshim

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processStartTime reads field 22 (starttime) of /proc/<pid>/stat: the process
// start time in clock ticks since boot, straight from the kernel.
//
// Ticks-since-boot are not wall-clock nanoseconds, and deliberately are not
// converted to any: the value is used only for EQUALITY against a previously
// recorded value for the same pid on the same boot, which is exactly the
// anti-reuse question being asked. Converting through a boot-time estimate
// would add drift to a comparison that needs none.
func processStartTime(pid int) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, os.ErrNotExist
		}
		return 0, fmt.Errorf("sessionshim: read /proc/%d/stat: %w", pid, err)
	}
	// comm (field 2) is parenthesised and may itself contain spaces and
	// parentheses, so fields are counted from the LAST ')'.
	commEnd := strings.LastIndexByte(string(data), ')')
	if commEnd < 0 || commEnd+2 >= len(data) {
		return 0, fmt.Errorf("sessionshim: malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(data[commEnd+2:]))
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
