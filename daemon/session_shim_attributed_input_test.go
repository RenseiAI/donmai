package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// TestSessionShimAttributedInputSystemPacedHumanImmediate is the daemon-level
// counterpart to sessionshim/attributed_input_test.go's real-wire coverage:
// Daemon.WriteAdoptedSessionShimInputAttributed carries userID through
// Controller.WriteAttributedInput to a REAL in-process-adopted (out-of-process
// harness) shim, and the last-hop pacing this whole PR is about actually
// reaches production's own entry point — the SYSTEM sentinel is paced, an
// ordinary human userId is not.
func TestSessionShimAttributedInputSystemPacedHumanImmediate(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = "host-attributed-input"
	// RequireAuthoritativeSnapshot is the actual gate ControllerOptions.RequireFullHostFrames
	// reads (cfg.RequireAuthoritativeSnapshot && d.sessionShimEnabled(), session_shim_spawn.go) —
	// enableHostedFullHostFramesForTest below wires the credential-attestation
	// machinery that satisfies sessionShimEnabled() but does not itself flip
	// this flag. It validates every one of these composing hooks is set — see
	// daemon/session_shim.go's own "needs PrepareAdoption, OnAdoption,
	// OnSessionEventDurable, OnAdoptionBatch, OnAdoptionPublished, and
	// OnCarrierActivationAcknowledged" check. newShimSpawnFixture already
	// supplies OnSessionEventDurable and enableHostedFullHostFramesForTest
	// supplies OnCarrierActivationAcknowledged; these stand-ins are the
	// minimum the rest need to let a dynamically launched (AcceptWork)
	// session actually reach a live, published carrier — none of them do
	// anything this test cares about beyond that.
	d.opts.SessionShim.PrepareAdoption = func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
		resume := preparation.LastHostSeq + 1
		return sessionshim.PreparedAdoption{
			ControllerGeneration: preparation.CurrentControllerGeneration + 1,
			Extensions:           shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "19"}},
			ResumeFrom:           &resume,
		}, nil
	}
	// The proof-bound v3+/v4 publication path requires the composing carrier
	// to (a) consume its mandatory fresh Snapshot during OnAdoption, (b)
	// observe that snapshot's exact staged sequence via OnSessionEventDurable,
	// and (c) acknowledge that same sequence in OnAdoptionPublished's receipt
	// before the candidate can activate — see
	// TestOnAdoptionCanEmitFreshSnapshotBeforeControllerPublication, which
	// this mirrors. This test has nothing to do with snapshot content; it
	// only needs a live, published carrier to write attributed input into.
	var stagedMu sync.Mutex
	var staged sessionshim.ControllerEvent
	d.opts.SessionShim.OnAdoption = func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
		if evidence.SnapshotProxy == nil {
			return SessionShimAdoptionReceipt{}, fmt.Errorf("no snapshot proxy for %s", evidence.Identity)
		}
		if _, err := evidence.SnapshotProxy.Emit(ctx); err != nil {
			return SessionShimAdoptionReceipt{}, fmt.Errorf("emit mandatory snapshot: %w", err)
		}
		return SessionShimAdoptionReceipt{}, nil
	}
	d.opts.SessionShim.OnSessionEventDurable = func(_ sessionshim.Identity, event sessionshim.ControllerEvent) error {
		if event.Kind == sessionshim.EventHostFrame && event.FrameType == attachwire.TypeSnapshot && event.RequestID != 0 {
			stagedMu.Lock()
			staged = event
			stagedMu.Unlock()
		}
		return nil
	}
	d.opts.SessionShim.OnAdoptionBatch = func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		return SessionShimAdoptionBatchReceipt{
			DurableCorrelation: []byte("revision-attributed-input"), AdoptionRevision: "revision-attributed-input",
		}, nil
	}
	d.opts.SessionShim.OnAdoptionPublished = func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
		if len(publication.Carriers) == 0 {
			return nil, nil
		}
		stagedMu.Lock()
		ackSeq := staged.Seq
		stagedMu.Unlock()
		return []SessionShimCarrierActivationReceipt{{Activation: publication.Carriers[0], AckSeq: ackSeq}}, nil
	}
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	enableHostedFullHostFramesForTest(t, d)

	id := f.identity("attributed-input-paced")
	if _, err := d.spawner.AcceptWork(f.interactiveSpec(id.SessionID)); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if !entry.controller.SupportsAttributedInput() {
		t.Fatalf("selected v%d, want attributed-input support (v4+)", entry.controller.SelectedVersion())
	}

	// Both round trips are measured in THIS run and compared to each other
	// rather than against a fixed wall-clock ceiling: an absolute "< N ms"
	// bound on the human case is exactly what flakes under -race on a loaded
	// machine, where scheduling/process jitter alone can push an ordinary
	// round trip past a tight threshold. A real last-hop delay can only make
	// the system case take AT LEAST the production ~120ms gap — never less —
	// so "human finished well before system, in the SAME run, under the SAME
	// load" is what actually distinguishes "delayed" from "not delayed", and
	// a generous absolute floor on the system case (well under the true
	// ~120ms, so it never itself flakes) confirms a real delay happened at
	// all rather than the two just landing in some order by chance.
	const (
		humanUserID = "user_01hz3k9xyz"
		sanityFloor = 40 * time.Millisecond
		pollEvery   = 5 * time.Millisecond
		waitBound   = 20 * time.Second
	)
	waitForAck := func(want string) time.Time {
		t.Helper()
		deadline := time.Now().Add(waitBound)
		for time.Now().Before(deadline) {
			out, _ := f.events.output(id)
			if strings.Contains(out, want) {
				return time.Now()
			}
			time.Sleep(pollEvery)
		}
		out, _ := f.events.output(id)
		t.Fatalf("timed out waiting for %q; saw %q", want, out)
		return time.Time{}
	}

	if err := d.WriteAdoptedSessionShimInputAttributed(id.OrgID, id.SessionID, humanUserID, []byte("human-token")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInputAttributed(human text): %v", err)
	}
	humanStart := time.Now()
	if err := d.WriteAdoptedSessionShimInputAttributed(id.OrgID, id.SessionID, humanUserID, []byte("\r")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInputAttributed(human CR): %v", err)
	}
	humanElapsed := waitForAck("ack:human-token").Sub(humanStart)

	if err := d.WriteAdoptedSessionShimInputAttributed(id.OrgID, id.SessionID, attachwire.SystemNudgeUserID, []byte("system-token")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInputAttributed(system text): %v", err)
	}
	systemStart := time.Now()
	if err := d.WriteAdoptedSessionShimInputAttributed(id.OrgID, id.SessionID, attachwire.SystemNudgeUserID, []byte("\r")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInputAttributed(system CR): %v", err)
	}
	systemElapsed := waitForAck("ack:system-token").Sub(systemStart)

	if systemElapsed < sanityFloor {
		t.Errorf("system-attributed round trip took only %v, want >= %v (the last-hop pacing gap)", systemElapsed, sanityFloor)
	}
	if humanElapsed >= systemElapsed {
		t.Errorf("human round trip (%v) was not clearly faster than the paced system round trip (%v); human input must never be delayed", humanElapsed, systemElapsed)
	}
}

