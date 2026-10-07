package seatbudget

import (
	"runtime"
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"", ModeNone, false},
		{"none", ModeNone, false},
		{"NONE", ModeNone, false},
		{" auto ", "auto", false},
		{"enforced", ModeEnforced, false},
		{"best-effort", ModeBestEffort, false},
		{"Best-Effort", ModeBestEffort, false},
		{"hard", "", true},
		{"cpus", "", true},
	}
	for _, tc := range cases {
		got, err := ParseMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseMode(%q) = %q, nil error; want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMode(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMode(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := (Budget{}).Validate(); err != nil {
		t.Errorf("zero Budget invalid: %v", err)
	}
	for _, bad := range []Budget{
		{CPUs: -1},
		{MemoryMB: -1},
		{IOWeight: -1},
		{IOWeight: 10001},
		{Mode: "hard"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Budget %+v validates; want error", bad)
		}
	}
	if err := (Budget{CPUs: 2, MemoryMB: 4096, IOWeight: 100, Mode: ModeBestEffort}).Validate(); err != nil {
		t.Errorf("full Budget invalid: %v", err)
	}
}

func TestEffectiveModeFor(t *testing.T) {
	cases := []struct {
		mode Mode
		goos string
		want Mode
	}{
		{"auto", "linux", ModeEnforced},
		{"auto", "darwin", ModeBestEffort},
		{"auto", "windows", ModeBestEffort},
		{ModeEnforced, "linux", ModeEnforced},
		{ModeEnforced, "darwin", ModeBestEffort},
		{ModeBestEffort, "linux", ModeBestEffort},
		{ModeBestEffort, "darwin", ModeBestEffort},
		{ModeNone, "linux", ModeNone},
		{"", "linux", ModeNone},
	}
	for _, tc := range cases {
		if got := EffectiveModeFor(tc.mode, tc.goos); got != tc.want {
			t.Errorf("EffectiveModeFor(%q, %q) = %q; want %q", tc.mode, tc.goos, got, tc.want)
		}
	}
	// EffectiveMode follows the host OS.
	if got := (Budget{Mode: "auto"}).EffectiveMode(); got != EffectiveModeFor("auto", runtime.GOOS) {
		t.Errorf("EffectiveMode() = %q; want host resolution %q", got, EffectiveModeFor("auto", runtime.GOOS))
	}
}

func TestResolve(t *testing.T) {
	t.Run("single seat keeps the whole host", func(t *testing.T) {
		got := Resolve(Budget{}, 1, 32, 32768)
		if got.CPUs != 32 {
			t.Errorf("CPUs = %d; want 32 (no single-seat throttling)", got.CPUs)
		}
		if got.MemoryMB != 0 {
			t.Errorf("MemoryMB = %d; want 0 (no cap on a lone seat)", got.MemoryMB)
		}
		if got.Mode != "auto" {
			t.Errorf("Mode = %q; want auto", got.Mode)
		}
	})
	t.Run("shared host divides by seat count", func(t *testing.T) {
		got := Resolve(Budget{}, 4, 32, 32768)
		if got.CPUs != 8 {
			t.Errorf("CPUs = %d; want 8", got.CPUs)
		}
		if got.MemoryMB != 8192 {
			t.Errorf("MemoryMB = %d; want 8192", got.MemoryMB)
		}
	})
	t.Run("authored fields win", func(t *testing.T) {
		got := Resolve(Budget{CPUs: 2, MemoryMB: 4096, Mode: ModeBestEffort}, 4, 32, 32768)
		if got.CPUs != 2 || got.MemoryMB != 4096 || got.Mode != ModeBestEffort {
			t.Errorf("Resolve overwrote authored fields: %+v", got)
		}
	})
	t.Run("cpu floor is one", func(t *testing.T) {
		got := Resolve(Budget{}, 16, 8, 32768)
		if got.CPUs != 1 {
			t.Errorf("CPUs = %d; want floor 1", got.CPUs)
		}
	})
}

func TestWorkerCapEnv(t *testing.T) {
	env := WorkerCapEnv(4)
	// Every knob is a real tool control at the seat share.
	want := map[string]string{
		"GOMAXPROCS":                 "4",
		"MAKEFLAGS":                  "-j4",
		"CMAKE_BUILD_PARALLEL_LEVEL": "4",
		"NINJAFLAGS":                 "-j4",
		"CARGO_BUILD_JOBS":           "4",
		"VITEST_MAX_WORKERS":         "4",
		"NEXT_BUILD_WORKERS":         "4",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q; want %q", k, env[k], v)
		}
	}
	if len(env) != len(want) {
		t.Errorf("WorkerCapEnv has %d entries; want exactly the %d known knobs", len(env), len(want))
	}
	// Zero/negative CPUs clamp to one worker, never to an empty or
	// unlimited value.
	for _, n := range []int{0, -3} {
		if got := WorkerCapEnv(n)["GOMAXPROCS"]; got != "1" {
			t.Errorf("WorkerCapEnv(%d)[GOMAXPROCS] = %q; want 1", n, got)
		}
	}
	// Keys list matches the map.
	keys := WorkerCapKeys()
	if len(keys) != len(want) {
		t.Fatalf("WorkerCapKeys = %v; want %d keys", keys, len(want))
	}
	for _, k := range keys {
		if _, ok := want[k]; !ok {
			t.Errorf("WorkerCapKeys has unknown knob %q", k)
		}
	}
}

func TestCPUSetAndQuota(t *testing.T) {
	if got := CPUSet(1); got != "0" {
		t.Errorf("CPUSet(1) = %q; want 0", got)
	}
	if got := CPUSet(4); got != "0-3" {
		t.Errorf("CPUSet(4) = %q; want 0-3", got)
	}
	if got := CPUQuotaPercent(2); got != "200%" {
		t.Errorf("CPUQuotaPercent(2) = %q; want 200%%", got)
	}
	if got := MemoryBytes(0); got != "" {
		t.Errorf("MemoryBytes(0) = %q; want empty (no cap)", got)
	}
	if got := MemoryBytes(512); got != "536870912" {
		t.Errorf("MemoryBytes(512) = %q; want 536870912", got)
	}
}

func TestSystemdScopeArgs(t *testing.T) {
	args := SystemdScopeArgs("donmai-seat-abc.scope", Budget{CPUs: 2, MemoryMB: 4096, IOWeight: 200})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"systemd-run", "--scope", "--collect",
		"AllowedCPUs=0-1", "CPUQuota=200%",
		"MemoryMax=4294967296", "MemoryHigh=4294967296", "IOWeight=200",
		"donmai-seat-abc.scope",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("scope args %q missing %q", joined, want)
		}
	}
	// No memory cap => no MemoryMax (which systemd would read as zero
	// memory), no weight => no IOWeight.
	bare := strings.Join(SystemdScopeArgs("donmai-seat-x.scope", Budget{CPUs: 1}), " ")
	if strings.Contains(bare, "MemoryMax") || strings.Contains(bare, "IOWeight") {
		t.Errorf("uncapped seat got a memory/IO property: %q", bare)
	}
}

