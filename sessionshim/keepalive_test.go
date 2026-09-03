package sessionshim

import (
	"context"
	"errors"
	"os"
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
	const deadline = 400 * time.Millisecond
	reg, shim, id, ctrl := startKeepaliveShim(t, deadline)

	// Losing the controller arms the deadline: this is the state a keepalive
	// is defined against, and it is the only state it may act in.
	_ = ctrl.Close()
	waitForPhase(t, shim, shimwire.PhaseOrphaned, 2*time.Second)

	record := waitForPublishedOrphanDeadline(t, reg, id, 2*time.Second)
	first := record.OrphanDeadlineUnixNano

	var lastArmed time.Time
	stopAt := time.Now().Add(4 * deadline)
	for time.Now().Before(stopAt) {
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
		time.Sleep(deadline / 4)
	}

	// Four deadlines later the harness is still running and the shim is still
	// orphaned rather than exited.
	shim.mu.Lock()
	phase := shim.phase
	shim.mu.Unlock()
	if phase != shimwire.PhaseOrphaned {
		t.Fatalf("phase after %s of keepalives = %q, want the shim still orphaned and alive", 4*deadline, phase)
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
	waitForPhase(t, shim, shimwire.PhaseExited, 6*deadline)
	waitForTombstone(t, reg, id, record, 5*time.Second)
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
	t.Parallel()
	const deadline = 300 * time.Millisecond
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
	waitForPhase(t, shim, shimwire.PhaseExited, 5*deadline)
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
