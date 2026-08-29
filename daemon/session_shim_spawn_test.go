package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/attachwire"
	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func proofResolvedResume(preparation SessionShimAdoptionPreparation) *uint64 {
	resume := preparation.LastHostSeq + 1
	return &resume
}

func TestOnAdoptionCanEmitFreshSnapshotBeforeControllerPublication(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = "host-takeover-snapshot"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	enableHostedFullHostFramesForTest(t, d, "test-org")
	d.opts.SessionShim.CallbackTimeout = 500 * time.Millisecond
	d.opts.SessionShim.PrepareAdoption = func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
		return sessionshim.PreparedAdoption{
			ControllerGeneration: preparation.CurrentControllerGeneration + 1,
			Extensions:           shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "19"}},
			ResumeFrom:           proofResolvedResume(preparation),
		}, nil
	}
	var emitted shimwire.SnapshotResult
	var staged sessionshim.ControllerEvent
	var stagedMu sync.Mutex
	var retainedProxy *SessionShimSnapshotProxy
	d.opts.SessionShim.OnAdoption = func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
		if evidence.SnapshotProxy == nil || !evidence.CarrierCompatible || evidence.ProtocolVersion != shimwire.V3 {
			return SessionShimAdoptionReceipt{}, fmt.Errorf("snapshot capability missing during adoption: %+v", evidence)
		}
		retainedProxy = evidence.SnapshotProxy
		var err error
		emitted, err = evidence.SnapshotProxy.Emit(ctx)
		if err != nil {
			return SessionShimAdoptionReceipt{}, err
		}
		return SessionShimAdoptionReceipt{DurableCorrelation: []byte("carrier-takeover-complete")}, nil
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
			DurableCorrelation: []byte("snapshot-batch"), AdoptionRevision: "snapshot-revision",
		}, nil
	}
	d.opts.SessionShim.OnAdoptionPublished = func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
		if _, err := d.adoptedShimEntry(f.orgID, "takeover-snapshot"); err != nil {
			return nil, fmt.Errorf("activation ran before local publication: %w", err)
		}
		stagedMu.Lock()
		ackSeq := staged.Seq
		stagedMu.Unlock()
		return []SessionShimCarrierActivationReceipt{{
			Activation: publication.Carriers[0], AckSeq: ackSeq,
		}}, nil
	}

	spec := f.interactiveSpec("takeover-snapshot")
	if _, err := d.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	stagedMu.Lock()
	stagedEvent := staged
	stagedMu.Unlock()
	frame, err := attachwire.DecodeFrame(stagedEvent.FrameBytes)
	if err != nil || frame.Type != attachwire.TypeSnapshot || !emitted.InStream || len(emitted.Bytes) != 0 ||
		stagedEvent.Seq != emitted.AtSeq+1 || stagedEvent.RequestID == 0 {
		t.Fatalf("callback emit = frame %+v result %+v err=%v", frame, emitted, err)
	}
	entry, err := d.adoptedShimEntry(f.orgID, spec.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.adoption.SnapshotProxy != nil {
		t.Fatal("ephemeral adoption SnapshotProxy was retained after synchronous callback")
	}
	if _, err := retainedProxy.Inspect(context.Background()); !errors.Is(err, shimwire.ErrVersionMismatch) {
		t.Fatalf("retained adoption proxy remained active after callback: %v", err)
	}
	fresh, err := d.InspectAdoptedSessionShimSnapshot(context.Background(), f.orgID, spec.SessionID)
	if err != nil || fresh.InStream || len(fresh.Bytes) == 0 {
		t.Fatalf("published daemon snapshot proxy = %+v, %v", fresh, err)
	}
}

func TestOnAdoptionSnapshotDoesNotReplayOrdinaryFramesBeforeActivation(t *testing.T) {
	f := newShimSpawnFixture(t)
	// Leave the first controller without a durable callback so Hello reports a
	// large live tail. Proof-bound preparation resolves ResumeFrom at that tail;
	// none of the older ordinary frames may cross the non-active candidate.
	f.daemon.opts.SessionShim.OnSessionEventDurable = nil
	spec := f.interactiveSpec("takeover-large-replay")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatal(err)
	}
	id := f.identity(spec.SessionID)
	for i := 0; i < 72; i++ {
		f.exchange(t, id, fmt.Sprintf("replay-%03d", i))
	}
	f.daemon.ReleaseAdoptedSessionShims()

	var durableCount int
	var durableSeq uint64
	var emitted shimwire.SnapshotResult
	var staged sessionshim.ControllerEvent
	var durableMu sync.Mutex
	var activationObservedForwarded uint64
	var replacement *Daemon
	replacement = New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true, RegistryDir: f.registry,
			HostID: "host-large-replay", RequireAuthoritativeSnapshot: true,
			RequireCredentialAttestation: true,
			GetCarrierProofV2Readiness:   testSessionShimProofV2Readiness,
			AttestationCapabilities:      RequiredSessionShimHostCapabilities(),
			PrepareAdoption: func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
				return sessionshim.PreparedAdoption{
					ControllerGeneration: preparation.CurrentControllerGeneration + 1,
					Extensions:           shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "23"}},
					ResumeFrom:           proofResolvedResume(preparation),
				}, nil
			},
			OnSessionEventDurable: func(_ sessionshim.Identity, event sessionshim.ControllerEvent) error {
				durableMu.Lock()
				defer durableMu.Unlock()
				if event.Kind == sessionshim.EventHostFrame {
					if event.Seq <= durableSeq {
						return fmt.Errorf("durable stream reordered: %d after %d", event.Seq, durableSeq)
					}
					frame, err := attachwire.DecodeFrame(event.FrameBytes)
					if err != nil || frame.Seq != event.Seq || !bytes.Equal(frame.Encode(), event.FrameBytes) {
						return fmt.Errorf("durable stream lost exact frame %d: %v", event.Seq, err)
					}
					durableSeq = event.Seq
					durableCount++
					if event.FrameType == attachwire.TypeSnapshot && event.RequestID != 0 {
						staged = event
					}
				}
				return nil
			},
			OnAdoption: func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
				var err error
				emitted, err = evidence.SnapshotProxy.Emit(ctx)
				if err != nil {
					return SessionShimAdoptionReceipt{}, err
				}
				durableMu.Lock()
				defer durableMu.Unlock()
				if durableCount != 1 || durableSeq != emitted.AtSeq+1 || staged.Seq != emitted.AtSeq+1 {
					return SessionShimAdoptionReceipt{}, fmt.Errorf("pre-active stream = count=%d seq=%d snapshot=%d", durableCount, durableSeq, emitted.AtSeq+1)
				}
				return SessionShimAdoptionReceipt{DurableCorrelation: []byte("large-replay-complete")}, nil
			},
			OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				return SessionShimAdoptionBatchReceipt{
					DurableCorrelation: []byte("large-replay-batch"), AdoptionRevision: "large-replay-revision",
				}, nil
			},
			OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
				activationObservedForwarded = replacement.SessionShimForwardedSeq(id.OrgID, id.SessionID)
				return []SessionShimCarrierActivationReceipt{{
					Activation: publication.Carriers[0], AckSeq: emitted.AtSeq + 1,
				}}, nil
			},
		},
	})
	enableHostedFullHostFramesForTest(t, replacement, "test-org")
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := replacement.adoptSessionShims(ctx); err != nil {
		t.Fatalf("replacement adoption with >64 replay frames: %v", err)
	}
	durableMu.Lock()
	stagedEvent := staged
	durableMu.Unlock()
	if len(emitted.Bytes) != 0 || !emitted.InStream || len(stagedEvent.FrameBytes) == 0 || stagedEvent.Seq != emitted.AtSeq+1 {
		t.Fatalf("takeover emitted snapshot = %+v", emitted)
	}
	if activationObservedForwarded >= emitted.AtSeq+1 {
		t.Fatalf("staged Snapshot advanced before carrier_active: forwarded=%d snapshot=%d", activationObservedForwarded, emitted.AtSeq+1)
	}
	if got := replacement.SessionShimForwardedSeq(id.OrgID, id.SessionID); got < emitted.AtSeq+1 {
		t.Fatalf("early durable high-water regressed at publication: got %d want >= %d", got, emitted.AtSeq+1)
	}
	fence, err := replacement.RequestSessionShimRestartFence(context.Background(), "immediate-after-takeover")
	if err != nil || len(fence.Sessions) != 1 || fence.Sessions[0].LastForwardedSeq < emitted.AtSeq+1 {
		t.Fatalf("immediate fence lost early durable high-water: fence=%+v err=%v", fence, err)
	}
}

