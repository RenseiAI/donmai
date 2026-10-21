//go:build linux

package sessionshim

import (
	"fmt"
	"os"
	"os/exec"
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

// TestProcessIdentityAliveAdoptsLegacyTicksRecord drives the cross-version
// adoption path end to end on Linux: a record holding the pre-conversion unit
// (raw /proc starttime ticks, as released shims wrote) must still verify a
// live process, because Alive coerces ticks through the current boot before
// comparing. A mismatched tick count must still report gone.
func TestProcessIdentityAliveAdoptsLegacyTicksRecord(t *testing.T) {
	t.Parallel()
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	pid := child.Process.Pid
	statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/stat: %v", pid, err)
	}
	ticks, err := parseProcPIDStatStartTicks(statData, pid)
	if err != nil {
		t.Fatalf("parseProcPIDStatStartTicks: %v", err)
	}
	if ticks >= legacyTicksStartedAtThreshold {
		t.Fatalf("raw ticks %d reach nanosecond magnitude; the legacy path has nothing to coerce", ticks)
	}
	legacy := ProcessIdentity{PID: pid, StartedAt: ticks}
	if alive, err := legacy.Alive(); err != nil || !alive {
		t.Fatalf("legacy ticks identity reported alive=%t err=%v; a live pre-conversion record must still verify", alive, err)
	}
	fresh, err := ProcessIdentityFor(pid)
	if err != nil {
		t.Fatalf("ProcessIdentityFor(%d): %v", pid, err)
	}
	if alive, err := fresh.Alive(); err != nil || !alive {
		t.Fatalf("fresh identity reported alive=%t err=%v", alive, err)
	}
	wrong := ProcessIdentity{PID: pid, StartedAt: ticks + 1000000}
	if wrong.StartedAt >= legacyTicksStartedAtThreshold {
		t.Fatalf("bumped ticks %d crossed into nanosecond magnitude", wrong.StartedAt)
	}
	if alive, err := wrong.Alive(); err != nil || alive {
		t.Fatalf("legacy identity with wrong ticks reported alive=%t err=%v; a different tick count is a different process", alive, err)
	}
}
