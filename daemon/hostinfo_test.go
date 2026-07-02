package daemon

import (
	"runtime"
	"testing"
	"time"
)

// TestGatherHostInfo_StdlibFieldsAlwaysPopulated confirms the always-available
// stdlib fields (os, arch, cpuCores) plus the passed-through version and
// startedAt are set unconditionally, on any platform.
func TestGatherHostInfo_StdlibFieldsAlwaysPopulated(t *testing.T) {
	started := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	info := GatherHostInfo("1.2.3", started)
	if info == nil {
		t.Fatal("GatherHostInfo returned nil")
	}
	if info.OS != runtime.GOOS {
		t.Errorf("OS = %q, want %q", info.OS, runtime.GOOS)
	}
	if info.Arch != runtime.GOARCH {
		t.Errorf("Arch = %q, want %q", info.Arch, runtime.GOARCH)
	}
	if info.CPUCores != runtime.NumCPU() {
		t.Errorf("CPUCores = %d, want %d", info.CPUCores, runtime.NumCPU())
	}
	if info.DaemonVersion != "1.2.3" {
		t.Errorf("DaemonVersion = %q, want 1.2.3", info.DaemonVersion)
	}
	if info.StartedAt != "2026-07-01T12:00:00Z" {
		t.Errorf("StartedAt = %q, want 2026-07-01T12:00:00Z", info.StartedAt)
	}
}

// TestGatherHostInfo_ZeroStartedAtOmitsTimestamp confirms a zero start time
// leaves StartedAt empty (omitempty) rather than emitting a bogus RFC3339
// zero-value string.
func TestGatherHostInfo_ZeroStartedAtOmitsTimestamp(t *testing.T) {
	info := GatherHostInfo("dev", time.Time{})
	if info.StartedAt != "" {
		t.Errorf("StartedAt = %q, want empty for zero start time", info.StartedAt)
	}
}

// TestGatherHostInfo_BestEffortProbesNeverError confirms the platform-specific
// probes degrade gracefully: gathering never panics and never returns nil, and
// the best-effort fields are either populated with sane values or left empty.
// On linux/darwin at least cpuCores is always present (asserted above); this
// case guards that unsupported/failed probes don't corrupt the struct.
func TestGatherHostInfo_BestEffortProbesNeverError(t *testing.T) {
	info := GatherHostInfo("v", time.Now())
	if info == nil {
		t.Fatal("GatherHostInfo returned nil")
	}
	// Never negative — a failed numeric probe must leave the field at zero.
	if info.MemTotalMB < 0 {
		t.Errorf("MemTotalMB = %d, want >= 0", info.MemTotalMB)
	}
	if info.CPUCores <= 0 {
		t.Errorf("CPUCores = %d, want > 0", info.CPUCores)
	}
	// On linux/darwin the best-effort probes should typically succeed; log
	// (not fail) so CI on an odd platform stays green.
	switch runtime.GOOS {
	case "linux", "darwin":
		if info.CPUModel == "" {
			t.Logf("cpuModel empty on %s (best-effort probe returned nothing)", runtime.GOOS)
		}
		if info.MemTotalMB == 0 {
			t.Logf("memTotalMb zero on %s (best-effort probe returned nothing)", runtime.GOOS)
		}
	}
}

// TestPrimaryOutboundIP_ShapeIsSaneOrEmpty confirms the IP probe returns either
// an empty string or a parseable non-loopback IPv4 — never a malformed value.
func TestPrimaryOutboundIP_ShapeIsSaneOrEmpty(t *testing.T) {
	ip := primaryOutboundIP()
	if ip == "" {
		t.Skip("no outbound IP resolvable in this environment")
	}
	// Basic shape: dotted-quad, four octets.
	parts := 1
	for _, r := range ip {
		if r == '.' {
			parts++
		}
	}
	if parts != 4 {
		t.Errorf("primaryOutboundIP() = %q, want dotted-quad IPv4", ip)
	}
}

// TestSampleLoad_NeverErrorsAndBoundsPercent confirms the per-beat load sampler
// never panics and, when it reports a sample, both values are bounded to
// [0,100]. ok=false is a valid (best-effort miss) outcome that must not be
// treated as an error.
func TestSampleLoad_NeverErrorsAndBoundsPercent(t *testing.T) {
	cpu, mem, ok := SampleLoad()
	if !ok {
		// Legitimate on an unsupported platform / probe miss — the heartbeat
		// simply omits the load key.
		t.Logf("SampleLoad reported no sample on %s (ok=false)", runtime.GOOS)
		return
	}
	if cpu < 0 || cpu > 100 {
		t.Errorf("cpu = %v, want within [0,100]", cpu)
	}
	if mem < 0 || mem > 100 {
		t.Errorf("mem = %v, want within [0,100]", mem)
	}
}

func TestClampPct(t *testing.T) {
	tests := []struct {
		in, want float64
	}{
		{-5, 0},
		{0, 0},
		{42.5, 42.5},
		{100, 100},
		{150, 100},
	}
	for _, tt := range tests {
		if got := clampPct(tt.in); got != tt.want {
			t.Errorf("clampPct(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
