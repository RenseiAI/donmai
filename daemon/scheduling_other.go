//go:build !darwin

package daemon

import (
	"golang.org/x/sys/unix"
)

// schedulingSelfCheck degrades gracefully on platforms without a QoS or
// IO-policy probe: QoS/IO read "unknown", nice is best-effort.
func schedulingSelfCheck() SchedulingReport {
	nice := -999
	if prio, err := unix.Getpriority(unix.PRIO_PROCESS, 0); err == nil {
		nice = prio
	}
	return schedulingReportUnknown(nice)
}