func TestConsumedAdoptedCandidateRecoveryCompletesPublicationAndActivationWithoutSecondSnapshot(t *testing.T) {
	f := newShimSpawnFixture(t)
	f.daemon.opts.SessionShim.Orphan.Deadline = 30 * time.Second
	spec := f.interactiveSpec("consumed-candidate-recovery")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)
	f.daemon.ReleaseAdoptedSessionShims()

	const carrierEpoch = uint64(41)
	originalAdoptionReceipt := []byte("original-consumed-adoption-receipt")
	var staged sessionshim.ControllerEvent
	consuming := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: f.registry, HostID: "host-consumed-recovery",
		RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
		PrepareAdoptionV2: func(_ context.Context, preparation SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error) {
			resumeFrom := preparation.LastHostSeq + 1
			return SessionShimAdoptionPreparationResult{
				State: SessionShimPreparationFreshCandidate,
				PreparedAdoption: sessionshim.PreparedAdoption{
					ControllerGeneration: preparation.CurrentControllerGeneration + 1,
					Extensions: shimwire.Extensions{Values: map[string]string{
						shimwire.ExtCarrierEpoch: "41",
					}},
					ResumeFrom: &resumeFrom,
				},
			}, nil
		},
		OnAdoptionV2: func(ctx context.Context, evidence SessionShimAdoptionEvidenceV2) (SessionShimAdoptionReceipt, error) {
			if evidence.PreparationResult.State != SessionShimPreparationFreshCandidate || evidence.Evidence.SnapshotProxy == nil {
				return SessionShimAdoptionReceipt{}, fmt.Errorf("fresh candidate lost Snapshot authority: %+v", evidence)
			}
			if _, err := evidence.Evidence.SnapshotProxy.Emit(ctx); err != nil {
				return SessionShimAdoptionReceipt{}, err
			}
			return SessionShimAdoptionReceipt{DurableCorrelation: append([]byte(nil), originalAdoptionReceipt...)}, nil
		},
		OnSessionEventDurable: func(gotID sessionshim.Identity, event sessionshim.ControllerEvent) error {
			if gotID == id && event.Kind == sessionshim.EventHostFrame && event.FrameType == attachwire.TypeSnapshot && event.RequestID != 0 {
				staged = event
			}
			return nil
		},
		OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("consumed-batch"), AdoptionRevision: "consumed-revision",
			}, nil
		},
		OnAdoptionPublished: func(context.Context, SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			return nil, errors.New("simulated controller loss after proof and receipt consume")
		},
	}})
	enableHostedFullHostFramesForTest(t, consuming, id.OrgID)
	if err := consuming.adoptSessionShims(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "simulated controller loss") {
		t.Fatalf("consuming adoption = %v, want post-consume activation loss", err)
	}
	if staged.Seq == 0 || staged.RequestID == 0 || len(staged.FrameBytes) == 0 {
		t.Fatalf("original staged Snapshot = %+v", staged)
	}
	if got := consuming.SessionShimForwardedSeq(id.OrgID, id.SessionID); got >= staged.Seq {
		t.Fatalf("failed activation advanced original staged cursor = %d, Snapshot=%d", got, staged.Seq)
	}
	consuming.ReleaseAdoptedSessionShims()

	credential, err := attachclient.NewV2RetainedCredential([]byte("original-consumed-candidate-bearer"))
	if err != nil {
		t.Fatal(err)
	}
	recoveryCorrelation, err := NewSessionShimRecoveryCorrelation([]byte("original-consumed-recovery-correlation"))
	if err != nil {
		t.Fatal(err)
	}
	preStageAck := staged.Seq - 1
	resumeFrom := staged.Seq + 1
	recoveryResult := SessionShimAdoptionPreparationResult{
		State: SessionShimPreparationAdoptedCandidateRecovery,
		PreparedAdoption: sessionshim.PreparedAdoption{
			Extensions: shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "41"}},
			ResumeFrom: &resumeFrom,
		},
		AdoptedCandidateRecovery: &SessionShimAdoptedCandidateRecovery{
			Credential: credential, RecoveryCorrelation: recoveryCorrelation,
			CarrierEpoch: carrierEpoch, PreStageAckSeq: preStageAck,
			StagedHighWater: staged.Seq, ResumeFrom: resumeFrom,
			CredentialExpiresAt: time.Now().Add(time.Hour),
			ResumeDisposition: attachclient.V2ResumeDisposition{
				ProofSchemaVersion:   attachclient.V2ProofSchemaV2,
				Authority:            attachclient.V2ResumeAdoptedCandidateRecovery,
				State:                attachclient.V2ResumeReceiptStored,
				PTYEpoch:             1,
				CarrierEpoch:         carrierEpoch,
				AckSeq:               preStageAck,
				CandidateSnapshotSeq: staged.Seq,
				CandidateSnapshot:    append([]byte(nil), staged.FrameBytes...),
			},
		},
	}
	if preStageAck != 0 {
		recoveryResult.AdoptedCandidateRecovery.ResumeDisposition.GapFromSeq = 1
		recoveryResult.AdoptedCandidateRecovery.ResumeDisposition.GapToSeq = preStageAck
		recoveryResult.AdoptedCandidateRecovery.ResumeDisposition.GapReason = attachwirev2.GapControllerUnforwarded
	}

	var mismatchPrepareCalls, mismatchAdoptionCalls, mismatchBatchCalls, mismatchActivationCalls, mismatchDurableFrames int
	var mismatchLivePTYEpoch uint64
	var mismatchControllerGeneration shimwire.Generation
	mismatch := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: f.registry, HostID: "host-consumed-recovery",
		RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
		PrepareAdoptionV2: func(_ context.Context, preparation SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error) {
			mismatchPrepareCalls++
			mismatchLivePTYEpoch = preparation.ProcessEpoch
			mismatchControllerGeneration = preparation.CurrentControllerGeneration
			resolved := cloneSessionShimAdoptionPreparationResult(recoveryResult)
			resolved.PreparedAdoption.ControllerGeneration = preparation.CurrentControllerGeneration + 1
			resolved.AdoptedCandidateRecovery.ResumeDisposition.PTYEpoch = preparation.ProcessEpoch + 1
			return resolved, nil
		},
		OnAdoptionV2: func(context.Context, SessionShimAdoptionEvidenceV2) (SessionShimAdoptionReceipt, error) {
			mismatchAdoptionCalls++
			return SessionShimAdoptionReceipt{}, errors.New("forbidden adoption callback reached")
		},
		OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error {
			mismatchDurableFrames++
			return nil
		},
		OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			mismatchBatchCalls++
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("must-not-publish"), AdoptionRevision: "must-not-publish",
			}, nil
		},
		OnAdoptionPublished: func(context.Context, SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			mismatchActivationCalls++
			return nil, nil
		},
	}})
	enableHostedFullHostFramesForTest(t, mismatch, id.OrgID)
	mismatchErr := mismatch.adoptSessionShims(context.Background())
	if mismatchErr == nil || !strings.Contains(mismatchErr.Error(), "PTY epoch") {
		t.Fatalf("cross-PTY recovery = %v, want pre-Welcome PTY epoch refusal", mismatchErr)
	}
	if mismatchPrepareCalls != 1 || mismatchAdoptionCalls != 0 || mismatchBatchCalls != 0 ||
		mismatchActivationCalls != 0 || mismatchDurableFrames != 0 {
		t.Fatalf("cross-PTY recovery crossed prepare: prepare/adoption/batch/activation/frames = %d/%d/%d/%d/%d",
			mismatchPrepareCalls, mismatchAdoptionCalls, mismatchBatchCalls, mismatchActivationCalls, mismatchDurableFrames)
	}
	if mismatchLivePTYEpoch != 1 || mismatchControllerGeneration != 2 {
		t.Fatalf("cross-PTY authenticated live identity = PTY %d controller generation %d, want PTY 1 generation 2",
			mismatchLivePTYEpoch, mismatchControllerGeneration)
	}
	if mismatch.SessionShimAdoptionComplete() || mismatch.SessionShimCarrierActivationComplete() ||
		mismatch.SessionShimForwardedSeq(id.OrgID, id.SessionID) != 0 {
		t.Fatalf("cross-PTY recovery published state: adoption=%v activation=%v cursor=%d",
			mismatch.SessionShimAdoptionComplete(), mismatch.SessionShimCarrierActivationComplete(),
			mismatch.SessionShimForwardedSeq(id.OrgID, id.SessionID))
	}
	mismatch.ReleaseAdoptedSessionShims()

	var prepareCalls, adoptionCalls, batchCalls, activationCalls, recoveryDurableFrames int
	var validLivePTYEpoch uint64
	var validPriorControllerGeneration shimwire.Generation
	var recovery *Daemon
	recovery = New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: f.registry, HostID: "host-consumed-recovery",
		RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
		PrepareAdoptionV2: func(_ context.Context, preparation SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error) {
			prepareCalls++
			validLivePTYEpoch = preparation.ProcessEpoch
			validPriorControllerGeneration = preparation.CurrentControllerGeneration
			if preparation.ProcessEpoch != recoveryResult.AdoptedCandidateRecovery.ResumeDisposition.PTYEpoch {
				return SessionShimAdoptionPreparationResult{}, fmt.Errorf("valid recovery live PTY epoch = %d, want %d",
					preparation.ProcessEpoch, recoveryResult.AdoptedCandidateRecovery.ResumeDisposition.PTYEpoch)
			}
			resolved := cloneSessionShimAdoptionPreparationResult(recoveryResult)
			resolved.PreparedAdoption.ControllerGeneration = preparation.CurrentControllerGeneration + 1
			return resolved, nil
		},
		OnAdoptionV2: func(_ context.Context, evidence SessionShimAdoptionEvidenceV2) (SessionShimAdoptionReceipt, error) {
			adoptionCalls++
			if evidence.Evidence.SnapshotProxy != nil || evidence.PreparationResult.State != SessionShimPreparationAdoptedCandidateRecovery {
				return SessionShimAdoptionReceipt{}, fmt.Errorf("consumed recovery received new Snapshot authority: %+v", evidence)
			}
			return SessionShimAdoptionReceipt{DurableCorrelation: append([]byte(nil), originalAdoptionReceipt...)}, nil
		},
		OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error {
			recoveryDurableFrames++
			return nil
		},
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			batchCalls++
			if len(batch.Adopted) != 1 || !bytes.Equal(batch.Adopted[0].Receipt.DurableCorrelation, originalAdoptionReceipt) {
				return SessionShimAdoptionBatchReceipt{}, fmt.Errorf("recovery batch lost original adoption receipt: %+v", batch.Adopted)
			}
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("recovery-batch"), AdoptionRevision: "recovery-revision",
			}, nil
		},
		OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			activationCalls++
			if _, err := recovery.adoptedShimEntry(id.OrgID, id.SessionID); err != nil {
				return nil, fmt.Errorf("activation ran before current adopted entry publication: %w", err)
			}
			return []SessionShimCarrierActivationReceipt{{
				Activation: publication.Carriers[0], AckSeq: staged.Seq,
			}}, nil
		},
	}})
	enableHostedFullHostFramesForTest(t, recovery, id.OrgID)
	t.Cleanup(recovery.ReleaseAdoptedSessionShims)
	if err := recovery.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("consumed-candidate recovery: %v", err)
	}
	if prepareCalls != 1 || adoptionCalls != 1 || batchCalls != 1 || activationCalls != 1 {
		t.Fatalf("recovery lifecycle calls prepare/adoption/batch/activation = %d/%d/%d/%d", prepareCalls, adoptionCalls, batchCalls, activationCalls)
	}
	if recoveryDurableFrames != 0 {
		t.Fatalf("consumed recovery emitted %d new durable frames, want zero", recoveryDurableFrames)
	}
	if got := recovery.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != staged.Seq {
		t.Fatalf("recovery forwarded cursor = %d, want original staged high-water %d", got, staged.Seq)
	}
	if !recovery.SessionShimAdoptionComplete() || !recovery.SessionShimCarrierActivationComplete() {
		t.Fatalf("recovery completion = adoption:%v activation:%v", recovery.SessionShimAdoptionComplete(), recovery.SessionShimCarrierActivationComplete())
	}
	entry, err := recovery.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if validLivePTYEpoch != mismatchLivePTYEpoch || validPriorControllerGeneration != mismatchControllerGeneration ||
		entry.controller.Generation() != validPriorControllerGeneration+1 {
		t.Fatalf("valid recovery identity/generation = PTY %d prior %d adopted %d; mismatch observed PTY %d prior %d",
			validLivePTYEpoch, validPriorControllerGeneration, entry.controller.Generation(),
			mismatchLivePTYEpoch, mismatchControllerGeneration)
	}
}

func TestConsumedRecoveryHeartbeatReleasesBlockedV3ProgressAfterCarrierActive(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "donmai-consumed-barrier-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := sessionshim.NewRegistry(filepath.Join(dir, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	id := sessionshim.Identity{OrgID: "org-consumed-barrier", SessionID: "session-consumed-barrier"}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 9,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", `stty -echo; while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`}},
		WorkareaPath: filepath.Join(dir, "workarea"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})
	if err := shim.Session().EmitMarker("pre-stage-boundary"); err != nil {
		t.Fatal(err)
	}
	first, err := sessionshim.Adopt(context.Background(), sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "consuming-controller", RequireFullHostFrames: true,
	})
	if err != nil || len(first.Adopted) != 1 {
		t.Fatalf("first adoption = %+v err=%v", first, err)
	}
	firstController := first.Adopted[0]
	var preStageAck uint64
	select {
	case event := <-firstController.Events():
		if event.Kind != sessionshim.EventHostFrame || event.FrameType != attachwire.TypeMarker || event.Seq == 0 {
			t.Fatalf("pre-stage frame = %+v", event)
		}
		preStageAck = event.Seq
	case <-time.After(5 * time.Second):
		t.Fatal("first controller did not receive pre-stage Marker")
	}
	if err := firstController.Heartbeat(preStageAck); err != nil {
		t.Fatalf("persist pre-stage ACK: %v", err)
	}
	retainedSnapshot, inStream, err := shim.Session().EmitSnapshot()
	if err != nil || !inStream || retainedSnapshot.Seq != preStageAck+1 {
		t.Fatalf("retained Snapshot = %+v inStream:%v err:%v", retainedSnapshot, inStream, err)
	}
	highWater := retainedSnapshot.Seq
	select {
	case event := <-firstController.Events():
		if event.Kind != sessionshim.EventHostFrame || event.FrameType != attachwire.TypeSnapshot ||
			event.Seq != highWater || !bytes.Equal(event.FrameBytes, retainedSnapshot.Encode()) {
			t.Fatalf("retained Snapshot event = %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first controller did not receive retained Snapshot")
	}
	first.Close()

	readAck := func() uint64 {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(dir, "registry"))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".ack") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, "registry", entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var ack struct {
				AckedSeq uint64 `json:"ackedSeq"`
			}
			if err := json.Unmarshal(raw, &ack); err != nil {
				t.Fatal(err)
			}
			return ack.AckedSeq
		}
		t.Fatal("durable ACK sidecar is missing")
		return 0
	}
	if readAck() != preStageAck {
		t.Fatalf("retained Snapshot was acknowledged before activation: %d", readAck())
	}

	type emitAttemptResult struct {
		name string
		err  error
	}
	cloneEvent := func(event sessionshim.ControllerEvent) sessionshim.ControllerEvent {
		event.Data = append([]byte(nil), event.Data...)
		event.FrameBytes = append([]byte(nil), event.FrameBytes...)
		return event
	}
	var (
		externalMu     sync.Mutex
		durableFrames  []sessionshim.ControllerEvent
		observedFrames []sessionshim.ControllerEvent
		recovery       *Daemon
		attemptOnce    sync.Once
		attempts       = make(chan emitAttemptResult, 3)
		resizeDone     = make(chan struct{})
		outputWritten  = make(chan struct{})
	)
	credential, err := attachclient.NewV2RetainedCredential([]byte("consumed-barrier-bearer"))
	if err != nil {
		t.Fatal(err)
	}
	recoveryCorrelation, err := NewSessionShimRecoveryCorrelation([]byte("consumed-barrier-correlation"))
	if err != nil {
		t.Fatal(err)
	}
	resumeFrom := highWater + 1
	recovery = New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: filepath.Join(dir, "registry"), HostID: "host-consumed-barrier",
		RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
		PrepareAdoptionV2: func(_ context.Context, preparation SessionShimAdoptionPreparation) (SessionShimAdoptionPreparationResult, error) {
			if preparation.ProcessEpoch != 9 || preparation.LastHostSeq != highWater {
				return SessionShimAdoptionPreparationResult{}, fmt.Errorf("recovery preparation = %+v", preparation)
			}
			attemptOnce.Do(func() {
				go func() {
					err := shim.Session().Resize(117, 43, 0, 0)
					attempts <- emitAttemptResult{name: "resize", err: err}
					close(resizeDone)
				}()
				go func() {
					<-resizeDone
					_, err := shim.Session().WriteInput([]byte("blocked-output\n"))
					attempts <- emitAttemptResult{name: "output", err: err}
					close(outputWritten)
				}()
				go func() {
					<-outputWritten
					deadline := time.Now().Add(5 * time.Second)
					for time.Now().Before(deadline) {
						_, last, snapshotErr := shim.Session().Snapshot()
						if snapshotErr != nil {
							attempts <- emitAttemptResult{name: "marker", err: snapshotErr}
							return
						}
						if uint64(last) >= highWater+2 {
							attempts <- emitAttemptResult{name: "marker", err: shim.Session().EmitMarker("blocked-marker")}
							return
						}
						time.Sleep(time.Millisecond)
					}
					attempts <- emitAttemptResult{name: "marker", err: errors.New("output did not allocate before marker deadline")}
				}()
			})
			return SessionShimAdoptionPreparationResult{
				State: SessionShimPreparationAdoptedCandidateRecovery,
				PreparedAdoption: sessionshim.PreparedAdoption{
					ControllerGeneration: preparation.CurrentControllerGeneration + 1,
					Extensions:           shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "61"}},
					ResumeFrom:           &resumeFrom,
				},
				AdoptedCandidateRecovery: &SessionShimAdoptedCandidateRecovery{
					Credential: credential, RecoveryCorrelation: recoveryCorrelation,
					CarrierEpoch: 61, PreStageAckSeq: preStageAck,
					StagedHighWater: highWater, ResumeFrom: resumeFrom,
					CredentialExpiresAt: time.Now().Add(time.Hour),
					ResumeDisposition: attachclient.V2ResumeDisposition{
						ProofSchemaVersion: attachclient.V2ProofSchemaV2,
						Authority:          attachclient.V2ResumeAdoptedCandidateRecovery,
						State:              attachclient.V2ResumeReceiptStored,
						PTYEpoch:           9, CarrierEpoch: 61, AckSeq: preStageAck,
						CandidateSnapshotSeq: highWater, CandidateSnapshot: retainedSnapshot.Encode(),
					},
				},
			}, nil
		},
		OnAdoptionV2: func(context.Context, SessionShimAdoptionEvidenceV2) (SessionShimAdoptionReceipt, error) {
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("consumed-barrier-adoption")}, nil
		},
		OnSessionEvent: func(_ sessionshim.Identity, event sessionshim.ControllerEvent) {
			externalMu.Lock()
			observedFrames = append(observedFrames, cloneEvent(event))
			externalMu.Unlock()
		},
		OnSessionEventDurable: func(_ sessionshim.Identity, event sessionshim.ControllerEvent) error {
			externalMu.Lock()
			durableFrames = append(durableFrames, cloneEvent(event))
			externalMu.Unlock()
			return nil
		},
		OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("consumed-barrier-batch"), AdoptionRevision: "consumed-barrier-revision",
			}, nil
		},
		OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			time.Sleep(100 * time.Millisecond)
			externalMu.Lock()
			externalCount := len(durableFrames) + len(observedFrames)
			externalMu.Unlock()
			_, lastSeq, snapshotErr := shim.Session().Snapshot()
			if snapshotErr != nil {
				return nil, snapshotErr
			}
			if len(attempts) != 0 || uint64(lastSeq) != highWater || externalCount != 0 ||
				recovery.SessionShimForwardedSeq(id.OrgID, id.SessionID) != highWater || readAck() != preStageAck {
				return nil, fmt.Errorf("pre-active effects = completed:%d last:%d external:%d forwarded:%d ack:%d",
					len(attempts), lastSeq, externalCount, recovery.SessionShimForwardedSeq(id.OrgID, id.SessionID), readAck())
			}
			return []SessionShimCarrierActivationReceipt{{Activation: publication.Carriers[0], AckSeq: highWater}}, nil
		},
	}})
	enableHostedFullHostFramesForTest(t, recovery, id.OrgID)
	t.Cleanup(recovery.ReleaseAdoptedSessionShims)
	if err := recovery.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("consumed recovery activation: %v", err)
	}
	completed := make(map[string]error, 3)
	for len(completed) < 3 {
		select {
		case result := <-attempts:
			completed[result.name] = result.err
		case <-time.After(5 * time.Second):
			t.Fatalf("blocked attempts completed = %+v", completed)
		}
	}
	for name, err := range completed {
		if err != nil {
			t.Fatalf("%s after activation: %v", name, err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		externalMu.Lock()
		complete := len(durableFrames) >= 3 && durableFrames[len(durableFrames)-1].FrameType == attachwire.TypeMarker
		externalMu.Unlock()
		if complete {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	externalMu.Lock()
	durable := append([]sessionshim.ControllerEvent(nil), durableFrames...)
	observed := append([]sessionshim.ControllerEvent(nil), observedFrames...)
	externalMu.Unlock()
	if len(durable) < 3 || len(observed) != len(durable) || durable[0].FrameType != attachwire.TypeResize ||
		durable[len(durable)-1].FrameType != attachwire.TypeMarker {
		t.Fatalf("post-active frame order = durable:%+v observed:%+v", durable, observed)
	}
	sawOutput := false
	for i := range durable {
		if durable[i].Seq != highWater+1+uint64(i) || !bytes.Equal(durable[i].FrameBytes, observed[i].FrameBytes) {
			t.Fatalf("post-active frame %d changed: durable=%+v observed=%+v", i, durable[i], observed[i])
		}
		if durable[i].FrameType == attachwire.TypeSnapshot || durable[i].FrameType == attachwire.TypeExit {
			t.Fatalf("forbidden second terminal frame: %+v", durable[i])
		}
		if durable[i].FrameType == attachwire.TypeOutput && bytes.Contains(durable[i].Data, []byte("ack:blocked-output")) {
			sawOutput = true
		}
	}
	if !sawOutput {
		t.Fatalf("post-active frames omitted PTY Output: %+v", durable)
	}
	wantFinal := highWater + uint64(len(durable))
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if recovery.SessionShimForwardedSeq(id.OrgID, id.SessionID) == wantFinal && readAck() == wantFinal {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := recovery.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != wantFinal || readAck() != wantFinal {
		t.Fatalf("post-active cursors = forwarded:%d ack:%d want:%d", got, readAck(), wantFinal)
	}
}

// This file is the daemon-side half of the ADR-2026-08-17 acceptance suite.
//
// # Why a real second process
//
// The claim under test is a PRODUCTION claim: the daemon's interactive spawn
// path launches a per-session shim, hands it the launch contract, adopts it over
// shimwire, and then drives stop/input/output and terminal cleanup through that
// connection. A fake in-process launcher would prove the plumbing compiles and
// nothing about the ownership move, because the shim and the daemon would share
// a lifetime by construction — the exact coupling §D1 exists to remove.
//
// So the shim here is a genuinely separate OS process: this test binary
// re-executed in helper mode, reading the SAME launch environment the daemon
// composes for a real worker, owning a real PTY and a real harness child.
//
// # What this suite does NOT claim
//
// It does not claim the ADR's first proof obligation — the real launchd/systemd
// smoke against the INSTALLED service. A setsid-only implementation can pass
// every subprocess test here and still be reaped by a service manager; that
// fixture belongs to the smokes repo. What is proven here is everything above
// the service-manager boundary.

const envDaemonShimHelper = "DONMAI_TEST_DAEMON_SESSION_SHIM_HELPER"

// interactiveFixture is a real line-oriented interactive program: it blocks on
// terminal input and answers each line, so a round trip proves BOTH directions
// are live through the adopted connection.
const interactiveFixture = `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`

// TestMain routes this binary into shim-helper mode when the daemon's own launch
// contract is present in the environment. The daemon composes that environment;
// the helper consumes it exactly as a real worker's ptycli driver does.
func TestMain(m *testing.M) {
	if os.Getenv(envDaemonShimHelper) == "1" {
		os.Exit(runDaemonShimHelper())
	}
	os.Exit(m.Run())
}

func runDaemonShimHelper() int {
	launch, err := sessionshim.LaunchFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon shim helper: launch env:", err)
		return 1
	}
	// The worker resolves the same <parent>/<sessionID> leaf the daemon
	// publishes, which is what makes the adoption-time workarea comparison a real
	// check rather than a value compared against itself (§D7).
	workarea := filepath.Join(os.Getenv("DONMAI_TEST_DAEMON_SESSION_SHIM_WORKAREA_PARENT"), launch.Identity.SessionID)
	shim, err := sessionshim.StartFromEnv(launch,
		ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}}, workarea)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon shim helper: start:", err)
		return 1
	}
	<-shim.Done()
	return 0
}

