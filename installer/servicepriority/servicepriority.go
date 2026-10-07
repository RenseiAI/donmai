// Package servicepriority defines the process-priority modes an installed
// daemon service can run under, and persists the operator's choice (state.go)
// so a later install that does not name a mode keeps it.
//
// It is a leaf package with no dependency on the OS-specific installers, so the
// launchd and systemd installers, the OS-agnostic installer facade and the
// daemon's status self-check can all share one vocabulary.
package servicepriority

import (
	"fmt"
	"strings"
)

// Mode is a daemon service process-priority mode. The zero value means the
// operator expressed no preference.
type Mode string

const (
	// Default leaves the service definition exactly as it is without this
	// feature: no scheduling or I/O keys are emitted.
	Default Mode = "default"

	// Background runs the daemon, and every process it spawns, at the lowest
	// CPU priority with throttled disk I/O. On macOS the launchd plist carries
	// ProcessType=Background plus LowPriorityIO and LowPriorityBackgroundIO;
	// on Linux the systemd unit carries Nice=, CPUSchedulingPolicy= and
	// IOSchedulingClass=. Children inherit both.
	Background Mode = "background"
)

// Modes lists every selectable mode, in the order operators see them.
func Modes() []Mode {
	return []Mode{Default, Background}
}

// Parse normalises operator input (surrounding space and case are ignored).
// Empty input returns the zero Mode and no error: it means "no preference".
func Parse(raw string) (Mode, error) {
	mode := Mode(strings.ToLower(strings.TrimSpace(raw)))
	if err := Validate(mode); err != nil {
		return "", err
	}
	return mode, nil
}

// Validate reports whether mode is a selectable mode. The zero Mode is valid
// and means "no preference".
func Validate(mode Mode) error {
	switch mode {
	case "", Default, Background:
		return nil
	default:
		return fmt.Errorf("unsupported process priority %q (want default or background)", string(mode))
	}
}

// Effective returns the mode a renderer should apply: Default for the zero
// Mode, mode itself otherwise.
func Effective(mode Mode) Mode {
	if mode == "" {
		return Default
	}
	return mode
}
