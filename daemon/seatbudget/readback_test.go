package seatbudget

import (
	"errors"
	"strings"
	"testing"
)

// The `systemctl show` fixtures below are what a real systemd 255 printed for
// transient scopes created with `systemd-run --scope --collect` and the named
// limits, in the order it printed them (which is not the requested order).
// systemctl exits 0 for every one of them, including the unit that was never
// created.
const (
	showEnforced200 = "CPUQuotaPerSecUSec=2s\nIOWeight=200\nMemoryMax=268435456\nLoadState=loaded\n"
	showQuota150    = "CPUQuotaPerSecUSec=1.500000s\nIOWeight=[not set]\nMemoryMax=infinity\nLoadState=loaded\n"
	showQuota50     = "CPUQuotaPerSecUSec=500ms\nIOWeight=[not set]\nMemoryMax=infinity\nLoadState=loaded\n"
	showQuota6400   = "CPUQuotaPerSecUSec=1min 4s\nIOWeight=[not set]\nMemoryMax=infinity\nLoadState=loaded\n"
	showNoLimits    = "CPUQuotaPerSecUSec=infinity\nIOWeight=[not set]\nMemoryMax=infinity\nLoadState=loaded\n"
	showNotFound    = "CPUQuotaPerSecUSec=infinity\nIOWeight=[not set]\nMemoryMax=infinity\nLoadState=not-found\n"
	// showNotFoundWithoutLoadState is the never-created unit answered for a
	// request that leaves LoadState out: indistinguishable from a live scope
	// that caps nothing, which is why the read requires LoadState.
	showNotFoundWithoutLoadState = "CPUQuotaPerSecUSec=infinity\nIOWeight=[not set]\nMemoryMax=infinity\n"
)

