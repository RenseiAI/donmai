package sessionshim

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

func TestOrphanPolicyEnforcesTheDoubleExecutionBound(t *testing.T) {
	t.Parallel()

	// §D8's contract is an INEQUALITY, not a number:
	//   deadline + termination grace + propagation margin < smallest external
	//   release threshold
	// If that ordering inverts, something external can hand the same work to
	// another host while the original harness is still running.
	cases := []struct {
		name    string
		policy  OrphanPolicy
		wantErr bool
	}{
		{
			name:   "defaults with no external releaser",
			policy: DefaultOrphanPolicy(),
		},
		{
			name: "comfortably inside an external threshold",
			policy: OrphanPolicy{
				Deadline: 90 * time.Second, TerminationGrace: 5 * time.Second,
				PropagationMargin: 30 * time.Second, ExternalReleaseThreshold: 10 * time.Minute,
			},
		},
		{
			name: "exactly equal is refused (the bound is STRICT)",
			policy: OrphanPolicy{
				Deadline: 60 * time.Second, TerminationGrace: 5 * time.Second,
				PropagationMargin: 35 * time.Second, ExternalReleaseThreshold: 100 * time.Second,
			},
			wantErr: true,
		},
		{
			name: "deadline alone exceeds the threshold",
			policy: OrphanPolicy{
				Deadline: 10 * time.Minute, TerminationGrace: 5 * time.Second,
				PropagationMargin: 30 * time.Second, ExternalReleaseThreshold: 5 * time.Minute,
			},
			wantErr: true,
		},
		{
			// The subtle one: the deadline fits but the grace and margin push the
			// total past the threshold. Checking the deadline alone would pass this.
			name: "grace and margin push the total over",
			policy: OrphanPolicy{
				Deadline: 4 * time.Minute, TerminationGrace: 30 * time.Second,
				PropagationMargin: 45 * time.Second, ExternalReleaseThreshold: 5 * time.Minute,
			},
			wantErr: true,
		},
		{
			name:    "non-positive deadline",
			policy:  OrphanPolicy{Deadline: 0, TerminationGrace: time.Second},
			wantErr: true,
		},
		{
			name:    "negative grace",
			policy:  OrphanPolicy{Deadline: time.Minute, TerminationGrace: -time.Second},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.policy.Validate()
			if tc.wantErr {
				if !errors.Is(err, ErrOrphanPolicyUnsafe) {
					t.Fatalf("Validate = %v, want ErrOrphanPolicyUnsafe", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate = %v, want nil", err)
			}
		})
	}
}

func TestOrphanPolicyErrorExplainsTheConsequence(t *testing.T) {
	t.Parallel()

	// The message has to tell an operator WHY the daemon refused to start, not
	// just that a number was wrong — this failure is refused at startup and the
	// operator has to fix a config they may not have written.
	p := OrphanPolicy{
		Deadline: 10 * time.Minute, TerminationGrace: 5 * time.Second,
		PropagationMargin: 30 * time.Second, ExternalReleaseThreshold: time.Minute,
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("Validate accepted an unsafe policy")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Fatalf("error does not explain the double-execution consequence: %v", err)
	}
}

func TestWithExternalThresholdReturnsACopyForRevalidation(t *testing.T) {
	t.Parallel()

	// §D8: a LATER reduction of an external threshold must FAIL a compatibility
	// check before rollout. Returning a copy forces the caller to re-Validate
	// rather than mutating a policy in place and hoping someone re-checks.
	base := DefaultOrphanPolicy()
	tightened := base.WithExternalThreshold(time.Second)

	if base.ExternalReleaseThreshold != 0 {
		t.Fatal("WithExternalThreshold mutated the receiver")
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("original policy should still validate: %v", err)
	}
	if err := tightened.Validate(); !errors.Is(err, ErrOrphanPolicyUnsafe) {
		t.Fatalf("tightened policy Validate = %v, want ErrOrphanPolicyUnsafe", err)
	}
}

func TestTotalBoundSumsEveryComponent(t *testing.T) {
	t.Parallel()

	p := OrphanPolicy{Deadline: time.Minute, TerminationGrace: 5 * time.Second, PropagationMargin: 10 * time.Second}
	if got, want := p.TotalBound(), 75*time.Second; got != want {
		t.Fatalf("TotalBound = %s, want %s", got, want)
	}
}

func TestQuarantineProjectionAlwaysConsumesCapacity(t *testing.T) {
	t.Parallel()

	// §D7 is unconditional: a quarantined shim is occupied capacity because its
	// harness is running. The constructor hard-codes this so no call site can
	// hide occupied capacity by passing false.
	now := time.Unix(1_700_000_100, 0)
	rec := Record{
		OrgID: "org-1", SessionID: "sess-1", ShimID: "shim-1", ProcessEpoch: 17,
		ProtocolMin: 7, ProtocolMax: 9, Phase: shimwire.PhaseRunning,
		CreatedAtUnixNano: now.Add(-90 * time.Second).UnixNano(),
	}
	for _, reason := range []QuarantineReason{
		QuarantineProtocolMismatch, QuarantineRecordMalformed, QuarantineDuplicateIdentity,
		QuarantineIdentityMismatch, QuarantineUnauthenticated, QuarantinePhaseUnknown,
		QuarantineGenerationNotAdvanced, QuarantineSocketUnreachable, QuarantineAdoptionFailed,
	} {
		q := NewQuarantinedSession(rec, reason, "detail", now)
		if !q.ConsumesCapacity {
			t.Errorf("reason %q produced consumesCapacity=false", reason)
		}
		if !q.Reason.Known() {
			t.Errorf("reason %q is not in the closed registry", reason)
		}
		if q.AgeSeconds != 90 {
			t.Errorf("reason %q age = %d, want 90", reason, q.AgeSeconds)
		}
		if q.ProtocolMin != 7 || q.ProtocolMax != 9 {
			t.Errorf("reason %q lost the protocol range an operator needs to diagnose it", reason)
		}
		if q.ProcessEpoch != 17 {
			t.Errorf("reason %q process epoch = %d, want 17", reason, q.ProcessEpoch)
		}
		if q.ControllerGeneration != 0 {
			t.Errorf("reason %q record-only generation = %d, want explicit unknown 0", reason, q.ControllerGeneration)
		}
		raw, err := json.Marshal(q)
		if err != nil || !strings.Contains(string(raw), `"controllerGeneration":0`) {
			t.Errorf("reason %q JSON omitted explicit unknown generation: %s err=%v", reason, raw, err)
		}
	}
	if QuarantineReason("because").Known() {
		t.Error("the quarantine-reason registry is not closed")
	}
}

func TestSortQuarantinedIsStableAcrossSnapshots(t *testing.T) {
	t.Parallel()

	// Host status and heartbeat payloads must be diffable: unstable ordering
	// would make every beat look like a change.
	in := []QuarantinedSession{
		{OrgID: "o", SessionID: "s2", ShimID: "b"},
		{OrgID: "n", SessionID: "s1", ShimID: "a"},
		{OrgID: "o", SessionID: "s1", ShimID: "z"},
		{OrgID: "o", SessionID: "s1", ShimID: "a", ProcessEpoch: 2},
		{OrgID: "o", SessionID: "s1", ShimID: "a", ProcessEpoch: 1},
		{OrgID: "o", SessionID: "s1", ShimID: "a", ProcessEpoch: 1, ControllerGeneration: 8},
	}
	SortQuarantined(in)
	want := []string{"n/s1/a/0/0", "o/s1/a/1/0", "o/s1/a/1/8", "o/s1/a/2/0", "o/s1/z/0/0", "o/s2/b/0/0"}
	for i, w := range want {
		got := fmt.Sprintf("%s/%s/%d/%d", in[i].Identity(), in[i].ShimID, in[i].ProcessEpoch, in[i].ControllerGeneration)
		if got != w {
			t.Fatalf("sorted[%d] = %s, want %s", i, got, w)
		}
	}
}
