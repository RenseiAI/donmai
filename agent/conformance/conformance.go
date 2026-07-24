// Package conformance holds shared, provider-agnostic assertions for the
// agent.Provider event contract (see agent/provider.go).
//
// It is the minimal seed of the cross-harness conformance suite (ADR-C):
// today it encodes only the terminal-event ordering invariant — the
// contract that the opencode D-1 bug violated (a successful run appended a
// spurious spawn_no_result ErrorEvent after the terminal ResultEvent).
// Provider test packages drain a Handle's Events channel into a
// []agent.Event and pass it to CheckTerminalContract; each harness
// (claude / codex / opencode / pi / stub) can be wired to the same check
// as the suite grows.
//
// The package deliberately imports only agent + the standard library so
// it can be consumed from any provider's test package without a
// dependency cycle.
package conformance

import (
	"fmt"

	"github.com/RenseiAI/donmai/agent"
)

// IsTerminal reports whether ev is a session-terminal event. Per the
// Provider contract (agent/provider.go) a session ends with "exactly one
// terminal ResultEvent (or ErrorEvent followed by close), then closes",
// so both ResultEvent and ErrorEvent are terminal.
func IsTerminal(ev agent.Event) bool {
	switch ev.(type) {
	case agent.ResultEvent, agent.ErrorEvent:
		return true
	default:
		return false
	}
}

// CheckTerminalContract validates the terminal-event ordering invariant
// over a fully drained event sequence: there must be exactly one terminal
// event and it must be the last event emitted (no events may follow it).
// It returns a descriptive error on violation and nil when the sequence
// conforms. It does not assert InitEvent presence or per-event ordering
// beyond the terminal rule — those belong to later conformance rows.
func CheckTerminalContract(events []agent.Event) error {
	terminalCount := 0
	terminalIdx := -1
	for i, ev := range events {
		if IsTerminal(ev) {
			terminalCount++
			terminalIdx = i
		}
	}
	switch {
	case terminalCount == 0:
		return fmt.Errorf("terminal-event contract: no terminal event (want exactly one ResultEvent or ErrorEvent)")
	case terminalCount > 1:
		return fmt.Errorf("terminal-event contract: %d terminal events (want exactly one, then channel close)", terminalCount)
	case terminalIdx != len(events)-1:
		return fmt.Errorf("terminal-event contract: terminal event at index %d of %d is not last (no events may follow the terminal event)", terminalIdx, len(events))
	default:
		return nil
	}
}
