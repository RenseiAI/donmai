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
// after SYSTEM-attributed text is paced (systeminput.go's ~120ms production
// gap, unshrunk here — ptyhost.systemInputPacingGap is package-private, not
// reachable from this package), while the exact same shape sent under an
// ordinary human userId is not.
//
// Both round trips are measured in THIS run and compared to each other
// rather than against a fixed wall-clock ceiling: an absolute "< N ms" bound
// on the human case is exactly what flakes under -race on a loaded machine,
// where scheduling/process jitter alone can push an ordinary round trip past
// a tight threshold. A real last-hop delay can only make the system case
// take AT LEAST the production gap — never less — so "human finished well
// before system, in the SAME run, under the SAME load" is what actually
// distinguishes "delayed" from "not delayed", and a generous absolute floor
// on the system case (well under the true ~120ms, so it never itself flakes)
// confirms a real delay happened at all rather than the two just landing in
// some order by chance.
//
// The harness here is a plain `sh` line reader, not a paste-heuristic TUI —
// it cannot itself demonstrate the "CR swallowed as a literal newline"
// symptom (that failure mode belongs to an application-level line editor,
// exhaustively covered against a fake writer in ptyhost). What this test
// proves is squarely its own: the WIRE plumbing carries userId end to end and
// actually triggers the delay at the real ptyhost.Session boundary.
func TestShimAttributedInputSystemPacedHumanImmediate(t *testing.T) {
	fixture := startInProcessV4Fixture(t, 0)
	if !fixture.controller.SupportsAttributedInput() {
		t.Fatalf("fixture selected v%d, want attributed-input support (v4+)", fixture.controller.SelectedVersion())
	}

	const (
		humanUserID = "user_01hz3k9xyz"
		// sanityFloor is well under the true ~120ms production gap (so it
		// never itself flakes under load) but high enough that an ordinary
		// unpaced round trip (typically low single-digit ms) could not
		// plausibly clear it by chance.
		sanityFloor = 40 * time.Millisecond
		waitBound   = 10 * time.Second
	)

	if err := fixture.controller.WriteAttributedInput([]byte(humanUserID), []byte("human-token")); err != nil {
		t.Fatalf("WriteAttributedInput(human text): %v", err)
	}
	humanStart := time.Now()
	if err := fixture.controller.WriteAttributedInput([]byte(humanUserID), []byte("\r")); err != nil {
		t.Fatalf("WriteAttributedInput(human CR): %v", err)
	}
	humanElapsed := waitForAck(t, fixture.controller, "human-token", waitBound).Sub(humanStart)

	if err := fixture.controller.WriteAttributedInput([]byte(attachwire.SystemNudgeUserID), []byte("system-token")); err != nil {
		t.Fatalf("WriteAttributedInput(system text): %v", err)
	}
	systemStart := time.Now()
	if err := fixture.controller.WriteAttributedInput([]byte(attachwire.SystemNudgeUserID), []byte("\r")); err != nil {
		t.Fatalf("WriteAttributedInput(system CR): %v", err)
	}
	systemElapsed := waitForAck(t, fixture.controller, "system-token", waitBound).Sub(systemStart)

	if systemElapsed < sanityFloor {
		t.Errorf("system-attributed round trip took only %v, want >= %v (the last-hop pacing gap)", systemElapsed, sanityFloor)
	}
	if humanElapsed >= systemElapsed {
		t.Errorf("human round trip (%v) was not clearly faster than the paced system round trip (%v); human input must never be delayed", humanElapsed, systemElapsed)
	}
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
