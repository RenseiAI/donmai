package seatbudget

import (
	"net"
	"os"
	"path/filepath"
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
	// Every knob is a control the named tool actually reads: the Go
	// runtime (GOMAXPROCS), make (MAKEFLAGS -j), cmake --build
	// (CMAKE_BUILD_PARALLEL_LEVEL), ninja (NINJAFLAGS -j) and cargo
	// (CARGO_BUILD_JOBS). JS runners take no seat env entry: vitest reads
	// --maxWorkers/maxWorkers config (not an env knob) and Next.js
	// build parallelism is a config/CLI flag (not an env knob), so
	// injecting a same-shaped variable for them would claim a cap the
	// tool ignores.
	want := map[string]string{
		"GOMAXPROCS":                 "4",
		"MAKEFLAGS":                  "-j4",
		"CMAKE_BUILD_PARALLEL_LEVEL": "4",
		"NINJAFLAGS":                 "-j4",
		"CARGO_BUILD_JOBS":           "4",
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
	if got := CPUSet(1); got != "1 CPUs" {
		t.Errorf("CPUSet(1) = %q; want 1 CPUs", got)
	}
	if got := CPUSet(4); got != "4 CPUs" {
		t.Errorf("CPUSet(4) = %q; want 4 CPUs", got)
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
		"CPUQuota=200%", "CPUWeight=200", "OOMPolicy=continue",
		"MemoryMax=4294967296", "MemoryHigh=4294967296", "IOWeight=200",
		"donmai-seat-abc.scope",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("scope args %q missing %q", joined, want)
		}
	}
	// No core pinning: the quota is the binding limit (identical
	// AllowedCPUs ranges across seats shared 2 cores of 6 budgeted, and
	// user services never get the cpuset controller delegated).
	for _, banned := range []string{"AllowedCPUs", "cpuset"} {
		if strings.Contains(joined, banned) {
			t.Errorf("scope args %q carry %q; quota+weight confine the seat", joined, banned)
		}
	}
	if strings.Contains(joined, "--user") {
		t.Errorf("system-service scope args %q carry --user; want the system-bus spelling", joined)
	}
	// No memory cap => no MemoryMax (which systemd would read as zero
	// memory), no weight => no IOWeight.
	bare := strings.Join(SystemdScopeArgs("donmai-seat-x.scope", Budget{CPUs: 1}), " ")
	if strings.Contains(bare, "MemoryMax") || strings.Contains(bare, "IOWeight") {
		t.Errorf("uncapped seat got a memory/IO property: %q", bare)
	}
	if !strings.Contains(bare, "CPUWeight=100") {
		t.Errorf("uncapped seat scope args %q missing the proportional CPUWeight share", bare)
	}
	// A per-user daemon service reaches only the user bus: same scope
	// shape with --user right after the binary.
	userArgs := SystemdScopeArgsForBus("donmai-seat-abc.scope", Budget{CPUs: 2}, true)
	if len(userArgs) < 3 || userArgs[0] != "systemd-run" || userArgs[1] != "--user" || userArgs[2] != "--scope" {
		t.Errorf("user-bus scope args = %q; want systemd-run --user --scope ...", strings.Join(userArgs, " "))
	}
	if !strings.Contains(strings.Join(userArgs, " "), "OOMPolicy=continue") {
		t.Errorf("user-bus scope args %q missing the seat-survives-OOM policy", strings.Join(userArgs, " "))
	}
}

// TestSystemdScopeArgs_BudgetlessSeatStillScopes pins the every-shim-scoped
// rule at the argv level: a budget that carries no limits renders no limit
// properties — but still renders the scope shape (--scope, --collect, the
// seat-survives-OOM policy, the unit). A budgetless seat gets its own scope
// with nothing capped, so it still outlives its daemon.
func TestSystemdScopeArgs_BudgetlessSeatStillScopes(t *testing.T) {
	args := SystemdScopeArgsForBus(ScopeNameForIncarnation("org", "s", 1), Budget{}, true)
	joined := strings.Join(args, " ")
	for _, want := range []string{"systemd-run", "--user", "--scope", "--collect", "OOMPolicy=continue", ".scope"} {
		if !strings.Contains(joined, want) {
			t.Errorf("budgetless scope args %q missing %q", joined, want)
		}
	}
	for _, banned := range []string{"CPUQuota=", "CPUWeight=", "MemoryMax=", "MemoryHigh=", "IOWeight="} {
		if strings.Contains(joined, banned) {
			t.Errorf("budgetless scope args %q carry %q; want no limit properties", joined, banned)
		}
	}
}