// TestParseSeatShowReadsRealSystemdOutput pins the parse against the real
// output shape: timespan quotas, `infinity` and `[not set]` defaults, and a
// never-created unit that systemctl answers with exit 0 and default limits.
// A never-created (or already collected) scope must read as not-ok so the
// caller falls back to the launch record, never as "a scope with no limits".
func TestParseSeatShowReadsRealSystemdOutput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		out    string
		want   SeatLimits
		wantOK bool
	}{
		{name: "enforced 200% quota, 256MiB, io weight 200", out: showEnforced200, want: SeatLimits{CPUs: 2, MemoryMB: 256, IOWeight: 200}, wantOK: true},
		{name: "fractional 150% quota rounds up", out: showQuota150, want: SeatLimits{CPUs: 2}, wantOK: true},
		{name: "sub-core 50% quota is still a quota", out: showQuota50, want: SeatLimits{CPUs: 1}, wantOK: true},
		{name: "multi-term 6400% quota", out: showQuota6400, want: SeatLimits{CPUs: 64}, wantOK: true},
		{name: "live scope with no limits", out: showNoLimits, want: SeatLimits{}, wantOK: true},
		{name: "never-created unit", out: showNotFound, wantOK: false},
		{name: "never-created unit without LoadState", out: showNotFoundWithoutLoadState, wantOK: false},
		{name: "empty output", out: "", wantOK: false},
		{name: "missing a limit property", out: "CPUQuotaPerSecUSec=2s\nMemoryMax=infinity\nLoadState=loaded\n", wantOK: false},
		{name: "unparseable quota", out: "CPUQuotaPerSecUSec=2 fortnights\nIOWeight=[not set]\nMemoryMax=infinity\nLoadState=loaded\n", wantOK: false},
		{name: "unparseable memory", out: "CPUQuotaPerSecUSec=infinity\nIOWeight=[not set]\nMemoryMax=lots\nLoadState=loaded\n", wantOK: false},
		{name: "unparseable io weight", out: "CPUQuotaPerSecUSec=infinity\nIOWeight=heavy\nMemoryMax=infinity\nLoadState=loaded\n", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseSeatShow([]byte(tc.out))
			if ok != tc.wantOK {
				t.Fatalf("parseSeatShow ok = %v, want %v (limits %+v)", ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Errorf("parseSeatShow = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestReadSeatLimitsGatesBeforeSystemctl pins what happens before a read
// reaches systemctl: off Linux there is no scope to read; a scope name this
// package did not render (a corrupted or hostile launch record) never reaches
// the unit argument; a failed call is not an observation. A valid name reaches
// the runner with the launch's bus decision.
func TestReadSeatLimitsGatesBeforeSystemctl(t *testing.T) {
	t.Parallel()

	valid := ScopeNameForIncarnation("org", "session", 1)
	calls := 0
	run := func(scope string, userScope bool) ([]byte, error) {
		calls++
		if scope != valid {
			t.Errorf("runner got scope %q; only the validated name may reach systemctl", scope)
		}
		if !userScope {
			t.Error("runner got userScope=false; the read must use the launch's bus")
		}
		return []byte(showEnforced200), nil
	}
	got, ok := readSeatLimitsWithRunner(valid, true, run, "linux")
	if !ok || got != (SeatLimits{CPUs: 2, MemoryMB: 256, IOWeight: 200}) {
		t.Fatalf("valid live read = %+v, %v; want 2 cpu / 256MiB / io 200", got, ok)
	}
	if _, ok := readSeatLimitsWithRunner(valid, true, run, "darwin"); ok {
		t.Error("off-Linux read reports ok=true; want the record fallback")
	}
	hostile := []string{
		"",
		"donmai-seat-abc-1.scope",
		"other.service",
		valid + "\n",
		"donmai-seat-" + strings.Repeat("a", 32) + "-1.scope; systemctl stop everything",
		"donmai-seat-$(id)-1.scope",
		"donmai-seat-" + strings.Repeat("A", 32) + "-1.scope",
		"../donmai-seat-" + strings.Repeat("a", 32) + "-1.scope",
	}
	for _, scope := range hostile {
		if _, ok := readSeatLimitsWithRunner(scope, true, run, "linux"); ok {
			t.Errorf("scope %q read ok=true; want refusal before systemctl", scope)
		}
	}
	if _, ok := readSeatLimitsWithRunner(valid, true, func(string, bool) ([]byte, error) {
		return nil, errors.New("bus refused")
	}, "linux"); ok {
		t.Error("failed systemctl call reads ok=true; want the record fallback")
	}
	if calls != 1 {
		t.Errorf("runner called %d times; want exactly the one valid Linux read", calls)
	}
}

// TestValidScopeNameAcceptsOnlyRenderedNames pins that both scope namers
// produce names the read-back accepts, and nothing else passes.
func TestValidScopeNameAcceptsOnlyRenderedNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		ScopeNameForIncarnation("org", "session", 1),
		ScopeNameForIncarnation("", "session", 0),
		ScopeName("a/hostile id with spaces"),
	} {
		if !ValidScopeName(name) {
			t.Errorf("ValidScopeName(%q) = false; the namer's own output must read back", name)
		}
	}
	for _, name := range []string{"donmai-seat-session.scope", "donmai-seat-0-1.scope", "sshd.service"} {
		if ValidScopeName(name) {
			t.Errorf("ValidScopeName(%q) = true; want only the digest form", name)
		}
	}
}

// TestParseSystemdTimespan pins the timespan forms systemd renders for a USec
// property and refuses everything else, so an unreadable quota falls back to
// the launch record instead of reading as zero.
func TestParseSystemdTimespan(t *testing.T) {
	t.Parallel()

	ok := []struct {
		in   string
		want uint64
	}{
		{"0", 0},
		{"2s", 2_000_000},
		{"1.500000s", 1_500_000},
		{"500ms", 500_000},
		{"1min 4s", 64_000_000},
		{"1h", 3_600_000_000},
		{"250us", 250},
		{"250μs", 250},
		{"250µs", 250},
		{"1.5ms", 1_500},
	}
	for _, tc := range ok {
		got, err := parseSystemdTimespan(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseSystemdTimespan(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
	for _, in := range []string{"", " ", "s", "2", "2d", "1..5s", "1.s", ".5s", "1.5.5s", "-1s", "18446744073709551615min", "1.0000000001s"} {
		if got, err := parseSystemdTimespan(in); err == nil {
			t.Errorf("parseSystemdTimespan(%q) = %d, nil; want an error", in, got)
		}
	}
}

// TestReportSeatLimitsLiveThenRecord pins the adopted-handle rule: live cgroup
// limits win and read as enforced; a live scope that caps nothing reports the
// record's best-effort caps or none, never enforced numbers nothing enforces;
// an unreadable scope falls back to the launch record's own posture; no record
// at all reports none with the reason named. Nothing here reads the daemon's
// current configuration.
func TestReportSeatLimitsLiveThenRecord(t *testing.T) {
	t.Parallel()

	scope := ScopeNameForIncarnation("org", "session", 1)
	enforced := LaunchedSeat{Scope: scope, Mode: ModeEnforced, CPUs: 8, MemoryMB: 8192, IOWeight: 100}
	bestEffort := LaunchedSeat{Scope: scope, Mode: ModeBestEffort, CPUs: 3, MemoryMB: 512}

	live := ReportSeatLimits(enforced, SeatLimits{CPUs: 2, MemoryMB: 256, IOWeight: 200}, true)
	if live.Mode != ModeEnforced || live.CPUs != 2 || live.MemoryMB != 256 {
		t.Errorf("live report = %+v; want the live 2 cpu / 256MiB, not the record's 8/8192", live)
	}
	if !strings.Contains(live.Detail, scope) || !strings.Contains(live.Detail, "io weight 200") {
		t.Errorf("live detail = %q; want the scope and the live io weight named", live.Detail)
	}
	if got := ReportSeatLimits(bestEffort, SeatLimits{}, true); got.Mode != ModeBestEffort || got.CPUs != 3 {
		t.Errorf("live limit-free scope, best-effort record = %+v; want the record's best-effort 3 cpu caps", got)
	}
	if got := ReportSeatLimits(enforced, SeatLimits{}, true); got.Mode != ModeNone || !strings.Contains(got.Detail, "carries no limits") {
		t.Errorf("live limit-free scope, enforced record = %+v; want none (nothing enforces the record's numbers)", got)
	}
	memoryOnly := ReportSeatLimits(LaunchedSeat{Scope: scope}, SeatLimits{MemoryMB: 128}, true)
	if memoryOnly.Mode != ModeEnforced || memoryOnly.CPUs != 0 || strings.Contains(memoryOnly.Detail, "cpus") {
		t.Errorf("memory-only scope = %+v; want enforced memory with no invented CPU share", memoryOnly)
	}

	fallback := ReportSeatLimits(enforced, SeatLimits{}, false)
	if fallback.Mode != ModeEnforced || fallback.CPUs != 8 || fallback.MemoryMB != 8192 {
		t.Errorf("unreadable scope, enforced record = %+v; want the record's 8 cpu / 8192MiB", fallback)
	}
	if !strings.Contains(fallback.Detail, scope) || !strings.Contains(fallback.Detail, "launch record") {
		t.Errorf("fallback detail = %q; want the scope and the record source named", fallback.Detail)
	}
	if got := ReportSeatLimits(bestEffort, SeatLimits{}, false); got.Mode != ModeBestEffort || got.CPUs != 3 {
		t.Errorf("unreadable scope, best-effort record = %+v; want best-effort 3 cpu", got)
	}
	if got := ReportSeatLimits(LaunchedSeat{Scope: scope, Mode: ModeNone}, SeatLimits{}, false); got.Mode != ModeNone || !strings.Contains(got.Detail, scope) {
		t.Errorf("budgetless record = %+v; want none with the owning scope named", got)
	}
	if got := ReportSeatLimits(LaunchedSeat{Mode: ModeNone}, SeatLimits{}, false); got.Mode != ModeNone {
		t.Errorf("off-Linux budgetless record = %+v; want none", got)
	}
	if got := ReportSeatLimits(LaunchedSeat{}, SeatLimits{}, false); got.Mode != ModeNone || !strings.Contains(got.Detail, "no seat launch record") {
		t.Errorf("no record = %+v; want none with the missing record named", got)
	}
	if got := ReportSeatLimits(LaunchedSeat{Mode: ModeEnforced}, SeatLimits{}, false); got.Mode == ModeEnforced {
		t.Errorf("enforced record without a scope = %+v; want no enforced claim", got)
	}
}
