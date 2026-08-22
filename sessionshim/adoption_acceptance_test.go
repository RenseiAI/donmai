package sessionshim

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

// This file is the ADR-2026-08-17 acceptance suite.
//
// # Why a real second process
//
// The property under test is that a session survives the death of the process
// that was controlling it. A single-process test cannot express that: if the
// shim lived in the test binary, "the daemon restarted" and "the shim restarted"
// would be the same event, and the test would pass without the ownership move
// existing at all. So the shim runs in a genuinely separate OS process (this
// test binary re-executed in helper mode), owning a real PTY and a real harness
// child, and the "daemon" is the controller state inside the test process —
// discarded entirely and rebuilt from the on-disk registry to model a restart.
//
// # What this suite does NOT claim
//
// The ADR's first proof obligation is a real launchd/systemd smoke against the
// INSTALLED service, because a setsid-only implementation can pass ordinary
// subprocess tests and still be reaped by the service manager. That fixture is
// out of scope for a Go unit suite and belongs in the smokes repo. What is
// proven here is everything above the service-manager boundary: adoption,
// unchanged harness identity, continued output and input, sequence continuity,
// generation fencing, capacity accounting, quarantine, and terminal cleanup.

const (
	envShimHelper   = "DONMAI_TEST_SESSION_SHIM_HELPER"
	envShimDir      = "DONMAI_TEST_SESSION_SHIM_DIR"
	envShimOrg      = "DONMAI_TEST_SESSION_SHIM_ORG"
	envShimSession  = "DONMAI_TEST_SESSION_SHIM_SESSION"
	envShimWorkarea = "DONMAI_TEST_SESSION_SHIM_WORKAREA"
	envShimOrphanMS = "DONMAI_TEST_SESSION_SHIM_ORPHAN_MS"
)

// interactiveFixture is a real interactive line-oriented program: it blocks on
// terminal input and answers each line. It exercises the properties that matter
// — the harness is genuinely waiting on the PTY, and a round trip proves BOTH
// directions still work after adoption.
const interactiveFixture = `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`

func TestMain(m *testing.M) {
	if os.Getenv(envShimHelper) == "1" {
		os.Exit(runShimHelperProcess())
	}
	os.Exit(m.Run())
}

// runShimHelperProcess is the re-executed test binary acting as a standalone
// shim process. It owns the PTY and the harness and outlives every controller.
func runShimHelperProcess() int {
	reg, err := NewRegistry(os.Getenv(envShimDir))
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim helper: registry:", err)
		return 1
	}
	orphan := DefaultOrphanPolicy()
	if ms := os.Getenv(envShimOrphanMS); ms != "" {
		d, convErr := strconv.Atoi(ms)
		if convErr != nil {
			fmt.Fprintln(os.Stderr, "shim helper: orphan ms:", convErr)
			return 1
		}
		orphan = OrphanPolicy{
			Deadline:          time.Duration(d) * time.Millisecond,
			TerminationGrace:  500 * time.Millisecond,
			PropagationMargin: 0,
		}
	}
	sh, err := Start(Options{
		Identity:     Identity{OrgID: os.Getenv(envShimOrg), SessionID: os.Getenv(envShimSession)},
		Registry:     reg,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}},
		WorkareaPath: os.Getenv(envShimWorkarea),
		Orphan:       orphan,
		ProcessEpoch: 1,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "shim helper: start:", err)
		return 1
	}
	<-sh.Done()
	return 0
}

// ---- fixture plumbing ------------------------------------------------------

type shimFixture struct {
	dir      string
	registry *Registry
	id       Identity
	workarea string
	cmd      *exec.Cmd
}