func TestSystemdUserScope(t *testing.T) {
	// Only the user bus reachable => per-user install => --user.
	if !SystemdUserScope(true, false) {
		t.Error("SystemdUserScope(user, no system) = false; want true (per-user service)")
	}
	// Root with both buses keeps the system bus. A NON-root process with
	// both buses reachable is still a per-user install: the system bus
	// socket is world-readable, so reachability alone would strand it on
	// the system spelling and every spawn would fail Access denied.
	if got := SystemdUserScopeForEUID(true, true, 0); got {
		t.Error("SystemdUserScopeForEUID(user, system, root) = true; want false (system bus wins for root)")
	}
	for _, euid := range []int{1000, 501, 65534} {
		if !SystemdUserScopeForEUID(true, true, euid) {
			t.Errorf("SystemdUserScopeForEUID(user, system, euid %d) = false; want true (non-root uses the user bus)", euid)
		}
	}
	if SystemdUserScope(false, true) {
		t.Error("SystemdUserScope(no user, system) = true; want false")
	}
	if SystemdUserScope(false, false) {
		t.Error("SystemdUserScope(neither) = true; want false")
	}
}

func TestUserBusSocketPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := userBusSocketPath(""); got != "/run/user/1000/bus" {
		t.Errorf("userBusSocketPath = %q; want XDG_RUNTIME_DIR/bus", got)
	}
	if got := userBusSocketPath("/custom/bus"); got != "/custom/bus" {
		t.Errorf("userBusSocketPath(override) = %q; want the override", got)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if got := userBusSocketPath(""); got == "" || got == "/bus" {
		t.Errorf("userBusSocketPath fallback = %q; want the per-UID path", got)
	}
}

func TestScopeName(t *testing.T) {
	// The scope name is a fixed-length digest of the launch identity plus
	// the incarnation — never a truncation of the session id. Two long
	// session ids sharing a prefix get distinct scopes, and a hostile id
	// can never escape the unit alphabet or grow the name.
	first := ScopeNameForIncarnation("org", "abc-123_X", 1)
	again := ScopeNameForIncarnation("org", "abc-123_X", 1)
	if first != again {
		t.Errorf("ScopeNameForIncarnation unstable: %q != %q", first, again)
	}
	if !strings.HasPrefix(first, "donmai-seat-") || !strings.HasSuffix(first, "-1.scope") {
		t.Errorf("ScopeNameForIncarnation = %q; want the digest unit for epoch 1", first)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(first, "donmai-seat-"), "-1.scope")
	for _, r := range body {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			t.Errorf("ScopeNameForIncarnation = %q; digest body carries %q outside hex", first, r)
		}
	}
	if len(first) != len(ScopeNameForIncarnation("org", strings.Repeat("x", 300), 1)) {
		t.Error("scope name length varies with session id length; want fixed-length")
	}
	prefix := strings.Repeat("shared-prefix-", 20)
	a := ScopeNameForIncarnation("org", prefix+"session-alpha", 1)
	b := ScopeNameForIncarnation("org", prefix+"session-bravo", 1)
	if a == b {
		t.Errorf("shared-prefix sessions share scope %q; want distinct scopes", a)
	}
	if got := ScopeNameForIncarnation("org", "s", 1); got == ScopeNameForIncarnation("org", "s", 2) {
		t.Errorf("epochs 1 and 2 share scope %q; want distinct scopes per incarnation", got)
	}
	if got := ScopeNameForIncarnation("org-a", "s", 1); got == ScopeNameForIncarnation("org-b", "s", 1) {
		t.Errorf("orgs share scope %q; the digest must cover the org half", got)
	}
	// Hostile and empty ids stay in the unit alphabet at fixed length.
	for _, id := range []string{"../../evil;rm -rf", "", "abc 123"} {
		got := ScopeNameForIncarnation("org", id, 1)
		if got == "" || !strings.HasSuffix(got, ".scope") || strings.ContainsAny(strings.TrimSuffix(got, ".scope"), "/; ") {
			t.Errorf("ScopeNameForIncarnation(%q) = %q; want a sanitised unit name", id, got)
		}
	}
	// The legacy single-argument form keeps compiling for the direct-spawn
	// path: it names the epoch-0 digest, which no shim launch ever uses.
	if got := ScopeName("abc-123_X"); got != ScopeNameForIncarnation("", "abc-123_X", 0) {
		t.Errorf("ScopeName = %q; want the epoch-0 digest alias", got)
	}
}