// shimSpawnFixture is a daemon configured to launch interactive sessions through
// a real shim process.
type shimSpawnFixture struct {
	daemon         *Daemon
	registry       string
	workareaParent string
	orgID          string
	events         *shimEventRecorder
}

// shimEventRecorder is the composing carrier's seat: it receives exactly what
// the daemon forwards from each adopted session. Reading output through this
// hook rather than off the controller's channel is not a convenience — the
// daemon's own consumer is the sole reader of that channel, and a test that
// raced it would be proving something no production consumer could rely on.
type shimEventRecorder struct {
	mu   sync.Mutex
	seen map[string]*strings.Builder
	seq  map[string]uint64
	gaps map[string]int
}

type exactFenceRecorder struct {
	mu       sync.Mutex
	requests []sessionshim.FenceRequest
	failOrg  string
}

func (r *exactFenceRecorder) AcknowledgeExact(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
	if len(request.Fence.Sessions) > 0 && request.Fence.Sessions[0].OrgID == r.failOrg {
		return sessionshim.FenceAcknowledgement{}, errors.New("organization fence unavailable")
	}
	return sessionshim.FenceAcknowledgement{
		RequestBytes:    append([]byte(nil), request.RequestBytes...),
		DurableRevision: "revision-" + request.Fence.Sessions[0].OrgID,
	}, nil
}

func (r *exactFenceRecorder) snapshot() []sessionshim.FenceRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sessionshim.FenceRequest(nil), r.requests...)
}

func newShimEventRecorder() *shimEventRecorder {
	return &shimEventRecorder{
		seen: map[string]*strings.Builder{},
		seq:  map[string]uint64{},
		gaps: map[string]int{},
	}
}

func (r *shimEventRecorder) record(id sessionshim.Identity, ev sessionshim.ControllerEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := id.Key()
	switch ev.Kind {
	case sessionshim.EventOutput:
		b, ok := r.seen[key]
		if !ok {
			b = &strings.Builder{}
			r.seen[key] = b
		}
		b.Write(ev.Data)
		if ev.Seq > r.seq[key] {
			r.seq[key] = ev.Seq
		}
	case sessionshim.EventGap:
		r.gaps[key]++
	}
}

func (r *shimEventRecorder) output(id sessionshim.Identity) (string, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := id.Key()
	if b, ok := r.seen[key]; ok {
		return b.String(), r.seq[key]
	}
	return "", r.seq[key]
}

func enableHostedFullHostFramesForTest(t *testing.T, d *Daemon, scopes ...string) {
	t.Helper()
	d.opts.SessionShim.RequireCredentialAttestation = true
	if d.opts.SessionShim.OnCarrierActivationAcknowledged == nil {
		d.opts.SessionShim.OnCarrierActivationAcknowledged = func(SessionShimPublishedBatchReceipt) {}
	}
	d.opts.SessionShim.GetCarrierProofV2Readiness = testSessionShimProofV2Readiness
	d.opts.SessionShim.AttestationCapabilities = RequiredSessionShimHostCapabilities()
	if err := d.refreshSessionShimIdentity().attestationErr; err != nil {
		t.Fatalf("resolve hosted full-frame attestation: %v", err)
	}
	if len(scopes) == 0 {
		scopes = []string{d.sessionShimConfig().orgID()}
	}
	receipts := make([]SessionShimScopeCredentialReceipt, 0, len(scopes))
	for _, scope := range scopes {
		receipts = append(receipts, SessionShimScopeCredentialReceipt{
			Scope: scope, WorkerHostID: d.sessionShimConfig().HostID, AdoptionRevision: "test-recovery-revision",
		})
	}
	if err := d.retainSessionShimCredentialReceipts(receipts); err != nil {
		t.Fatalf("retain hosted full-frame receipt: %v", err)
	}
}

func newShimSpawnFixture(t *testing.T) *shimSpawnFixture {
	t.Helper()
	// A Unix socket path has a short platform limit (as low as 104 bytes), and
	// t.TempDir() bakes the test name into the path. Keep the registry short.
	dir, err := os.MkdirTemp("/tmp", "dsp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	events := newShimEventRecorder()
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption:  true,
			EnableOwnership: true,
			OrgID:           "test-org",
			RegistryDir:     dir + "/registry",
			LaunchTimeout:   60 * time.Second,
			OnSessionEvent:  events.record,
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error {
				return nil
			},
			Orphan: sessionshim.OrphanPolicy{
				Deadline:          2 * time.Second,
				TerminationGrace:  500 * time.Millisecond,
				PropagationMargin: 0,
			},
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 4,
		//nolint:gosec // G204: os.Args[0] is this test binary; helper mode is selected by env
		WorkerCommand:     []string{os.Args[0], "-test.run", "TestMain"},
		WorktreeParentDir: dir,
		BaseEnv: map[string]string{
			envDaemonShimHelper: "1",
			"DONMAI_TEST_DAEMON_SESSION_SHIM_WORKAREA_PARENT": dir,
			// The helper is a race-instrumented copy of this test binary. It has
			// three goroutines of real work, so extra Ps buy nothing and cost CPU
			// every other package's process-spawn deadlines need under a parallel
			// `go test -race ./...`.
			"GOMAXPROCS": "2",
		},
	})
	d.spawner.opts.ShimSpawn = d.launchSessionShim
	d.spawner.Resume()

	f := &shimSpawnFixture{daemon: d, registry: dir + "/registry", workareaParent: dir, orgID: "test-org", events: events}
	t.Cleanup(func() {
		for _, id := range d.AdoptedSessionShims() {
			_ = d.StopAdoptedSessionShim(id.OrgID, id.SessionID, shimwire.StopHostShutdown)
		}
		d.ReleaseAdoptedSessionShims()
	})
	return f
}

// interactiveSpec is a session spec whose run mode selects shim ownership.
func (f *shimSpawnFixture) interactiveSpec(sessionID string) SessionSpec {
	return SessionSpec{
		SessionID:  sessionID,
		ProjectID:  "p1",
		Repository: "https://example.invalid/x/y",
		Mode:       interactiveRunMode,
	}
}

func (f *shimSpawnFixture) identity(sessionID string) sessionshim.Identity {
	return sessionshim.Identity{OrgID: f.orgID, SessionID: sessionID}
}

type dynamicPublicationProbe struct {
	mu                 sync.Mutex
	publications       []SessionShimAdoptionPublication
	batches            []SessionShimAdoptionBatch
	batchCallsInFlight atomic.Int64
	maxBatchCalls      atomic.Int64
	revision           atomic.Int64
	carrierEpoch       atomic.Uint64
	prepareBarrier     *sync.WaitGroup
}

func (p *dynamicPublicationProbe) recordPublication(publication SessionShimAdoptionPublication) {
	p.mu.Lock()
	p.publications = append(p.publications, SessionShimAdoptionPublication{
		ControllerID: publication.ControllerID,
		Batches:      append([]SessionShimPublishedBatchReceipt(nil), publication.Batches...),
		Carriers:     append([]SessionShimCarrierActivation(nil), publication.Carriers...),
	})
	p.mu.Unlock()
}

func (p *dynamicPublicationProbe) snapshot() ([]SessionShimAdoptionPublication, []SessionShimAdoptionBatch) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]SessionShimAdoptionPublication(nil), p.publications...),
		append([]SessionShimAdoptionBatch(nil), p.batches...)
}

func configureDynamicPublicationProbe(t *testing.T, d *Daemon, probe *dynamicPublicationProbe) {
	t.Helper()
	d.opts.SessionShim.CallbackTimeout = 5 * time.Second
	d.opts.SessionShim.PrepareAdoption = func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
		if probe.prepareBarrier != nil {
			probe.prepareBarrier.Done()
			probe.prepareBarrier.Wait()
		}
		epoch := probe.carrierEpoch.Add(1)
		return sessionshim.PreparedAdoption{
			ControllerGeneration: preparation.CurrentControllerGeneration + 1,
			Extensions: shimwire.Extensions{Values: map[string]string{
				shimwire.ExtCarrierEpoch: fmt.Sprintf("%d", epoch),
			}},
			ResumeFrom: proofResolvedResume(preparation),
		}, nil
	}
	d.opts.SessionShim.OnAdoption = func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
		if evidence.SnapshotProxy == nil {
			return SessionShimAdoptionReceipt{}, errors.New("dynamic publication omitted mandatory Snapshot authority")
		}
		if _, err := evidence.SnapshotProxy.Emit(ctx); err != nil {
			return SessionShimAdoptionReceipt{}, err
		}
		return SessionShimAdoptionReceipt{DurableCorrelation: []byte("dynamic-adoption-" + evidence.Identity.Key())}, nil
	}
	d.opts.SessionShim.OnAdoptionBatch = func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		inFlight := probe.batchCallsInFlight.Add(1)
		for {
			prior := probe.maxBatchCalls.Load()
			if prior >= inFlight || probe.maxBatchCalls.CompareAndSwap(prior, inFlight) {
				break
			}
		}
		defer probe.batchCallsInFlight.Add(-1)
		probe.mu.Lock()
		probe.batches = append(probe.batches, cloneSessionShimAdoptionBatch(batch))
		probe.mu.Unlock()
		// Make an un-serialized implementation overlap deterministically after both
		// real shim handshakes crossed the prepare barrier.
		time.Sleep(75 * time.Millisecond)
		revision := probe.revision.Add(1)
		return SessionShimAdoptionBatchReceipt{
			DurableCorrelation: []byte(fmt.Sprintf("dynamic-batch-%d", revision)),
			AdoptionRevision:   fmt.Sprintf("dynamic-revision-%d", revision),
		}, nil
	}
	d.opts.SessionShim.OnAdoptionPublished = func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
		probe.recordPublication(publication)
		receipts := make([]SessionShimCarrierActivationReceipt, 0, len(publication.Carriers))
		d.shims.mu.RLock()
		defer d.shims.mu.RUnlock()
		for _, carrier := range publication.Carriers {
			id := sessionshim.Identity{OrgID: carrier.OrgID, SessionID: carrier.SessionID}
			pending, ok := d.shims.pendingSnapshots[id]
			if !ok {
				return nil, fmt.Errorf("publication included carrier without pending Snapshot: %s", id)
			}
			receipts = append(receipts, SessionShimCarrierActivationReceipt{
				Activation: carrier,
				AckSeq:     pending.Seq,
			})
		}
		return receipts, nil
	}
}

