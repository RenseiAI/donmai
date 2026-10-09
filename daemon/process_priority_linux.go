package daemon

import "os"

// observeProcessPriority reads the daemon's own scheduling policy from /proc.
func observeProcessPriority() (processPriorityObservation, bool) {
	return observeProcStatPriority(func() ([]byte, error) { return os.ReadFile("/proc/self/stat") }), true
}