func TestSelectPlacement(t *testing.T) {
	if got := SelectPlacement(true, true); got != PlacementSystemd {
		t.Errorf("systemd present => %q; want systemd", got)
	}
	// A writable hierarchy without systemd still selects the named
	// systemd-less placement — but that placement performs NO direct
	// writes: the seat runs unconfined and the report says none.
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

func TestHasSystemdAt(t *testing.T) {
	// The live confinement suite gates on HasSystemd; pin the probe
	// matrix through the injected form on any host so a broken probe
	// cannot silently skip (or falsely run) the live proof.
	dir := t.TempDir()
	systemdComm := filepath.Join(dir, "comm-systemd")
	if err := os.WriteFile(systemdComm, []byte("systemd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherComm := filepath.Join(dir, "comm-other")
	if err := os.WriteFile(otherComm, []byte("init\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "run-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: test fixture: a LookPath target whose --version prints nothing (unknown version keeps the placement)
		t.Fatal(err)
	}
	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)
	if runtime.GOOS != "linux" {
		if HasSystemdAt("run-helper", systemdComm) {
			t.Error("HasSystemdAt = true off Linux; want false (GOOS gate)")
		}
		return
	}
	if !HasSystemdAt("run-helper", systemdComm) {
		t.Error("HasSystemdAt(helper, systemd comm) = false; want true")
	}
	if HasSystemdAt("run-helper", otherComm) {
		t.Error("HasSystemdAt(helper, non-systemd comm) = true; want false")
	}
	if HasSystemdAt("run-helper", filepath.Join(dir, "missing")) {
		t.Error("HasSystemdAt(helper, missing comm) = true; want false")
	}
	if HasSystemdAt("definitely-not-on-path-xyz", systemdComm) {
		t.Error("HasSystemdAt(missing helper, systemd comm) = true; want false")
	}
}

// TestCgroupDocMatchesQuotaOnlyDesign pins the enforcement package doc to
// the shipped quota-only design: the scope carries quota, weight, memory
// and IO limits plus the seat-survives-OOM policy on the per-user bus —
// and deliberately no core pinning. The header previously specified
// AllowedCPUs pinning the implementation never applies (user services
// never get the cpuset controller delegated), so a reader following the
// doc expected a confinement the seat never gets. The sibling doc.go and
// the daemon README already state the quota-only design; this test keeps
// the two package docs from disagreeing again.
func TestCgroupDocMatchesQuotaOnlyDesign(t *testing.T) {
	raw, err := os.ReadFile("cgroup.go")
	if err != nil {
		t.Fatalf("read cgroup.go: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{"CPUQuota=", "CPUWeight=", "MemoryMax=", "OOMPolicy=continue", "--user"} {
		if !strings.Contains(doc, want) {
			t.Errorf("cgroup.go doc missing %q; want the quota-only scope design documented", want)
		}
	}
	for _, banned := range []string{"AllowedCPUs", "CPU set pinning", "CPUSet="} {
		if strings.Contains(doc, banned) {
			t.Errorf("cgroup.go doc mentions %q; the scope carries no core pinning", banned)
		}
	}
	if !strings.Contains(doc, "no core pinning") {
		t.Error("cgroup.go doc does not state the deliberate no-pinning rule; want it explicit")
	}
}

// TestHasSystemdAt_VersionGate pins the systemd floor for the scope
// placement through the injected version probe (no real systemd needed):
// a manager older than MinScopeSystemd gets no placement, so its seats run
// unconfined and report none instead of dying on "Unknown assignment:
// OOMPolicy=continue" (measured on 249) or claiming a CPU quota its user
// manager never delegated.
func TestHasSystemdAt_VersionGate(t *testing.T) {
	dir := t.TempDir()
	comm := filepath.Join(dir, "comm-systemd")
	if err := os.WriteFile(comm, []byte("systemd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(dir, "run-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: test fixture: LookPath target only, the version is injected
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		version int
		want    bool
	}{
		{249, false},
		{251, false},
		{MinScopeSystemd, true},
		{255, true},
		{0, true},
	} {
		want := tc.want && runtime.GOOS == "linux"
		v := tc.version
		if got := hasSystemdAt("run-helper", comm, func() int { return v }); got != want {
			t.Errorf("hasSystemdAt(systemd %d) = %v; want %v", v, got, want)
		}
	}
}

func TestParseSystemdVersion(t *testing.T) {
	for in, want := range map[string]int{
		"systemd 249 (249.11-0ubuntu3.22)\n+PAM +AUDIT +SELINUX": 249,
		"systemd 252 (252.39-1~deb12u2)\n":                       252,
		"systemd 255 (255.4-1ubuntu8.17)":                        255,
		"":                                                       0,
		"systemd":                                                0,
		"systemd abc (x)":                                        0,
		"libsystemd 255":                                         0,
	} {
		if got := ParseSystemdVersion(in); got != want {
			t.Errorf("ParseSystemdVersion(%q) = %d; want %d", in, got, want)
		}
	}
	if ScopeSystemdSupported(249) || !ScopeSystemdSupported(252) || !ScopeSystemdSupported(0) {
		t.Errorf("ScopeSystemdSupported floor drifted from %d", MinScopeSystemd)
	}
}

// TestSocketAliveDialsStream pins the bus probe's socket type: D-Bus
// listens on SOCK_STREAM, so a probe that dials any other type reports
// every live bus as dead. Per-user installs then lose --user (the scope
// fails with "Access denied") and the live confinement test skips
// silently instead of running.
func TestSocketAliveDialsStream(t *testing.T) {
	dir, err := os.MkdirTemp("", "sb") // short path: unix socket paths are length-limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "bus")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if !socketAlive(path) {
		t.Error("socketAlive(stream listener) = false; want true (D-Bus listens on SOCK_STREAM)")
	}
	if socketAlive(filepath.Join(dir, "missing")) {
		t.Error("socketAlive(missing socket) = true; want false")
	}
}
