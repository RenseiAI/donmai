package sessionshim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestReleaseDecisionNeverReleasesOnExpiryAlone(t *testing.T) {
	t.Parallel()

	// This is the single most important table in the package. §D10: "fence expiry
	// must never release a claim while the harness may still be running." Every
	// row where proof is absent must resolve to held or reconcile — NEVER allowed.
	now := time.Unix(1_700_000_000, 0)
	id := Identity{OrgID: "org-1", SessionID: "sess-1"}
	covered := []FencedSession{{OrgID: id.OrgID, SessionID: id.SessionID, ShimID: "shim-1"}}

	held := Fence{
		FenceID: "f1", Sessions: covered, State: FenceHeld,
		IssuedAtUnixNano:  now.Add(-time.Minute).UnixNano(),
		HoldUntilUnixNano: now.Add(time.Minute).UnixNano(),
	}
	expired := Fence{
		FenceID: "f1", Sessions: covered, State: FenceHeld,
		IssuedAtUnixNano:  now.Add(-time.Hour).UnixNano(),
		HoldUntilUnixNano: now.Add(-time.Minute).UnixNano(),
	}
	other := Fence{
		FenceID: "f2", State: FenceHeld,
		Sessions:          []FencedSession{{OrgID: "org-1", SessionID: "somebody-else"}},
		HoldUntilUnixNano: now.Add(time.Minute).UnixNano(),
	}
	reapedTombstone := &Tombstone{
		OrgID: id.OrgID, SessionID: id.SessionID, ShimID: "shim-1", GroupReaped: true,
	}
	unreapedTombstone := &Tombstone{
		OrgID: id.OrgID, SessionID: id.SessionID, ShimID: "shim-1", GroupReaped: false,
	}
	wrongTombstone := &Tombstone{
		OrgID: id.OrgID, SessionID: "different-session", ShimID: "shim-1", GroupReaped: true,
	}

	cases := []struct {
		name  string
		fence *Fence
		proof TerminalProof
		want  ReleaseVerdict
	}{
		{
			name:  "adopted receipt releases even under a live fence",
			fence: &held, proof: TerminalProof{AdoptedReceipt: true}, want: ReleaseAllowed,
		},
		{
			name:  "reaped tombstone releases",
			fence: &expired, proof: TerminalProof{Tombstone: reapedTombstone}, want: ReleaseAllowed,
		},
		{
			name:  "live fence, no proof, holds",
			fence: &held, want: ReleaseHeld,
		},
		{
			// The load-bearing row. Time passed; nothing observed the harness stop.
			name:  "EXPIRED fence, no proof, does NOT release",
			fence: &expired, want: ReleaseReconcile,
		},
		{
			name: "no fence at all, no proof, does NOT release",
			want: ReleaseReconcile,
		},
		{
			// A fence that does not name this session protects nothing about it.
			name:  "fence covering a different session does not hold this one",
			fence: &other, want: ReleaseReconcile,
		},
		{
			// A tombstone that could not PROVE the reap is not proof.
			name:  "unreaped tombstone is not proof",
			fence: &expired, proof: TerminalProof{Tombstone: unreapedTombstone}, want: ReleaseReconcile,
		},
		{
			name:  "wrong tombstone cannot release sole fenced correlation",
			fence: &held, proof: TerminalProof{Tombstone: wrongTombstone}, want: ReleaseHeld,
		},
		{
			name: "already-reconciling fence no longer holds",
			fence: &Fence{
				FenceID: "f1", Sessions: covered, State: FenceReconciliationRequired,
				HoldUntilUnixNano: now.Add(time.Minute).UnixNano(),
			},
			want: ReleaseReconcile,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ReleaseDecision(tc.fence, id, tc.proof, now); got != tc.want {
				t.Fatalf("ReleaseDecision = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReleaseDecisionRequiresEveryDuplicateCorrelationProof(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	id := Identity{OrgID: "org-duplicate", SessionID: "session-duplicate"}
	fence := Fence{
		FenceID: "fence-duplicate",
		State:   FenceHeld,
		Sessions: []FencedSession{
			{OrgID: id.OrgID, SessionID: id.SessionID, ShimID: "shim-a", ProcessEpoch: 1},
			{OrgID: id.OrgID, SessionID: id.SessionID, ShimID: "shim-b", ProcessEpoch: 2},
		},
		HoldUntilUnixNano: now.Add(time.Minute).UnixNano(),
	}
	tombstoneA := Tombstone{
		OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-a", ProcessEpoch: 1, GroupReaped: true,
	}
	proof := TerminalProof{Correlations: []TerminalCorrelationProof{{
		ShimID: "shim-a", ProcessEpoch: 1, Tombstone: &tombstoneA,
	}}}
	if got := ReleaseDecision(&fence, id, proof, now); got != ReleaseHeld {
		t.Fatalf("one of two proofs under held fence = %q, want held", got)
	}
	expired := fence
	expired.HoldUntilUnixNano = now.Add(-time.Second).UnixNano()
	if got := ReleaseDecision(&expired, id, proof, now); got != ReleaseReconcile {
		t.Fatalf("one of two proofs after expiry = %q, want reconciliation", got)
	}
	tombstoneB := Tombstone{
		OrgID: id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-b", ProcessEpoch: 2, GroupReaped: true,
	}
	proof.Correlations = append(proof.Correlations, TerminalCorrelationProof{
		ShimID: "shim-b", ProcessEpoch: 2, Tombstone: &tombstoneB,
	})
	if got := ReleaseDecision(&fence, id, proof, now); got != ReleaseAllowed {
		t.Fatalf("both exact proofs = %q, want allowed", got)
	}
}

func TestReconcileNeverReachesAReleasedState(t *testing.T) {
	t.Parallel()

	// There is deliberately no transition in this package that releases a claim.
	// Expiry moves held → reconciliation_required and stops there.
	now := time.Unix(1_700_000_000, 0)
	f := Fence{
		FenceID: "f1", State: FenceHeld,
		HoldUntilUnixNano: now.Add(-time.Second).UnixNano(),
	}
	got := f.Reconcile(now)
	if got.State != FenceReconciliationRequired {
		t.Fatalf("Reconcile(expired) state = %q, want %q", got.State, FenceReconciliationRequired)
	}

	// Reconciling an unexpired fence is a no-op.
	live := Fence{FenceID: "f1", State: FenceHeld, HoldUntilUnixNano: now.Add(time.Minute).UnixNano()}
	if got := live.Reconcile(now); got.State != FenceHeld {
		t.Fatalf("Reconcile(live) state = %q, want %q", got.State, FenceHeld)
	}
}

func TestFenceExpiredIsNotAReleaseSignal(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	f := Fence{HoldUntilUnixNano: now.Add(-time.Second).UnixNano()}
	if !f.Expired(now) {
		t.Fatal("Expired = false for a past hold window")
	}
	// A fence with no hold window has not "expired" — it was never armed.
	if (Fence{}).Expired(now) {
		t.Fatal("an unarmed fence reported itself expired")
	}
}

func TestRequestFenceIsLocallySatisfiedWithoutAStore(t *testing.T) {
	t.Parallel()

	// §D9: an OSS-only daemon has no remote reaper and therefore needs no hosted
	// fence. A nil store must be a fully supported configuration, not a failure.
	now := time.Unix(1_700_000_000, 0)
	policy := FencePolicy{RestartBudget: time.Minute, Orphan: DefaultOrphanPolicy()}
	ids := []FencedSession{{OrgID: "o", SessionID: "s"}}

	f, err := RequestFence(context.Background(), nil, "fence-1", "host-1", ids, policy, now)
	if err != nil {
		t.Fatalf("RequestFence with nil store: %v", err)
	}
	if f.State != FenceHeld || !f.Covers(Identity{OrgID: "o", SessionID: "s"}) {
		t.Fatalf("locally satisfied fence = %+v", f)
	}
	if f.DurableRevision != "" {
		t.Fatalf("nil-store fence durable revision = %q, want empty (local intent is not remote durability)", f.DurableRevision)
	}
	// The hold window must cover the restart budget AND the whole orphan bound —
	// a fence sized to the restart alone expires when a slow restart needs it most.
	wantHold := policy.RestartBudget + DefaultOrphanPolicy().TotalBound()
	if got := f.HoldUntil().Sub(now); got != wantHold {
		t.Fatalf("hold window = %s, want %s", got, wantHold)
	}
}

func TestRequestFencePreservesLegacyFenceStoreCompatibility(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	ids := []FencedSession{
		{OrgID: "o", SessionID: "z"},
		{OrgID: "o", SessionID: "a"},
	}
	store := legacyFenceStoreFunc(func(_ context.Context, fence Fence) (Fence, error) {
		if len(fence.Sessions) != 2 || fence.Sessions[0].SessionID != "a" || fence.Sessions[1].SessionID != "z" {
			return Fence{}, errors.New("legacy store did not receive v0.67 sorted coverage")
		}
		return fence, nil
	})
	f, err := RequestFence(context.Background(), store, "fence-legacy", "host-1", ids, FencePolicy{}, now)
	if err != nil {
		t.Fatalf("RequestFence with legacy store: %v", err)
	}
	if f.DurableRevision != "" {
		t.Fatalf("legacy fence durable revision = %q, want empty", f.DurableRevision)
	}
}

type exactFenceStoreFunc func(context.Context, FenceRequest) (FenceAcknowledgement, error)

func (f exactFenceStoreFunc) AcknowledgeExact(ctx context.Context, request FenceRequest) (FenceAcknowledgement, error) {
	return f(ctx, request)
}

type legacyFenceStoreFunc func(context.Context, Fence) (Fence, error)

func (f legacyFenceStoreFunc) Acknowledge(ctx context.Context, fence Fence) (Fence, error) {
	return f(ctx, fence)
}

func exactDurableAcknowledgement(request FenceRequest) FenceAcknowledgement {
	return FenceAcknowledgement{
		RequestBytes:    append([]byte(nil), request.RequestBytes...),
		DurableRevision: "revision-1",
	}
}

func TestRequestFenceRefusesPartialAcknowledgement(t *testing.T) {
	t.Parallel()

	// A store that acknowledges a SUBSET has not agreed to protect the rest.
	// Treating that as success is how a release path forgets the fence for one
	// session — the ADR's named top risk.
	now := time.Unix(1_700_000_000, 0)
	ids := []FencedSession{
		{OrgID: "o", SessionID: "s1"},
		{OrgID: "o", SessionID: "s2"},
	}
	store := exactFenceStoreFunc(func(_ context.Context, request FenceRequest) (FenceAcknowledgement, error) {
		partial := request.Fence
		partial.Sessions = partial.Sessions[:1] // drops s2
		partialBytes, err := json.Marshal(partial)
		if err != nil {
			return FenceAcknowledgement{}, err
		}
		return FenceAcknowledgement{RequestBytes: partialBytes, DurableRevision: "revision-1"}, nil
	})
	_, err := RequestFenceExact(context.Background(), store, "fence-1", "host-1", ids, FencePolicy{}, now)
	if !errors.Is(err, ErrFenceRequired) {
		t.Fatalf("RequestFence with partial ack = %v, want ErrFenceRequired", err)
	}
}

func TestRequestFenceRefusesReorderedAcknowledgement(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	ids := []FencedSession{
		{OrgID: "o", SessionID: "s1", ProcessEpoch: 1, LastForwardedSeq: 10},
		{OrgID: "o", SessionID: "s2", ProcessEpoch: 2, LastForwardedSeq: 20},
	}
	store := exactFenceStoreFunc(func(_ context.Context, request FenceRequest) (FenceAcknowledgement, error) {
		reordered := request.Fence
		reordered.Sessions = []FencedSession{request.Fence.Sessions[1], request.Fence.Sessions[0]}
		reorderedBytes, err := json.Marshal(reordered)
		if err != nil {
			return FenceAcknowledgement{}, err
		}
		return FenceAcknowledgement{RequestBytes: reorderedBytes, DurableRevision: "revision-1"}, nil
	})
	_, err := RequestFenceExact(context.Background(), store, "fence-1", "host-1", ids, FencePolicy{}, now)
	if !errors.Is(err, ErrFenceRequired) {
		t.Fatalf("RequestFence with reordered ack = %v, want ErrFenceRequired", err)
	}
}

func TestRequestFenceRefusesMismatchedOrFailedAcknowledgement(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	ids := []FencedSession{{
		OrgID: "o", SessionID: "s1", ShimID: "shim-1", ProcessEpoch: 7, LastForwardedSeq: 41,
	}}

	t.Run("store error", func(t *testing.T) {
		t.Parallel()
		store := exactFenceStoreFunc(func(context.Context, FenceRequest) (FenceAcknowledgement, error) {
			return FenceAcknowledgement{}, errors.New("control plane unreachable")
		})
		_, err := RequestFenceExact(context.Background(), store, "f1", "h1", ids, FencePolicy{}, now)
		if !errors.Is(err, ErrFenceRequired) {
			t.Fatalf("RequestFence = %v, want ErrFenceRequired (the caller must refuse the restart)", err)
		}
	})

	t.Run("wrong fence id", func(t *testing.T) {
		t.Parallel()
		store := exactFenceStoreFunc(func(_ context.Context, request FenceRequest) (FenceAcknowledgement, error) {
			changed := request.Fence
			changed.FenceID = "someone-elses-fence"
			changedBytes, err := json.Marshal(changed)
			return FenceAcknowledgement{RequestBytes: changedBytes, DurableRevision: "revision-1"}, err
		})
		_, err := RequestFenceExact(context.Background(), store, "f1", "h1", ids, FencePolicy{}, now)
		if !errors.Is(err, ErrFenceRequired) {
			t.Fatalf("RequestFence = %v, want ErrFenceRequired", err)
		}
	})

	t.Run("wrong process epoch", func(t *testing.T) {
		t.Parallel()
		store := exactFenceStoreFunc(func(_ context.Context, request FenceRequest) (FenceAcknowledgement, error) {
			changed := request.Fence
			changed.Sessions = append([]FencedSession(nil), changed.Sessions...)
			changed.Sessions[0].ProcessEpoch++
			changedBytes, err := json.Marshal(changed)
			return FenceAcknowledgement{RequestBytes: changedBytes, DurableRevision: "revision-1"}, err
		})
		_, err := RequestFenceExact(context.Background(), store, "f1", "h1", ids, FencePolicy{}, now)
		if !errors.Is(err, ErrFenceRequired) {
			t.Fatalf("RequestFence with wrong process epoch = %v, want ErrFenceRequired", err)
		}
	})

	t.Run("wrong last forwarded sequence", func(t *testing.T) {
		t.Parallel()
		store := exactFenceStoreFunc(func(_ context.Context, request FenceRequest) (FenceAcknowledgement, error) {
			changed := request.Fence
			changed.Sessions = append([]FencedSession(nil), changed.Sessions...)
			changed.Sessions[0].LastForwardedSeq++
			changedBytes, err := json.Marshal(changed)
			return FenceAcknowledgement{RequestBytes: changedBytes, DurableRevision: "revision-1"}, err
		})
		_, err := RequestFenceExact(context.Background(), store, "f1", "h1", ids, FencePolicy{}, now)
		if !errors.Is(err, ErrFenceRequired) {
			t.Fatalf("RequestFence with wrong forwarded sequence = %v, want ErrFenceRequired", err)
		}
	})

	t.Run("missing durable revision", func(t *testing.T) {
		t.Parallel()
		store := exactFenceStoreFunc(func(_ context.Context, request FenceRequest) (FenceAcknowledgement, error) {
			ack := exactDurableAcknowledgement(request)
			ack.DurableRevision = ""
			return ack, nil
		})
		_, err := RequestFenceExact(context.Background(), store, "f1", "h1", ids, FencePolicy{}, now)
		if !errors.Is(err, ErrFenceRequired) {
			t.Fatalf("RequestFence with empty durable revision = %v, want ErrFenceRequired", err)
		}
	})

	t.Run("missing fence id", func(t *testing.T) {
		t.Parallel()
		_, err := RequestFence(context.Background(), nil, "", "h1", ids, FencePolicy{}, now)
		if !errors.Is(err, ErrFenceRequired) {
			t.Fatalf("RequestFence with empty id = %v, want ErrFenceRequired", err)
		}
	})
}

func TestRequestFencePreservesExactOrderedCoverageAndAcknowledgementBytes(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	ids := []FencedSession{
		{OrgID: "o", SessionID: "s3", LastForwardedSeq: 3},
		{OrgID: "o", SessionID: "s1", ShimID: "shim", ProcessEpoch: 2, ControllerGeneration: 5, LastForwardedSeq: 2},
		{OrgID: "n", SessionID: "s2", LastForwardedSeq: 1},
	}
	var received FenceRequest
	store := exactFenceStoreFunc(func(_ context.Context, request FenceRequest) (FenceAcknowledgement, error) {
		received = request
		return exactDurableAcknowledgement(request), nil
	})
	f, err := RequestFenceExact(context.Background(), store, "f1", "h1", ids, FencePolicy{}, now)
	if err != nil {
		t.Fatalf("RequestFence: %v", err)
	}
	want := []string{"o/s3//0/0/3", "o/s1/shim/2/5/2", "n/s2//0/0/1"}
	for i, w := range want {
		got := fmt.Sprintf("%s/%s/%d/%d/%d", f.Sessions[i].Identity(), f.Sessions[i].ShimID, f.Sessions[i].ProcessEpoch, f.Sessions[i].ControllerGeneration, f.Sessions[i].LastForwardedSeq)
		if got != w {
			t.Fatalf("Sessions[%d] = %s, want %s", i, got, w)
		}
	}
	if f.DurableRevision != "revision-1" {
		t.Fatalf("durable revision = %q, want revision-1", f.DurableRevision)
	}
	requestBytes, err := json.Marshal(received.Fence)
	if err != nil {
		t.Fatalf("marshal received fence: %v", err)
	}
	if !bytes.Equal(received.RequestBytes, requestBytes) {
		t.Fatalf("store request bytes differ from request fence:\nbytes=%s\nfence=%s", received.RequestBytes, requestBytes)
	}
	if bytes.Contains(received.RequestBytes, []byte(`"revision"`)) {
		t.Fatalf("request bytes contain response-only durable revision: %s", received.RequestBytes)
	}
	if !bytes.Contains(received.RequestBytes, []byte(`"processEpoch":0`)) {
		t.Fatalf("request bytes omitted the zero-valued processEpoch correlation: %s", received.RequestBytes)
	}
	if !bytes.Contains(received.RequestBytes, []byte(`"controllerGeneration":5`)) {
		t.Fatalf("request bytes omitted the per-session controller generation: %s", received.RequestBytes)
	}
}

// TestTerminalProofCoversHonoursBothAdmissibleForms pins the incarnation-scoped
// question to the same evidence §D10 admits.
//
// A pre-check that is stricter than the predicate it guards is not caution — it
// is a different rule. Consulting only Correlations refused every proof carried
// in the scalar fields, so a session whose adopted owner had already reported an
// ordinary terminal receipt answered `reconcile` forever.
func TestTerminalProofCoversHonoursBothAdmissibleForms(t *testing.T) {
	t.Parallel()
	id := Identity{OrgID: "org-covers", SessionID: "session-covers"}
	const (
		shimID = "shim-covers"
		epoch  = uint64(5)
	)
	reaped := &Tombstone{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: shimID, ProcessEpoch: epoch, GroupReaped: true,
	}
	sibling := &Tombstone{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-sibling", ProcessEpoch: 9, GroupReaped: true,
	}
	unreaped := &Tombstone{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: shimID, ProcessEpoch: epoch,
	}
	for _, tc := range []struct {
		name  string
		proof TerminalProof
		want  bool
	}{
		{name: "no evidence proves nothing"},
		{
			name:  "an exact correlation tombstone covers it",
			proof: TerminalProof{Correlations: []TerminalCorrelationProof{{ShimID: shimID, ProcessEpoch: epoch, Tombstone: reaped}}},
			want:  true,
		},
		{
			name:  "the scalar tombstone for this exact incarnation covers it",
			proof: TerminalProof{Tombstone: reaped},
			want:  true,
		},
		{
			name:  "an adopted owner's terminal receipt covers it",
			proof: TerminalProof{AdoptedReceipt: true},
			want:  true,
		},
		{
			name:  "a sibling's tombstone does not cover it",
			proof: TerminalProof{Tombstone: sibling},
		},
		{
			name:  "a tombstone that did not prove a reap does not cover it",
			proof: TerminalProof{Tombstone: unreaped},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := TerminalProofCovers(tc.proof, id, shimID, epoch); got != tc.want {
				t.Fatalf("TerminalProofCovers = %t, want %t", got, tc.want)
			}
			if tc.want && !tc.proof.Proves() {
				t.Fatal("the incarnation-scoped answer is more permissive than Proves() — those cannot disagree in this direction")
			}
		})
	}
}
