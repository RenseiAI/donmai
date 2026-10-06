//go:build darwin

package daemon

import (
	"golang.org/x/sys/unix"
)

// QoS class values from <sys/qos.h> (qos_class_t is an unsigned int enum).
const (
	qosClassUserInteractive = 0x21
	qosClassUserInitiated   = 0x19
	qosClassDefault         = 0x15
	qosClassUtility         = 0x11
	qosClassBackground      = 0x09
	qosClassUnspecified     = 0x00
)

// Scheduling tier constants from <sys/resource.h> for getiopolicy_np.
const (
	ioPolicyTypeDisk = 0

	ioPolicyScopeProcess  = 0
	ioPolicyScopeDarwinBG = 2
)

// qosClassSelfName returns the current thread's effective QoS class name,
// issued through the raw libc trampoline in scheduling_trampoline_darwin.s
// (x/sys does not wrap qos_class_self, and the build runs CGO_ENABLED=0).
func qosClassSelfName() string {
	switch qosClassSelfABI() {
	case qosClassUserInteractive:
		return "user-interactive"
	case qosClassUserInitiated:
		return "user-initiated"
	case qosClassDefault:
		return "default"
	case qosClassUtility:
		return "utility"
	case qosClassBackground:
		return "background"
	case qosClassUnspecified:
		return "unspecified"
	default:
		return "unknown"
	}
}

// ioPolicyName maps a getiopolicy_np disk-policy value (<sys/resource.h>
// IOPOL_*_PRIORITY) to its report token.
func ioPolicyName(policy int) string {
	switch policy {
	case 0: // IOPOL_DEFAULT
		return "default"
	case 1: // IOPOL_IMPORTANT
		return "important"
	case 2: // IOPOL_PASSIVE
		return "passive"
	case 3: // IOPOL_THROTTLE
		return "throttle"
	case 4: // IOPOL_UTILITY
		return "utility"
	case 5: // IOPOL_STANDARD
		return "standard"
	default:
		return "unknown"
	}
}

func schedulingSelfCheck() SchedulingReport {
	qos := normalizeSchedulingToken(qosClassSelfName())
	ioPolicy := "unknown"
	if policy, err := getiopolicy_np(ioPolicyTypeDisk, ioPolicyScopeProcess); err == nil {
		ioPolicy = ioPolicyName(policy)
	}
	backgroundIO := "unknown"
	if policy, err := getiopolicy_np(ioPolicyTypeDisk, ioPolicyScopeDarwinBG); err == nil {
		backgroundIO = ioPolicyName(policy)
	}
	nice := -999
	if prio, err := unix.Getpriority(unix.PRIO_PROCESS, 0); err == nil {
		nice = prio
	}
	return SchedulingReport{
		QOSClass:     qos,
		IOPolicy:     ioPolicy,
		BackgroundIO: backgroundIO,
		Nice:         nice,
		Source:       "darwin",
	}
}