func assertDynamicPublicationBlocked(t *testing.T, d *Daemon) {
	t.Helper()
	if d.State() != StateRecovering || !d.sessionShimReadinessWithdrawn.Load() || d.spawner.IsAccepting() {
		t.Fatalf("dynamic publication gate = state:%s blocked:%v accepting:%v, want recovering/true/false",
			d.State(), d.sessionShimReadinessWithdrawn.Load(), d.spawner.IsAccepting())
	}
	if d.RegistrationStatus() != RegistrationDraining || d.heartbeatMaxConcurrentSessions() != 0 {
		t.Fatalf("dynamic publication advertised capacity: registration=%s max=%d",
			d.RegistrationStatus(), d.heartbeatMaxConcurrentSessions())
	}
	if suspended, _ := d.PollClaimGate()(); !suspended {
		t.Fatal("dynamic publication left poll/claim admission open")
	}
	if _, err := d.AcceptWork(SessionSpec{SessionID: "must-remain-blocked"}); err == nil {
		t.Fatal("dynamic publication left daemon spawn admission open")
	}
}

func TestDynamicPublicationActivatesOnlyTheNewSessionAndWaitsForHeartbeat(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.setState(StateRunning)
	d.shims.adoptionComplete = true
	d.opts.SessionShim.HostID = "host-dynamic"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	enableHostedFullHostFramesForTest(t, d, f.orgID)
	probe := &dynamicPublicationProbe{}
	probe.carrierEpoch.Store(40)
	configureDynamicPublicationProbe(t, d, probe)
	var (
		releaseMu sync.Mutex
		releases  []SessionShimPublishedBatchReceipt
	)
	d.opts.SessionShim.OnCarrierActivationAcknowledged = func(receipt SessionShimPublishedBatchReceipt) {
		releaseMu.Lock()
		releases = append(releases, receipt)
		releaseMu.Unlock()
	}
	releaseSnapshot := func() []SessionShimPublishedBatchReceipt {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		return append([]SessionShimPublishedBatchReceipt(nil), releases...)
	}

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("dynamic-one")); err != nil {
		t.Fatalf("first dynamic launch: %v", err)
	}
	assertDynamicPublicationBlocked(t, d)
	firstProjection, err := d.SessionShimHeartbeatProjection(f.orgID)
	if err != nil {
		t.Fatalf("first dynamic heartbeat projection: %v", err)
	}
	staleFirst := firstProjection
	staleFirst.AdoptionRevision = "test-recovery-revision"
	d.AcknowledgeSessionShimRecoveryHeartbeat(f.orgID, staleFirst)
	d.AcknowledgeSessionShimRecoveryHeartbeat("foreign-org", firstProjection)
	assertDynamicPublicationBlocked(t, d)
	if got := releaseSnapshot(); len(got) != 0 {
		t.Fatalf("stale/foreign heartbeat released carrier authority: %+v", got)
	}
	d.AcknowledgeSessionShimRecoveryHeartbeat(f.orgID, firstProjection)
	if d.State() != StateRunning || !d.spawner.IsAccepting() {
		t.Fatalf("exact first heartbeat did not reopen: state=%s accepting=%v", d.State(), d.spawner.IsAccepting())
	}
	if got := releaseSnapshot(); len(got) != 1 || got[0].Scope != f.orgID || got[0].AdoptionRevision != firstProjection.AdoptionRevision {
		t.Fatalf("first exact heartbeat releases = %+v", got)
	}
	if _, err := d.spawner.AcceptWork(f.interactiveSpec("dynamic-two")); err != nil {
		t.Fatalf("second dynamic launch: %v", err)
	}

	publications, _ := probe.snapshot()
	if len(publications) != 2 {
		t.Fatalf("dynamic publications = %d, want 2: %+v", len(publications), publications)
	}
	for i, wantSession := range []string{"dynamic-one", "dynamic-two"} {
		if len(publications[i].Carriers) != 1 || publications[i].Carriers[0].SessionID != wantSession {
			t.Fatalf("dynamic publication %d carriers = %+v, want only %s", i+1, publications[i].Carriers, wantSession)
		}
	}
	assertDynamicPublicationBlocked(t, d)
	if !d.SessionShimCarrierActivationComplete() {
		t.Fatal("exact second carrier activation did not complete before heartbeat gate")
	}
	secondProjection, err := d.SessionShimHeartbeatProjection(f.orgID)
	if err != nil {
		t.Fatalf("second dynamic heartbeat projection: %v", err)
	}
	staleSecond := secondProjection
	staleSecond.AdoptionRevision = firstProjection.AdoptionRevision
	d.AcknowledgeSessionShimRecoveryHeartbeat(f.orgID, staleSecond)
	assertDynamicPublicationBlocked(t, d)
	if got := releaseSnapshot(); len(got) != 1 {
		t.Fatalf("stale second heartbeat released carrier authority: %+v", got)
	}
	d.AcknowledgeSessionShimRecoveryHeartbeat(f.orgID, secondProjection)
	if d.State() != StateRunning || !d.spawner.IsAccepting() || d.sessionShimReadinessWithdrawn.Load() {
		t.Fatalf("exact second heartbeat did not reopen: state=%s blocked=%v accepting=%v",
			d.State(), d.sessionShimReadinessWithdrawn.Load(), d.spawner.IsAccepting())
	}
	if got := releaseSnapshot(); len(got) != 2 || got[1].AdoptionRevision != secondProjection.AdoptionRevision {
		t.Fatalf("second exact heartbeat releases = %+v", got)
	}
}

func TestConcurrentDynamicPublicationIsGloballySerialized(t *testing.T) {
	for _, tc := range []struct {
		name   string
		orgIDs []string
	}{
		{name: "same-scope", orgIDs: []string{"org-shared", "org-shared"}},
		{name: "cross-scope", orgIDs: []string{"org-alpha", "org-beta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newShimSpawnFixture(t)
			d := f.daemon
			d.setState(StateRunning)
			d.shims.adoptionComplete = true
			d.opts.SessionShim.HostID = "host-concurrent"
			d.opts.SessionShim.RequireAuthoritativeSnapshot = true
			scopes := []string{tc.orgIDs[0]}
			if tc.orgIDs[1] != tc.orgIDs[0] {
				scopes = append(scopes, tc.orgIDs[1])
			}
			enableHostedFullHostFramesForTest(t, d, scopes...)
			barrier := &sync.WaitGroup{}
			barrier.Add(2)
			probe := &dynamicPublicationProbe{prepareBarrier: barrier}
			probe.carrierEpoch.Store(70)
			configureDynamicPublicationProbe(t, d, probe)
			var (
				releaseMu sync.Mutex
				releases  []SessionShimPublishedBatchReceipt
			)
			d.opts.SessionShim.OnCarrierActivationAcknowledged = func(receipt SessionShimPublishedBatchReceipt) {
				releaseMu.Lock()
				releases = append(releases, receipt)
				releaseMu.Unlock()
			}

			errs := make(chan error, 2)
			for i, orgID := range tc.orgIDs {
				spec := f.interactiveSpec(fmt.Sprintf("concurrent-%d", i+1))
				spec.OrganizationID = orgID
				go func() {
					_, err := d.spawner.AcceptWork(spec)
					errs <- err
				}()
			}
			for range 2 {
				if err := <-errs; err != nil {
					t.Fatalf("concurrent dynamic launch: %v", err)
				}
			}
			if got := probe.maxBatchCalls.Load(); got != 1 {
				t.Fatalf("maximum concurrent batch publications = %d, want 1", got)
			}
			publications, batches := probe.snapshot()
			if len(publications) != 2 || len(batches) != 2 {
				t.Fatalf("publication counts = callbacks:%d batches:%d, want 2/2", len(publications), len(batches))
			}
			if tc.name == "same-scope" {
				lengths := []int{len(batches[0].Adopted), len(batches[1].Adopted)}
				sort.Ints(lengths)
				if !reflect.DeepEqual(lengths, []int{1, 2}) {
					t.Fatalf("serialized same-scope batch sizes = %v, want [1 2]", lengths)
				}
			}
			assertDynamicPublicationBlocked(t, d)
			uniqueScopes := append([]string(nil), scopes...)
			sort.Strings(uniqueScopes)
			for i, scope := range uniqueScopes {
				projection, err := d.SessionShimHeartbeatProjection(scope)
				if err != nil {
					t.Fatalf("scope %s heartbeat projection: %v", scope, err)
				}
				d.AcknowledgeSessionShimRecoveryHeartbeat(scope, projection)
				releaseMu.Lock()
				released := append([]SessionShimPublishedBatchReceipt(nil), releases...)
				releaseMu.Unlock()
				if len(released) != i+1 || released[i].Scope != scope || released[i].AdoptionRevision != projection.AdoptionRevision {
					t.Fatalf("scope %s exact release sequence = %+v", scope, released)
				}
				if i+1 < len(uniqueScopes) {
					assertDynamicPublicationBlocked(t, d)
				}
			}
			if d.State() != StateRunning || !d.spawner.IsAccepting() {
				t.Fatalf("all scoped heartbeat acks did not reopen: state=%s accepting=%v", d.State(), d.spawner.IsAccepting())
			}
		})
	}
}

func TestQueuedDynamicPublicationStopsAfterPriorActivationFailure(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.setState(StateRunning)
	d.shims.adoptionComplete = true
	d.opts.SessionShim.HostID = "host-failed-publication"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	enableHostedFullHostFramesForTest(t, d, "org-failed-publication")
	barrier := &sync.WaitGroup{}
	barrier.Add(2)
	probe := &dynamicPublicationProbe{prepareBarrier: barrier}
	probe.carrierEpoch.Store(90)
	configureDynamicPublicationProbe(t, d, probe)
	d.opts.SessionShim.OnAdoptionPublished = func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
		probe.recordPublication(publication)
		return nil, errors.New("first serialized activation refused")
	}

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		spec := f.interactiveSpec(fmt.Sprintf("failed-publication-%d", i+1))
		spec.OrganizationID = "org-failed-publication"
		go func() {
			_, err := d.spawner.AcceptWork(spec)
			errs <- err
		}()
	}
	var priorFailureRefusals int
	for range 2 {
		if err := <-errs; err != nil && strings.Contains(err.Error(), "prior dynamic adoption publication failed") {
			priorFailureRefusals++
		}
	}
	if priorFailureRefusals != 1 {
		t.Fatalf("queued prior-publication refusals = %d, want exactly 1", priorFailureRefusals)
	}
	publications, batches := probe.snapshot()
	if len(publications) != 1 || len(batches) != 1 {
		t.Fatalf("callbacks after failed activation = publications:%d batches:%d, want 1/1", len(publications), len(batches))
	}
	if !d.shims.dynamicPublicationFailed || d.State() != StateRecovering || d.spawner.IsAccepting() {
		t.Fatalf("failed publication did not latch closed: failed=%v state=%s accepting=%v",
			d.shims.dynamicPublicationFailed, d.State(), d.spawner.IsAccepting())
	}
}

func TestDynamicLaunchWithoutPublicationHookDoesNotPoisonLaterSessions(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = "standalone-host"
	var attempts atomic.Int64
	d.opts.SessionShim.OnAdoption = func(context.Context, SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
		if attempts.Add(1) == 1 {
			return SessionShimAdoptionReceipt{}, errors.New("first standalone adoption refused")
		}
		return SessionShimAdoptionReceipt{}, nil
	}
	if _, err := d.spawner.AcceptWork(f.interactiveSpec("standalone-refused")); err == nil {
		t.Fatal("first standalone launch unexpectedly succeeded")
	}
	if _, err := d.spawner.AcceptWork(f.interactiveSpec("standalone-next")); err != nil {
		t.Fatalf("independent standalone launch was poisoned: %v", err)
	}
	if _, err := d.adoptedShimEntry(f.orgID, "standalone-next"); err != nil {
		t.Fatalf("second standalone session was not adopted: %v", err)
	}
}

// exchange writes one line into the adopted session and waits for the harness's
// answer to reach the carrier hook, returning the highest sequence forwarded.
// It proves BOTH directions are live through the adopted connection.
func (f *shimSpawnFixture) exchange(t *testing.T, id sessionshim.Identity, token string) uint64 {
	t.Helper()
	if err := f.daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte(token+"\r")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInput: %v", err)
	}
	want := "ack:" + token
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, seq := f.events.output(id)
		if strings.Contains(out, want) {
			return seq
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, _ := f.events.output(id)
	t.Fatalf("timed out waiting for %q to reach the carrier; saw %q", want, out)
	return 0
}

