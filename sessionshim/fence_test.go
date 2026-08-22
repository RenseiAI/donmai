package sessionshim

import (
	"context"
	"errors"
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
	reapedTombstone := &Tombstone{GroupReaped: true}
	unreapedTombstone := &Tombstone{GroupReaped: false}

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
	// The hold window must cover the restart budget AND the whole orphan bound —
	// a fence sized to the restart alone expires when a slow restart needs it most.
	wantHold := policy.RestartBudget + DefaultOrphanPolicy().TotalBound()
	if got := f.HoldUntil().Sub(now); got != wantHold {
		t.Fatalf("hold window = %s, want %s", got, wantHold)
	}
}

type fenceStoreFunc func(context.Context, Fence) (Fence, error)

func (f fenceStoreFunc) Acknowledge(ctx context.Context, fence Fence) (Fence, error) {
	return f(ctx, fence)
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
	store := fenceStoreFunc(func(_ context.Context, f Fence) (Fence, error) {
		f.Sessions = f.Sessions[:1] // drops s2
		return f, nil
	})
	_, err := RequestFence(context.Background(), store, "fence-1", "host-1", ids, FencePolicy{}, now)
	if !errors.Is(err, ErrFenceRequired) {
		t.Fatalf("RequestFence with partial ack = %v, want ErrFenceRequired", err)
	}
}

func TestRequestFenceRefusesMismatchedOrFailedAcknowledgement(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	ids := []FencedSession{{OrgID: "o", SessionID: "s1"}}

	t.Run("store error", func(t *testing.T) {
		t.Parallel()
		store := fenceStoreFunc(func(context.Context, Fence) (Fence, error) {
			return Fence{}, errors.New("control plane unreachable")
		})
		_, err := RequestFence(context.Background(), store, "f1", "h1", ids, FencePolicy{}, now)
		if !errors.Is(err, ErrFenceRequired) {
			t.Fatalf("RequestFence = %v, want ErrFenceRequired (the caller must refuse the restart)", err)
		}
	})

	t.Run("wrong fence id", func(t *testing.T) {
		t.Parallel()
		store := fenceStoreFunc(func(_ context.Context, f Fence) (Fence, error) {
			f.FenceID = "someone-elses-fence"
			return f, nil
		})
		_, err := RequestFence(context.Background(), store, "f1", "h1", ids, FencePolicy{}, now)
		if !errors.Is(err, ErrFenceRequired) {
			t.Fatalf("RequestFence = %v, want ErrFenceRequired", err)
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

func TestRequestFenceSortsCoverageDeterministically(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	ids := []FencedSession{
		{OrgID: "o", SessionID: "s3"},
		{OrgID: "o", SessionID: "s1"},
		{OrgID: "n", SessionID: "s2"},
	}
	f, err := RequestFence(context.Background(), nil, "f1", "h1", ids, FencePolicy{}, now)
	if err != nil {
		t.Fatalf("RequestFence: %v", err)
	}
	want := []string{"n/s2", "o/s1", "o/s3"}
	for i, w := range want {
		if got := f.Sessions[i].Identity().String(); got != w {
			t.Fatalf("Sessions[%d] = %s, want %s", i, got, w)
		}
	}
}
