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
