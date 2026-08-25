//go:build darwin || linux

package sessionshim

import (
	"testing"
	"time"
)

// TestSelfStartedAtIsUnixNanoseconds pins the ONE cross-platform property of
// ProcessIdentity.StartedAt that its consumers depend on: it is a wall-clock
// instant in Unix nanoseconds. Every transport of the identity — the
// authenticated hello, the registry record, the tombstone, the adopted-session
// projection — carries the number off this host, where nothing can reinterpret
// a host-local unit. A value in any other unit lands outside this window by
// many orders of magnitude.
func TestSelfStartedAtIsUnixNanoseconds(t *testing.T) {
	t.Parallel()

	self, err := Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}

	started := time.Unix(0, self.StartedAt)
	// The process cannot have started before this package existed, and cannot
	// have started after now (a second of slack absorbs clock granularity).
	floor := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	ceiling := time.Now().Add(time.Second)
	if started.Before(floor) || started.After(ceiling) {
		t.Fatalf("Self().StartedAt = %d resolves to %s, outside [%s, %s]; StartedAt is documented as Unix nanoseconds",
			self.StartedAt, started.UTC().Format(time.RFC3339Nano), floor.Format(time.RFC3339), ceiling.UTC().Format(time.RFC3339))
	}
}

// TestAliveRejectsAnUnrelatedStartInstant keeps the anti-reuse guarantee
// honest on every supported platform: a live pid paired with a start time that
// is not its own is NOT the recorded process, whatever compatibility the
// platform layer offers for older encodings.
func TestAliveRejectsAnUnrelatedStartInstant(t *testing.T) {
	t.Parallel()

	self, err := Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	// One second later than this process actually started: a value in the
	// current encoding's range, so no legacy-compatibility path applies.
	imposter := ProcessIdentity{PID: self.PID, StartedAt: self.StartedAt + int64(time.Second)}
	alive, err := imposter.Alive()
	if err != nil {
		t.Fatalf("Alive: %v", err)
	}
	if alive {
		t.Fatalf("Alive() = true for %s, but the process started at %d", imposter, self.StartedAt)
	}
}
