package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
)

// armAcceptanceSeam configures a valid private acceptance token file for the
// duration of one test, which is the only thing that makes the acceptance route
// — and the acceptance ring override — exist at all.
func armAcceptanceSeam(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "acc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("0123456789abcdef0123456789abcdef0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sessionShimAcceptanceTokenPathEnvironment(), path)
}

// TestAcceptanceRingIsSmallerThanTheSeamGuarantees keeps the ring budget and the
// burst that has to overflow it from drifting apart.
//
// This is the check whose absence made the whole lane vacuous: the seam's volume
// was calibrated against the daemon collapsing, never against the ring, and
// nobody could see that it missed the 8 MiB budget by two orders of magnitude.
// Deriving the bound from the seam's own constants is what makes "this burst
// evicts" a fact rather than a hope.
func TestAcceptanceRingIsSmallerThanTheSeamGuarantees(t *testing.T) {
	t.Parallel()
	// The exact payload the seam's own geometry produces, encoded by the same
	// code the PTY host rings.
	payload, err := attachwire.ResizePayload{Cols: 99, Rows: 29}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	guaranteed := acceptanceGapResizeCycles * len(payload)
	if guaranteed <= acceptanceRingBytes {
		t.Fatalf("acceptance burst produces at least %d ring bytes against a %d-byte ring: it cannot evict",
			guaranteed, acceptanceRingBytes)
	}
	if ratio := guaranteed / acceptanceRingBytes; ratio < 2 {
		t.Fatalf("acceptance burst exceeds the ring by only %dx; want margin for a smaller frame mix", ratio)
	}
}

