package pi

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// processListingStrings returns every string in pid's exec-time argument
// area — the executable path, argv and the environment, as the kernel holds
// them (KERN_PROCARGS2). It is the exact block a process listing renders: a
// process that rewrites its own title overwrites the argv strings in place,
// and a listing then prints the environment strings that follow as if they
// were arguments. An in-process environment assignment never writes into
// this block.
func processListingStrings(pid int) ([]string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("kern.procargs2 %d: %w", pid, err)
	}
	if len(raw) < 4 {
		return nil, fmt.Errorf("kern.procargs2 %d: short read", pid)
	}
	return splitNULStrings(raw[4:]), nil
}

// sameUserPIDs lists every live process owned by this process's user.
func sameUserPIDs() ([]int, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, fmt.Errorf("kern.proc.uid: %w", err)
	}
	pids := make([]int, 0, len(procs))
	for _, proc := range procs {
		pids = append(pids, int(proc.Proc.P_pid))
	}
	return pids, nil
}