// TestSessionShimAttributedInputForFallsBackBelowV4 pins the degrade rule at
// the daemon's control-ref entry point (WriteAdoptedSessionShimInputAttributedFor):
// without enableHostedFullHostFramesForTest this daemon's controllers never
// negotiate past v2 (ControllerOptions.protocolRange's released default), so
// SupportsAttributedInput is false and the write must still land, verbatim,
// via the exact byte-identical WriteInput fallback — no error, no dropped
// input, just no last-hop guarantee.
func TestSessionShimAttributedInputForFallsBackBelowV4(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	id := f.identity("attributed-input-fallback")
	if _, err := d.spawner.AcceptWork(f.interactiveSpec(id.SessionID)); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if entry.controller.SupportsAttributedInput() {
		t.Fatalf("selected v%d, want NO attributed-input support (full host frames not opted in)", entry.controller.SelectedVersion())
	}
	ref := SessionShimControlRef{
		Identity: id, ShimID: entry.shimID,
		ProcessEpoch: entry.adoption.ProcessEpoch, ControllerGeneration: entry.adoption.ControllerGeneration,
	}
	if err := d.WriteAdoptedSessionShimInputAttributedFor(ref, attachwire.SystemNudgeUserID, []byte("fallback-token\r")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInputAttributedFor (fallback): %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := f.events.output(id); strings.Contains(out, "ack:fallback-token") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, _ := f.events.output(id)
	t.Fatalf("timed out waiting for the fallback write to land; saw %q", out)
}
