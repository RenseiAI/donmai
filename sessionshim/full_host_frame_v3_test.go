package sessionshim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

type inProcessV3Fixture struct {
	shim       *Shim
	controller *Controller
	result     AdoptionResult
}

func startInProcessV3Fixture(t *testing.T, ringBytes int) *inProcessV3Fixture {
	t.Helper()
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-v3", SessionID: "session-v3"}
	shim, err := Start(Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		Spec:   ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}, RingBytes: ringBytes},
		Orphan: OrphanPolicy{Deadline: 30 * time.Second, TerminationGrace: 250 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Adopt(context.Background(), AdoptOptions{
		Registry: registry, ControllerID: "controller-v3", RequireFullHostFrames: true,
	})
	if err != nil || len(result.Adopted) != 1 {
		_ = shim.Terminate(context.Background())
		t.Fatalf("Adopt = %+v, %v", result, err)
	}
	fixture := &inProcessV3Fixture{shim: shim, controller: result.Adopted[0], result: result}
	t.Cleanup(func() {
		result.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
		_ = shim.Close()
	})
	if fixture.controller.SelectedVersion() != shimwire.V3 || !fixture.controller.SupportsFullHostFrames() {
		t.Fatalf("selected/capability = v%d/%v", fixture.controller.SelectedVersion(), fixture.controller.SupportsFullHostFrames())
	}
	return fixture
}

type hostFrameCollection struct {
	frames []attachwire.Frame
	events []ControllerEvent
	err    error
}

