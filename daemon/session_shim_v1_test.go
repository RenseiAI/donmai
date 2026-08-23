package daemon

import (
	"context"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func TestSelectedV1StaysLocallyAdoptedButPublishesCarrierQuarantine(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "donmai-shim-v1-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	id := sessionshim.Identity{OrgID: "org-v1-overlap", SessionID: "session-v1-overlap"}
	reg, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	socketPath := reg.SocketPath(id)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if err := os.Chmod(socketPath, sessionshim.RecordFileMode); err != nil {
		t.Fatal(err)
	}
	self, err := sessionshim.Self()
	if err != nil {
		t.Fatal(err)
	}
	record := sessionshim.Record{
		SchemaVersion: sessionshim.RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID, ShimID: "released-v1-shim", ProcessEpoch: 9,
		PID: self.PID, ProcessStartedAt: self.StartedAt, SocketPath: socketPath,
		ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V1, Phase: shimwire.PhaseRunning,
		CreatedAtUnixNano: time.Now().Add(-time.Second).UnixNano(),
	}
	if err := reg.Put(record); err != nil {
		t.Fatal(err)
	}
	var selected atomic.Uint32
	unexpected := make(chan shimwire.MessageType, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := ln.AcceptUnix()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		w, r := shimwire.NewWriter(conn), shimwire.NewReader(conn)
		hello, _ := shimwire.EncodeHello(shimwire.Hello{
			Protocol: shimwire.ProtocolName, Min: shimwire.V1, Max: shimwire.V1,
			OrgID: id.OrgID, SessionID: id.SessionID, ShimID: record.ShimID,
			ProcessEpoch: record.ProcessEpoch, PID: self.PID, ProcessStartedAt: self.StartedAt,
			HarnessPID: self.PID, HarnessStartedAt: self.StartedAt,
			Phase: shimwire.PhaseRunning, Generation: 4,
		})
		if w.Write(shimwire.TypeHello, hello) != nil {
			return
		}
		msg, readErr := r.Read()
		if readErr != nil || msg.Type != shimwire.TypeWelcome {
			return
		}
		welcome, decErr := shimwire.DecodeWelcome(msg.Body)
		if decErr != nil {
			return
		}
		selected.Store(welcome.Selected)
		adopted, _ := shimwire.EncodeAdopted(shimwire.Adopted{
			Generation: welcome.ProposedGeneration, Extensions: welcome.Extensions,
			Contiguous: true, Phase: shimwire.PhaseRunning,
		})
		if w.Write(shimwire.TypeAdopted, adopted) != nil {
			return
		}
		for {
			msg, readErr = r.Read()
			if readErr != nil {
				return
			}
			select {
			case unexpected <- msg.Type:
			default:
			}
		}
	}()

	var prepareCalls, adoptionCalls int
	var batch SessionShimAdoptionBatch
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true, RegistryDir: dir,
			ControllerID: "controller-v1-overlap", HostID: "stable-host-v1-overlap",
			RequireAuthoritativeSnapshot: true,
			RequireCredentialAttestation: true,
			AttestationCapabilities:      RequiredSessionShimHostCapabilities(),
			PrepareAdoption: func(context.Context, SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
				prepareCalls++
				return sessionshim.PreparedAdoption{}, nil
			},
			OnAdoption: func(context.Context, SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
				adoptionCalls++
				return SessionShimAdoptionReceipt{DurableCorrelation: []byte("must-not-exist")}, nil
			},
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
			PrepareAdoptionBatch: func(context.Context, string, string) ([]byte, error) {
				return []byte("expected-v1"), nil
			},
			OnAdoptionBatch: func(_ context.Context, got SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				batch = got
				return SessionShimAdoptionBatchReceipt{
					DurableCorrelation: []byte("revision-v1"), AdoptionRevision: "revision-v1",
				}, nil
			},
			OnAdoptionPublished: func(context.Context, SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
				return nil, nil
			},
		},
	})
	enableHostedFullHostFramesForTest(t, d, id.OrgID)
	t.Cleanup(d.ReleaseAdoptedSessionShims)
	if err := d.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("adoptSessionShims: %v", err)
	}
	if selected.Load() != shimwire.V1 || prepareCalls != 0 || adoptionCalls != 0 {
		t.Fatalf("selected=%d prepare=%d adoption=%d, want v1 and no carrier prepare/adoption", selected.Load(), prepareCalls, adoptionCalls)
	}
	if len(batch.Adopted) != 0 || len(batch.Quarantined) != 1 {
		t.Fatalf("batch adopted/quarantined = %d/%d, want 0/1", len(batch.Adopted), len(batch.Quarantined))
	}
	q := batch.Quarantined[0]
	if q.Reason != sessionshim.QuarantineAuthoritativeSnapshotUnsupported || !q.ConsumesCapacity ||
		q.ControllerGeneration != 5 || q.ProtocolMin != 1 || q.ProtocolMax != 1 ||
		q.Phase != shimwire.PhaseRunning || q.Detail != "" {
		t.Fatalf("v1 carrier quarantine = %+v", q)
	}
	if d.SessionShimOccupancy() != 1 || len(d.AdoptedSessionShims()) != 1 || len(d.QuarantinedSessions()) != 0 {
		t.Fatalf("local ownership projection lost: occupancy=%d adopted=%v quarantined=%v", d.SessionShimOccupancy(), d.AdoptedSessionShims(), d.QuarantinedSessions())
	}
	select {
	case mt := <-unexpected:
		t.Fatalf("selected-v1 shim received unexpected post-adoption message %s", mt)
	case <-time.After(150 * time.Millisecond):
	}
	d.ReleaseAdoptedSessionShims()
	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("v1 fake shim did not observe controller close")
	}
}

