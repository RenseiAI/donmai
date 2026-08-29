package daemon

// Provenance: terminal-ack-survives-a-dead-stream-2026-08-28 — grep a build for
// this marker to prove it reports an Exit whose acknowledgement could not be
// delivered.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
)

// TestExitSurvivesAnUndeliverableCursorAcknowledgement is the measured strand.
//
// A shim publishes Exit, finishes its own bounded finalization and closes. The
// daemon receives that Exit and durably persists it to the carrier — and THEN
// tries to acknowledge the cursor back to the shim, which is a write to a
// socket nobody is holding any more. Pre-fix, that failed write made the
// consume loop drop a terminal observation it had already made durable: the
// session went to `socket_unreachable` quarantine with its harness already
// reaped, which §D10 calls unresolved rather than ended, and the platform's
// stop could never reach it again.
//
// The acknowledgement is the SHIM's replay cursor. A shim that published Exit
// and closed has nothing left to replay, so failing to deliver it is not a
// reason to throw away the outcome.
func TestExitSurvivesAnUndeliverableCursorAcknowledgement(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	// The cursor acknowledgement only exists on the selected-v3 full-host-frame
	// rail; that is the rail the measured host runs.
	d.setState(StateRunning)
	d.shims.adoptionComplete = true
	d.shims.carrierActivationComplete = true
	d.opts.SessionShim.HostID = "host-ack-lost"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	enableHostedFullHostFramesForTest(t, d, f.orgID)
	probe := &dynamicPublicationProbe{}
	probe.carrierEpoch.Store(40)
	configureDynamicPublicationProbe(t, d, probe)

	var mu sync.Mutex
	closedOnExit := false
	ackFailed := false
	terminalReports := 0
	// SessionEventEnded is emitted ONLY by the Exit path (finishAdoptedShim).
	// The quarantine-and-recover path reports evidence but never emits a
	// lifecycle end, so this is the assertion that tells the two apart.
	lifecycleEnds := 0
	d.spawner.On(func(ev SessionEvent) {
		if ev.Kind != SessionEventEnded {
			return
		}
		mu.Lock()
		lifecycleEnds++
		mu.Unlock()
	})
	f.daemon.opts.SessionShim.OnTerminalEvidence = func(_ context.Context, evidence SessionShimTerminalEvidence) error {
		mu.Lock()
		defer mu.Unlock()
		if evidence.Tombstone.GroupReaped {
			terminalReports++
		}
		return nil
	}
	f.daemon.opts.SessionShim.OnSessionEventDurable = func(id sessionshim.Identity, event sessionshim.ControllerEvent) error {
		// The carrier accepts this Exit — and by the time the daemon turns
		// round to acknowledge it, the shim's stream is gone. Closing the
		// controller here is the same observable state as the measured EPIPE:
		// the very next Heartbeat write fails.
		if event.Kind == sessionshim.EventHostFrame && event.FrameType == attachwire.TypeExit {
			mu.Lock()
			closedOnExit = true
			mu.Unlock()
			if entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID); err == nil && entry.controller != nil {
				_ = entry.controller.Close()
				// Prove the ack this daemon is about to attempt really is
				// undeliverable; otherwise the assertions below pass whether or
				// not the failure path was ever entered.
				if hbErr := entry.controller.Heartbeat(event.Seq); hbErr != nil {
					mu.Lock()
					ackFailed = true
					mu.Unlock()
				}
			}
		}
		return nil
	}

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-ack-lost")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-ack-lost")
	waitFor(t, 30*time.Second, "the launched session to be adopted", func() bool {
		_, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
		return err == nil
	})
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if !entry.controller.SupportsFullHostFrames() {
		t.Fatal("the session is not on the selected-v3 rail; the cursor acknowledgement this test is about does not exist")
	}

	if !d.StopSession(id.SessionID) {
		t.Fatal("StopSession did not route to the adopted shim")
	}

	waitFor(t, 30*time.Second, "the adopted session to reach a terminal outcome", func() bool {
		return d.SessionShimOccupancy() == 0
	})

	mu.Lock()
	sawExit, sawAckFailure := closedOnExit, ackFailed
	mu.Unlock()
	if !sawExit {
		t.Fatal("the durable carrier never saw an Exit frame; this test proves nothing about the ack path")
	}
	if !sawAckFailure {
		t.Fatal("the cursor acknowledgement still succeeded; this test proves nothing about the failure path")
	}
	if q := d.QuarantinedSessions(); len(q) != 0 {
		t.Fatalf("a session whose Exit was durably persisted was quarantined instead of terminalized: %+v", q)
	}
	if got := d.ActiveSessions(); len(got) != 0 {
		t.Fatalf("ActiveSessions after the terminal outcome = %+v, want empty", got)
	}
	waitFor(t, 15*time.Second, "the terminal outcome to be reported exactly once", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return terminalReports == 1
	})
	mu.Lock()
	reports, ends := terminalReports, lifecycleEnds
	mu.Unlock()
	if reports != 1 {
		t.Fatalf("terminal outcome reported %d times, want exactly one", reports)
	}
	if ends != 1 {
		t.Fatalf("lifecycle end emitted %d times, want exactly one — the Exit this daemon durably persisted "+
			"must be honoured directly, not recovered later through quarantine", ends)
	}
}