// startShimHelper launches a real shim process and waits until its discovery
// record is published.
func startShimHelper(t *testing.T, id Identity, orphanMS int) *shimFixture {
	t.Helper()
	dir := shortTempDir(t)
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	workarea := dir + "/workarea"

	//nolint:gosec // G204: os.Args[0] is this test binary; the helper mode is selected by env
	cmd := exec.Command(os.Args[0], "-test.run", "TestMain")
	cmd.Env = append(os.Environ(),
		envShimHelper+"=1",
		envShimDir+"="+dir,
		envShimOrg+"="+id.OrgID,
		envShimSession+"="+id.SessionID,
		envShimWorkarea+"="+workarea,
		// The helper is a race-instrumented copy of this test binary and would
		// otherwise start a scheduler sized for every core on the machine. It has
		// three goroutines of real work (accept, PTY pump, control read), so the
		// extra Ps buy nothing and cost real CPU — which, under a full parallel
		// `go test -race ./...`, is CPU taken from every other package's
		// process-spawn deadlines. This is also a fair model of production: the
		// shim is deliberately small.
		"GOMAXPROCS=2",
	)
	if orphanMS > 0 {
		cmd.Env = append(cmd.Env, envShimOrphanMS+"="+strconv.Itoa(orphanMS))
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start shim helper: %v", err)
	}

	f := &shimFixture{dir: dir, registry: reg, id: id, workarea: workarea, cmd: cmd}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	})
	f.waitForRecord(t)
	return f
}

