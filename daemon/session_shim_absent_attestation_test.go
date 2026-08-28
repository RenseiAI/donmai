package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
)

// An attestation and a tombstone are different claims. A report carrying both
// would let a receiver take the stronger reading of a lineage this daemon only
// proved unobservable — the exact way an attestation becomes a forged reap
// proof.
func TestTerminalEvidenceRefusesBothProofs(t *testing.T) {
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		OnTerminalEvidence: func(context.Context, SessionShimTerminalEvidence) error { return nil },
	}})
	id := sessionshim.Identity{OrgID: "org", SessionID: "session"}
	err := d.reportSessionShimTerminalEvidence(context.Background(), SessionShimTerminalEvidence{
		Identity: id, HostID: "host", ShimID: "shim", ProcessEpoch: 1,
		Absent: &SessionShimAbsentAttestation{
			ProcessIdentityAbsent: true, RegistryRecordAbsent: true, ObservedAtUnixNano: 1,
		},
		Tombstone: sessionshim.Tombstone{
			OrgID: "org", SessionID: "session", ShimID: "shim",
			ProcessEpoch: 1, GroupReaped: true,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "both an absent attestation and a tombstone") {
		t.Fatalf("err = %v, want a refusal naming both proofs", err)
	}
}

// A partial attestation is not weaker evidence, it is no evidence. A dead
// process with a live registry record is still adoptable; a missing record
// with a live process is a shim no daemon may forget about.
func TestPartialAbsentAttestationIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attest   SessionShimAbsentAttestation
		shimID   string
		epoch    uint64
		wantFrag string
	}{
		{
			name:   "process not proven absent",
			attest: SessionShimAbsentAttestation{RegistryRecordAbsent: true, ObservedAtUnixNano: 1},
			shimID: "shim", epoch: 1,
			wantFrag: "both process and record absence",
		},
		{
			name:   "record not proven absent",
			attest: SessionShimAbsentAttestation{ProcessIdentityAbsent: true, ObservedAtUnixNano: 1},
			shimID: "shim", epoch: 1,
			wantFrag: "both process and record absence",
		},
		{
			name:   "no observation time",
			attest: SessionShimAbsentAttestation{ProcessIdentityAbsent: true, RegistryRecordAbsent: true},
			shimID: "shim", epoch: 1,
			wantFrag: "both process and record absence",
		},
		{
			name: "no exact incarnation",
			attest: SessionShimAbsentAttestation{
				ProcessIdentityAbsent: true, RegistryRecordAbsent: true, ObservedAtUnixNano: 1,
			},
			wantFrag: "exact shim incarnation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
				OnTerminalEvidence: func(context.Context, SessionShimTerminalEvidence) error {
					called = true
					return nil
				},
			}})
			attest := tc.attest
			err := d.reportSessionShimTerminalEvidence(context.Background(), SessionShimTerminalEvidence{
				Identity: sessionshim.Identity{OrgID: "org", SessionID: "session"},
				HostID:   "host", ShimID: tc.shimID, ProcessEpoch: tc.epoch,
				Absent: &attest,
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantFrag) {
				t.Fatalf("err = %v, want a refusal naming %q", err, tc.wantFrag)
			}
			if called {
				t.Error("a refused attestation still reached the composer")
			}
		})
	}
}

// A complete attestation reaches the composer unchanged, and carries no
// tombstone for the receiver to mistake for reap proof.
func TestCompleteAbsentAttestationReachesTheComposer(t *testing.T) {
	var seen SessionShimTerminalEvidence
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{
		OnTerminalEvidence: func(_ context.Context, evidence SessionShimTerminalEvidence) error {
			seen = evidence
			return nil
		},
	}})
	if err := d.reportSessionShimTerminalEvidence(context.Background(), SessionShimTerminalEvidence{
		Identity: sessionshim.Identity{OrgID: "org", SessionID: "session"},
		HostID:   "host", ShimID: "shim", ProcessEpoch: 3,
		Absent: &SessionShimAbsentAttestation{
			ProcessIdentityAbsent: true, RegistryRecordAbsent: true, ObservedAtUnixNano: 42,
		},
	}); err != nil {
		t.Fatalf("complete attestation refused: %v", err)
	}
	if seen.Absent == nil || !seen.Absent.Complete() {
		t.Fatalf("composer received %+v", seen.Absent)
	}
	if seen.Tombstone != (sessionshim.Tombstone{}) {
		t.Errorf("attestation carried a tombstone: %+v", seen.Tombstone)
	}
	if seen.Adoption != nil {
		t.Errorf("attestation carried a live adoption fact: %+v", seen.Adoption)
	}
}

// The acceptance clear used to leave through an absent-attestation terminal
// report; it now leaves through the batch's cleared section with an explicit
// abandoned disposition, and the retain-on-refusal invariant this file used to
// pin moved with it — see TestClearedReceiptEchoRefusalRetainsTheQuarantine in
// session_shim_cleared_disposition_test.go. The attestation itself remains a
// valid terminal-evidence shape, pinned by the tests above.
