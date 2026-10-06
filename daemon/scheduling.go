package daemon

import (
	"runtime"
	"strings"
)

// SchedulingReport is the daemon's runtime self-check of its effective
// scheduling tier: the QoS class the process is actually running at plus
// the disk-IO policy the kernel applies to it. Status and doctor surfaces
// carry it so operators and embedders can confirm an installed
// process-priority tier took effect (or notice when it did not — e.g. a
// plist written by an older build, or a launchd job started outside the
// registered service).
//
// Every field is a plain string (never an enum) and every probe degrades
// to "" or "unknown" rather than failing: the report must never break
// status/doctor on a platform where a probe is unavailable.
type SchedulingReport struct {
	// QOSClass is the process's effective QoS class as reported by the
	// platform probe: one of user-interactive, user-initiated, default,
	// utility, background, unspecified — or "unknown" when the probe
	// cannot tell (non-Darwin, sandboxed, probe failure).
	QOSClass string `json:"qosClass"`
	// IOPolicy is the process-scoped disk-IO policy: one of default,
	// important, passive, throttle, utility, standard — or "unknown".
	IOPolicy string `json:"ioPolicy"`
	// BackgroundIO reports the Darwin-background disk-IO tier the kernel
	// applies to background-classified work: one of the IOPolicy values
	// or "unknown". Empty on platforms without the concept.
	BackgroundIO string `json:"backgroundIo,omitempty"`
	// Nice is the process nice value (getpriority PRIO_PROCESS). -999
	// when the probe is unavailable.
	Nice int `json:"nice"`
	// Source names the probe that produced this report: "darwin" on
	// macOS, "unsupported" elsewhere.
	Source string `json:"source"`
}

// SchedulingSelfCheck reports the daemon process's effective scheduling
// tier. It is safe to call on any platform: non-Darwin builds return an
// "unsupported" report with Nice populated best-effort.
func SchedulingSelfCheck() SchedulingReport {
	return schedulingSelfCheck()
}

// schedulingSource describes which platform probe backs the report.
func schedulingSource() string {
	if runtime.GOOS == "darwin" {
		return "darwin"
	}
	return "unsupported"
}

// schedulingReportUnknown builds the degraded report used on platforms
// without a QoS/IO-policy probe. niceProbe supplies the nice value (or
// -999 when even that is unavailable).
func schedulingReportUnknown(nice int) SchedulingReport {
	return SchedulingReport{
		QOSClass: "unknown",
		IOPolicy: "unknown",
		Nice:     nice,
		Source:   schedulingSource(),
	}
}

// normalizeSchedulingToken lowercases a raw probe token and maps "" to
// "unknown" so every report field is always populated.
func normalizeSchedulingToken(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}
