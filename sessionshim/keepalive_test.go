package sessionshim

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

// startKeepaliveShim starts one real shim whose orphan deadline is short enough
// that a test can watch it fire, and adopts it once so the shim has a
// controller to lose.
func startKeepaliveShim(t *testing.T, deadline time.Duration) (*Registry, *Shim, Identity, *Controller) {
	t.Helper()
	// A Unix socket path has a short platform limit and t.TempDir bakes the
	// test name into it, so keep the registry root short.
	dir, err := os.MkdirTemp("/tmp", "kal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	reg, err := NewRegistry(dir + "/registry")
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-keepalive", SessionID: "session-keepalive"}
	shim, err := Start(Options{
		Identity: id, Registry: reg, ProcessEpoch: 3,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`}},
		WorkareaPath: dir + "/workarea",
		Orphan: OrphanPolicy{
			Deadline: deadline, TerminationGrace: 200 * time.Millisecond, PropagationMargin: 0,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})
	result, err := Adopt(context.Background(), AdoptOptions{Registry: reg, ControllerID: "controller-keepalive"})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("Adopt = %+v, %v", result, err)
	}
	return reg, shim, id, result.Adopted[0]
}

// waitForPhase polls the shim's own phase until it reaches want.
func waitForPhase(t *testing.T, shim *Shim, want shimwire.Phase, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		shim.mu.Lock()
		phase := shim.phase
		shim.mu.Unlock()
		if phase == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("shim did not reach phase %q within %s", want, within)
}

// waitForPublishedOrphanDeadline polls until the shim has published its armed
// deadline. armOrphan sets the phase before it writes the record, so reading
// the record the instant the phase turns would race the write it is waiting for.
func waitForPublishedOrphanDeadline(t *testing.T, reg *Registry, id Identity, within time.Duration) Record {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		record, err := reg.Get(id)
		if err == nil && record.OrphanDeadlineUnixNano != 0 {
			return record
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the armed orphan deadline was not published to the discovery record within %s", within)
	return Record{}
}

// TestOrphanKeepaliveRearmsTheDeadlineAndOutlivesIt is the shim half of the
// §D8 2026-09-03 obligation. A shim whose controller is gone reaps its harness
// at its own deadline; a shim whose controller is gone but whose daemon is
// still visibly there does not, because each keepalive re-arms that deadline
// and republishes it.
//
// The pin is that the harness survives FOUR TIMES its own orphan deadline
// under a keepalive interval below it, and that the record's published
// deadline moves forward each time — a shim that answered the keepalive
// without re-arming would still be reaped, and one that re-armed without
// republishing would leave the daemon reading a deadline that has passed.
func TestOrphanKeepaliveRearmsTheDeadlineAndOutlivesIt(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	t.Parallel()
	// Ten keepalives of margin per deadline, and a deadline long enough in
	// absolute terms that an ordinary scheduling stall fits inside it: this
	// drives a real harness process and CI runs it beside a dozen others under
	// -race.
	const (
		deadline = 5 * time.Second
		cadence  = deadline / 10
	)
	reg, shim, id, ctrl := startKeepaliveShim(t, deadline)

	// Losing the controller arms the deadline: this is the state a keepalive
	// is defined against, and it is the only state it may act in.
	_ = ctrl.Close()
	waitForPhase(t, shim, shimwire.PhaseOrphaned, 2*time.Second)

	record := waitForPublishedOrphanDeadline(t, reg, id, 2*time.Second)
	first := record.OrphanDeadlineUnixNano

	var (
		lastArmed time.Time
		lastSent  = time.Now()
	)
	stopAt := time.Now().Add(2 * deadline)
	for time.Now().Before(stopAt) {
		// If this loop itself was descheduled for longer than the shim's own
		// deadline could absorb, the shim reaping is correct behaviour and the
		// pin can no longer distinguish it from the defect. Say so rather than
		// failing: the property is real, the machine simply stopped running the
		// test. The mutant this pin exists for dies on the FIRST keepalive,
		// long before any stall could be reached.
		if gap := time.Since(lastSent); gap > cadence+deadline/2 {
			t.Skipf("the test loop was descheduled for %s, past what a %s deadline absorbs; "+
				"this pin cannot tell a stall from a missing re-arm", gap, deadline)
		}
		lastSent = time.Now()
		armed, err := KeepAlive(context.Background(), record, KeepAliveOptions{
			ExpectedShimID: record.ShimID, ExpectedProcessEpoch: record.ProcessEpoch,
		})
		if err != nil {
			t.Fatalf("KeepAlive after %s of extension: %v", time.Until(stopAt), err)
		}
		if !armed.After(lastArmed) {
			t.Fatalf("keepalive re-armed to %s, not after the previous %s", armed, lastArmed)
		}
		lastArmed = armed
		time.Sleep(cadence)
	}

	// Two deadlines later the harness is still running and the shim is still
	// orphaned rather than exited.
	shim.mu.Lock()
	phase := shim.phase
	shim.mu.Unlock()
	if phase != shimwire.PhaseOrphaned {
		t.Fatalf("phase after %s of keepalives = %q, want the shim still orphaned and alive", 2*deadline, phase)
	}
	republished, err := reg.Get(id)
	if err != nil {
		t.Fatalf("registry.Get after keepalives: %v", err)
	}
	if republished.OrphanDeadlineUnixNano <= first {
		t.Fatalf("published orphan deadline = %d, want it moved past the first armed %d",
			republished.OrphanDeadlineUnixNano, first)
	}

	// And the fallback: stop extending and the ordinary deadline governs
	// again, unextended. Nothing had to put the shim back on its own clock.
	waitForPhase(t, shim, shimwire.PhaseExited, 4*deadline)
	waitForTombstone(t, reg, id, record, 15*time.Second)
}

// waitForTombstone polls for the shim's own terminal proof. The deadline sets
// the phase before finalizeTerminal writes the tombstone, so the two are not
// observable at the same instant.
func waitForTombstone(t *testing.T, reg *Registry, id Identity, record Record, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, lastErr = reg.GetTombstoneIncarnation(id, record.ShimID, record.ProcessEpoch); lastErr == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no terminal tombstone after the keepalives stopped: %v", lastErr)
}

// TestOrphanKeepaliveIsRefusedOutsideAnArmedDeadline pins the two states a
// keepalive must never act in: a shim with a controller attached (nothing to
// extend, and extending would make the keepalive a second liveness rule) and a
// shim whose deadline has already fired (the terminal observation is on its way
// and nothing may take it back).
func TestOrphanKeepaliveIsRefusedOutsideAnArmedDeadline(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	// Not parallel, and a deadline measured in seconds: the assertions are about
	// REFUSAL, not about timing, but the second half has to wait for a real
	// harness to be reaped, and a sub-second deadline makes that wait a race
	// with the scheduler rather than with the shim.
	const deadline = 2 * time.Second
	reg, shim, id, ctrl := startKeepaliveShim(t, deadline)
	t.Cleanup(func() { _ = ctrl.Close() })

	record, err := reg.Get(id)
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	if _, err := KeepAlive(context.Background(), record, KeepAliveOptions{}); !errors.Is(err, ErrKeepAliveRefused) {
		t.Fatalf("KeepAlive with a controller attached = %v, want ErrKeepAliveRefused", err)
	}

	_ = ctrl.Close()
	waitForPhase(t, shim, shimwire.PhaseExited, 60*time.Second)
	if _, err := KeepAlive(context.Background(), record, KeepAliveOptions{}); err == nil {
		t.Fatal("KeepAlive after the deadline fired reported success; a fired deadline may never be taken back")
	}
}

// TestOrphanKeepaliveRefusesAnotherIncarnation pins the identity check. A
// keepalive that landed on a different incarnation than the one being
// re-adopted would extend the wrong lineage's clock, which is the one way this
// mechanism could keep a harness alive that nothing is coming back for.
func TestOrphanKeepaliveRefusesAnotherIncarnation(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	t.Parallel()
	reg, shim, id, ctrl := startKeepaliveShim(t, time.Hour)
	_ = ctrl.Close()
	waitForPhase(t, shim, shimwire.PhaseOrphaned, 2*time.Second)

	record, err := reg.Get(id)
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	for _, tc := range []struct {
		name string
		opts KeepAliveOptions
	}{
		{name: "another shim id", opts: KeepAliveOptions{ExpectedShimID: record.ShimID + "-other"}},
		{name: "another process epoch", opts: KeepAliveOptions{ExpectedProcessEpoch: record.ProcessEpoch + 1}},
	} {
		if _, err := KeepAlive(context.Background(), record, tc.opts); !errors.Is(err, ErrKeepAliveRefused) {
			t.Errorf("%s: KeepAlive = %v, want ErrKeepAliveRefused", tc.name, err)
		}
	}
	if _, err := KeepAlive(context.Background(), record, KeepAliveOptions{
		ExpectedShimID: record.ShimID, ExpectedProcessEpoch: record.ProcessEpoch,
	}); err != nil {
		t.Fatalf("KeepAlive on the matching incarnation = %v, want it honoured", err)
	}
}

// TestOrphanKeepaliveOnAVanishedShimIsUnobservable pins the fallback direction:
// a shim the daemon cannot reach is not extended, and the caller learns that
// from a distinct sentinel rather than from a generic failure it might retry
// forever.
func TestOrphanKeepaliveOnAVanishedShimIsUnobservable(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	t.Parallel()
	reg, shim, id, ctrl := startKeepaliveShim(t, time.Hour)
	record, err := reg.Get(id)
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	_ = ctrl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shim.Terminate(ctx); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if err := shim.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := KeepAlive(context.Background(), record, KeepAliveOptions{}); !errors.Is(err, ErrKeepAliveUnobservable) {
		t.Fatalf("KeepAlive on a shim that is gone = %v, want ErrKeepAliveUnobservable", err)
	}
}

// TestKeepAliveOnAShimThatPredatesTheContract pins the mixed-version rollout.
//
// A shim binary built before serveOrphanKeepalive reaches its handshake's
// `expected Welcome` branch and answers CodeMalformed. The daemon must read
// that as ErrKeepAliveRefused — "this lineage is bounded by its plain orphan
// deadline after all" — and not as a transport fault it retries into, and not
// as a success that would have it believe the clock was extended.
//
// The fake speaks the wire directly rather than running an old build, which is
// the only way to have a pre-contract answer in the tree at all.
func TestKeepAliveOnAShimThatPredatesTheContract(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "kol")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := dir + "/old.sock"
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	device, inode, err := statSocket(socket)
	if err != nil {
		t.Fatal(err)
	}
	self, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         "org-old", SessionID: "session-old",
		ShimID: "0123456789abcdef0123456789abcdef", ProcessEpoch: 2,
		PID: self.PID, ProcessStartedAt: self.StartedAt,
		SocketPath: socket, SocketDevice: device, SocketInode: inode,
		ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V1,
		Phase:             shimwire.PhaseOrphaned,
		WorkareaPath:      dir + "/workarea",
		CreatedAtUnixNano: time.Now().UnixNano(),
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("fake record: %v", err)
	}
	served := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer func() { _ = conn.Close() }()
		w := shimwire.NewWriter(conn)
		r := shimwire.NewReader(conn)
		hello, err := shimwire.EncodeHello(shimwire.Hello{
			Protocol: shimwire.ProtocolName, Min: shimwire.V1, Max: shimwire.V1,
			OrgID: record.OrgID, SessionID: record.SessionID,
			ShimID: record.ShimID, ProcessEpoch: record.ProcessEpoch,
			PID: record.PID, ProcessStartedAt: record.ProcessStartedAt,
			WorkareaPath: record.WorkareaPath, Phase: shimwire.PhaseOrphaned,
		})
		if err != nil {
			served <- err
			return
		}
		if err := w.Write(shimwire.TypeHello, hello); err != nil {
			served <- err
			return
		}
		if _, err := r.Read(); err != nil {
			served <- err
			return
		}
		// Verbatim the pre-contract answer: the handshake's own refusal for a
		// frame it does not recognise where a Welcome belongs.
		served <- sendError(w, shimwire.CodeMalformed, "expected Welcome")
	}()

	_, err = KeepAlive(context.Background(), record, KeepAliveOptions{})
	if !errors.Is(err, ErrKeepAliveRefused) {
		t.Fatalf("KeepAlive against a pre-contract shim = %v, want ErrKeepAliveRefused", err)
	}
	if errors.Is(err, ErrKeepAliveUnobservable) {
		t.Fatalf("KeepAlive read a refusal as unobservability: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatalf("fake shim: %v", err)
	}
}

// TestOrdinaryHeartbeatBytesStayReadableByAStrictOldDecoder pins the reverse
// direction of the same rollout. shimwire's decoder disallows unknown fields,
// so if OrphanDeadlineAt ever appeared on an ordinary heartbeat, an old peer
// would refuse the frame outright. It is set only in the keepalive answer,
// which only a new daemon can provoke — this asserts the encoding, so a future
// author who populates the field elsewhere finds out here.
func TestOrdinaryHeartbeatBytesStayReadableByAStrictOldDecoder(t *testing.T) {
	t.Parallel()
	body, err := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{Generation: 7, AckedSeq: 42, Phase: shimwire.PhaseRunning})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); strings.Contains(got, "orphanDeadlineAt") {
		t.Fatalf("an ordinary heartbeat carries the keepalive-only field: %s", got)
	}
	if _, err := shimwire.DecodeHeartbeat(body); err != nil {
		t.Fatalf("strict decode of an ordinary heartbeat: %v", err)
	}
}

// TestOrphanKeepaliveRefusesADeadlineThatHasAlreadyFired pins the §D10 half of
// refreshOrphanDeadline's guard, in the one state the phase check beside it
// cannot cover.
//
// Between the orphan timer firing and Terminate moving the phase to exited, the
// shim is still PhaseOrphaned with a non-nil timer. A keepalive arriving in
// that window must be refused: the terminal observation is already on its way,
// and re-arming would put a liveness deadline back on a harness the shim has
// committed to reaping. Nothing may take that back.
func TestOrphanKeepaliveRefusesADeadlineThatHasAlreadyFired(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	t.Parallel()
	reg, shim, id, ctrl := startKeepaliveShim(t, time.Hour)
	_ = ctrl.Close()
	waitForPhase(t, shim, shimwire.PhaseOrphaned, 2*time.Second)
	waitForPublishedOrphanDeadline(t, reg, id, 2*time.Second)

	// A keepalive is honoured while the deadline is live.
	if _, ok := shim.refreshOrphanDeadline(); !ok {
		t.Fatal("precondition: an armed deadline refused a keepalive")
	}

	// Now stand the shim in the state the guard exists for: the timer has
	// fired, and the phase has not moved yet. Substituting an already-fired
	// timer reproduces it exactly and deterministically, which racing the real
	// deadline cannot.
	fired := make(chan struct{})
	shim.mu.Lock()
	shim.orphanTimer.Stop()
	shim.orphanTimer = time.AfterFunc(time.Millisecond, func() { close(fired) })
	shim.mu.Unlock()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("the substituted timer never fired")
	}

	if deadline, ok := shim.refreshOrphanDeadline(); ok {
		t.Fatalf("a keepalive re-armed a deadline that had already fired, to %s", deadline)
	}
	if phase := func() shimwire.Phase {
		shim.mu.Lock()
		defer shim.mu.Unlock()
		return shim.phase
	}(); phase != shimwire.PhaseOrphaned {
		t.Fatalf("precondition: phase = %q, want the shim still orphaned so the phase guard is not what refused", phase)
	}
}
