package sessionshim

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

func emitAndPersistV3Ack(t *testing.T, fixture *inProcessV3Fixture, value string) uint64 {
	t.Helper()
	if err := fixture.controller.WriteInput([]byte(value + "\r")); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-fixture.controller.Events():
			if event.Kind == EventHostFrame && event.FrameType == attachwire.TypeOutput &&
				strings.Contains(string(event.Data), "ack:"+value) {
				if err := fixture.controller.Heartbeat(event.Seq); err != nil {
					t.Fatalf("persist Heartbeat(%d): %v", event.Seq, err)
				}
				return event.Seq
			}
		case <-deadline:
			t.Fatalf("timed out waiting for output %q", value)
		}
	}
}

func TestSelectedV3ColdAdoptionDefaultsToShimPersistedAckPlusOne(t *testing.T) {
	fixture := startInProcessV3Fixture(t, 0)
	acked := emitAndPersistV3Ack(t, fixture, "durable-cursor")
	record, err := fixture.shim.registry.Get(fixture.shim.id)
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := fixture.shim.registry.getDurableAck(record)
	if err != nil || sidecar.AckedSeq != acked || sidecar.ControllerGeneration != fixture.controller.Generation() {
		t.Fatalf("durable sidecar = %+v, %v", sidecar, err)
	}
	info, err := os.Stat(fixture.shim.registry.Dir() + "/" + durableAckName(
		fixture.shim.id, record.ShimID, record.ProcessEpoch,
	))
	if err != nil || info.Mode().Perm() != RecordFileMode {
		t.Fatalf("durable sidecar mode = %v, %v", info, err)
	}

	fixture.result.Close()
	select {
	case <-fixture.controller.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("first controller did not close")
	}
	var coldPreparation AdoptionPreparation
	replacement, err := Adopt(context.Background(), AdoptOptions{
		Registry: fixture.shim.registry, ControllerID: "controller-v3-cold-replacement", RequireFullHostFrames: true,
		Prepare: func(_ context.Context, evidence AdoptionPreparation) (PreparedAdoption, error) {
			coldPreparation = evidence
			return PreparedAdoption{}, nil
		},
	})
	if err != nil || len(replacement.Adopted) != 1 {
		t.Fatalf("cold replacement = %+v, %v", replacement, err)
	}
	t.Cleanup(replacement.Close)
	controller := replacement.Adopted[0]
	if controller.ResumeFrom() != acked+1 {
		t.Fatalf("cold replacement ResumeFrom = %d, want %d", controller.ResumeFrom(), acked+1)
	}
	if coldPreparation.LastForwardedSeq != acked {
		t.Fatalf("cold hosted Prepare evidence cursor = %d, want truthful persisted %d",
			coldPreparation.LastForwardedSeq, acked)
	}
	if err := fixture.shim.Session().EmitMarker("after-cold-recovery"); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-controller.Events():
		if event.Kind != EventHostFrame || event.Seq <= acked {
			t.Fatalf("cold replacement first event = %+v, want seq > %d", event, acked)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cold replacement did not receive post-ack event")
	}
}

func TestSelectedV3ExternalCursorCannotRegressShimPersistedAck(t *testing.T) {
	fixture := startInProcessV3Fixture(t, 0)
	acked := emitAndPersistV3Ack(t, fixture, "external-regression")
	fixture.result.Close()
	select {
	case <-fixture.controller.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("first controller did not close")
	}
	result, err := Adopt(context.Background(), AdoptOptions{
		Registry: fixture.shim.registry, ControllerID: "controller-v3-regressed-external",
		RequireFullHostFrames: true,
		ResumeFrom:            func(Identity) uint64 { return acked },
	})
	result.Close()
	if !errors.Is(err, ErrAdoptionPreparation) {
		t.Fatalf("regressed external cursor = %v, want ErrAdoptionPreparation", err)
	}
}

func TestSelectedV3HeartbeatRefusesRegressedAheadAndStaleCursors(t *testing.T) {
	t.Run("regressed", func(t *testing.T) {
		fixture := startInProcessV3Fixture(t, 0)
		acked := emitAndPersistV3Ack(t, fixture, "regressed-ack")
		if err := fixture.controller.Heartbeat(acked - 1); err == nil {
			t.Fatal("regressed heartbeat was accepted")
		}
		record, _ := fixture.shim.registry.Get(fixture.shim.id)
		sidecar, err := fixture.shim.registry.getDurableAck(record)
		if err != nil || sidecar.AckedSeq != acked {
			t.Fatalf("regressed heartbeat changed sidecar = %+v, %v", sidecar, err)
		}
	})
	t.Run("ahead", func(t *testing.T) {
		fixture := startInProcessV3Fixture(t, 0)
		_, lastSeq, err := fixture.shim.Session().Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.controller.Heartbeat(uint64(lastSeq) + 1); err == nil {
			t.Fatal("ahead heartbeat was accepted")
		}
		record, _ := fixture.shim.registry.Get(fixture.shim.id)
		if _, err := fixture.shim.registry.getDurableAck(record); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ahead heartbeat created sidecar: %v", err)
		}
	})
	t.Run("stale generation", func(t *testing.T) {
		fixture := startInProcessV3Fixture(t, 0)
		if err := fixture.shim.Session().EmitMarker("stale-generation-bound"); err != nil {
			t.Fatal(err)
		}
		_, lastSeq, _ := fixture.shim.Session().Snapshot()
		fixture.controller.gen--
		if err := fixture.controller.Heartbeat(uint64(lastSeq)); err == nil {
			t.Fatal("stale-generation heartbeat was accepted")
		}
		record, _ := fixture.shim.registry.Get(fixture.shim.id)
		if _, err := fixture.shim.registry.getDurableAck(record); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("stale heartbeat created sidecar: %v", err)
		}
	})
}

