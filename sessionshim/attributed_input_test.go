package sessionshim

import (
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/shimwire"
)

// waitForAck drains c.Events() until an accumulated "ack:<token>" has
// appeared, across either event shape a controller emits — EventOutput for
// selected < v3, EventHostFrame for selected v3+ (both populate Data with the
// decoded output bytes; see decodeHostFrameEvent in controller.go). It
// returns the instant the full token was observed, so a caller can measure
// the gap between sending a write and the harness actually seeing it land.
func waitForAck(t *testing.T, c *Controller, token string, deadline time.Duration) time.Time {
	t.Helper()
	want := "ack:" + token
	var seen strings.Builder
	timer := time.After(deadline)
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				t.Fatalf("controller stream closed before %q; saw %q", want, seen.String())
			}
			if ev.Kind != EventOutput && ev.Kind != EventHostFrame {
				continue
			}
			seen.Write(ev.Data)
			if strings.Contains(seen.String(), want) {
				return time.Now()
			}
		case <-timer:
			t.Fatalf("timed out waiting for %q; saw %q", want, seen.String())
		}
	}
}

// TestShimAttributedInputSystemPacedHumanImmediate is the shim-path
// counterpart to ptyhost/systeminput_test.go's fake-writer coverage: it
// drives the REAL wire (Controller.WriteAttributedInput ->
// shimwire.TypeAttributedInput -> Shim.dispatch -> Session.WriteAttributedInput
// -> the real PTY) and proves attribution actually reaches the last-hop
// write boundary through every hop — a SYSTEM-attributed bare CR sent right
// after SYSTEM-attributed text is paced (systeminput.go's ~120ms gap),
// while the exact same shape sent under an ordinary human userId is not.
//
// The harness here is a plain `sh` line reader, not a paste-heuristic TUI —
// it cannot itself demonstrate the "CR swallowed as a literal newline"
// symptom (that failure mode belongs to an application-level line editor,
// exhaustively covered against a fake writer in ptyhost). What this test
// proves is squarely its own: the WIRE plumbing carries userId end to end and
// actually triggers the delay at the real ptyhost.Session boundary.
func TestShimAttributedInputSystemPacedHumanImmediate(t *testing.T) {
	fixture := startInProcessV3Fixture(t, 0)
	if !fixture.controller.SupportsAttributedInput() {
		t.Fatalf("fixture selected v%d, want attributed-input support (v4+)", fixture.controller.SelectedVersion())
	}

	const (
		humanUserID = "user_01hz3k9xyz"
		// pacingFloor sits comfortably below the ~120ms production gap (so a
		// real delay is unambiguously detected even under scheduling jitter)
		// and comfortably above an ordinary unpaced local round trip (a few
		// ms), so the human case is unambiguously NOT delayed.
		pacingFloor = 90 * time.Millisecond
		waitBound   = 10 * time.Second
	)

	t.Run("human userId is never delayed", func(t *testing.T) {
		if err := fixture.controller.WriteAttributedInput([]byte(humanUserID), []byte("human-token")); err != nil {
			t.Fatalf("WriteAttributedInput(text): %v", err)
		}
		start := time.Now()
		if err := fixture.controller.WriteAttributedInput([]byte(humanUserID), []byte("\r")); err != nil {
			t.Fatalf("WriteAttributedInput(CR): %v", err)
		}
		ackAt := waitForAck(t, fixture.controller, "human-token", waitBound)
		if elapsed := ackAt.Sub(start); elapsed >= pacingFloor {
			t.Errorf("human-attributed round trip took %v, want < %v (human input must never be delayed)", elapsed, pacingFloor)
		}
	})

	t.Run("the shared SYSTEM sentinel is paced", func(t *testing.T) {
		if err := fixture.controller.WriteAttributedInput([]byte(attachwire.SystemNudgeUserID), []byte("system-token")); err != nil {
			t.Fatalf("WriteAttributedInput(text): %v", err)
		}
		start := time.Now()
		if err := fixture.controller.WriteAttributedInput([]byte(attachwire.SystemNudgeUserID), []byte("\r")); err != nil {
			t.Fatalf("WriteAttributedInput(CR): %v", err)
		}
		ackAt := waitForAck(t, fixture.controller, "system-token", waitBound)
		if elapsed := ackAt.Sub(start); elapsed < pacingFloor {
			t.Errorf("system-attributed round trip took only %v, want >= %v (the last-hop pacing gap)", elapsed, pacingFloor)
		}
	})
}

// TestControllerWriteAttributedInputFallsBackBelowV4 pins
// Controller.WriteAttributedInput's degrade path: a controller pinned to a
// pre-v4 selected version never sends shimwire.TypeAttributedInput at all
// (which an older shim could not decode) — it falls back to the exact
// byte-identical TypeInput/EncodeInput send those versions have always used,
// and the write still lands, verbatim.
func TestControllerWriteAttributedInputFallsBackBelowV4(t *testing.T) {
	id := Identity{OrgID: "org-attributed-fallback", SessionID: "session-attributed-fallback"}
	f := startShimHelper(t, id, 0)
	result := f.adoptAsMax(t, "controller-attributed-fallback", shimwire.V2)
	if len(result.Adopted) != 1 {
		t.Fatalf("adopt = %+v", result)
	}
	c := result.Adopted[0]
	if c.SupportsAttributedInput() {
		t.Fatalf("selected v%d, want NO attributed-input support (pinned below v4)", c.SelectedVersion())
	}
	if err := c.WriteAttributedInput([]byte(attachwire.SystemNudgeUserID), []byte("fallback-token\r")); err != nil {
		t.Fatalf("WriteAttributedInput (fallback): %v", err)
	}
	waitForAck(t, c, "fallback-token", 20*time.Second)
}
