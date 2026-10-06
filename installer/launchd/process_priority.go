package launchd

import (
	"fmt"
	"strings"
)

// ProcessPriority controls how launchd should start the daemon process.
//
// default    — no wrapper; preserves today's plist byte-for-byte.
// utility    — wraps the daemon command in `taskpolicy -c utility`.
// background — wraps the daemon command in `taskpolicy -b`.
//
// Background intentionally uses the taskpolicy wrapper rather than a
// ProcessType/LowPriorityIO plist pair so the same single wrapper primitive
// carries both the launch-time policy and a straightforward runtime probe
// (`ps` priority 4) for status/doctor.
type ProcessPriority string

const (
	// ProcessPriorityDefault preserves today's plist with no wrapper.
	ProcessPriorityDefault ProcessPriority = "default"
	// ProcessPriorityUtility wraps the daemon in `taskpolicy -c utility`.
	ProcessPriorityUtility ProcessPriority = "utility"
	// ProcessPriorityBackground wraps the daemon in `taskpolicy -b`.
	ProcessPriorityBackground ProcessPriority = "background"
)

// NormalizeProcessPriority maps empty input to default and lower-cases the
// supported public values.
func NormalizeProcessPriority(priority ProcessPriority) ProcessPriority {
	switch strings.ToLower(strings.TrimSpace(string(priority))) {
	case "", string(ProcessPriorityDefault):
		return ProcessPriorityDefault
	case string(ProcessPriorityUtility):
		return ProcessPriorityUtility
	case string(ProcessPriorityBackground):
		return ProcessPriorityBackground
	default:
		return ProcessPriority(strings.ToLower(strings.TrimSpace(string(priority))))
	}
}

// ValidateProcessPriority rejects unsupported public values.
func ValidateProcessPriority(priority ProcessPriority) error {
	normalized := NormalizeProcessPriority(priority)
	switch normalized {
	case ProcessPriorityDefault, ProcessPriorityUtility, ProcessPriorityBackground:
		return nil
	default:
		return fmt.Errorf("launchd: unsupported process priority %q (want default, utility, or background)", priority)
	}
}

// ProgramArguments returns the launchd ProgramArguments array for the host
// binary and selected process priority.
func ProgramArguments(hostBinPath string, priority ProcessPriority) ([]string, error) {
	if hostBinPath == "" {
		return nil, fmt.Errorf("launchd: hostBinPath is required")
	}
	priority = NormalizeProcessPriority(priority)
	if err := ValidateProcessPriority(priority); err != nil {
		return nil, err
	}
	args := []string{hostBinPath}
	args = append(args, strings.Fields(DaemonSubcommand)...)
	switch priority {
	case ProcessPriorityUtility:
		return append([]string{"/usr/sbin/taskpolicy", "-c", "utility"}, args...), nil
	case ProcessPriorityBackground:
		return append([]string{"/usr/sbin/taskpolicy", "-b"}, args...), nil
	default:
		return args, nil
	}
}

// ServiceCommand renders ProgramArguments as a display string for install
// reporting.
func ServiceCommand(hostBinPath string, priority ProcessPriority) (string, error) {
	args, err := ProgramArguments(hostBinPath, priority)
	if err != nil {
		return "", err
	}
	return strings.Join(args, " "), nil
}

// DetectProcessPriorityFromPlist infers the configured mode from a generated
// launchd plist's ProgramArguments. Unknown content reads as default.
func DetectProcessPriorityFromPlist(plist string) ProcessPriority {
	switch {
	case strings.Contains(plist, "<string>/usr/sbin/taskpolicy</string>") &&
		strings.Contains(plist, "<string>-c</string>") &&
		strings.Contains(plist, "<string>utility</string>"):
		return ProcessPriorityUtility
	case strings.Contains(plist, "<string>/usr/sbin/taskpolicy</string>") &&
		strings.Contains(plist, "<string>-b</string>"):
		return ProcessPriorityBackground
	default:
		return ProcessPriorityDefault
	}
}