// TestInteractiveSpawnLaunchesThroughAShimAndAdoptsIt is the V16 anchor for the
// SELECTION rule: an interactive session accepted by this daemon must be owned
// by a separate shim process this daemon then adopts, not by a daemon-parented
// child.
//
// Bypassing the selection in shimOwnsSession (returning false, or dropping the
// Mode check) turns this test RED at the AdoptedSessionShims assertion, because
// the session then takes the ordinary direct-child path and no shim exists.
func TestInteractiveSpawnLaunchesThroughAShimAndAdoptsIt(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	handle, err := d.spawner.AcceptWork(f.interactiveSpec("sess-launch"))
	if err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-launch")

	adopted := d.AdoptedSessionShims()
	if len(adopted) != 1 || adopted[0] != id {
		t.Fatalf("AdoptedSessionShims = %+v, want exactly [%s] — the interactive spawn did not go through a shim", adopted, id)
	}

	// §D1: the daemon holds no process bookkeeping for a shim-owned session. A
	// spawner entry would mean a second owner, and the reaper attached to it
	// would end the session on the next daemon shutdown.
	if _, tracked := d.spawner.sessions[id.SessionID]; tracked {
		t.Fatal("the spawner registered a direct-child entry for a shim-owned session; the daemon must not be a second owner")
	}

	// The published PID is the HARNESS, reported by the shim — not the shim
	// process and not a daemon child. It is the value an unchanged-across-restart
	// comparison is made against (§D2).
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if handle.PID == 0 || handle.PID != entry.controller.HarnessIdentity().PID {
		t.Fatalf("handle PID = %d, want the shim-reported harness pid %d", handle.PID, entry.controller.HarnessIdentity().PID)
	}
	if handle.PID == os.Getpid() {
		t.Fatal("handle PID is this process; the harness must run under the shim, not the controller")
	}
	if !entry.controller.HarnessSurvived() {
		t.Fatal("HarnessSurvived = false immediately after launch")
	}
	if entry.controller.Generation() == 0 {
		t.Fatal("adoption did not commit a controller generation; single-controller fencing is unenforced")
	}
	if !entry.launched {
		t.Error("the launched shim is not marked as launched by this daemon")
	}
}

func TestExactSessionShimControlRefFencesReplacementAuthority(t *testing.T) {
	f := newShimSpawnFixture(t)
	id := f.identity("exact-control-ref")
	if _, err := f.daemon.spawner.AcceptWork(f.interactiveSpec(id.SessionID)); err != nil {
		t.Fatalf("launch exact-control-ref session: %v", err)
	}
	entry, err := f.daemon.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	ref := SessionShimControlRef{
		Identity: id, ShimID: entry.shimID,
		ProcessEpoch: entry.adoption.ProcessEpoch, ControllerGeneration: entry.adoption.ControllerGeneration,
	}
	mutations := map[string]func(SessionShimControlRef) SessionShimControlRef{
		"identity": func(in SessionShimControlRef) SessionShimControlRef {
			in.Identity.SessionID = "replacement-session"
			return in
		},
		"shim id": func(in SessionShimControlRef) SessionShimControlRef {
			in.ShimID = "replacement-shim"
			return in
		},
		"process epoch": func(in SessionShimControlRef) SessionShimControlRef {
			in.ProcessEpoch++
			return in
		},
		"controller generation": func(in SessionShimControlRef) SessionShimControlRef {
			in.ControllerGeneration++
			return in
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			stale := mutate(ref)
			if err := f.daemon.WriteAdoptedSessionShimInputFor(stale, []byte("stale\r")); err == nil {
				t.Fatal("stale control reference wrote input")
			}
			if err := f.daemon.ResizeAdoptedSessionShimFor(stale, 100, 40, 0, 0); err == nil {
				t.Fatal("stale control reference resized the PTY")
			}
			if err := f.daemon.StopAdoptedSessionShimFor(stale, shimwire.StopOperator); err == nil {
				t.Fatal("stale control reference stopped the session")
			}
		})
	}
	if err := f.daemon.WriteAdoptedSessionShimInputFor(ref, []byte("exact-control\r")); err != nil {
		t.Fatalf("current control reference input: %v", err)
	}
	waitFor(t, 10*time.Second, "current control-reference input", func() bool {
		output, _ := f.events.output(id)
		return strings.Contains(output, "ack:exact-control")
	})
	if err := f.daemon.ResizeAdoptedSessionShimFor(ref, 101, 41, 0, 0); err != nil {
		t.Fatalf("current control reference resize: %v", err)
	}
	if err := f.daemon.StopAdoptedSessionShimFor(ref, shimwire.StopOperator); err != nil {
		t.Fatalf("current control reference stop: %v", err)
	}
}

func TestExactSessionShimControlRefEmitsOneActiveSnapshot(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.setState(StateRunning)
	d.shims.adoptionComplete = true
	d.opts.SessionShim.HostID = "host-exact-snapshot"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	enableHostedFullHostFramesForTest(t, d, f.orgID)
	d.opts.SessionShim.PrepareAdoption = func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
		return sessionshim.PreparedAdoption{
			ControllerGeneration: preparation.CurrentControllerGeneration + 1,
			Extensions:           shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "201"}},
			ResumeFrom:           proofResolvedResume(preparation),
		}, nil
	}
	d.opts.SessionShim.OnAdoption = func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
		if _, err := evidence.SnapshotProxy.Emit(ctx); err != nil {
			return SessionShimAdoptionReceipt{}, err
		}
		return SessionShimAdoptionReceipt{DurableCorrelation: []byte("exact-snapshot-adoption")}, nil
	}
	d.opts.SessionShim.OnAdoptionBatch = func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		return SessionShimAdoptionBatchReceipt{
			DurableCorrelation: []byte("exact-snapshot-batch"), AdoptionRevision: "exact-snapshot-revision",
		}, nil
	}
	d.opts.SessionShim.OnAdoptionPublished = func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
		if len(publication.Carriers) != 1 {
			return nil, fmt.Errorf("exact Snapshot publication carriers = %d, want 1", len(publication.Carriers))
		}
		carrier := publication.Carriers[0]
		id := sessionshim.Identity{OrgID: carrier.OrgID, SessionID: carrier.SessionID}
		d.shims.mu.RLock()
		pending, ok := d.shims.pendingSnapshots[id]
		d.shims.mu.RUnlock()
		if !ok {
			return nil, errors.New("exact Snapshot publication omitted pending candidate")
		}
		return []SessionShimCarrierActivationReceipt{{Activation: carrier, AckSeq: pending.Seq}}, nil
	}
	observed := make(chan sessionshim.ControllerEvent, 2)
	durable := make(chan sessionshim.ControllerEvent, 2)
	var captureActive atomic.Bool
	cloneSnapshotEvent := func(event sessionshim.ControllerEvent) sessionshim.ControllerEvent {
		event.FrameBytes = append([]byte(nil), event.FrameBytes...)
		event.Data = append([]byte(nil), event.Data...)
		return event
	}
	observeExisting := d.opts.SessionShim.OnSessionEvent
	d.opts.SessionShim.OnSessionEvent = func(id sessionshim.Identity, event sessionshim.ControllerEvent) {
		observeExisting(id, event)
		if captureActive.Load() && event.Kind == sessionshim.EventHostFrame && event.FrameType == attachwire.TypeSnapshot {
			observed <- cloneSnapshotEvent(event)
		}
	}
	d.opts.SessionShim.OnSessionEventDurable = func(_ sessionshim.Identity, event sessionshim.ControllerEvent) error {
		if captureActive.Load() && event.Kind == sessionshim.EventHostFrame && event.FrameType == attachwire.TypeSnapshot {
			durable <- cloneSnapshotEvent(event)
		}
		return nil
	}
	id := f.identity("exact-snapshot-control-ref")
	if _, err := d.spawner.AcceptWork(f.interactiveSpec(id.SessionID)); err != nil {
		t.Fatalf("launch exact Snapshot session: %v", err)
	}
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.controller.SupportsFullHostFrames() {
		t.Fatal("exact Snapshot fixture did not negotiate selected v3")
	}
	projection, err := d.SessionShimHeartbeatProjection(id.OrgID)
	if err != nil {
		t.Fatalf("exact Snapshot heartbeat projection: %v", err)
	}
	d.AcknowledgeSessionShimRecoveryHeartbeat(id.OrgID, projection)
	ref := SessionShimControlRef{
		Identity: id, ShimID: entry.shimID,
		ProcessEpoch: entry.adoption.ProcessEpoch, ControllerGeneration: entry.adoption.ControllerGeneration,
	}
	staleRefs := map[string]func(SessionShimControlRef) SessionShimControlRef{
		"identity": func(in SessionShimControlRef) SessionShimControlRef {
			in.Identity.SessionID = "replacement-session"
			return in
		},
		"shim id": func(in SessionShimControlRef) SessionShimControlRef {
			in.ShimID = "replacement-shim"
			return in
		},
		"process epoch": func(in SessionShimControlRef) SessionShimControlRef {
			in.ProcessEpoch++
			return in
		},
		"controller generation": func(in SessionShimControlRef) SessionShimControlRef {
			in.ControllerGeneration++
			return in
		},
	}
	for name, mutate := range staleRefs {
		t.Run(name, func(t *testing.T) {
			if _, err := d.EmitAdoptedSessionShimSnapshotFor(context.Background(), mutate(ref)); err == nil {
				t.Fatal("stale control reference emitted a Snapshot")
			}
		})
	}
	captureActive.Store(true)
	result, err := d.EmitAdoptedSessionShimSnapshotFor(context.Background(), ref)
	if err != nil {
		t.Fatalf("current exact Snapshot: %v", err)
	}
	if !result.InStream || len(result.Bytes) != 0 {
		t.Fatalf("selected-v3 Snapshot result = %+v, want correlation-only in-stream result", result)
	}
	wantSeq := result.AtSeq + 1
	readSnapshot := func(label string, ch <-chan sessionshim.ControllerEvent) sessionshim.ControllerEvent {
		t.Helper()
		select {
		case event := <-ch:
			if event.Seq != wantSeq || event.RequestID == 0 {
				t.Fatalf("%s Snapshot correlation = %+v, want seq %d with request id", label, event, wantSeq)
			}
			frame, decodeErr := attachwire.DecodeFrame(event.FrameBytes)
			if decodeErr != nil || frame.Type != attachwire.TypeSnapshot || frame.Seq != wantSeq || !bytes.Equal(frame.Encode(), event.FrameBytes) {
				t.Fatalf("%s raw Snapshot changed: frame=%+v err=%v event=%+v", label, frame, decodeErr, event)
			}
			return event
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s Snapshot", label)
			return sessionshim.ControllerEvent{}
		}
	}
	observedEvent := readSnapshot("observed", observed)
	durableEvent := readSnapshot("durable", durable)
	if !bytes.Equal(observedEvent.FrameBytes, durableEvent.FrameBytes) {
		t.Fatal("observer and durable callbacks received different Snapshot bytes")
	}
	for label, ch := range map[string]<-chan sessionshim.ControllerEvent{"observed": observed, "durable": durable} {
		select {
		case duplicate := <-ch:
			t.Fatalf("%s callback received a second mandatory candidate Snapshot: %+v", label, duplicate)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestInteractiveShimUsesPerSessionOrganizationAndGroupedExactFences(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	store := &exactFenceRecorder{}
	d.opts.SessionShim.HostIDForOrg = func(_ context.Context, orgID string) (string, error) {
		return "stable-host-" + orgID, nil
	}
	d.opts.SessionShim.ExactFenceStore = store

	for _, tc := range []struct {
		orgID, sessionID string
	}{
		{orgID: "org-alpha", sessionID: "sess-alpha"},
		{orgID: "org-beta", sessionID: "sess-beta"},
	} {
		spec := f.interactiveSpec(tc.sessionID)
		spec.OrganizationID = tc.orgID
		if _, err := d.spawner.AcceptWork(spec); err != nil {
			t.Fatalf("AcceptWork(%s): %v", tc.orgID, err)
		}
		if _, err := d.adoptedShimEntry(tc.orgID, tc.sessionID); err != nil {
			t.Fatalf("per-session lifecycle identity %s/%s was not adopted: %v", tc.orgID, tc.sessionID, err)
		}
	}

	// Prove the fence host does not come from the rotating worker/controller id.
	d.mu.Lock()
	d.workerID = "worker-controller-correlation"
	d.mu.Unlock()
	fences, err := d.RequestSessionShimRestartFences(context.Background(), "fence-shared-id")
	if err != nil {
		t.Fatalf("RequestSessionShimRestartFences: %v", err)
	}
	if len(fences) != 2 {
		t.Fatalf("fences = %+v, want one per organization", fences)
	}
	requests := store.snapshot()
	if len(requests) != 2 {
		t.Fatalf("exact store requests = %d, want 2", len(requests))
	}
	for _, request := range requests {
		if len(request.Fence.Sessions) != 1 {
			t.Fatalf("cross-organization exact request = %+v, want one homogeneous session", request.Fence.Sessions)
		}
		covered := request.Fence.Sessions[0]
		if request.Fence.HostID != "stable-host-"+covered.OrgID {
			t.Errorf("fence hostId = %q, want per-org stable host authority for %s", request.Fence.HostID, covered.OrgID)
		}
		if covered.OrgID == "" || covered.ControllerGeneration == 0 {
			t.Errorf("fence omitted per-session org/controller correlation: %+v", covered)
		}
		entry, err := d.adoptedShimEntry(covered.OrgID, covered.SessionID)
		if err != nil {
			t.Fatalf("adoptedShimEntry(%s): %v", covered.Identity(), err)
		}
		if covered.ControllerGeneration != uint64(entry.controller.Generation()) {
			t.Errorf("fenced generation = %d, exact shim generation = %d", covered.ControllerGeneration, entry.controller.Generation())
		}
		if entry.adoption.ControllerID != d.ControllerID() ||
			entry.adoption.ControllerID == request.Fence.HostID ||
			entry.adoption.ControllerID == d.WorkerID() {
			t.Errorf("controller/host correlations collapsed or drifted: controller=%q host=%q",
				entry.adoption.ControllerID, request.Fence.HostID)
		}
		for _, session := range request.Fence.Sessions {
			if session.OrgID != covered.OrgID {
				t.Fatalf("exact fence mixed organizations: %+v", request.Fence.Sessions)
			}
		}
	}
}

func TestSessionShimAdoptionAndTerminalCallbacksCarryExactCorrelation(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = "host-callback"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	enableHostedFullHostFramesForTest(t, d, "org-callback")
	d.opts.SessionShim.PrepareAdoption = func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
		if preparation.Identity.OrgID != "org-callback" || preparation.HostID != "host-callback" ||
			preparation.ShimID == "" || preparation.ProcessEpoch == 0 {
			return sessionshim.PreparedAdoption{}, fmt.Errorf("incomplete preparation evidence: %+v", preparation)
		}
		return sessionshim.PreparedAdoption{
			ControllerGeneration: preparation.CurrentControllerGeneration + 7,
			ResumeFrom:           proofResolvedResume(preparation),
			Extensions: shimwire.Extensions{
				Values:   map[string]string{shimwire.ExtCarrierEpoch: "19"},
				Required: []string{shimwire.ExtCarrierEpoch},
			},
			Correlation: []byte(`{"fenceRevision":"73","expectedAdoptionRevision":"81"}`),
		}, nil
	}
	var adoption SessionShimAdoptionEvidence
	var emitted shimwire.SnapshotResult
	var durableExit atomic.Bool
	d.opts.SessionShim.OnAdoption = func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
		adoption = evidence
		var err error
		emitted, err = evidence.SnapshotProxy.Emit(ctx)
		if err != nil {
			return SessionShimAdoptionReceipt{}, err
		}
		return SessionShimAdoptionReceipt{DurableCorrelation: []byte(`{"fenceRevision":"73","adoptionRevision":"81"}`)}, nil
	}
	d.opts.SessionShim.OnSessionEventDurable = func(_ sessionshim.Identity, event sessionshim.ControllerEvent) error {
		if event.Kind == sessionshim.EventHostFrame && event.FrameType == attachwire.TypeExit {
			frame, err := attachwire.DecodeFrame(event.FrameBytes)
			if err != nil || frame.Seq != event.Seq || !bytes.Equal(frame.Encode(), event.FrameBytes) {
				return fmt.Errorf("terminal HostFrame lost exact bytes: %v", err)
			}
			durableExit.Store(true)
		}
		return nil
	}
	d.opts.SessionShim.OnAdoptionBatch = func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		return SessionShimAdoptionBatchReceipt{
			DurableCorrelation: []byte("callback-batch"), AdoptionRevision: "callback-revision",
		}, nil
	}
	d.opts.SessionShim.OnAdoptionPublished = func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
		return []SessionShimCarrierActivationReceipt{{
			Activation: publication.Carriers[0], AckSeq: emitted.AtSeq + 1,
		}}, nil
	}
	terminal := make(chan SessionShimTerminalEvidence, 1)
	d.opts.SessionShim.OnTerminalEvidence = func(_ context.Context, evidence SessionShimTerminalEvidence) error {
		if !durableExit.Load() {
			return errors.New("terminal proof overtook durable Exit HostFrame")
		}
		registry, err := sessionshim.NewRegistry(f.registry)
		if err != nil {
			return err
		}
		if _, err := registry.GetTombstone(evidence.Identity); err != nil {
			return fmt.Errorf("terminal callback ran before tombstone publication: %w", err)
		}
		terminal <- evidence
		return nil
	}

	spec := f.interactiveSpec("sess-callback")
	spec.OrganizationID = "org-callback"
	if _, err := d.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := sessionshim.Identity{OrgID: spec.OrganizationID, SessionID: spec.SessionID}
	if adoption.Identity != id || adoption.HostID != "host-callback" {
		t.Fatalf("adoption evidence identity/host = %+v", adoption)
	}
	if adoption.ControllerGeneration != 7 || adoption.ProcessEpoch == 0 || adoption.ShimID == "" {
		t.Fatalf("adoption evidence omitted exact shim/process/controller correlation: %+v", adoption)
	}
	if got, ok := adoption.Extensions.Get(shimwire.ExtCarrierEpoch); !ok || got != "19" {
		t.Fatalf("adoption carrier_epoch = %q/%v, want 19/true", got, ok)
	}
	if string(adoption.PreparedCorrelation) != `{"fenceRevision":"73","expectedAdoptionRevision":"81"}` {
		t.Fatalf("prepared correlation changed before adoption: %s", adoption.PreparedCorrelation)
	}
	if !d.StopSession(spec.SessionID) {
		t.Fatal("StopSession did not reach shim")
	}

	select {
	case evidence := <-terminal:
		if evidence.Identity != id || evidence.HostID != adoption.HostID || evidence.Adoption == nil ||
			evidence.ShimID != adoption.ShimID || evidence.ProcessEpoch != adoption.ProcessEpoch ||
			evidence.Adoption.ControllerGeneration != adoption.ControllerGeneration {
			t.Fatalf("terminal correlation = %+v, want adoption %+v", evidence, adoption)
		}
		if string(evidence.DurableAdoptionCorrelation) != `{"fenceRevision":"73","adoptionRevision":"81"}` {
			t.Fatalf("opaque durable adoption correlation changed: %s", evidence.DurableAdoptionCorrelation)
		}
		if !evidence.Tombstone.GroupReaped {
			t.Fatal("terminal callback ran without positive process-group reap proof")
		}
		if evidence.Adoption.SnapshotProxy != nil {
			t.Fatal("terminal evidence retained the ephemeral adoption SnapshotProxy")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for terminal evidence callback")
	}
}