func TestSelectedV2ConservesOwnershipButRefusesFullFrameCarrier(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "donmai-shim-v2-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := sessionshim.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-v2-overlap", SessionID: "session-v2-overlap"}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		ProtocolMin: shimwire.V1, ProtocolMax: shimwire.V2,
		Spec:   ptyhost.Spec{Command: []string{"/bin/sh", "-c", "sleep 30"}},
		Orphan: sessionshim.DefaultOrphanPolicy(),
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

	var prepareCalls, adoptionCalls, activationCalls int
	var batch SessionShimAdoptionBatch
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: dir,
		ControllerID: "controller-v2-overlap", HostID: "stable-host-v2-overlap",
		RequireAuthoritativeSnapshot: true,
		RequireCredentialAttestation: true,
		AttestationCapabilities:      RequiredSessionShimHostCapabilities(),
		PrepareAdoption: func(context.Context, SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
			prepareCalls++
			return sessionshim.PreparedAdoption{}, nil
		},
		OnAdoption: func(context.Context, SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
			adoptionCalls++
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("must-not-exist")}, nil
		},
		OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
		OnAdoptionBatch: func(_ context.Context, got SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			batch = got
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("revision-v2"), AdoptionRevision: "revision-v2",
			}, nil
		},
		OnAdoptionPublished: func(context.Context, SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			activationCalls++
			return nil, nil
		},
	}})
	enableHostedFullHostFramesForTest(t, d, id.OrgID)
	t.Cleanup(d.ReleaseAdoptedSessionShims)
	if err := d.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("adoptSessionShims: %v", err)
	}
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.controller.SelectedVersion() != shimwire.V2 || prepareCalls != 0 || adoptionCalls != 0 || activationCalls != 0 {
		t.Fatalf("selected=%d prepare=%d adoption=%d activation=%d, want v2 conservation only",
			entry.controller.SelectedVersion(), prepareCalls, adoptionCalls, activationCalls)
	}
	if entry.adoption.CarrierCompatible ||
		entry.adoption.CarrierIncompatibility != SessionShimCarrierDurableHostFrameV3Required {
		t.Fatalf("selected-v2 carrier evidence = %+v", entry.adoption)
	}
	if len(batch.Adopted) != 0 || len(batch.Quarantined) != 1 ||
		batch.Quarantined[0].Reason != sessionshim.QuarantineDurableHostFrameUnsupported ||
		!batch.Quarantined[0].ConsumesCapacity {
		t.Fatalf("selected-v2 batch outcome = %+v", batch)
	}
	if d.SessionShimOccupancy() != 1 || !d.SessionShimCarrierActivationComplete() {
		t.Fatalf("selected-v2 conservation = occupancy:%d activationComplete:%v",
			d.SessionShimOccupancy(), d.SessionShimCarrierActivationComplete())
	}
	inspect, err := d.InspectAdoptedSessionShimSnapshot(context.Background(), id.OrgID, id.SessionID)
	if err != nil || inspect.InStream || len(inspect.Bytes) == 0 {
		t.Fatalf("selected-v2 snapshot authority = %+v, %v", inspect, err)
	}
}
