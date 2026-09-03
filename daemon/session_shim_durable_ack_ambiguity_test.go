package daemon

// Provenance: durable-ack-is-ambiguity — grep a build for this marker to prove
// a slow durable side can no longer publish `socket_unreachable` for a socket
// that answered the whole time.
//
// # THE STRAND THESE TESTS UNDO
//
// Measured on a consumer daemon: a healthy interactive session, a control plane
// answering every request 200, and one slow DURABLE receipt. The shim
// connection was alive throughout; only the acknowledgement latency was high.
// The controller's own back-pressure gave up after thirty seconds, this
// daemon read that ending as the shim's, quarantined the lineage
// `socket_unreachable` with no re-adoption at all, and the control plane
// terminalized it ninety-five seconds later off that reason.
//
// ADR-2026-08-30 D2 puts "a transport-level acknowledgement without the durable
// post-condition" on the closed list of evidence that is ALWAYS ambiguous, and
// ambiguous evidence "may not unlink, tombstone, release an external claim,
// ... or emit a terminal session outcome". `socket_unreachable` is a claim
// about the socket, and ADR-2026-08-17's 2026-09-02 amendment reaches it only
// "when every [re-adoption] attempt fails" after a loss that was "the daemon
// lost its durable carrier, not the shim's socket". Neither describes a
// reachable socket whose receipts were slow.

import (
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestDelayedDurableAckKeepsTheLineageAdopted is the RED test for the field
// incident: the durable side is slow, never refusing, and the shim is alive and
// re-adoptable. The lineage must stay adopted under a strictly newer
// generation, with no quarantine and no terminal evidence anywhere.
func TestDelayedDurableAckKeepsTheLineageAdopted(t *testing.T) {
	t.Parallel()
	// The durable side answers — late. The first attempt is refused the way a
	// still-catching-up carrier refuses; the second is accepted.
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 10 * time.Millisecond}, func(attempt int) error {
		if attempt == 1 {
			return errors.New("durable acknowledgement still catching up")
		}
		return nil
	})
	d := f.daemon
	previousGeneration := f.controller.Generation()

	d.releaseShimIfLive(f.id, f.controller, shimStreamDurableAckAmbiguous)

	_, batches := f.snapshot()
	for i, batch := range batches {
		if len(batch.Quarantined) != 0 {
			t.Fatalf("batch %d quarantined a lineage whose socket answered throughout: %+v", i, batch.Quarantined)
		}
	}
	if projected := d.QuarantinedSessions(); len(projected) != 0 {
		t.Fatalf("a delayed durable acknowledgement left %d lineages quarantined: %+v", len(projected), projected)
	}
	entry, err := d.adoptedShimEntry(f.id.OrgID, f.id.SessionID)
	if err != nil {
		t.Fatalf("the lineage left the adopted set over a slow durable side: %v", err)
	}
	if entry.controller == f.controller || entry.controller.Generation() <= previousGeneration {
		t.Fatalf("adopted entry still names the lost controller (generation %d)", entry.controller.Generation())
	}
}

// TestExhaustedDurableAckAmbiguityQuarantinesUnderItsOwnReason pins the
// degrade-visibly half. When even re-adoption cannot recover the lineage the
// daemon does withdraw — but under a reason that describes what happened, and
// the quarantine-only reconciler must not treat that reason as terminal.
func TestExhaustedDurableAckAmbiguityQuarantinesUnderItsOwnReason(t *testing.T) {
	t.Parallel()
	f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 3, Backoff: 5 * time.Millisecond}, func(int) error {
		return errors.New("durable acknowledgement never landed")
	})
	d := f.daemon

	d.releaseShimIfLive(f.id, f.controller, shimStreamDurableAckAmbiguous)

	projected := d.QuarantinedSessions()
	if len(projected) != 1 {
		t.Fatalf("quarantine projection = %+v, want exactly the withdrawn lineage", projected)
	}
	if projected[0].Reason != sessionshim.QuarantineDurableAckTimeout {
		t.Fatalf("quarantine reason = %q, want %q — the socket answered throughout",
			projected[0].Reason, sessionshim.QuarantineDurableAckTimeout)
	}
	if projected[0].Reason == sessionshim.QuarantineSocketUnreachable {
		t.Fatal("a reachable socket was published as unreachable; that reason is what the control plane terminalized")
	}
	if !projected[0].ConsumesCapacity {
		t.Fatal("the withdrawn lineage stopped consuming capacity; its harness is still held")
	}
	if !projected[0].Reason.Known() {
		t.Fatalf("reason %q is outside the closed registry; a consumer branching on it would fall through", projected[0].Reason)
	}

	// NOT TERMINAL. The reconciler discharges a quarantined lineage only on a
	// group-reaped tombstone for that exact incarnation, and this shim is
	// alive, so nothing about this lineage may close.
	if published := d.reconcileQuarantinedTombstones(); len(published) != 0 {
		t.Fatalf("the reconciler terminalized a durable_ack_timeout lineage with no tombstone: %+v", published)
	}
	after := d.QuarantinedSessions()
	if len(after) != 1 || after[0].Reason != sessionshim.QuarantineDurableAckTimeout {
		t.Fatalf("quarantine projection after reconciliation = %+v, want the lineage still held under its own reason", after)
	}
}

