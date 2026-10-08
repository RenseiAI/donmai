package pi

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// processListingStrings returns every string in pid's exec-time argument
// and environment areas (/proc/<pid>/cmdline and /proc/<pid>/environ) — the
// blocks a process listing reads. An in-process environment assignment
// never writes into the environ block.
func processListingStrings(pid int) ([]string, error) {
	var out []string
	for _, name := range []string{"cmdline", "environ"} {
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), name)) //nolint:gosec // fixed /proc path built from a pid
		if err != nil {
			return nil, fmt.Errorf("/proc/%d/%s: %w", pid, name, err)
		}
		out = append(out, splitNULStrings(raw)...)
	}
	return out, nil
}

// sameUserPIDs lists every live process owned by this process's user.
func sameUserPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	uid := uint32(os.Getuid()) //nolint:gosec // a uid is never negative
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		info, err := os.Stat(filepath.Join("/proc", entry.Name()))
		if err != nil {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid == uid {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
