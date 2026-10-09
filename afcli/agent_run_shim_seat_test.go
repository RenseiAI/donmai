package afcli

import (
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestShimSeatFromEnv pins the operator boundary that arms the shim-owned
// runner path: without the launch contract the seat is nil and the runner
// behaves exactly as before; a malformed contract is also nil (never a
// half-read seat with a zero attempt that could shadow a fresh run); a valid
// contract carries the contract's monotonic process epoch as the outbox
// attempt.
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
	if got.Attempt != valid.ProcessEpoch {
		t.Fatalf("attempt = %d, want process epoch %d", got.Attempt, valid.ProcessEpoch)
	}

	t.Setenv(sessionshim.EnvProcessEpoch, "not-a-number")
	if got := shimSeatFromEnv(); got != nil {
		t.Fatalf("malformed contract seat = %+v, want nil", got)
	}
}
