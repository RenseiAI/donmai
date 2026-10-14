// Package seatqos defines the opt-in per-seat scheduling class a daemon
// applies to the worker processes it spawns on hosts without core
// affinity. The zero value is Default: seats spawn exactly as before.
//
// The class names the intent (default, utility, background), never the
// mechanism: on macOS the spawner renders utility and background through
// the system task-policy tool, and every other OS ignores the setting.
// Reporting names the configured class verbatim so status can show what
// the operator asked for even where the OS ignores it.
package seatqos

import (
	"fmt"
	"strings"
)

// Class is one selectable per-seat scheduling class. The zero value means
// the operator expressed no preference.
type Class string

const (
	// Default spawns seats exactly as before: no scheduling wrapper, no
	// QoS clamp. This is the default on every OS.
	Default Class = "default"

	// Utility prefers efficiency cores, uses performance cores only when
	// idle, and always yields to interactive work. The host desktop stays
	// responsive under a full seat load without confining seats to the
	// efficiency cores only (which background does).
	Utility Class = "utility"

	// Background confines seats to the lowest scheduling band with
	// throttled disk I/O: the slowest option, for hosts that want seats
	// out of the way entirely.
	Background Class = "background"
)

// Classes lists every selectable class, in the order operators see them.
func Classes() []Class {
	return []Class{Default, Utility, Background}
}

// Parse normalises operator input (surrounding space and case are ignored).
// Empty input returns the zero Class and no error: it means "no preference".
func Parse(raw string) (Class, error) {
	class := Class(strings.ToLower(strings.TrimSpace(raw)))
	if err := Validate(class); err != nil {
		return "", err
	}
	return class, nil
}

// Validate reports whether class is a selectable class. The zero Class is
// valid and means "no preference".
func Validate(class Class) error {
	switch class {
	case "", Default, Utility, Background:
		return nil
	default:
		return fmt.Errorf("unsupported seat scheduling class %q (want default, utility or background)", string(class))
	}
}

// Effective returns the class a renderer should apply: Default for the zero
// Class, class itself otherwise.
func Effective(class Class) Class {
	if class == "" {
		return Default
	}
	return class
}

// Wraps reports whether spawning under class changes the child command at
// all: only the selectable non-default classes wrap. Callers gate wrapper
// rendering on this so the default path stays byte-identical to unwrapped
// spawns. A value outside the selectable set (one that bypassed Parse, as
// an embedder composing spawner options directly can) never wraps: it
// reads as default rather than reaching the wrapper as an unvetted clamp.
func Wraps(class Class) bool {
	switch class {
	case Utility, Background:
		return true
	default:
		return false
	}
}