// TestShimOwnedSessionIsVisibleInSessionsAndCapacity pins §D7 at the two
// surfaces that decide whether more work is sent here.
func TestShimOwnedSessionIsVisibleInSessionsAndCapacity(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-visible")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}

	sessions := d.ActiveSessions()
	if len(sessions) != 1 || sessions[0].SessionID != "sess-visible" {
		t.Fatalf("ActiveSessions = %+v, want the shim-owned session listed", sessions)
	}
	if sessions[0].State != SessionRunning {
		t.Errorf("shim-owned session state = %q, want %q", sessions[0].State, SessionRunning)
	}
	if sessions[0].WorktreePath == "" {
		t.Error("shim-owned session has no worktree path; a local reader cannot find its .agent state")
	}

	active, interactive := d.spawnerActiveSessionCounts()
	if active != 1 || interactive != 1 {
		t.Fatalf("occupancy = (active %d, interactive %d), want (1, 1)", active, interactive)
	}
	if d.SessionShimOccupancy() != 1 {
		t.Fatalf("SessionShimOccupancy = %d, want 1", d.SessionShimOccupancy())
	}
}

// TestAdoptedSessionAcceptsInputAndProducesOutput proves both directions of the
// terminal are live through the adopted connection — the concrete meaning of
// "the session works after adoption" (§D5).
func TestAdoptedSessionAcceptsInputAndProducesOutput(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-io")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-io")

	first := f.exchange(t, id, "one")
	second := f.exchange(t, id, "two")
	if second <= first {
		t.Fatalf("host output sequence did not advance: %d then %d; the shim is the sole allocator and must be monotonic", first, second)
	}

	// Geometry is a mutating frame and must be accepted under this daemon's
	// committed generation.
	if err := d.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, 100, 40, 0, 0); err != nil {
		t.Fatalf("ResizeAdoptedSessionShim: %v", err)
	}
}

// TestStopAndTerminalCleanupAfterAdoption is the terminal half of the contract:
// a generation-fenced Stop reaches the shim, the harness is reaped, capacity is
// released, and the durable tombstone is consumed rather than left behind (§D8,
// §D10).
func TestStopAndTerminalCleanupAfterAdoption(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-stop")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-stop")
	f.exchange(t, id, "alive")

	// The control-API id is a bare session id; it must reach the shim rather than
	// falling through to a direct-child stop that would find nothing.
	if !d.StopSession(id.SessionID) {
		t.Fatal("StopSession did not route to the adopted shim")
	}

	waitFor(t, 30*time.Second, "the adopted session to reach a terminal outcome", func() bool {
		return d.SessionShimOccupancy() == 0
	})

	if got := d.ActiveSessions(); len(got) != 0 {
		t.Fatalf("ActiveSessions after terminal outcome = %+v, want empty", got)
	}

	// The tombstone was the proof of death; once the outcome is durably recorded
	// it is disposed. Both halves matter — an undisposed tombstone would keep the
	// session in reconciliation forever.
	registry, err := sessionshim.NewRegistry(f.registry)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// Both halves, and in this order: the liveness claim is withdrawn first, then
	// the proof is disposed. Asserting only that the tombstone is gone would pass
	// against a registry still advertising a live shim for a session that ended.
	waitFor(t, 15*time.Second, "the discovery record to be withdrawn", func() bool {
		_, err := registry.Get(id)
		return err != nil
	})
	waitFor(t, 15*time.Second, "the tombstone to be disposed after the outcome was recorded", func() bool {
		_, err := registry.GetTombstone(id)
		return err != nil
	})
	if proof := d.SessionShimTerminalProof(id.OrgID, id.SessionID); !proof.Proves() {
		t.Fatal("no terminal proof retained for a session this daemon watched end; the outcome would be unresolvable")
	}
}

func TestShimLaunchLifecycleTransfersPreSpawnCleanupExactlyOnce(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.spawner.opts.ShimOwns = d.shimOwnsSession

	var preSpawnCalls atomic.Int32
	var abortCalls atomic.Int32
	var startCalls atomic.Int32
	var endCalls atomic.Int32
	var cleanupCalls atomic.Int32
	var resourceOwned atomic.Bool
	ended := make(chan SessionEvent, 2)
	d.spawner.opts.OnPreSpawn = func(_ SessionSpec, env []string) ([]string, error) {
		preSpawnCalls.Add(1)
		if !resourceOwned.CompareAndSwap(false, true) {
			t.Error("OnPreSpawn acquired an already-owned resource")
		}
		return env, nil
	}
	d.spawner.opts.OnSpawnAborted = func(SessionSpec, error) {
		abortCalls.Add(1)
		resourceOwned.Store(false)
	}
	d.spawner.On(func(ev SessionEvent) {
		switch ev.Kind {
		case SessionEventStarted:
			startCalls.Add(1)
		case SessionEventEnded:
			endCalls.Add(1)
			if !resourceOwned.CompareAndSwap(true, false) {
				t.Error("SessionEventEnded did not receive OnPreSpawn resource ownership")
			}
			cleanupCalls.Add(1)
			ended <- ev
		}
	})

	spec := f.interactiveSpec("sess-lifecycle")
	handle, err := d.spawner.AcceptWork(spec)
	if err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	if got := preSpawnCalls.Load(); got != 1 {
		t.Fatalf("OnPreSpawn calls after launch = %d, want 1", got)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("SessionEventStarted calls after launch = %d, want 1", got)
	}
	if got := abortCalls.Load(); got != 0 {
		t.Fatalf("OnSpawnAborted calls after successful ownership transfer = %d, want 0", got)
	}
	if !d.StopSession(spec.SessionID) {
		t.Fatal("StopSession did not route to the launched shim")
	}

	var terminal SessionEvent
	select {
	case terminal = <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for shim SessionEventEnded")
	}
	if terminal.Spec.SessionID != spec.SessionID {
		t.Errorf("Ended spec session = %q, want %q", terminal.Spec.SessionID, spec.SessionID)
	}
	if terminal.Handle.SessionID != handle.SessionID || terminal.Handle.PID != handle.PID {
		t.Errorf("Ended handle = %+v, want original lifecycle handle %+v", terminal.Handle, *handle)
	}
	if terminal.Handle.State != SessionTerminated {
		t.Errorf("Ended state = %q, want %q after Stop", terminal.Handle.State, SessionTerminated)
	}
	waitFor(t, 15*time.Second, "terminal lifecycle release", func() bool {
		return d.SessionShimOccupancy() == 0
	})
	if got := endCalls.Load(); got != 1 {
		t.Errorf("SessionEventEnded calls = %d, want exactly 1", got)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Errorf("listener cleanup calls = %d, want exactly 1", got)
	}
	if resourceOwned.Load() {
		t.Error("OnPreSpawn resource remained owned after terminal lifecycle delivery")
	}
	if got := abortCalls.Load(); got != 0 {
		t.Errorf("OnSpawnAborted calls after terminal completion = %d, want 0", got)
	}
	select {
	case duplicate := <-ended:
		t.Fatalf("duplicate terminal lifecycle event: %+v", duplicate)
	default:
	}
}

func TestShimControllerDisconnectDoesNotEmitTerminalLifecycle(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.spawner.opts.ShimOwns = d.shimOwnsSession

	var endCalls atomic.Int32
	d.spawner.On(func(ev SessionEvent) {
		if ev.Kind == SessionEventEnded {
			endCalls.Add(1)
		}
	})
	spec := f.interactiveSpec("sess-controller-gap")
	if _, err := d.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if err := entry.controller.Close(); err != nil {
		t.Fatalf("close controller: %v", err)
	}
	waitFor(t, 5*time.Second, "controller gap quarantine", func() bool {
		return len(d.AdoptedSessionShims()) == 0 && len(d.QuarantinedSessions()) == 1
	})
	if got := d.SessionShimOccupancy(); got != 1 {
		t.Fatalf("SessionShimOccupancy after controller disconnect = %d, want 1 while the harness remains live", got)
	}
	quarantined := d.QuarantinedSessions()
	if len(quarantined) != 1 {
		t.Fatalf("QuarantinedSessions after controller disconnect = %+v, want one visible survivor", quarantined)
	}
	q := quarantined[0]
	processEpoch := entry.controller.Hello().ProcessEpoch
	if q.Identity() != id || q.ShimID != entry.shimID || q.ProcessEpoch != processEpoch ||
		q.ControllerGeneration != uint64(entry.controller.Generation()) {
		t.Errorf("quarantine correlation = %s/%s/%d, want exact %s/%s/%d",
			q.Identity(), q.ShimID, q.ProcessEpoch, id, entry.shimID, processEpoch)
	}
	if q.Reason != sessionshim.QuarantineSocketUnreachable || !q.ConsumesCapacity {
		t.Errorf("quarantine = %+v, want socket_unreachable and consumesCapacity=true", q)
	}
	if q.Detail != "controller stream ended before a terminal observation" {
		t.Errorf("quarantine detail = %q, want bounded controller-loss detail", q.Detail)
	}
	// Repeated projections must not duplicate the same quarantine or capacity
	// charge while heartbeat/status readers race terminal reconciliation.
	for range 32 {
		if got := d.SessionShimOccupancy(); got != 1 {
			t.Fatalf("repeated occupancy during controller gap = %d, want 1", got)
		}
		if got := len(d.QuarantinedSessions()); got != 1 {
			t.Fatalf("repeated quarantine projection length = %d, want 1", got)
		}
	}
	if got := endCalls.Load(); got != 0 {
		t.Fatalf("SessionEventEnded calls after controller disconnect = %d, want 0", got)
	}

	// Let the shim-owned orphan rule reap the harness so the helper process does
	// not outlive the test. A tombstone is durable proof, but this disconnected
	// daemon did not receive the immutable Exit frame and still must not invent an
	// Ended event.
	registry, err := sessionshim.NewRegistry(f.registry)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	var tombstone sessionshim.Tombstone
	waitFor(t, 10*time.Second, "orphan tombstone after controller gap", func() bool {
		var err error
		tombstone, err = registry.GetTombstone(id)
		return err == nil
	})
	waitFor(t, 5*time.Second, "orphan tombstone publication to withdraw the live record", func() bool {
		_, err := registry.Get(id)
		return err != nil
	})
	if err := registry.RemoveTombstoneIncarnation(tombstone); err != nil {
		t.Fatalf("remove exact tombstone for wrong-epoch control: %v", err)
	}
	wrongEpoch := tombstone
	wrongEpoch.ProcessEpoch++
	if err := registry.PutTombstone(wrongEpoch); err != nil {
		t.Fatalf("publish wrong-epoch tombstone control: %v", err)
	}
	if got := d.SessionShimOccupancy(); got != 1 {
		t.Fatalf("occupancy with wrong-epoch tombstone = %d, want 1", got)
	}
	if got := len(d.QuarantinedSessions()); got != 1 {
		t.Fatalf("wrong-epoch tombstone removed quarantine; projection length = %d, want 1", got)
	}
	if err := registry.PutTombstone(tombstone); err != nil {
		t.Fatalf("restore exact tombstone: %v", err)
	}
	// One owning reconcile FIRST. A projection surface that lands inside the
	// owner's report window correctly still sees the lineage: its obligation is
	// `active` until the terminal evidence is durably accepted, and the
	// composer's completeness cover-set for an active quarantined obligation is
	// the quarantined and cleared sections, so every surface must keep saying
	// "quarantined" until the handoff lands. What the 32 readers below pin is
	// that REPEATED projections never resurrect a withdrawn charge, so they
	// assert the settled answer — after the owning handoff, not across it.
	d.SessionShimOccupancy()
	waitFor(t, 30*time.Second, "the owning reconcile to hand off the restored tombstone", func() bool {
		return d.SessionShimOccupancy() == 0 && len(d.QuarantinedSessions()) == 0
	})
	var reconcileReaders sync.WaitGroup
	reconcileReaders.Add(32)
	for range 32 {
		go func() {
			defer reconcileReaders.Done()
			if got := d.SessionShimOccupancy(); got != 0 {
				t.Errorf("concurrent reconciled occupancy = %d, want 0", got)
			}
			if got := len(d.QuarantinedSessions()); got != 0 {
				t.Errorf("concurrent reconciled quarantine length = %d, want 0", got)
			}
		}()
	}
	reconcileReaders.Wait()
	waitFor(t, 5*time.Second, "safe tombstone quarantine reconciliation", func() bool {
		return d.SessionShimOccupancy() == 0 && len(d.QuarantinedSessions()) == 0
	})
	proof := d.SessionShimTerminalProof(id.OrgID, id.SessionID)
	if !proof.Proves() {
		t.Fatal("terminal tombstone was not retained as durable proof after quarantine reconciliation")
	}
	if got := endCalls.Load(); got != 0 {
		t.Fatalf("SessionEventEnded calls after disconnected orphan completion = %d, want 0", got)
	}
}