// TestAcceptanceRingOverrideIsUnreachableWithoutTheSeam pins the constraint that
// makes a smaller ring safe to ship: it is not a default, and no production
// launch can reach it.
//
// Returning acceptanceRingBytes unconditionally turns this RED.
func TestAcceptanceRingOverrideIsUnreachableWithoutTheSeam(t *testing.T) {
	if got := acceptanceLaunchRingBytes(); got != 0 {
		t.Fatalf("ring override without an acceptance token = %d, want 0", got)
	}
	t.Setenv(sessionShimAcceptanceTokenPathEnvironment(), "/nonexistent/token")
	if got := acceptanceLaunchRingBytes(); got != 0 {
		t.Fatalf("ring override with an absent token file = %d, want 0", got)
	}
	dir, err := os.MkdirTemp("/tmp", "acc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	loose := filepath.Join(dir, "loose")
	// The permissive mode IS the negative control: a token file others can read
	// must not arm the seam.
	//nolint:gosec // G306: deliberately world-readable; the assertion below is that this is refused
	if err := os.WriteFile(loose, []byte("0123456789abcdef0123456789abcdef0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sessionShimAcceptanceTokenPathEnvironment(), loose)
	if got := acceptanceLaunchRingBytes(); got != 0 {
		t.Fatalf("ring override with a world-readable token file = %d, want 0", got)
	}

	armAcceptanceSeam(t)
	if got := acceptanceLaunchRingBytes(); got != acceptanceRingBytes {
		t.Fatalf("ring override with the seam armed = %d, want %d", got, acceptanceRingBytes)
	}
}

// TestAcceptanceRingOverrideRidesTheLaunchContract pins that the override
// reaches a worker the same way every other launch fact does, and that an
// ordinary launch's environment is unchanged byte for byte.
func TestAcceptanceRingOverrideRidesTheLaunchContract(t *testing.T) {
	ordinary := sessionshim.Launch{
		Identity:    sessionshim.Identity{OrgID: "org", SessionID: "session"},
		RegistryDir: "/tmp/registry",
		Orphan: sessionshim.OrphanPolicy{
			Deadline: 2 * time.Second, TerminationGrace: 500 * time.Millisecond,
		},
		ProcessEpoch: 1,
	}
	if _, present := ordinary.Env()[sessionshim.EnvRingBytes]; present {
		t.Fatal("an ordinary launch carried a ring override")
	}
	accepted := ordinary
	accepted.RingBytes = acceptanceRingBytes
	env := accepted.Env()
	if env[sessionshim.EnvRingBytes] == "" {
		t.Fatal("an acceptance launch did not carry its ring override")
	}
	decoded, err := sessionshim.LaunchFromEnv(func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("decode acceptance launch: %v", err)
	}
	if decoded.RingBytes != acceptanceRingBytes {
		t.Fatalf("decoded ring override = %d, want %d", decoded.RingBytes, acceptanceRingBytes)
	}
}

// TestAcceptanceSeamEvictsTheRingAndRecoversThroughAGap is the point of the
// whole seam, and it is the assertion nobody could make before.
//
// forceSessionShimAcceptanceGap exists to drive the shim-owned ring past
// eviction so the product's real recovery path is observable: exactly one
// declared Gap, its exact recovery Snapshot, and a continued sequence. Against
// the production 8 MiB budget the seam's own volume is about 50 KB — 0.6% —
// so nothing was ever evicted. The lane could not see that while the daemon's
// controller was collapsing first, and once it stopped collapsing the lane
// failed with "ring miss produced no snapshot".
//
// Raising the ring back to the production budget turns this RED at the eviction
// assertion: the resume stays a ring HIT and no Gap is ever declared.
func TestAcceptanceSeamEvictsTheRingAndRecoversThroughAGap(t *testing.T) {
	fixture := newReadoptedBurstFixture(t, 0, 0, acceptanceRingBytes,
		func(*Daemon, sessionshim.Identity, sessionshim.ControllerEvent) {})
	id := fixture.identity

	if first := fixture.shim.Session().FirstBufferedSeq(); first != 1 {
		t.Fatalf("ring already evicted before the burst: first buffered seq = %d", first)
	}
	if err := fixture.daemon.forceSessionShimAcceptanceGap(id); err != nil {
		t.Fatalf("acceptance burst: %v", err)
	}

	// The eviction itself: the position a stale resume asks for is gone.
	firstBuffered := uint64(0)
	waitFor(t, 30*time.Second, "the acceptance burst to evict the head of the ring", func() bool {
		firstBuffered = uint64(fixture.shim.Session().FirstBufferedSeq())
		return firstBuffered > 1
	})

	// Hand the session back so a fresh controller can resume from a position the
	// ring no longer holds — the same shape the restart fixture drives.
	fixture.daemon.ReleaseAdoptedSessionShims()

	registry, err := sessionshim.NewRegistry(fixture.registryDir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := sessionshim.Adopt(context.Background(), sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "ring-miss-controller",
		// The smallest position that is a real applied-position request: the ring
		// defines "from oldest" (afterSeq 0) as an unconditional hit, so a resume
		// from sequence 1 can never miss no matter how much was evicted.
		ResumeFrom: func(sessionshim.Identity) uint64 { return 2 },
	})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("stale-cursor adoption = %+v err=%v", result, err)
	}
	t.Cleanup(result.Close)
	controller := result.Adopted[0]

	// Exactly one Gap, then its exact recovery Snapshot, then the sequence
	// continues. Anything else is not the recovery path.
	var gap *sessionshim.ControllerEvent
	deadline := time.After(30 * time.Second)
	for gap == nil {
		select {
		case event, ok := <-controller.Events():
			if !ok {
				t.Fatal("controller stream ended before a Gap was declared")
			}
			if event.Kind == sessionshim.EventGap {
				captured := event
				gap = &captured
			}
		case <-deadline:
			t.Fatal("no Gap was declared for a resume the ring cannot serve")
		}
	}
	if gap.Gap.FromSeq > 2 || gap.Gap.ToSeq+1 < firstBuffered {
		t.Fatalf("declared Gap = [%d,%d], want it to cover everything the ring evicted (first buffered %d)",
			gap.Gap.FromSeq, gap.Gap.ToSeq, firstBuffered)
	}
	recovered := false
	for !recovered {
		select {
		case event, ok := <-controller.Events():
			if !ok {
				t.Fatal("controller stream ended before the recovery Snapshot")
			}
			if event.Kind == sessionshim.EventGap {
				t.Fatalf("a second Gap was declared: %+v", event.Gap)
			}
			switch {
			case event.Kind == sessionshim.EventSnapshot:
				if event.Snapshot.AtSeq < gap.Gap.ToSeq {
					t.Fatalf("recovery Snapshot atSeq = %d, want at least the gap end %d",
						event.Snapshot.AtSeq, gap.Gap.ToSeq)
				}
				recovered = true
			case event.Kind == sessionshim.EventHostFrame && event.FrameType == attachwire.TypeSnapshot:
				if event.Seq != gap.Gap.ToSeq+1 {
					t.Fatalf("recovery Snapshot seq = %d, want the exact successor of the gap %d",
						event.Seq, gap.Gap.ToSeq+1)
				}
				recovered = true
			}
		case <-deadline:
			t.Fatal("the declared Gap was never followed by a recovery Snapshot")
		}
	}
}

// TestAcceptanceRingReachesARealLaunchedShim closes the loop the other tests
// only cover in halves: a shim launched as a SEPARATE PROCESS by the daemon,
// while the acceptance seam is armed, must actually receive the smaller ring
// and actually evict under the seam's own burst.
//
// Everything here crosses the real launch contract — the daemon composes the
// environment, another process decodes it and builds its own PTY spec — so
// dropping the override anywhere along that path turns this RED at the Gap.
func TestAcceptanceRingReachesARealLaunchedShim(t *testing.T) {
	armAcceptanceSeam(t)
	f := newShimSpawnFixture(t)
	f.daemon.opts.SessionShim.Orphan.Deadline = 30 * time.Second
	spec := f.interactiveSpec("sess-acceptance-ring")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)

	if err := f.daemon.forceSessionShimAcceptanceGap(id); err != nil {
		t.Fatalf("acceptance burst through a launched shim: %v", err)
	}
	f.daemon.ReleaseAdoptedSessionShims()

	registry, err := sessionshim.NewRegistry(f.registry)
	if err != nil {
		t.Fatal(err)
	}
	result, err := sessionshim.Adopt(context.Background(), sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "launched-ring-miss",
		ResumeFrom: func(sessionshim.Identity) uint64 { return 2 },
	})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("stale-cursor adoption of a launched shim = %+v err=%v", result, err)
	}
	t.Cleanup(result.Close)

	deadline := time.After(30 * time.Second)
	for {
		select {
		case event, ok := <-result.Adopted[0].Events():
			if !ok {
				t.Fatal("controller stream ended before a Gap was declared")
			}
			if event.Kind == sessionshim.EventGap {
				if event.Gap.FromSeq > 2 || event.Gap.ToSeq < event.Gap.FromSeq {
					t.Fatalf("declared Gap = [%d,%d]", event.Gap.FromSeq, event.Gap.ToSeq)
				}
				return
			}
		case <-deadline:
			t.Fatal("a shim launched under the acceptance seam did not evict its ring")
		}
	}
}