func TestScopeName(t *testing.T) {
	if got := ScopeName("abc-123_X"); got != "donmai-seat-abc-123_X.scope" {
		t.Errorf("ScopeName = %q", got)
	}
	// Hostile ids stay in the unit alphabet.
	got := ScopeName("../../evil;rm -rf")
	if strings.ContainsAny(strings.TrimSuffix(got, ".scope"), "/; ") || !strings.HasSuffix(got, ".scope") {
		t.Errorf("ScopeName(%q) = %q; want sanitised unit name", "../../evil;rm -rf", got)
	}
	if got := ScopeName(""); got != "donmai-seat-session.scope" {
		t.Errorf("ScopeName(\"\") = %q", got)
	}
}

func TestSelectPlacement(t *testing.T) {
	if got := SelectPlacement(true, true); got != PlacementSystemd {
		t.Errorf("systemd present => %q; want systemd", got)
	}
	if got := SelectPlacement(false, true); got != PlacementCgroupFS {
		t.Errorf("cgroupfs only => %q; want cgroupfs", got)
	}
	if got := SelectPlacement(false, false); got != PlacementNone {
		t.Errorf("nothing present => %q; want none", got)
	}
}

func TestComposeEnv(t *testing.T) {
	base := []string{"PATH=/bin", "GOMAXPROCS=16"}
	got := ComposeEnv(base, 2)
	// Operator value wins; the rest of the knobs land at the seat share.
	byKey := map[string]string{}
	for _, kv := range got {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			byKey[kv[:i]] = kv[i+1:]
		}
	}
	if byKey["GOMAXPROCS"] != "16" {
		t.Errorf("GOMAXPROCS = %q; want operator value 16", byKey["GOMAXPROCS"])
	}
	if byKey["MAKEFLAGS"] != "-j2" {
		t.Errorf("MAKEFLAGS = %q; want -j2", byKey["MAKEFLAGS"])
	}
	if len(base) != 2 {
		t.Errorf("ComposeEnv mutated base: %v", base)
	}
	if got := ComposeEnv(nil, 0); len(got) != len(WorkerCapKeys()) {
		t.Errorf("ComposeEnv(nil, 0) has %d entries; want %d knobs at 1 CPU", len(got), len(WorkerCapKeys()))
	}
}

func TestDisabled(t *testing.T) {
	if !(Budget{}).Disabled() {
		t.Error("zero Budget is not disabled")
	}
	if !(Budget{Mode: ModeNone, CPUs: 4}).Disabled() {
		t.Error("mode none with numbers is not disabled")
	}
	if (Budget{CPUs: 2}).Disabled() {
		t.Error("numeric budget reports disabled")
	}
	if (Budget{Mode: "auto"}).Disabled() {
		t.Error("auto budget reports disabled")
	}
}
