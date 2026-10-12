package afcli

import (
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestShimSeatFromEnv pins the operator boundary that arms the shim-owned
// runner path: without the launch contract the seat is nil and the runner
// behaves exactly as before; a malformed contract is also nil (never a
// half-read seat with a zero attempt that could shadow a fresh run); a valid
// contract arms the seat at the fixed session-scoped terminal sequence — the
// launch contract's monotonic process epoch advances on resume and must
// never key the outbox, or the resumed incarnation would orphan the record
// the previous one persisted.
func TestShimSeatFromEnv(t *testing.T) {
	// The seam reads the real process environment, so cases run
	// sequentially and each one states the full contract explicitly.
	for _, key := range sessionshim.EnvKeys() {
		t.Setenv(key, "")
	}

	if got := shimSeatFromEnv(); got != nil {
		t.Fatalf("contract-absent seat = %+v, want nil", got)
	}

	valid := sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "org-9", SessionID: "sess-9"},
		RegistryDir:  t.TempDir(),
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 7,
	}
	for key, value := range valid.Env() {
		t.Setenv(key, value)
	}
	got := shimSeatFromEnv()
	if got == nil {
		t.Fatal("valid contract produced no seat")
	}
	if got.Attempt != shimTerminalAttempt {
		t.Fatalf("attempt = %d, want fixed terminal sequence %d", got.Attempt, shimTerminalAttempt)
	}

	// A resumed incarnation carries a different process epoch but must
	// still address the same terminal record.
	resumed := valid
	resumed.ProcessEpoch = 8
	for key, value := range resumed.Env() {
		t.Setenv(key, value)
	}
	regot := shimSeatFromEnv()
	if regot == nil {
		t.Fatal("resumed contract produced no seat")
	}
	if regot.Attempt != got.Attempt {
		t.Fatalf("resumed attempt = %d, want %d", regot.Attempt, got.Attempt)
	}

	t.Setenv(sessionshim.EnvProcessEpoch, "not-a-number")
	if got := shimSeatFromEnv(); got != nil {
		t.Fatalf("malformed contract seat = %+v, want nil", got)
	}
}