// TestShimOwnershipIsOffByDefaultForInteractiveSessions pins §D11's migration
// law at the selection rule: shipping this code must not change who owns a
// terminal until an operator says so.
func TestShimOwnershipIsOffByDefaultForInteractiveSessions(t *testing.T) {
	t.Parallel()

	d := New(Options{SkipRegistration: true})
	spec := SessionSpec{SessionID: "s1", Mode: interactiveRunMode}
	if d.shimOwnsSession(spec) {
		t.Fatal("shim ownership is on by default; §D11 step 1 ships the protocol with ownership OFF")
	}
	handle, err := d.launchSessionShim(spec, ProjectConfig{ID: "p"}, nil)
	if err != nil {
		t.Fatalf("launchSessionShim with ownership disabled: %v", err)
	}
	if handle != nil {
		t.Fatalf("launchSessionShim returned %+v with ownership disabled, want nil (fall through to the direct path)", handle)
	}
}

// TestOnlyInteractiveSessionsAreShimOwned pins the other half of the selection
// rule. The first delivery is interactive-only (§D11): a headless worker that
// dies with its daemon is re-dispatched, a human's terminal is not.
func TestOnlyInteractiveSessionsAreShimOwned(t *testing.T) {
	t.Parallel()

	d := New(Options{
		SkipRegistration: true,
		SessionShim:      SessionShimConfig{EnableOwnership: true},
	})
	cases := []struct {
		mode string
		want bool
	}{
		{mode: interactiveRunMode, want: true},
		{mode: "", want: false},
		{mode: "interview", want: false},
		{mode: "batch", want: false},
	}
	for _, tc := range cases {
		if got := d.shimOwnsSession(SessionSpec{SessionID: "s", Mode: tc.mode}); got != tc.want {
			t.Errorf("shimOwnsSession(mode=%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// TestLaunchFailureFailsTheAcceptClosed proves the spawner does not quietly
// demote a shim-owned session to a daemon-parented child when the launch fails.
// A silent demotion would produce exactly the terminal-dies-on-upgrade behaviour
// the ADR exists to remove, with nothing in the logs saying so.
func TestLaunchFailureFailsTheAcceptClosed(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("/tmp", "dsp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableOwnership: true,
			OrgID:           "test-org",
			RegistryDir:     dir + "/registry",
			LaunchTimeout:   750 * time.Millisecond,
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		// A worker that exits immediately and never publishes a record.
		WorkerCommand:     []string{"/bin/sh", "-c", "exit 0"},
		WorktreeParentDir: dir,
	})
	d.spawner.opts.ShimSpawn = d.launchSessionShim
	d.spawner.Resume()

	handle, err := d.spawner.AcceptWork(SessionSpec{
		SessionID: "sess-fail", ProjectID: "p1",
		Repository: "https://example.invalid/x/y", Mode: interactiveRunMode,
	})
	if err == nil {
		t.Fatalf("AcceptWork = %+v, nil; a shim launch that never announced itself must fail the accept", handle)
	}
	if !strings.Contains(err.Error(), "session shim") {
		t.Errorf("error %q does not name the shim launch as the cause", err)
	}
	if got := d.SessionShimOccupancy(); got != 0 {
		t.Errorf("SessionShimOccupancy after a failed launch = %d, want 0", got)
	}
	if _, tracked := d.spawner.sessions["sess-fail"]; tracked {
		t.Error("a failed shim launch left a direct-child entry behind")
	}
}

// TestLaunchEnvironmentCarriesTheContractAndNoSecrets pins §D6's no-secret bound
// at the carrier the daemon actually writes. The launch env is visible in the
// process table, so anything secret here would be leaked by the carrier itself.
func TestLaunchEnvironmentCarriesTheContractAndNoSecrets(t *testing.T) {
	t.Parallel()

	launch := sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "o", SessionID: "s"},
		RegistryDir:  "/tmp/reg",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 3,
	}
	pairs := envPairs(launch.Env())
	joined := strings.Join(pairs, "\n")
	for _, key := range sessionshim.EnvKeys() {
		if !strings.Contains(joined, key+"=") {
			t.Errorf("launch environment is missing %s", key)
		}
	}
	// Sorted, so a spawn environment is byte-stable across runs.
	for i := 1; i < len(pairs); i++ {
		if pairs[i-1] > pairs[i] {
			t.Fatalf("launch environment is not sorted: %q before %q", pairs[i-1], pairs[i])
		}
	}
	// Round trip: the only producer and the only consumer must agree.
	env := launch.Env()
	got, err := sessionshim.LaunchFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("LaunchFromEnv on the daemon's own overlay: %v", err)
	}
	if got.Identity != launch.Identity || got.RegistryDir != launch.RegistryDir ||
		got.ProcessEpoch != launch.ProcessEpoch || got.Orphan != launch.Orphan {
		t.Fatalf("round trip = %+v, want %+v", got, launch)
	}
}

// TestForwardedSequenceIsRecordedNotAllocated pins §D5's division of labour: the
// daemon records the highest sequence it forwarded so a LATER adoption can
// resume from it, and never allocates one itself.
func TestForwardedSequenceIsRecordedNotAllocated(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-seq")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-seq")
	f.exchange(t, id, "seq")

	waitFor(t, 20*time.Second, "the daemon to record a forwarded sequence", func() bool {
		return d.SessionShimForwardedSeq(id.OrgID, id.SessionID) > 0
	})
	if got := d.SessionShimForwardedSeq(id.OrgID, "not-a-session"); got != 0 {
		t.Errorf("forwarded sequence for an unknown session = %d, want 0", got)
	}
}

func TestForwardedSequenceRequiresDurableCarrier(t *testing.T) {
	f := newShimSpawnFixture(t)
	// The ordinary event hook is intentionally still present: it is an observer,
	// not proof that a composing carrier durably accepted the frame.
	f.daemon.opts.SessionShim.OnSessionEventDurable = nil

	spec := f.interactiveSpec("sess-observer-only")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)
	seq := f.exchange(t, id, "observer-only")
	if seq == 0 {
		t.Fatal("observer did not receive output")
	}
	time.Sleep(250 * time.Millisecond)
	if got := f.daemon.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != 0 {
		t.Fatalf("observer-only forwarded sequence = %d, want durable cursor unchanged at 0", got)
	}
}

func TestForwardedSequenceRejectsDurableCarrierError(t *testing.T) {
	f := newShimSpawnFixture(t)
	f.daemon.opts.SessionShim.OnSessionEventDurable = func(sessionshim.Identity, sessionshim.ControllerEvent) error {
		return errors.New("carrier unavailable")
	}

	spec := f.interactiveSpec("sess-carrier-error")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)
	if err := f.daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("carrier-error\r")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInput: %v", err)
	}
	waitFor(t, 5*time.Second, "the observer to receive the rejected frame", func() bool {
		out, _ := f.events.output(id)
		return strings.Contains(out, "carrier-error")
	})
	if got := f.daemon.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != 0 {
		t.Fatalf("carrier-error forwarded sequence = %d, want durable cursor unchanged at 0", got)
	}
}

// TestRestartFenceRetainsTheAdoptionResumeCursor pins the replacement-daemon
// seam: before any new output arrives, the fence must still report the durable
// last-forwarded sequence from which this controller resumed. Resetting it to
// zero would make the composing store acknowledge a correlation older than the
// carrier's durable state.
func TestRestartFenceRetainsTheAdoptionResumeCursor(t *testing.T) {
	f := newShimSpawnFixture(t)
	first := f.daemon

	if _, err := first.spawner.AcceptWork(f.interactiveSpec("sess-resume-fence")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-resume-fence")
	f.exchange(t, id, "resume-fence")
	waitFor(t, 20*time.Second, "the first daemon to record forwarded output", func() bool {
		return first.SessionShimForwardedSeq(id.OrgID, id.SessionID) > 0
	})
	lastForwarded := first.SessionShimForwardedSeq(id.OrgID, id.SessionID)
	first.ReleaseAdoptedSessionShims()

	replacement := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true,
			OrgID:          f.orgID,
			RegistryDir:    f.registry,
			ResumeFrom: func(orgID, sessionID string) uint64 {
				if orgID == id.OrgID && sessionID == id.SessionID {
					return lastForwarded + 1
				}
				return 0
			},
			Orphan: sessionshim.OrphanPolicy{
				Deadline:          2 * time.Second,
				TerminationGrace:  500 * time.Millisecond,
				PropagationMargin: 0,
			},
		},
	})
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)
	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("replacement adoptSessionShims: %v", err)
	}

	fence, err := replacement.RequestSessionShimRestartFence(context.Background(), "fence-resume")
	if err != nil {
		t.Fatalf("RequestSessionShimRestartFence: %v", err)
	}
	if len(fence.Sessions) != 1 {
		t.Fatalf("fence sessions = %+v, want one resumed session", fence.Sessions)
	}
	if got := fence.Sessions[0].LastForwardedSeq; got != lastForwarded {
		t.Fatalf("fence lastForwardedSeq = %d, want durable adoption cursor %d", got, lastForwarded)
	}
}