// TestSocketFailureAndCarrierRefusalStillQuarantineSocketUnreachable is the
// control. The new reason is for one fact pattern only; every ending that
// really is about the socket keeps the disposition it had.
func TestSocketFailureAndCarrierRefusalStillQuarantineSocketUnreachable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		cause shimStreamEndCause
	}{
		{
			// The socket went away, or the shim broke the sequence contract:
			// the pre-existing disposition, quarantined at once with no re-dial.
			name:  "a real socket failure",
			cause: shimStreamEnded,
		},
		{
			// An explicit refusal from the durable carrier, re-adopted first
			// and refused every time.
			name:  "an explicit carrier refusal whose re-adoption never lands",
			cause: shimStreamCarrierLost,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newReadoptFixture(t, SessionShimReadoptionPolicy{Attempts: 2, Backoff: 5 * time.Millisecond}, func(int) error {
				return errors.New("carrier candidate dial refused")
			})
			d := f.daemon

			d.releaseShimIfLive(f.id, f.controller, tc.cause)

			projected := d.QuarantinedSessions()
			if len(projected) != 1 || projected[0].Reason != sessionshim.QuarantineSocketUnreachable {
				t.Fatalf("quarantine projection = %+v, want one socket_unreachable lineage", projected)
			}
			if !projected[0].ConsumesCapacity {
				t.Fatal("the quarantined lineage stopped consuming capacity")
			}
		})
	}
}

// TestClassifyShimStreamEndOnlyRefinesTheAmbiguitySentinel pins the seam
// between the two packages. This is the single decision that decides which
// reason a lineage is published under, and it must turn on exact evidence
// rather than on any drop the controller happened to make.
func TestClassifyShimStreamEndOnlyRefinesTheAmbiguitySentinel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cause  shimStreamEndCause
		endErr error
		want   shimStreamEndCause
	}{
		{
			name: "the peer ended the stream",
			want: shimStreamEnded,
		},
		{
			name:   "this daemon gave up with a durable acknowledgement outstanding",
			endErr: sessionshim.ErrDurableAckAmbiguityBound,
			want:   shimStreamDurableAckAmbiguous,
		},
		{
			name:   "the ambiguity sentinel arrives wrapped",
			endErr: errors.Join(errors.New("controller dropped its shim connection"), sessionshim.ErrDurableAckAmbiguityBound),
			want:   shimStreamDurableAckAmbiguous,
		},
		{
			// A consumer that genuinely stopped, with nothing outstanding to
			// prove otherwise, is not ambiguity and keeps its disposition.
			name:   "a stopped consumer exceeded the backlog budget",
			endErr: sessionshim.ErrEventBacklogExceeded,
			want:   shimStreamEnded,
		},
		{
			// A carrier loss is already the recovering disposition; the
			// refinement must never demote or re-label it.
			name:   "a carrier loss is left exactly as it was",
			cause:  shimStreamCarrierLost,
			endErr: sessionshim.ErrDurableAckAmbiguityBound,
			want:   shimStreamCarrierLost,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyShimStreamEnd(tc.cause, tc.endErr); got != tc.want {
				t.Fatalf("classifyShimStreamEnd(%d, %v) = %d, want %d", tc.cause, tc.endErr, got, tc.want)
			}
		})
	}
}