func TestSelectedV3CarriesEveryHostFrameExactlyOnce(t *testing.T) {
	fixture := startInProcessV3Fixture(t, 0)
	direct, err := fixture.shim.Session().Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close() //nolint:errcheck
	directDone := make(chan hostFrameCollection, 1)
	go func() {
		var collected hostFrameCollection
		for frame := range direct.Frames() {
			collected.frames = append(collected.frames, frame)
			if frame.Type == attachwire.TypeExit {
				directDone <- collected
				return
			}
		}
		collected.err = errors.New("direct stream closed before Exit")
		directDone <- collected
	}()
	controllerDone := make(chan hostFrameCollection, 1)
	go func() {
		var collected hostFrameCollection
		for event := range fixture.controller.Events() {
			if event.Kind != EventHostFrame {
				if event.Kind == EventOutput || event.Kind == EventSnapshot || event.Kind == EventSnapshotFrame || event.Kind == EventExit {
					collected.err = fmt.Errorf("selected v3 emitted legacy event %s", event.Kind)
					controllerDone <- collected
					return
				}
				continue
			}
			collected.events = append(collected.events, event)
			if event.FrameType == attachwire.TypeExit {
				controllerDone <- collected
				return
			}
		}
		collected.err = errors.New("controller stream closed before Exit HostFrame")
		controllerDone <- collected
	}()

	if err := fixture.controller.WriteInput([]byte("full-host-frame\r")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.controller.Resize(101, 37, 1001, 737); err != nil {
		t.Fatal(err)
	}
	if err := fixture.shim.Session().EmitMarker("v3-marker"); err != nil {
		t.Fatal(err)
	}
	if _, inStream, err := fixture.shim.Session().EmitSnapshot(); err != nil || !inStream {
		t.Fatalf("ordinary EmitSnapshot = inStream:%v err:%v", inStream, err)
	}
	if err := fixture.controller.Stop(shimwire.StopOperator); err != nil {
		t.Fatal(err)
	}

	var directResult, controllerResult hostFrameCollection
	select {
	case directResult = <-directDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for direct host stream")
	}
	select {
	case controllerResult = <-controllerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for selected-v3 controller stream")
	}
	if directResult.err != nil || controllerResult.err != nil {
		t.Fatalf("collection errors: direct=%v controller=%v", directResult.err, controllerResult.err)
	}
	directBySeq := make(map[uint64]attachwire.Frame, len(directResult.frames))
	for _, frame := range directResult.frames {
		directBySeq[frame.Seq] = frame
	}
	seenTypes := make(map[attachwire.EventType]bool)
	seenSeq := make(map[uint64]bool)
	for _, event := range controllerResult.events {
		if seenSeq[event.Seq] {
			t.Fatalf("duplicate selected-v3 event for sequence %d", event.Seq)
		}
		seenSeq[event.Seq] = true
		seenTypes[event.FrameType] = true
		directFrame, ok := directBySeq[event.Seq]
		if !ok || !bytes.Equal(event.FrameBytes, directFrame.Encode()) {
			t.Fatalf("sequence %d raw bytes differ from PTY-host authority", event.Seq)
		}
		if event.RequestID != 0 {
			t.Fatalf("ordinary frame sequence %d carries request id %d", event.Seq, event.RequestID)
		}
	}
	if len(controllerResult.events) != len(directResult.frames) {
		t.Fatalf("selected-v3 event count = %d, exact PTY-host frame count = %d",
			len(controllerResult.events), len(directResult.frames))
	}
	for _, frame := range directResult.frames {
		if !seenSeq[frame.Seq] {
			t.Errorf("selected-v3 stream omitted PTY-host sequence %d (%s)", frame.Seq, frame.Type)
		}
	}
	for _, eventType := range []attachwire.EventType{
		attachwire.TypeOutput, attachwire.TypeResize, attachwire.TypeMarker,
		attachwire.TypeSnapshot, attachwire.TypeExit,
	} {
		if !seenTypes[eventType] {
			t.Errorf("selected-v3 stream omitted %s", eventType)
		}
	}
}

func TestSelectedV3LiveSnapshotIsOneRawEventPlusEmptyResult(t *testing.T) {
	fixture := startInProcessV3Fixture(t, 0)
	const requestID = 77
	result, err := fixture.controller.SnapshotWithID(context.Background(), requestID, shimwire.SnapshotEmit)
	if err != nil {
		t.Fatal(err)
	}
	if !result.InStream || len(result.Bytes) != 0 {
		t.Fatalf("v3 live result = inStream:%v bytes:%d, want correlation-only", result.InStream, len(result.Bytes))
	}
	var event ControllerEvent
	select {
	case event = <-fixture.controller.Events():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for requested HostFrame")
	}
	if event.Kind != EventHostFrame || event.FrameType != attachwire.TypeSnapshot ||
		event.RequestID != requestID || event.Seq != result.AtSeq+1 || len(event.FrameBytes) == 0 {
		t.Fatalf("requested HostFrame/result = event:%+v result:%+v", event, result)
	}
	retry, err := fixture.controller.SnapshotWithID(context.Background(), requestID, shimwire.SnapshotEmit)
	if err != nil || !snapshotResultsEqual(retry, result) {
		t.Fatalf("exact retry = %+v, %v", retry, err)
	}
	select {
	case duplicate := <-fixture.controller.Events():
		if duplicate.Kind == EventHostFrame && duplicate.RequestID == requestID {
			t.Fatalf("exact retry emitted a second HostFrame: %+v", duplicate)
		}
	case <-time.After(100 * time.Millisecond):
	}

	inspect, err := fixture.controller.InspectSnapshot(context.Background())
	if err != nil || inspect.InStream || len(inspect.Bytes) == 0 {
		t.Fatalf("v3 inspect changed v2 semantics: %+v, %v", inspect, err)
	}
	if err := fixture.controller.Stop(shimwire.StopOperator); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case terminal := <-fixture.controller.Events():
			if terminal.Kind == EventHostFrame && terminal.FrameType == attachwire.TypeExit {
				postExit, err := fixture.controller.EmitSnapshot(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				frame, err := attachwire.DecodeFrame(postExit.Bytes)
				if err != nil || postExit.InStream || frame.Seq != 0 || frame.RelTime != 0 || postExit.AtSeq != terminal.Seq {
					t.Fatalf("post-Exit v3 result changed v2 direct semantics: result=%+v frame=%+v err=%v", postExit, frame, err)
				}
				return
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for Exit HostFrame")
		}
	}
}

func TestSelectedV3RingMissOrdersGapThenOneRawRecoverySnapshot(t *testing.T) {
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-v3-gap", SessionID: "session-v3-gap"}
	shim, err := Start(Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		Spec:   ptyhost.Spec{Command: []string{"/bin/sh", "-c", "sleep 30"}, RingBytes: 48},
		Orphan: OrphanPolicy{Deadline: 30 * time.Second, TerminationGrace: 250 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
		_ = shim.Close()
	})
	for i := 0; i < 32; i++ {
		if err := shim.Session().EmitMarker(fmt.Sprintf("evict-%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := Adopt(context.Background(), AdoptOptions{
		Registry: registry, ControllerID: "controller-v3-gap",
		RequireFullHostFrames: true,
		ResumeFrom:            func(Identity) uint64 { return 2 },
	})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("Adopt = %+v, %v", result, err)
	}
	t.Cleanup(result.Close)
	controller := result.Adopted[0]
	var gap ControllerEvent
	select {
	case gap = <-controller.Events():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Gap")
	}
	if gap.Kind != EventGap || gap.Gap.FromSeq != 2 {
		t.Fatalf("first recovery event = %+v, want Gap from 2", gap)
	}
	var recovery ControllerEvent
	select {
	case recovery = <-controller.Events():
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for raw recovery Snapshot")
	}
	if recovery.Kind != EventHostFrame || recovery.FrameType != attachwire.TypeSnapshot ||
		recovery.RequestID != 0 || recovery.Seq != gap.Gap.ToSeq+1 {
		t.Fatalf("Gap recovery order = gap:%+v frame:%+v", gap, recovery)
	}
	frame, err := attachwire.DecodeFrame(recovery.FrameBytes)
	if err != nil || !bytes.Equal(frame.Encode(), recovery.FrameBytes) {
		t.Fatalf("recovery Snapshot bytes are not exact: frame=%+v err=%v", frame, err)
	}
	select {
	case duplicate := <-controller.Events():
		if duplicate.Kind == EventSnapshot {
			t.Fatalf("selected v3 emitted legacy recovery Snapshot: %+v", duplicate)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSelectedV3AheadOfStreamRefusesInsteadOfFabricatingGapSnapshotOrder(t *testing.T) {
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-v3-ahead", SessionID: "session-v3-ahead"}
	shim, err := Start(Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		Spec:   ptyhost.Spec{Command: []string{"/bin/sh", "-c", "sleep 30"}},
		Orphan: OrphanPolicy{Deadline: 30 * time.Second, TerminationGrace: 250 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
		_ = shim.Close()
	})
	result, err := Adopt(context.Background(), AdoptOptions{
		Registry: registry, ControllerID: "controller-v3-ahead", RequireFullHostFrames: true,
		ResumeFrom: func(Identity) uint64 { return 100 },
	})
	if err != nil {
		t.Fatalf("Adopt classification: %v", err)
	}
	t.Cleanup(result.Close)
	if len(result.Adopted) != 0 || len(result.Quarantined) != 1 ||
		result.Quarantined[0].Reason != QuarantineIdentityMismatch || !result.Quarantined[0].ConsumesCapacity {
		t.Fatalf("ahead-of-stream result = %+v", result)
	}
	if _, lastSeq, snapErr := shim.Session().Snapshot(); snapErr != nil || lastSeq != 0 {
		t.Fatalf("ahead refusal fabricated host sequence: last=%d err=%v", lastSeq, snapErr)
	}
}

func TestSelectedV3SlowControllerCannotBlockTerminalTombstone(t *testing.T) {
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-v3-stall", SessionID: "session-v3-stall"}
	const grace = 75 * time.Millisecond
	shim, err := Start(Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		// Keep the harness alive after filling the controller's receive path so
		// the test can prove the pump is stalled before allowing terminalization.
		Spec: ptyhost.Spec{Command: []string{
			"/bin/sh", "-c", "IFS= read -r _; dd if=/dev/zero bs=32768 count=128 2>/dev/null; IFS= read -r _",
		}, RingBytes: 8 << 20},
		Orphan: OrphanPolicy{Deadline: 30 * time.Second, TerminationGrace: grace},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
		_ = shim.Close()
	})
	result, err := Adopt(context.Background(), AdoptOptions{
		Registry: registry, ControllerID: "controller-v3-stall", RequireFullHostFrames: true,
	})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("Adopt = %+v, %v", result, err)
	}
	t.Cleanup(result.Close)
	controller := result.Adopted[0]
	if controller.SelectedVersion() != shimwire.V3 {
		t.Fatalf("selected version = %d, want 3", controller.SelectedVersion())
	}
	if err := controller.WriteInput([]byte("flood\r")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for len(controller.events) < cap(controller.events) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got, want := len(controller.events), cap(controller.events); got != want {
		t.Fatalf("controller event queue = %d/%d; receive path did not stall", got, want)
	}
	// Let the bounded priority queue/socket reach their slow-consumer posture,
	// then terminate the harness directly. Controller closure or backpressure may
	// not prevent the immutable terminal proof.
	time.Sleep(100 * time.Millisecond)
	started := time.Now()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := shim.Terminate(stopCtx); err != nil {
		t.Fatal(err)
	}

	for time.Since(started) < 3*time.Second {
		tombstone, tombErr := registry.GetTombstone(id)
		if tombErr == nil {
			if !tombstone.GroupReaped || tombstone.LastSeq <= uint64(cap(controller.events)) {
				t.Fatalf("terminal proof = %+v", tombstone)
			}
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("stalled controller delayed tombstone for %s", elapsed)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stalled selected-v3 controller prevented terminal tombstone persistence")
}