// waitForRecord polls until the shim has published a valid discovery record.
func (f *shimFixture) waitForRecord(t *testing.T) Record {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		rec, err := f.registry.Get(f.id)
		if err == nil {
			return rec
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("shim never published a discovery record: %v", lastErr)
	return Record{}
}

// waitForPhase polls until the shim's published record reports phase.
func (f *shimFixture) waitForPhase(t *testing.T, phase shimwire.Phase) Record {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last shimwire.Phase
	for time.Now().Before(deadline) {
		rec, err := f.registry.Get(f.id)
		if err == nil {
			last = rec.Phase
			if rec.Phase == phase {
				return rec
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("shim never reached phase %q (last seen %q)", phase, last)
	return Record{}
}

// adoptAs runs a full startup adoption pass as a named daemon generation.
func (f *shimFixture) adoptAs(t *testing.T, controllerID string) AdoptionResult {
	return f.adoptFrom(t, controllerID, 0)
}

// adoptFrom runs a full startup adoption pass with an exact durable resume
// cursor. The controller must retain that cursor because it is the only
// authoritative pre-existing last-forwarded correlation available before new
// output advances the replacement daemon's in-memory bookkeeping.
func (f *shimFixture) adoptFrom(t *testing.T, controllerID string, resumeFrom uint64) AdoptionResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := Adopt(ctx, AdoptOptions{
		Registry:     f.registry,
		ControllerID: controllerID,
		ExpectedWorkarea: func(Identity) string {
			// Workarea identity is verified at adoption, not assumed: a shim
			// running against a different workarea than this daemon believes is
			// the §D7 ambiguity, not a session to take over.
			return f.workarea
		},
		ResumeFrom: func(Identity) uint64 { return resumeFrom },
	})
	if err != nil {
		t.Fatalf("Adopt as %s: %v", controllerID, err)
	}
	t.Cleanup(res.Close)
	return res
}

// exchange writes one line to the harness and waits for its answer, returning
// the highest output sequence observed. It proves BOTH directions are live.
func exchange(t *testing.T, c *Controller, token string) uint64 {
	t.Helper()
	if err := c.WriteInput([]byte(token + "\r")); err != nil {
		t.Fatalf("WriteInput(%q): %v", token, err)
	}
	want := "ack:" + token
	var maxSeq uint64
	deadline := time.After(20 * time.Second)
	var seen strings.Builder
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				t.Fatalf("controller stream closed before %q; saw: %q", want, seen.String())
			}
			if ev.Kind != EventOutput {
				continue
			}
			if ev.Seq > maxSeq {
				maxSeq = ev.Seq
			}
			seen.Write(ev.Data)
			if strings.Contains(seen.String(), want) {
				return maxSeq
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q from the harness; saw: %q", want, seen.String())
		}
	}
}

// ---- the acceptance test ---------------------------------------------------

func TestDaemonRestartPreservesLiveInteractiveSession(t *testing.T) {
	// Deliberately not parallel: this spawns real processes and a real PTY.
	id := Identity{OrgID: "org-acceptance", SessionID: "sess-acceptance"}
	f := startShimHelper(t, id, 0)

	// ---- daemon generation 1 -------------------------------------------------
	res1 := f.adoptAs(t, "daemon-generation-1")
	if len(res1.Adopted) != 1 {
		t.Fatalf("first adoption adopted %d shims, want 1 (quarantined=%d, stale=%d)",
			len(res1.Adopted), len(res1.Quarantined), len(res1.Stale))
	}
	c1 := res1.Adopted[0]
	if c1.Identity() != id {
		t.Fatalf("adopted identity = %s, want %s", c1.Identity(), id)
	}
	if !c1.HarnessSurvived() {
		t.Fatalf("shim reports phase %q; the harness should be live", c1.Hello().Phase)
	}
	gen1 := c1.Generation()
	harness1 := c1.HarnessIdentity()
	if harness1.PID <= 0 || harness1.StartedAt == 0 {
		t.Fatalf("shim did not report a usable harness identity: %s", harness1)
	}
	if alive, err := harness1.Alive(); err != nil || !alive {
		t.Fatalf("harness %s not alive before restart: %v", harness1, err)
	}
	if !res1.Adopted[0].Adoption().Contiguous {
		t.Fatal("first adoption was not contiguous; nothing had been evicted yet")
	}
	seqBefore := exchange(t, c1, "before-restart")
	if res1.OccupiedSlots() != 1 {
		t.Fatalf("occupied slots before restart = %d, want 1", res1.OccupiedSlots())
	}

	// ---- the restart ---------------------------------------------------------
	//
	// The daemon process goes away. Every controller socket drops; the shim
	// process, the PTY, and the harness child are untouched by construction —
	// the daemon never held any of them.
	res1.Close()
	select {
	case <-c1.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("controller did not observe its own connection closing")
	}

	// ---- daemon generation 2 -------------------------------------------------
	res2 := f.adoptFrom(t, "daemon-generation-2", seqBefore+1)
	if len(res2.Adopted) != 1 {
		t.Fatalf("post-restart adoption adopted %d shims, want 1 (quarantined=%d, stale=%d)",
			len(res2.Adopted), len(res2.Quarantined), len(res2.Stale))
	}
	c2 := res2.Adopted[0]
	if c2.ResumeFrom() != seqBefore+1 {
		t.Fatalf("replacement controller resume cursor = %d, want %d", c2.ResumeFrom(), seqBefore+1)
	}

	// (a) UNCHANGED CHILD IDENTITY — the workload continued rather than being
	//     restarted underneath a reused session id. This is the assertion the
	//     whole ADR exists for.
	harness2 := c2.HarnessIdentity()
	if !harness1.Matches(harness2) {
		t.Fatalf("harness identity changed across the restart: was %s, now %s — the session did NOT survive",
			harness1, harness2)
	}

	// (b) LIFECYCLE IDENTITY is unchanged: a new shim incarnation would be a
	//     different session, and there is no second namespace.
	if c2.Identity() != id {
		t.Fatalf("post-restart identity = %s, want %s", c2.Identity(), id)
	}

	// (c) GENERATION ADVANCED — single-controller is now enforceable.
	if c2.Generation() <= gen1 {
		t.Fatalf("controller generation did not advance: was %d, now %d", gen1, c2.Generation())
	}

	// (d) SEQUENCE NEVER RESET — the shim is the sole allocator, so a restarted
	//     daemon resumes into the same namespace instead of starting a new one.
	if c2.Hello().LastSeq < seqBefore {
		t.Fatalf("shim output sequence went backwards across the restart: %d < %d",
			c2.Hello().LastSeq, seqBefore)
	}

	// (e) OUTPUT AND INPUT STILL WORK after adoption.
	seqAfter := exchange(t, c2, "after-restart")
	if seqAfter <= seqBefore {
		t.Fatalf("post-restart output sequence %d did not advance past pre-restart %d", seqAfter, seqBefore)
	}

	// (f) CAPACITY ACCOUNTING — the restarted daemon knows this slot is taken
	//     before it would advertise anything.
	if res2.OccupiedSlots() != 1 {
		t.Fatalf("occupied slots after restart = %d, want 1", res2.OccupiedSlots())
	}
	if len(res2.Quarantined) != 0 {
		t.Fatalf("a healthy shim was quarantined: %+v", res2.Quarantined)
	}

	// ---- terminal cleanup ----------------------------------------------------
	if err := c2.Stop(shimwire.StopOperator); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitForProcessExit(t, f.cmd)

	// The harness process group is really gone.
	if alive, err := harness1.Alive(); err != nil {
		t.Fatalf("check harness after stop: %v", err)
	} else if alive {
		t.Fatalf("harness %s survived the stop; the process group was not reaped", harness1)
	}

	// A durable tombstone proving the reap replaced the live record.
	tomb, err := f.registry.GetTombstone(id)
	if err != nil {
		t.Fatalf("no terminal tombstone after stop: %v", err)
	}
	if !tomb.GroupReaped {
		t.Fatal("tombstone does not record a PROVEN reap; without that it cannot release a claim")
	}
	if tomb.HarnessPID != harness1.PID || tomb.HarnessStartedAt != harness1.StartedAt {
		t.Fatalf("tombstone names harness pid=%d start=%d, want %s", tomb.HarnessPID, tomb.HarnessStartedAt, harness1)
	}
	if _, err := f.registry.Get(id); err == nil {
		t.Fatal("the live discovery record survived termination; the session would look live and terminal at once")
	}

	// (g) CAPACITY IS RELEASED — a terminal session no longer occupies a slot,
	//     but its tombstone is still reported so the lifecycle can be closed.
	res3 := f.adoptAs(t, "daemon-generation-3")
	if res3.OccupiedSlots() != 0 {
		t.Fatalf("occupied slots after termination = %d, want 0", res3.OccupiedSlots())
	}
	if len(res3.Tombstoned) != 1 {
		t.Fatalf("tombstones reported = %d, want 1", len(res3.Tombstoned))
	}

	// (h) THE TOMBSTONE IS PROOF: only now may a claim be released.
	proof := TerminalProof{Tombstone: &res3.Tombstoned[0]}
	if v := ReleaseDecision(nil, id, proof, time.Now()); v != ReleaseAllowed {
		t.Fatalf("ReleaseDecision with a proven tombstone = %q, want %q", v, ReleaseAllowed)
	}
}

func TestIncompatibleShimIsQuarantinedNotKilled(t *testing.T) {
	id := Identity{OrgID: "org-quarantine", SessionID: "sess-quarantine"}
	f := startShimHelper(t, id, 0)
	f.waitForRecord(t)

	// Learn the harness identity while the shim is still adoptable, so the
	// "still alive afterwards" assertion has something concrete to check.
	baseline := f.adoptAs(t, "daemon-baseline")
	if len(baseline.Adopted) != 1 {
		t.Fatalf("baseline adoption adopted %d, want 1", len(baseline.Adopted))
	}
	harness := baseline.Adopted[0].HarnessIdentity()
	baseline.Close()

	// Losing its controller makes the shim REPUBLISH its record to arm the orphan
	// deadline (§D8). Tampering before that lands would have the shim overwrite
	// the tampered record with its own, and the test would flake. Waiting for the
	// orphaned phase is the deterministic barrier: after it, the shim publishes
	// nothing further until an adoption it is about to be refused.
	rec := f.waitForPhase(t, shimwire.PhaseOrphaned)

	// Rewrite the record to advertise a protocol range this daemon cannot speak.
	// This is the upgrade-compatibility case: a shim from a future release.
	incompatible := rec
	incompatible.ProtocolMin = 99
	incompatible.ProtocolMax = 99
	if err := f.registry.Put(incompatible); err != nil {
		t.Fatalf("Put incompatible record: %v", err)
	}

	res := f.adoptAs(t, "daemon-incompatible")

	if len(res.Adopted) != 0 {
		t.Fatalf("adopted %d protocol-incompatible shims, want 0", len(res.Adopted))
	}
	if len(res.Quarantined) != 1 {
		t.Fatalf("quarantined %d shims, want 1 (adopted=%d, stale=%d)",
			len(res.Quarantined), len(res.Adopted), len(res.Stale))
	}
	q := res.Quarantined[0]
	if q.Reason != QuarantineProtocolMismatch {
		t.Fatalf("quarantine reason = %q, want %q", q.Reason, QuarantineProtocolMismatch)
	}
	if q.Identity() != id {
		t.Fatalf("quarantine identity = %s, want %s", q.Identity(), id)
	}
	if !q.ConsumesCapacity {
		t.Fatal("quarantined shim reported consumesCapacity=false; its harness is still running")
	}
	if q.ProtocolMin != 99 || q.ProtocolMax != 99 {
		t.Fatalf("quarantine projection lost the protocol range: [%d,%d]", q.ProtocolMin, q.ProtocolMax)
	}

	// §D7's other half: the shim is NOT killed. Killing would make the
	// compatibility path exactly as destructive as the restart it must survive.
	if f.cmd.ProcessState != nil {
		t.Fatal("the shim process exited; an incompatible shim must be quarantined, never killed")
	}
	if alive, err := harness.Alive(); err != nil {
		t.Fatalf("check harness: %v", err)
	} else if !alive {
		t.Fatalf("harness %s was killed by the quarantine path", harness)
	}

	// And it counts against capacity, so the host cannot advertise a slot that a
	// running harness already occupies.
	if res.OccupiedSlots() != 1 {
		t.Fatalf("occupied slots with one quarantined shim = %d, want 1", res.OccupiedSlots())
	}

	// The projection is deterministically ordered for host status / heartbeat.
	if got := res.QuarantinedProjection(); len(got) != 1 || !got[0].ConsumesCapacity {
		t.Fatalf("QuarantinedProjection = %+v", got)
	}
}

func TestStaleControllerIsFencedOutAfterANewerAdoption(t *testing.T) {
	id := Identity{OrgID: "org-fence", SessionID: "sess-fence"}
	f := startShimHelper(t, id, 0)

	res1 := f.adoptAs(t, "daemon-old")
	if len(res1.Adopted) != 1 {
		t.Fatalf("first adoption adopted %d, want 1", len(res1.Adopted))
	}
	old := res1.Adopted[0]
	oldGen := old.Generation()
	exchange(t, old, "old-controller-works")

	// A NEWER daemon adopts while the old controller is still holding its socket.
	// This is the split-brain scenario: two daemons believe they are in charge.
	res2 := f.adoptAs(t, "daemon-new")
	if len(res2.Adopted) != 1 {
		t.Fatalf("second adoption adopted %d, want 1", len(res2.Adopted))
	}
	fresh := res2.Adopted[0]
	if fresh.Generation() <= oldGen {
		t.Fatalf("generation did not advance: was %d, now %d", oldGen, fresh.Generation())
	}

	// (a) The old controller's SOCKET is taken away on commit. A file lock would
	//     not be enough — an old daemon can hold an open fd after losing a lock —
	//     so §D4 closes the fd itself.
	select {
	case <-old.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the superseded controller's socket was not closed; both daemons still hold the terminal")
	}

	// (b) The GENERATION fence rejects a stale mutating frame even on a live
	//     connection. This is the case an OS can actually produce: a packet from
	//     the old controller delivered AFTER the new adoption committed.
	staleToken := "stale-input-must-not-reach-the-harness"
	staleBody := shimwire.EncodeInput(oldGen, []byte(staleToken+"\r"))
	if err := fresh.w.Write(shimwire.TypeInput, staleBody); err != nil {
		t.Fatalf("write stale-generation input: %v", err)
	}

	sawStaleRefusal := false
	deadline := time.After(15 * time.Second)
	var seen strings.Builder
wait:
	for {
		select {
		case ev, ok := <-fresh.Events():
			if !ok {
				break wait
			}
			switch ev.Kind {
			case EventError:
				if ev.Err.Code == shimwire.CodeStaleGeneration {
					sawStaleRefusal = true
					break wait
				}
			case EventOutput:
				seen.Write(ev.Data)
			}
		case <-deadline:
			break wait
		}
	}
	if !sawStaleRefusal {
		t.Fatal("a stale-generation Input was not refused with stale_generation; the split-brain fence is not enforced")
	}
	if strings.Contains(seen.String(), "ack:"+staleToken) {
		t.Fatal("stale-generation input reached the harness; an old daemon still has write authority")
	}

	// (c) The CURRENT controller still works — fencing the old one did not break
	//     the new one.
	exchange(t, fresh, "new-controller-still-works")
}

func TestOrphanDeadlineReapsTheHarnessAndLeavesProof(t *testing.T) {
	// §D8: losing the controller starts a bounded, SHIM-OWNED deadline. This runs
	// with a short deadline so the bound is observable; production uses 90s.
	id := Identity{OrgID: "org-orphan", SessionID: "sess-orphan"}
	f := startShimHelper(t, id, 1500)

	res := f.adoptAs(t, "daemon-that-never-returns")
	if len(res.Adopted) != 1 {
		t.Fatalf("adopted %d, want 1", len(res.Adopted))
	}
	harness := res.Adopted[0].HarnessIdentity()
	exchange(t, res.Adopted[0], "before-orphan")

	// The controller goes away and never comes back.
	res.Close()
	waitForProcessExit(t, f.cmd)

	if alive, err := harness.Alive(); err != nil {
		t.Fatalf("check harness: %v", err)
	} else if alive {
		t.Fatalf("harness %s outlived the orphan deadline", harness)
	}

	tomb, err := f.registry.GetTombstone(id)
	if err != nil {
		t.Fatalf("orphan deadline left no tombstone: %v", err)
	}
	if !tomb.GroupReaped {
		t.Fatal("orphan tombstone does not record a proven reap")
	}

	// The critical asymmetry: the deadline reaped the harness, but the DEADLINE
	// ITSELF authorizes nothing. Only the tombstone it left behind is proof, and
	// without that proof a claim stays in reconciliation.
	if v := ReleaseDecision(nil, id, TerminalProof{}, time.Now()); v != ReleaseReconcile {
		t.Fatalf("ReleaseDecision without proof = %q, want %q — elapsed time is not proof of death", v, ReleaseReconcile)
	}
	if v := ReleaseDecision(nil, id, TerminalProof{Tombstone: &tomb}, time.Now()); v != ReleaseAllowed {
		t.Fatalf("ReleaseDecision with the shim's own tombstone = %q, want %q", v, ReleaseAllowed)
	}
}

func TestStaleRecordIsClassifiedWithoutSignallingAReusedPID(t *testing.T) {
	t.Parallel()

	// §D10: a record whose process is gone classifies STALE. Nothing is
	// signalled, because the pid may belong to an unrelated process by now — the
	// exact case where cleanup code kills the wrong target.
	reg := newTestRegistry(t)
	id := Identity{OrgID: "org-stale", SessionID: "sess-stale"}
	rec := testRecord(t, id, reg)
	// A pid that is live (our own) but whose recorded START IDENTITY does not
	// match: the shape PID reuse actually takes.
	rec.PID = os.Getpid()
	rec.ProcessStartedAt = 1 // definitely not our real start time
	if err := reg.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := Adopt(ctx, AdoptOptions{Registry: reg, ControllerID: "daemon-1"})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	t.Cleanup(res.Close)

	if len(res.Stale) != 1 {
		t.Fatalf("stale records = %d, want 1 (adopted=%d, quarantined=%d)",
			len(res.Stale), len(res.Adopted), len(res.Quarantined))
	}
	if len(res.Adopted) != 0 {
		t.Fatalf("adopted %d records whose process identity does not match", len(res.Adopted))
	}
	// A stale record's workload is over, so it must NOT hold capacity hostage.
	if res.OccupiedSlots() != 0 {
		t.Fatalf("stale record occupied %d slots, want 0", res.OccupiedSlots())
	}
	// And this process — whose pid the record named — is obviously still running.
	if os.Getpid() != rec.PID {
		t.Fatal("test precondition lost")
	}
}

func TestDuplicateIdentityQuarantinesBothClaimants(t *testing.T) {
	t.Parallel()

	// §D7: two live records claiming one lifecycle identity is an ambiguity.
	// Adopting either would be a GUESS about which is real, so both are refused
	// and both are counted.
	reg := newTestRegistry(t)
	id := Identity{OrgID: "org-dup", SessionID: "sess-dup"}
	rec := testRecord(t, id, reg)
	rec.PID = os.Getpid()
	self, err := Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	rec.ProcessStartedAt = self.StartedAt
	if err := reg.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A second record for the same identity under a different filename. This is
	// what a crash mid-rename or a botched migration leaves behind.
	second := rec
	second.ShimID = "shim-2"
	data, err := second.encode()
	if err != nil {
		t.Fatalf("encode second: %v", err)
	}
	if err := reg.publish("ffff"+id.RecordName(), data); err != nil {
		t.Fatalf("publish second: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := Adopt(ctx, AdoptOptions{Registry: reg, ControllerID: "daemon-1"})
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	t.Cleanup(res.Close)

	if len(res.Adopted) != 0 {
		t.Fatalf("adopted %d shims under an ambiguous identity, want 0", len(res.Adopted))
	}
	if len(res.Quarantined) != 2 {
		t.Fatalf("quarantined %d records, want 2 — BOTH claimants", len(res.Quarantined))
	}
	for _, q := range res.Quarantined {
		if q.Reason != QuarantineDuplicateIdentity {
			t.Errorf("quarantine reason = %q, want %q", q.Reason, QuarantineDuplicateIdentity)
		}
		if !q.ConsumesCapacity {
			t.Error("an ambiguous claimant reported consumesCapacity=false")
		}
	}
	if res.OccupiedSlots() != 2 {
		t.Fatalf("occupied slots = %d, want 2", res.OccupiedSlots())
	}
}

func TestAdoptOnAnEmptyRegistryAdvertisesNothingOccupied(t *testing.T) {
	t.Parallel()

	// The ordinary cold-start path: nothing survived, nothing is occupied, and
	// the daemon may advertise its full capacity.
	reg := newTestRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := Adopt(ctx, AdoptOptions{Registry: reg, ControllerID: "daemon-1"})
	if err != nil {
		t.Fatalf("Adopt on an empty registry: %v", err)
	}
	if res.OccupiedSlots() != 0 || len(res.Adopted) != 0 || len(res.Quarantined) != 0 {
		t.Fatalf("empty registry produced %+v", res)
	}
}

func TestAdoptRequiresARegistry(t *testing.T) {
	t.Parallel()

	if _, err := Adopt(context.Background(), AdoptOptions{}); err == nil {
		t.Fatal("Adopt without a Registry succeeded")
	}
}

// waitForProcessExit waits for the shim helper process to exit.
func waitForProcessExit(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("shim helper process did not exit")
	}
}
