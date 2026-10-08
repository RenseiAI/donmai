package claude

import (
	"runtime"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/agent/conformance"
)

// fakeSuccessCLI writes a minimal /bin/sh script emitting the canonical
// claude stream-json envelope (init → assistant text → terminal
// success result), for the shared cross-harness event-contract
// conformance check (ADR-2026-06-06 D6 / ADR-C row 6;
// runs/2026-07-21-open-harness-strategy/12-work-breakdown.md W0 item 2).
func fakeSuccessCLI(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses /bin/sh; skip on windows")
	}
	// Publish through the shared immutable dispatcher (see writeFakeCLI
	// in oneshot_test.go), never as a freshly written executable: a
	// sibling test that forks while this file's writer is open hands the
	// writer to its child, and execve of the script then fails with
	// ETXTBSY.
	return writeFakeCLI(t, "fake-claude-ok.sh",
		"#!/bin/sh\n"+
			`printf '{"type":"system","subtype":"init","session_id":"sess-conf-1"}\n'`+"\n"+
			`printf '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}\n'`+"\n"+
			`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'`+"\n")
}

// drainAllWithIdle collects events from ch until it closes, ctx-independent
// idle elapses with no new event, or the overall deadline fires — whichever
// comes first. clijsonl's Handle keeps the channel open after the
// subprocess exits (closed by Stop), so an idle gap is how the test
// observes "the reader has emitted everything it will" without requiring
// an explicit Stop() race.
func drainAllWithIdle(t *testing.T, ch <-chan agent.Event, idle, deadline time.Duration) []agent.Event {
	t.Helper()
	var got []agent.Event
	overall := time.NewTimer(deadline)
	defer overall.Stop()
	idleTimer := time.NewTimer(idle)
	defer idleTimer.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
			if !idleTimer.Stop() {
				<-idleTimer.C
			}
			idleTimer.Reset(idle)
		case <-idleTimer.C:
			return got
		case <-overall.C:
			t.Fatalf("drainAllWithIdle: deadline exceeded; collected %d events so far", len(got))
			return got
		}
	}
}

// TestProvider_Spawn_ConformsToTerminalEventContract wires the shared
// cross-harness event-contract conformance check (agent/conformance,
// seeded by donmai PR #199 for opencode) onto the claude harness: a
// successful run must emit exactly one terminal event
// (ResultEvent/ErrorEvent), and it must be the last event on the
// channel — the same D-1-class invariant opencode's adapter once
// violated.
func TestProvider_Spawn_ConformsToTerminalEventContract(t *testing.T) {
	t.Parallel()

	cli := fakeSuccessCLI(t)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h, err := p.Spawn(t.Context(), agent.Spec{Prompt: "say hi"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(t.Context()) }()

	// Idle window generously sized (not 2s): under a full-suite -race
	// parallel run, fork/exec scheduling for the fake CLI subprocess can
	// be delayed well past a short window by CPU contention from dozens
	// of concurrently-running tests — a short idle can elapse before the
	// FIRST event ever arrives, reporting a false "no terminal event"
	// (reproduced: this test and the opencode D-1 regression test both
	// flaked this way in the same full -race run, then passed on rerun).
	events := drainAllWithIdle(t, h.Events(), 5*time.Second, 45*time.Second)
	if err := conformance.CheckTerminalContract(events); err != nil {
		t.Errorf("terminal-event contract violated: %v", err)
	}
}
