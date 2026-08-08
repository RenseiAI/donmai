// Package conformance is the cross-harness certification suite for the
// agent.Provider event contract (see agent/provider.go) and for the
// pre-spawn adaptation authority a harness is admitted under
// (agent/prepared_harness.go).
//
// # What this package is for
//
// A harness adapter is only as trustworthy as what has been PROVEN about it.
// A manifest is a set of claims; this package turns the claims a harness can
// be held to into executable checks, and awards a capability TIER only when
// every check in that tier passed. Nothing here reads a tier off a manifest:
// declaring `noticeDelivery` earns nothing, delivering a message into a
// running session earns the live-notice tier.
//
// # Two layers
//
// The lower layer is a set of PURE functions over a fully drained event
// sequence — CheckSingleInit, CheckTerminalContract,
// CheckCompleteAssistantTexts, and the CheckEventContract composite. They
// need no live process, so any provider test package can drain a Handle into
// a []agent.Event and assert against them. This is the layer the in-tree
// harness tests already use.
//
// The upper layer is the runnable suite: Run drives a Subject (an adapter
// plus the small amount of harness-specific glue only its author can write)
// through every Check and returns a Report of per-check pass / fail /
// not-applicable plus the tiers earned. It is deliberately runnable by an
// outsider: it imports agent + the standard library and nothing else, reaches
// no network service, and needs no credentials beyond whatever the author's
// own harness binary already requires.
//
// # Honest output is the point
//
// Two rules are mechanized here because a skip that reads as a pass is the
// failure mode this whole program keeps rediscovering:
//
//  1. A not-applicable result MUST carry a reason. A result that claims
//     StatusNotApplicable with no reason is rewritten into StatusFail by
//     newResult — the suite cannot emit a silent skip even by mistake.
//  2. A not-applicable result NEVER earns its tier. A tier is earned only
//     when every one of its checks passed, so "we could not test it" and
//     "it works" can never render the same.
//
// Report.Unverified goes further and names, on every run, the claims this
// suite does NOT check — so a green report cannot be mistaken for full
// certification against the harness-addition checklist.
//
// # Relationship to the harness-addition checklist
//
// The suite implements row 6 (event-contract conformance) in full and row 10
// (applied receipt fixtures) in the part observable from the adapter and its
// host-compiled authority alone. Row 11 (child conformance) is deliberately
// NOT implemented here: proving native-child identity/event/cancel/terminal
// mapping needs a live child spawn against an independently admitted
// execution cell, which is a smokes-lane capability, and a check that cannot
// observe a child would be exactly the silent skip rule 1 exists to prevent.
// Rows 1-5, 7, 8 and 12 are named in Report.Unverified rather than faked.
//
// The package deliberately imports only agent + the standard library so it
// can be consumed from any provider's test package without a dependency
// cycle.
package conformance

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
)

// Per-token streaming detection thresholds for CheckCompleteAssistantTexts.
//
// The contract says assistant texts are COMPLETE messages, never per-token
// deltas, but "complete" has no syntactic marker on the wire: both shapes are
// AssistantTextEvent. What separates them is shape at volume — a per-token
// stream is a long run of tiny events, and a message-complete stream is not.
// These two constants are the run length and the per-event size that define
// "long run of tiny events". They are deliberately package constants and not
// Subject knobs: a threshold the harness under test can move is not a check.
const (
	// perTokenMaxRunes is the size at or below which an assistant-text event
	// is small enough to be a token rather than a message.
	perTokenMaxRunes = 16
	// perTokenRunLength is how many consecutive small events it takes before
	// the stream is called per-token. Chosen well above the longest plausible
	// run of genuinely short complete messages.
	perTokenRunLength = 8
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
// beyond the terminal rule — see CheckSingleInit and CheckEventContract.
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

// CheckSingleInit validates the init half of the event contract over a fully
// drained sequence: exactly one InitEvent, and it is the first event emitted.
// The session identity a caller captures for resume comes off that event, so
// a second one silently re-anchors every consumer that reads it, and a late
// one means events were attributed to a session that had not been announced.
func CheckSingleInit(events []agent.Event) error {
	initCount := 0
	firstInit := -1
	for i, ev := range events {
		if _, ok := ev.(agent.InitEvent); ok {
			initCount++
			if firstInit < 0 {
				firstInit = i
			}
		}
	}
	switch {
	case initCount == 0:
		return fmt.Errorf("init-event contract: no InitEvent (want exactly one, first)")
	case initCount > 1:
		return fmt.Errorf("init-event contract: %d InitEvents (want exactly one)", initCount)
	case firstInit != 0:
		return fmt.Errorf("init-event contract: InitEvent at index %d is not first (%d events precede it)", firstInit, firstInit)
	default:
		return nil
	}
}

// CheckCompleteAssistantTexts validates that assistant text arrives as
// complete messages rather than per-token deltas.
//
// This is the one event-contract rule with no syntactic marker to assert on,
// so it is a SHAPE test with a stated threshold: a run of perTokenRunLength
// consecutive AssistantTextEvents each carrying at most perTokenMaxRunes
// runes is reported as per-token streaming. The asymmetry is deliberate — the
// check is built to produce a finding only on an unambiguous per-token shape,
// so a nil return means "no per-token shape was observed in THIS sequence",
// never "this harness is proven message-complete". A harness that emits no
// assistant text at all therefore passes; the run that certifies a harness
// should carry a prompt that produces prose.
func CheckCompleteAssistantTexts(events []agent.Event) error {
	run := 0
	for i, ev := range events {
		text, ok := ev.(agent.AssistantTextEvent)
		if !ok {
			run = 0
			continue
		}
		if utf8.RuneCountInString(text.Text) > perTokenMaxRunes {
			run = 0
			continue
		}
		run++
		if run >= perTokenRunLength {
			return fmt.Errorf(
				"assistant-text completeness: %d consecutive assistant_text events ending at index %d each carry <= %d runes, which is per-token streaming; the contract requires complete assistant messages",
				run, i, perTokenMaxRunes)
		}
	}
	return nil
}

// CheckEventContract runs every pure event-sequence invariant over a fully
// drained sequence and joins the violations. It is the single call a provider
// test package makes to assert the whole of the event contract that a
// recorded sequence can carry; channel closure and Stop idempotence are
// properties of the live Handle, not of the sequence, and are checked by the
// suite (see IDChannelCloses, IDStopIdempotent).
func CheckEventContract(events []agent.Event) error {
	return errors.Join(
		CheckSingleInit(events),
		CheckTerminalContract(events),
		CheckCompleteAssistantTexts(events),
	)
}