func TestRegistryScanIgnoresDurableAckSidecar(t *testing.T) {
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         "org-ack-scan", SessionID: "session-ack-scan", ShimID: "shim-ack-scan", ProcessEpoch: 1,
		PID: os.Getpid(), ProcessStartedAt: time.Now().UnixNano(), SocketPath: registry.SocketPath(Identity{OrgID: "org-ack-scan", SessionID: "session-ack-scan"}),
		SocketDevice: 1, SocketInode: 1, ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V3,
		Phase: shimwire.PhaseRunning, CreatedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.Put(record); err != nil {
		t.Fatal(err)
	}
	if err := registry.putDurableAck(durableAckCursor{
		SchemaVersion: durableAckSchemaVersion,
		OrgID:         record.OrgID, SessionID: record.SessionID, ShimID: record.ShimID, ProcessEpoch: record.ProcessEpoch,
		ControllerGeneration: 2, AckedSeq: 3,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := registry.Scan()
	if err != nil || len(entries) != 1 || entries[0].Err != nil || entries[0].Record != record {
		t.Fatalf("registry scan with sidecar = %+v, %v", entries, err)
	}
}

func TestSelectedV2HeartbeatRemainsWriteOnlyForReleasedBehavior(t *testing.T) {
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	shim, err := Start(Options{
		Identity: Identity{OrgID: "org-v2-heartbeat", SessionID: "session-v2-heartbeat"},
		Registry: registry, ProcessEpoch: 1, ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V2,
		Spec: ptyhost.Spec{Command: []string{"/bin/sh", "-c", "sleep 30"}}, Orphan: DefaultOrphanPolicy(),
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
	result, err := Adopt(context.Background(), AdoptOptions{Registry: registry, ControllerID: "controller-v2-heartbeat"})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("selected-v2 adoption = %+v, %v", result, err)
	}
	t.Cleanup(result.Close)
	started := time.Now()
	if err := result.Adopted[0].Heartbeat(0); err != nil || time.Since(started) > time.Second {
		t.Fatalf("selected-v2 Heartbeat changed nonblocking behavior: %v", err)
	}
}

func TestSelectedV2IgnoresCorruptDurableAckSidecar(t *testing.T) {
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-v2-corrupt-ack", SessionID: "session-v2-corrupt-ack"}
	shim, err := Start(Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V2,
		Spec: ptyhost.Spec{Command: []string{"/bin/sh", "-c", "sleep 30"}}, Orphan: DefaultOrphanPolicy(),
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
	record, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	ackPath := registry.Dir() + "/" + durableAckName(id, record.ShimID, record.ProcessEpoch)
	if err := os.WriteFile(ackPath, []byte("not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Adopt(context.Background(), AdoptOptions{Registry: registry, ControllerID: "controller-v2-corrupt-ack"})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("selected-v2 adoption with corrupt sidecar = %+v, %v", result, err)
	}
	t.Cleanup(result.Close)
	if err := result.Adopted[0].Heartbeat(9); err != nil {
		t.Fatalf("selected-v2 Heartbeat inspected corrupt sidecar: %v", err)
	}
	raw, err := os.ReadFile(ackPath)
	if err != nil || string(raw) != "not-json" {
		t.Fatalf("selected-v2 changed ignored sidecar = %q, %v", raw, err)
	}
}

func TestSelectedV3RefusesNonExactAckModesAndAheadGeneration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		globalFail bool
		mutate     func(*inProcessV3Fixture, Record, string)
	}{
		{
			name: "file mode",
			mutate: func(_ *inProcessV3Fixture, _ Record, path string) {
				if err := os.Chmod(path, 0o400); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory mode",
			mutate: func(fixture *inProcessV3Fixture, _ Record, _ string) {
				if err := os.Chmod(fixture.shim.registry.Dir(), 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(fixture.shim.registry.Dir(), RegistryDirMode) })
			},
		},
		{
			name:       "generation ahead of Hello",
			globalFail: true,
			mutate: func(fixture *inProcessV3Fixture, record Record, _ string) {
				if err := fixture.shim.registry.putDurableAck(durableAckCursor{
					SchemaVersion: durableAckSchemaVersion,
					OrgID:         record.OrgID, SessionID: record.SessionID, ShimID: record.ShimID, ProcessEpoch: record.ProcessEpoch,
					ControllerGeneration: fixture.controller.Generation() + 1, AckedSeq: 1,
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := startInProcessV3Fixture(t, 0)
			acked := emitAndPersistV3Ack(t, fixture, "refuse-"+tc.name)
			record, err := fixture.shim.registry.Get(fixture.shim.id)
			if err != nil {
				t.Fatal(err)
			}
			path := fixture.shim.registry.Dir() + "/" + durableAckName(fixture.shim.id, record.ShimID, record.ProcessEpoch)
			fixture.result.Close()
			select {
			case <-fixture.controller.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("first controller did not close")
			}
			tc.mutate(fixture, record, path)
			result, err := Adopt(context.Background(), AdoptOptions{
				Registry: fixture.shim.registry, ControllerID: "controller-v3-invalid-sidecar", RequireFullHostFrames: true,
			})
			result.Close()
			if tc.globalFail {
				if !errors.Is(err, ErrAdoptionPreparation) || len(result.Adopted) != 0 {
					t.Fatalf("selected-v3 invalid sidecar ack=%d result=%+v err=%v", acked, result, err)
				}
				return
			}
			if err != nil || len(result.Adopted) != 0 || len(result.Quarantined) != 1 {
				t.Fatalf("selected-v3 invalid sidecar ack=%d result=%+v err=%v", acked, result, err)
			}
		})
	}
}

func TestSelectedV3ProofResumeFreezesHelloTailUntilMandatorySnapshot(t *testing.T) {
	fixture := startInProcessV3Fixture(t, 0)
	acked := emitAndPersistV3Ack(t, fixture, "proof-floor")
	if err := fixture.shim.Session().EmitMarker("unforwarded-one"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.shim.Session().EmitMarker("unforwarded-two"); err != nil {
		t.Fatal(err)
	}
	_, helloTail, err := fixture.shim.Session().Snapshot()
	if err != nil || uint64(helloTail) <= acked {
		t.Fatalf("proof fixture tail = %d, ack = %d, err=%v", helloTail, acked, err)
	}
	fixture.result.Close()
	select {
	case <-fixture.controller.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("first controller did not close")
	}

	var preparation AdoptionPreparation
	replacement, err := Adopt(context.Background(), AdoptOptions{
		Registry: fixture.shim.registry, ControllerID: "controller-v3-proof-replacement", RequireFullHostFrames: true,
		Prepare: func(_ context.Context, evidence AdoptionPreparation) (PreparedAdoption, error) {
			preparation = evidence
			resume := evidence.LastHostSeq + 1
			return PreparedAdoption{
				ResumeFrom: &resume,
				Extensions: shimwire.Extensions{
					Values: map[string]string{shimwire.ExtCarrierEpoch: "41"}, Required: []string{shimwire.ExtCarrierEpoch},
				},
			}, nil
		},
	})
	if err != nil || len(replacement.Adopted) != 1 {
		t.Fatalf("proof replacement = %+v, %v", replacement, err)
	}
	t.Cleanup(replacement.Close)
	if preparation.LocalResumeFrom != acked+1 || preparation.LastForwardedSeq != acked ||
		preparation.LastHostSeq != uint64(helloTail) {
		t.Fatalf("proof preparation = %+v, want local=%d tail=%d", preparation, acked+1, helloTail)
	}
	controller := replacement.Adopted[0]
	if controller.ResumeFrom() != uint64(helloTail)+1 {
		t.Fatalf("proof ResumeFrom = %d, want %d", controller.ResumeFrom(), helloTail+1)
	}

	markerDone := make(chan error, 1)
	go func() { markerDone <- fixture.shim.Session().EmitMarker("queued-behind-proof-barrier") }()
	select {
	case err := <-markerDone:
		t.Fatalf("marker crossed Hello output barrier before mandatory Snapshot: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := controller.EmitSnapshot(ctx)
	if err != nil || !result.InStream || len(result.Bytes) != 0 || result.AtSeq != uint64(helloTail) {
		t.Fatalf("mandatory proof Snapshot = %+v, %v", result, err)
	}
	if err := <-markerDone; err != nil {
		t.Fatal(err)
	}

	var seen []ControllerEvent
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case event := <-controller.Events():
			if event.Kind == EventGap {
				t.Fatalf("proof-resolved K+1 resume emitted local replay gap: %+v", event)
			}
			if event.Kind == EventHostFrame {
				seen = append(seen, event)
			}
		case <-deadline:
			t.Fatalf("proof barrier events = %+v", seen)
		}
	}
	if seen[0].FrameType != attachwire.TypeSnapshot || seen[0].Seq != uint64(helloTail)+1 ||
		seen[1].FrameType != attachwire.TypeMarker || seen[1].Seq <= seen[0].Seq {
		t.Fatalf("proof barrier ordering = %+v", seen)
	}
}
