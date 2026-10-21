//go:build linux

package sessionshim

import (
	"os"
	"testing"
	"time"
)

// TestProcessStartTimeIsUnixNano pins the platform contract on Linux: the
// reported start time is wall-clock nanoseconds between boot and now, not raw
// /proc starttime ticks (which would be orders of magnitude smaller).
func TestProcessStartTimeIsUnixNano(t *testing.T) {
	t.Parallel()
	started, err := processStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("processStartTime(self): %v", err)
	}
	boot, err := readLinuxBootTimeUnixNano()
	if err != nil {
		t.Fatalf("readLinuxBootTimeUnixNano: %v", err)
	}
	now := time.Now().UnixNano()
	if started < boot {
		t.Fatalf("processStartTime(self) = %d, want >= boot %d", started, boot)
	}
	if started > now+60*1e9 {
		t.Fatalf("processStartTime(self) = %d, want <= now %d (plus skew)", started, now)
	}
	// Raw ticks-since-boot never reach wall-clock magnitude; a value below
	// 1e15 proves the conversion was skipped.
	if started < 1e15 {
		t.Fatalf("processStartTime(self) = %d, want wall-clock nanoseconds (>= 1e15)", started)
	}
}