func TestStartupAdoptionReleasesEachScopeOnlyAfterExactHeartbeat(t *testing.T) {
	f := newShimSpawnFixture(t)
	f.daemon.opts.SessionShim.Orphan.Deadline = 15 * time.Second
	const primaryOrg, satelliteOrg, emptyOrg = "org-restart-primary", "org-restart-satellite", "org-restart-empty"
	for i, orgID := range []string{primaryOrg, satelliteOrg} {
		spec := f.interactiveSpec(fmt.Sprintf("restart-multi-%d", i+1))
		spec.OrganizationID = orgID
		if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
			t.Fatalf("launch %s: %v", orgID, err)
		}
	}
	f.daemon.ReleaseAdoptedSessionShims()

	var (
		epoch        atomic.Uint64
		snapshotMu   sync.Mutex
		snapshotSeqs = make(map[sessionshim.Identity]uint64)
		releases     []SessionShimPublishedBatchReceipt
	)
	epoch.Store(100)
	replacement := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		EnableAdoption: true, RegistryDir: f.registry, HostID: "host-restart-multi", OrgID: primaryOrg,
		AdoptionBatchOrgIDs:          []string{primaryOrg, satelliteOrg, emptyOrg},
		RequireAuthoritativeSnapshot: true, RequireCredentialAttestation: true,
		GetCarrierProofV2Readiness: testSessionShimProofV2Readiness,
		AttestationCapabilities:    RequiredSessionShimHostCapabilities(),
		PrepareAdoption: func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
			return sessionshim.PreparedAdoption{
				ControllerGeneration: preparation.CurrentControllerGeneration + 1,
				Extensions: shimwire.Extensions{Values: map[string]string{
					shimwire.ExtCarrierEpoch: fmt.Sprintf("%d", epoch.Add(1)),
				}},
				ResumeFrom: proofResolvedResume(preparation),
			}, nil
		},
		OnAdoption: func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
			result, err := evidence.SnapshotProxy.Emit(ctx)
			if err != nil {
				return SessionShimAdoptionReceipt{}, err
			}
			snapshotMu.Lock()
			snapshotSeqs[evidence.Identity] = result.AtSeq + 1
			snapshotMu.Unlock()
			return SessionShimAdoptionReceipt{DurableCorrelation: []byte("restart-multi-" + evidence.Identity.Key())}, nil
		},
		OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
		OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
			wantAdopted := 1
			if batch.OrgID == emptyOrg {
				wantAdopted = 0
			}
			if len(batch.Adopted) != wantAdopted {
				return SessionShimAdoptionBatchReceipt{}, fmt.Errorf("startup batch %s adopted = %d, want %d", batch.OrgID, len(batch.Adopted), wantAdopted)
			}
			return SessionShimAdoptionBatchReceipt{
				DurableCorrelation: []byte("restart-multi-batch-" + batch.OrgID),
				AdoptionRevision:   "restart-multi-revision-" + batch.OrgID,
			}, nil
		},
		OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
			if len(publication.Carriers) != 2 {
				return nil, fmt.Errorf("startup activation carriers = %d, want 2", len(publication.Carriers))
			}
			receipts := make([]SessionShimCarrierActivationReceipt, 0, len(publication.Carriers))
			snapshotMu.Lock()
			defer snapshotMu.Unlock()
			for _, carrier := range publication.Carriers {
				id := sessionshim.Identity{OrgID: carrier.OrgID, SessionID: carrier.SessionID}
				receipts = append(receipts, SessionShimCarrierActivationReceipt{
					Activation: carrier, AckSeq: snapshotSeqs[id],
				})
			}
			return receipts, nil
		},
		OnCarrierActivationAcknowledged: func(receipt SessionShimPublishedBatchReceipt) {
			snapshotMu.Lock()
			releases = append(releases, receipt)
			snapshotMu.Unlock()
		},
	}})
	enableHostedFullHostFramesForTest(t, replacement, primaryOrg, satelliteOrg, emptyOrg)
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)
	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("multi-session startup adoption: %v", err)
	}
	adopted := replacement.AdoptedSessionShims()
	if len(adopted) != 2 || !replacement.SessionShimCarrierActivationComplete() {
		t.Fatalf("multi-session startup result = adopted:%+v active:%v", adopted, replacement.SessionShimCarrierActivationComplete())
	}
	for _, id := range adopted {
		entry, err := replacement.adoptedShimEntry(id.OrgID, id.SessionID)
		if err != nil || !entry.carrierActivationResolved || replacement.SessionShimForwardedSeq(id.OrgID, id.SessionID) == 0 {
			t.Errorf("startup carrier %s = entry:%+v forwarded:%d err:%v",
				id, entry, replacement.SessionShimForwardedSeq(id.OrgID, id.SessionID), err)
		}
	}
	replacement.setState(StateRecovering)
	primaryProjection, err := replacement.SessionShimHeartbeatProjection(primaryOrg)
	if err != nil {
		t.Fatalf("primary startup heartbeat projection: %v", err)
	}
	stalePrimary := primaryProjection
	stalePrimary.AdoptionRevision = "stale-startup-revision"
	replacement.AcknowledgeSessionShimRecoveryHeartbeat(primaryOrg, stalePrimary)
	snapshotMu.Lock()
	if len(releases) != 0 {
		t.Fatalf("stale startup heartbeat released scopes: %+v", releases)
	}
	snapshotMu.Unlock()
	replacement.AcknowledgeSessionShimRecoveryHeartbeat(primaryOrg, primaryProjection)
	snapshotMu.Lock()
	primaryReleases := append([]SessionShimPublishedBatchReceipt(nil), releases...)
	snapshotMu.Unlock()
	if len(primaryReleases) != 1 || primaryReleases[0].Scope != primaryOrg ||
		replacement.State() != StateRecovering || !replacement.sessionShimReadinessWithdrawn.Load() {
		t.Fatalf("primary-only startup release = releases:%+v state:%s blocked:%v",
			primaryReleases, replacement.State(), replacement.sessionShimReadinessWithdrawn.Load())
	}
	satelliteProjection, err := replacement.SessionShimHeartbeatProjection(satelliteOrg)
	if err != nil {
		t.Fatalf("satellite startup heartbeat projection: %v", err)
	}
	replacement.AcknowledgeSessionShimRecoveryHeartbeat(satelliteOrg, satelliteProjection)
	snapshotMu.Lock()
	carrierScopeReleases := append([]SessionShimPublishedBatchReceipt(nil), releases...)
	snapshotMu.Unlock()
	if len(carrierScopeReleases) != 2 || carrierScopeReleases[1].Scope != satelliteOrg ||
		replacement.State() != StateRecovering || !replacement.sessionShimReadinessWithdrawn.Load() {
		t.Fatalf("carrier-scope startup releases = releases:%+v state:%s blocked:%v",
			carrierScopeReleases, replacement.State(), replacement.sessionShimReadinessWithdrawn.Load())
	}
	emptyProjection, err := replacement.SessionShimHeartbeatProjection(emptyOrg)
	if err != nil {
		t.Fatalf("empty-scope startup heartbeat projection: %v", err)
	}
	replacement.AcknowledgeSessionShimRecoveryHeartbeat(emptyOrg, emptyProjection)
	snapshotMu.Lock()
	allReleases := append([]SessionShimPublishedBatchReceipt(nil), releases...)
	snapshotMu.Unlock()
	if len(allReleases) != 3 || allReleases[2].Scope != emptyOrg ||
		replacement.State() != StateRunning || replacement.sessionShimReadinessWithdrawn.Load() {
		t.Fatalf("complete startup release = releases:%+v state:%s blocked:%v",
			allReleases, replacement.State(), replacement.sessionShimReadinessWithdrawn.Load())
	}
}

func TestStartupAdoptionRefusesReadyUntilDurableCarrierRehydration(t *testing.T) {
	f := newShimSpawnFixture(t)
	// Give two replacement attempts ample room before the shim-owned orphan
	// deadline. The first is deliberately refused by the composing callback.
	f.daemon.opts.SessionShim.Orphan.Deadline = 15 * time.Second
	spec := f.interactiveSpec("sess-startup-callback")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)
	f.daemon.ReleaseAdoptedSessionShims()

	refusing := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true,
			RegistryDir:    f.registry,
			HostID:         "host-startup",
			OnAdoption: func(context.Context, SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
				return SessionShimAdoptionReceipt{}, errors.New("durable carrier unavailable")
			},
		},
	})
	refusing.config = &Config{Capacity: CapacityConfig{MaxConcurrentSessions: 4}}
	refusing.setState(StateRunning)
	err := refusing.adoptSessionShims(context.Background())
	if err == nil || !strings.Contains(err.Error(), "durable carrier unavailable") {
		t.Fatalf("adoptSessionShims = %v, want durable carrier refusal", err)
	}
	if refusing.SessionShimAdoptionComplete() {
		t.Fatal("adoption reads complete after durable carrier refusal")
	}
	if got := refusing.RegistrationStatus(); got != RegistrationDraining {
		t.Fatalf("RegistrationStatus after callback refusal = %q, want draining", got)
	}

	var emitted shimwire.SnapshotResult
	var replacement *Daemon
	replacement = New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption:               true,
			RegistryDir:                  f.registry,
			HostID:                       "host-startup",
			RequireAuthoritativeSnapshot: true,
			RequireCredentialAttestation: true,
			GetCarrierProofV2Readiness:   testSessionShimProofV2Readiness,
			AttestationCapabilities:      RequiredSessionShimHostCapabilities(),
			PrepareAdoption: func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
				return sessionshim.PreparedAdoption{
					Extensions: shimwire.Extensions{
						Values: map[string]string{shimwire.ExtCarrierEpoch: "20"},
					}, ControllerGeneration: preparation.CurrentControllerGeneration + 1,
					ResumeFrom: proofResolvedResume(preparation),
				}, nil
			},
			OnAdoption: func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
				if evidence.Identity != id {
					return SessionShimAdoptionReceipt{}, fmt.Errorf("wrong identity %s", evidence.Identity)
				}
				var err error
				emitted, err = evidence.SnapshotProxy.Emit(ctx)
				if err != nil {
					return SessionShimAdoptionReceipt{}, err
				}
				return SessionShimAdoptionReceipt{DurableCorrelation: []byte("durable-startup")}, nil
			},
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
			PrepareAdoptionBatch: func(context.Context, string, string) ([]byte, error) {
				return []byte("expected-startup-batch"), nil
			},
			OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				if len(batch.Adopted) != 1 || batch.Adopted[0].Evidence.SnapshotProxy != nil {
					return SessionShimAdoptionBatchReceipt{}, fmt.Errorf("batch retained ephemeral snapshot proxy: %+v", batch.Adopted)
				}
				return SessionShimAdoptionBatchReceipt{
					DurableCorrelation: []byte("startup-batch-revision"), AdoptionRevision: "startup-revision",
				}, nil
			},
			OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
				if _, err := replacement.adoptedShimEntry(id.OrgID, id.SessionID); err != nil {
					return nil, fmt.Errorf("activation before publication: %w", err)
				}
				return []SessionShimCarrierActivationReceipt{{
					Activation: publication.Carriers[0], AckSeq: emitted.AtSeq + 1,
				}}, nil
			},
		},
	})
	enableHostedFullHostFramesForTest(t, replacement, "test-org")
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)
	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("replacement adoptSessionShims: %v", err)
	}
	if !replacement.SessionShimAdoptionComplete() {
		t.Fatal("adoption did not complete after durable carrier handoff")
	}
	entry, err := replacement.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if got, _ := entry.adoption.Extensions.Get(shimwire.ExtCarrierEpoch); got != "20" {
		t.Fatalf("replacement adoption carrier_epoch = %q, want 20", got)
	}
	if err := replacement.StopAdoptedSessionShim(id.OrgID, id.SessionID, shimwire.StopHostShutdown); err != nil {
		t.Fatalf("StopAdoptedSessionShim: %v", err)
	}
	waitFor(t, 30*time.Second, "replacement terminal cleanup", func() bool {
		return replacement.SessionShimOccupancy() == 0
	})
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// TestAdmissionRefusesWorkAgainstShimHeldCapacity closes the double-booking gap
// §D7 opens at the ADMISSION boundary rather than the advertisement one.
//
// A shim-owned session never enters the spawner's own registry by design, so a
// host that reported its occupancy honestly and then admitted against its
// direct-child count alone would still accept work it has no core to run.
func TestAdmissionRefusesWorkAgainstShimHeldCapacity(t *testing.T) {
	t.Parallel()

	held := 0
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30"},
		ExternalOccupancy:     func() int { return held },
	})
	s.Resume()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.DrainContext(ctx)
	})

	spec := func(id string) SessionSpec {
		return SessionSpec{SessionID: id, ProjectID: "p1", Repository: "https://example.invalid/x/y"}
	}

	// Two shims already hold this host's whole envelope. Nothing may be admitted,
	// even though the spawner parents no children at all.
	held = 2
	if _, err := s.AcceptWork(spec("s1")); err == nil {
		t.Fatal("AcceptWork succeeded while shims held every slot on the host")
	}

	// One slot frees up; exactly one session fits, and the next is refused.
	held = 1
	if _, err := s.AcceptWork(spec("s2")); err != nil {
		t.Fatalf("AcceptWork with one free slot: %v", err)
	}
	if _, err := s.AcceptWork(spec("s3")); err == nil {
		t.Fatal("AcceptWork admitted a third session into a two-slot host")
	}
}

func TestStatusAndDoctorExposeRealSecretFreeSessionShimDiagnostics(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	if err := d.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("initial adoption pass: %v", err)
	}
	d.config = DefaultConfig()
	d.setState(StateRunning)
	id := f.identity("diagnostic-live")
	if _, err := d.AcceptWork(f.interactiveSpec(id.SessionID)); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	seq := f.exchange(t, id, "diagnostic-output")
	d.shims.mu.Lock()
	d.shims.quarantined = append(d.shims.quarantined, sessionshim.QuarantinedSession{
		OrgID: "test-org", SessionID: "diagnostic-quarantine", ShimID: "shim-quarantine",
		ProcessEpoch: 9, ControllerGeneration: 11, ProtocolMin: 1, ProtocolMax: 1,
		Reason: sessionshim.QuarantineDuplicateIdentity, Detail: "socket /private/secret/path",
		AgeSeconds: 3, ConsumesCapacity: true,
	})
	d.shims.mu.Unlock()

	server := NewServer(d)
	statusRecorder := httptest.NewRecorder()
	server.handleStatus(statusRecorder, httptest.NewRequest("GET", "/api/daemon/status", nil))
	var status afclient.DaemonStatusResponse
	if err := json.NewDecoder(statusRecorder.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	diagnostic := status.SessionShim
	if diagnostic.OwnershipMode != afclient.DaemonSessionShimAdoptionAndOwnership ||
		!diagnostic.AdoptionComplete || diagnostic.AdoptionCompletedAt == "" || diagnostic.OccupiedSlots != 2 {
		t.Fatalf("status sessionShim summary = %+v", diagnostic)
	}
	if len(diagnostic.Adopted) != 1 {
		t.Fatalf("status adopted = %+v, want one", diagnostic.Adopted)
	}
	adopted := diagnostic.Adopted[0]
	if adopted.OrgID != id.OrgID || adopted.SessionID != id.SessionID || adopted.ShimID == "" ||
		adopted.ProcessEpoch == 0 || adopted.ControllerGeneration == 0 || adopted.LastForwardedSeq != seq ||
		adopted.HarnessPID <= 0 || adopted.HarnessStartedAt <= 0 || adopted.ProtocolMin != 1 || adopted.ProtocolMax != 3 ||
		adopted.ProtocolVersion != 2 || !adopted.AuthoritativeSnapshot || adopted.ControllerID != d.ControllerID() ||
		adopted.Phase == "" || !adopted.ConsumesCapacity || diagnostic.ControllerID != d.ControllerID() {
		t.Fatalf("status adopted correlation = %+v", adopted)
	}
	if len(diagnostic.Quarantined) != 1 || diagnostic.Quarantined[0].ShimID != "shim-quarantine" ||
		diagnostic.Quarantined[0].ControllerGeneration != 11 ||
		diagnostic.Quarantined[0].Detail != "" || !diagnostic.Quarantined[0].ConsumesCapacity {
		t.Fatalf("status quarantine = %+v", diagnostic.Quarantined)
	}
	raw, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"hostId"`, `"workarea"`, `"token"`, `"receipt"`, `"path"`, `"data"`} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("session-shim diagnostic contains forbidden field %s: %s", forbidden, raw)
		}
	}

	doctorRecorder := httptest.NewRecorder()
	server.handleDoctor(doctorRecorder, httptest.NewRequest("GET", "/api/daemon/doctor", nil))
	var doctor struct {
		SessionShim afclient.DaemonSessionShimStatus `json:"sessionShim"`
	}
	if err := json.NewDecoder(doctorRecorder.Body).Decode(&doctor); err != nil {
		t.Fatalf("decode doctor: %v", err)
	}
	if !reflect.DeepEqual(doctor.SessionShim, diagnostic) {
		t.Fatalf("doctor/status session-shim drift:\ndoctor=%+v\nstatus=%+v", doctor.SessionShim, diagnostic)
	}
}
