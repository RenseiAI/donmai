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

// TestProcessIdentityAliveToleratesBootClockStep drives Alive with a btime
// seam moved by one second: the stored identity is pinned with the real boot,
// then the boot reader steps forward as a wall-clock step would. The same live
// process must still verify; without the tolerance window the stepped reading
// compares unequal and the live shim reads dead.
func TestProcessIdentityAliveToleratesBootClockStep(t *testing.T) {
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	pid := child.Process.Pid
	pinned, err := ProcessIdentityFor(pid)
	if err != nil {
		t.Fatalf("ProcessIdentityFor(%d): %v", pid, err)
	}
	boot, err := readLinuxBootTimeUnixNano()
	if err != nil {
		t.Fatalf("readLinuxBootTimeUnixNano: %v", err)
	}
	prev := linuxBootTimeReader
	linuxBootTimeReader = func() (int64, error) { return boot + 1e9, nil }
	t.Cleanup(func() { linuxBootTimeReader = prev })
	if alive, err := pinned.Alive(); err != nil || !alive {
		t.Fatalf("stepped-boot identity reported alive=%t err=%v; a 1s wall-clock step must not read a live shim dead", alive, err)
	}
}

// TestProcessIdentityAliveRejectsUnconvertibleStoredValue pins the
// fail-closed path for a stored value that cannot convert: a tick-magnitude
// value whose conversion overflows is an error (the caller quarantines),
// never a stale classification that would release a claim on a live shim.
func TestProcessIdentityAliveRejectsUnconvertibleStoredValue(t *testing.T) {
	const unconvertible = int64(100000000000000) // < 1e15 legacy ticks, overflows int64 ns
	if unconvertible >= legacyTicksStartedAtThreshold {
		t.Fatalf("fixture %d is not legacy magnitude", unconvertible)
	}
	if _, err := procStartTimeUnixNano(unconvertible, linuxClockTicksPerSecond, 1700000000*1e9); err == nil {
		t.Fatalf("procStartTimeUnixNano(%d, ...) returned no error; the fixture converts and cannot pin the guard", unconvertible)
	}
	p := ProcessIdentity{PID: os.Getpid(), StartedAt: unconvertible}
	if alive, err := p.Alive(); err == nil {
		t.Fatalf("Alive with unconvertible stored value reported alive=%t err=nil; want a quarantine error", alive)
	}
}
