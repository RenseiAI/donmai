package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon/seatbudget"
	"github.com/RenseiAI/donmai/internal/localqueue"
	"github.com/RenseiAI/donmai/sessionshim"
)

// TestSpawner_SeatBudget_EnvCapsThroughAcceptWork drives the PRODUCTION spawn
// path (AcceptWork) with a budget and asserts the seat env carries the
// worker caps. OnPreSpawn captures the exact env the child execs with, so
// the assertion pins the deciding logic — removing the overlay must fail.
func TestSpawner_SeatBudget_EnvCapsThroughAcceptWork(t *testing.T) {
	var gotEnv []string
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		SeatBudget:            SeatBudget{CPUs: 3, Mode: "best-effort"},
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			gotEnv = append([]string(nil), env...)
			return nil, nil
		},
		StdoutPrefixWriter: PrefixWriterFunc(func(_, _ string) {}),
		StderrPrefixWriter: PrefixWriterFunc(func(_, _ string) {}),
	})
	ended := sessionEnds(s)
	handle, err := s.AcceptWork(SessionSpec{SessionID: "seat-env-caps", Repository: "github.com/a/b"})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	byKey := map[string]string{}
	for _, kv := range gotEnv {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			byKey[kv[:i]] = kv[i+1:]
		}
	}
	if byKey["GOMAXPROCS"] != "3" {
		t.Errorf("GOMAXPROCS in seat env = %q; want 3", byKey["GOMAXPROCS"])
	}
	if byKey["MAKEFLAGS"] != "-j3" {
		t.Errorf("MAKEFLAGS in seat env = %q; want -j3", byKey["MAKEFLAGS"])
	}
	// The unverified JS spellings must stay absent: a seat carrying them
	// would report caps its tools ignore.
	for _, dead := range []string{"VITEST_MAX_WORKERS", "NEXT_BUILD_WORKERS"} {
		if _, ok := byKey[dead]; ok {
			t.Errorf("seat env carries %q; no such tool control exists", dead)
		}
	}
	if handle.SeatBudget == nil {
		t.Fatal("handle.SeatBudget is nil; want the seat posture")
	}
	if handle.SeatBudget.Mode != "best-effort" || handle.SeatBudget.CPUs != 3 {
		t.Errorf("handle.SeatBudget = %+v; want best-effort 3 cpu", handle.SeatBudget)
	}
}

// TestSpawner_SeatBudget_DisabledSpawnsUnchanged pins that a zero budget
// leaves the spawn byte-identical: no caps in env, mode none on the handle.
func TestSpawner_SeatBudget_DisabledSpawnsUnchanged(t *testing.T) {
	var gotEnv []string
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			gotEnv = append([]string(nil), env...)
			return nil, nil
		},
		StdoutPrefixWriter: PrefixWriterFunc(func(_, _ string) {}),
		StderrPrefixWriter: PrefixWriterFunc(func(_, _ string) {}),
	})
	ended := sessionEnds(s)
	handle, err := s.AcceptWork(SessionSpec{SessionID: "seat-off", Repository: "github.com/a/b"})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	for _, kv := range gotEnv {
		for _, dead := range []string{"GOMAXPROCS=", "VITEST_MAX_WORKERS=", "NEXT_BUILD_WORKERS="} {
			if strings.HasPrefix(kv, dead) {
				t.Errorf("disabled budget leaked cap %q into seat env", kv)
			}
		}
	}
	if handle.SeatBudget == nil || handle.SeatBudget.Mode != "none" {
		t.Errorf("handle.SeatBudget = %+v; want mode none", handle.SeatBudget)
	}
}

// TestSpawner_SeatBudget_ExplicitKnobWins pins that an operator-set knob
// survives the budget overlay on the production path.
func TestSpawner_SeatBudget_ExplicitKnobWins(t *testing.T) {
	var gotEnv []string
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		BaseEnv:               map[string]string{"GOMAXPROCS": "16"},
		SeatBudget:            SeatBudget{CPUs: 2, Mode: "best-effort"},
		OnPreSpawn: func(_ SessionSpec, env []string) ([]string, error) {
			gotEnv = append([]string(nil), env...)
			return nil, nil
		},
		StdoutPrefixWriter: PrefixWriterFunc(func(_, _ string) {}),
		StderrPrefixWriter: PrefixWriterFunc(func(_, _ string) {}),
	})
	ended := sessionEnds(s)
	if _, err := s.AcceptWork(SessionSpec{SessionID: "seat-explicit", Repository: "github.com/a/b"}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)
	found := ""
	for _, kv := range gotEnv {
		if strings.HasPrefix(kv, "GOMAXPROCS=") {
			found = kv
		}
	}
	if found != "GOMAXPROCS=16" {
		t.Errorf("GOMAXPROCS in seat env = %q; want operator value GOMAXPROCS=16", found)
	}
}

// TestApplySeatBudgetToCmd_SystemdWrap pins the Linux wrap: under an
// enforced mode with a systemd placement the seat command is prefixed with
// the transient-scope argv. A test double for HostPlacement is unnecessary:
// the placement is an explicit argument, and the spawner passes the live
// probe — here the wrap itself is what the test drives.
func TestApplySeatBudgetToCmd_SystemdWrap(t *testing.T) {
	b := seatbudget.Budget{CPUs: 2, MemoryMB: 1024, Mode: "enforced"}
	got := applySeatBudgetToCmd([]string{"/bin/sh", "-c", "exit 0"}, "abc-123", b, true, seatbudget.PlacementSystemd)
	joined := strings.Join(got, " ")
	for _, want := range []string{"systemd-run", "--scope", "CPUQuota=200%", "CPUWeight=200", "OOMPolicy=continue", "MemoryMax=1073741824", "--unit", "donmai-seat-", "/bin/sh"} {
		if !strings.Contains(joined, want) {
			t.Errorf("wrapped command %q missing %q", joined, want)
		}
	}
	// No core pinning rides the scope: the quota is the binding limit
	// (identical AllowedCPUs ranges across seats shared 2 cores of 6
	// budgeted, and user services never get cpuset delegated).
	if strings.Contains(joined, "AllowedCPUs") {
		t.Errorf("wrapped command %q pins cores; want quota+weight only", joined)
	}
	if strings.Contains(joined, "--user") {
		t.Errorf("system-service wrap %q carries --user; want the system-bus spelling", joined)
	}
	// A per-user daemon service reaches only the user bus: its seats wrap
	// with --user, on the same transient scope shape.
	userGot := applySeatBudgetToCmdForBus([]string{"/bin/sh", "-c", "exit 0"}, "abc-123", b, true, seatbudget.PlacementSystemd, true)
	userJoined := strings.Join(userGot, " ")
	for _, want := range []string{"systemd-run", "--user", "--scope", "--unit", "donmai-seat-", "/bin/sh"} {
		if !strings.Contains(userJoined, want) {
			t.Errorf("user-bus wrapped command %q missing %q", userJoined, want)
		}
	}
}

// TestSeatBudgetShimCommandMatchesDirectWrap pins the shim launch wrap through
// the PRODUCTION entry point startShimProcess calls. On Linux every shim
// launch wraps in its own scope, budget or not; the scope carries limits only
// when the budget asks for hard enforcement — the same rule the direct lane
// applies — so a best-effort budget runs inside a limit-free scope on its
// cooperative caps. Without the systemd placement the launch is refused; off
// Linux the bare command runs. Reverting the production wrap to the bare
// worker command, or letting a best-effort budget carry limits, fails here.
func TestSeatBudgetShimCommandMatchesDirectWrap(t *testing.T) {
	worker := []string{"/bin/worker", "agent", "run"}
	unit := seatBudgetShimScopeName("test-org", "seat-shim-1", 1)
	limitProps := []string{"CPUQuota=", "CPUWeight=", "MemoryMax=", "MemoryHigh=", "IOWeight="}
	cases := []struct {
		name       string
		budget     seatbudget.Budget
		ok         bool
		wantLimits []string
	}{
		{
			name: "enforced", budget: seatbudget.Budget{CPUs: 2, MemoryMB: 1024, IOWeight: 300, Mode: "enforced"}, ok: true,
			wantLimits: []string{"CPUQuota=200%", "CPUWeight=200", "MemoryMax=1073741824", "IOWeight=300"},
		},
		{
			name: "auto confines on Linux", budget: seatbudget.Budget{CPUs: 2, Mode: "auto"}, ok: true,
			wantLimits: []string{"CPUQuota=200%", "CPUWeight=200"},
		},
		{name: "best-effort scopes without limits", budget: seatbudget.Budget{CPUs: 2, MemoryMB: 1024, Mode: "best-effort"}, ok: true},
		{name: "budgetless scopes without limits", budget: seatbudget.Budget{}, ok: false},
	}
	for _, tc := range cases {
		got, ok := seatBudgetShimCommandForLaunch(worker, unit, tc.budget, tc.ok, seatbudget.PlacementSystemd, false, "linux")
		if !ok {
			t.Fatalf("%s: shim wrap refused on a systemd host; want the scope argv", tc.name)
		}
		joined := strings.Join(got, " ")
		for _, want := range []string{"systemd-run", "--scope", "--collect", "OOMPolicy=continue", "/bin/worker agent run"} {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: shim command %q missing %q", tc.name, joined, want)
			}
		}
		if unitOf(got) != unit {
			t.Errorf("%s: shim command unit = %q; want the launch's scope %q", tc.name, unitOf(got), unit)
		}
		for _, want := range tc.wantLimits {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: shim command %q missing limit %q", tc.name, joined, want)
			}
		}
		if len(tc.wantLimits) == 0 {
			for _, banned := range limitProps {
				if strings.Contains(joined, banned) {
					t.Errorf("%s: shim command %q carries %q; this seat's scope must cap nothing", tc.name, joined, banned)
				}
			}
		}
	}
	// The scope name is stable for one launch identity and distinct per
	// incarnation.
	if seatBudgetShimScopeName("test-org", "seat-shim-1", 1) != unit {
		t.Error("scope name is not stable for one launch identity")
	}
	if seatBudgetShimScopeName("test-org", "seat-shim-1", 2) == unit {
		t.Error("epoch 1 and 2 share a scope; want distinct scopes per incarnation")
	}
	// No systemd placement: the launch is refused, never unwrapped. An
	// unscoped shim dies with its daemon while its handle claims a
	// survivorship it does not have.
	enforced := seatbudget.Budget{CPUs: 2, Mode: "enforced"}
	for _, placement := range []seatbudget.Placement{seatbudget.PlacementNone, seatbudget.PlacementCgroupFS} {
		if _, ok := seatBudgetShimCommandForLaunch(worker, unit, enforced, true, placement, false, "linux"); ok {
			t.Errorf("shim wrap succeeded on the %s placement; want refusal (nothing launches unscoped)", placement)
		}
	}
	if err := shimScopeGateError(seatbudget.PlacementNone); err == nil || !strings.Contains(err.Error(), "nothing launches unscoped") {
		t.Errorf("gate error = %v; want the unscoped-launch refusal named", err)
	}
	bare, ok := seatBudgetShimCommandForLaunch(worker, "", enforced, true, seatbudget.PlacementSystemd, false, "darwin")
	if !ok || strings.Join(bare, " ") != strings.Join(worker, " ") {
		t.Errorf("off-Linux shim command = %q, %v; want the bare %q (no scope exists there)", bare, ok, worker)
	}
}

// unitOf returns the --unit value of a systemd-run argv.
func unitOf(argv []string) string {
	for i, a := range argv {
		if a == "--unit" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// TestApplySeatBudgetToCmd_NoWrapWithoutPlacement pins the safe fallback:
// enforced mode with no backend leaves the command unwrapped (the seat runs
// unconfined and the handle report says none) rather than failing the spawn.
func TestApplySeatBudgetToCmd_NoWrapWithoutPlacement(t *testing.T) {
	b := seatbudget.Budget{CPUs: 2, Mode: "enforced"}
	cmd := []string{"/bin/worker", "agent", "run"}
	if got := applySeatBudgetToCmd(cmd, "s1", b, true, seatbudget.PlacementNone); strings.Join(got, " ") != strings.Join(cmd, " ") {
		t.Errorf("no-placement wrap changed the command: %q", got)
	}
	rep := sessionSeatBudgetReport(b, true, seatbudget.PlacementNone)
	if rep.Mode != "none" {
		t.Errorf("report mode = %q; want none when no backend exists", rep.Mode)
	}
	// Cgroupfs placement without the systemd wrapper: the launcher performs
	// no direct placement there, so the seat runs unwrapped and the report
	// is mode none with the missing backend named — never enforced for a
	// seat that runs unconfined.
	if got := applySeatBudgetToCmd(cmd, "s1", b, true, seatbudget.PlacementCgroupFS); strings.Join(got, " ") != strings.Join(cmd, " ") {
		t.Errorf("cgroupfs wrap changed the command: %q", got)
	}
	rep = sessionSeatBudgetReport(b, true, seatbudget.PlacementCgroupFS)
	if rep.Mode != "none" || !strings.Contains(rep.Detail, "cgroupfs") {
		t.Errorf("report = %+v; want none naming cgroupfs as unapplied", rep)
	}
	best := seatbudget.Budget{CPUs: 2, Mode: "best-effort"}
	if got := applySeatBudgetToCmd(cmd, "s1", best, true, seatbudget.PlacementSystemd); strings.Join(got, " ") != strings.Join(cmd, " ") {
		t.Errorf("best-effort wrap changed the command: %q", got)
	}
	if got := applySeatBudgetToCmd(cmd, "s1", b, false, seatbudget.PlacementSystemd); strings.Join(got, " ") != strings.Join(cmd, " ") {
		t.Errorf("disabled-budget wrap changed the command: %q", got)
	}
}

// TestSessionSeatBudgetReport_CgroupfsIsUnconfined pins the honest-fallback
// rule through the production report helper: a systemd-less Linux host
// (cgroupfs placement) runs the seat WITHOUT confinement, so the handle
// reports mode none with the missing backend named — never enforced for a
// seat that runs unwrapped. The status route (seatBudgetReport) applies the
// same downgrade; see TestServer_Status_SeatBudget_CgroupfsReportsNone.
func TestSessionSeatBudgetReport_CgroupfsIsUnconfined(t *testing.T) {
	b, ok := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		SeatBudget:            SeatBudget{CPUs: 2, MemoryMB: 1024, Mode: "enforced"},
	}).seatBudgetForSpawn()
	if !ok {
		t.Fatal("seatBudgetForSpawn disabled; want the enforced share")
	}
	rep := sessionSeatBudgetReport(b, ok, seatbudget.PlacementCgroupFS)
	if rep.Mode != "none" {
		t.Errorf("report mode = %q; want none (no direct cgroupfs placement exists)", rep.Mode)
	}
	if !strings.Contains(rep.Detail, "cgroupfs") {
		t.Errorf("report detail = %q; want the unapplied cgroupfs backend named", rep.Detail)
	}
	if !strings.Contains(rep.Detail, "systemd "+strconv.Itoa(seatbudget.MinScopeSystemd)) {
		t.Errorf("report detail = %q; want the systemd floor named (an old manager reads as no usable systemd)", rep.Detail)
	}
	if rep.CPUs != 0 || rep.MemoryMB != 0 {
		t.Errorf("report = %+v; want no values on an unconfined seat", rep)
	}
}

// TestSpawner_SeatBudget_SetSeatBudget pins the live-update path: a budget
// swapped after construction governs the next spawn.
func TestSpawner_SeatBudget_SetSeatBudget(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 2,
		StdoutPrefixWriter:    PrefixWriterFunc(func(_, _ string) {}),
		StderrPrefixWriter:    PrefixWriterFunc(func(_, _ string) {}),
	})
	s.SetSeatBudget(SeatBudget{CPUs: 5, Mode: "best-effort"})
	b, ok := s.seatBudgetForSpawn()
	if !ok || b.CPUs != 5 {
		t.Fatalf("seatBudgetForSpawn = %+v, %v; want 5 cpus", b, ok)
	}
	if _, ok := NewWorkerSpawner(SpawnerOptions{}).seatBudgetForSpawn(); ok {
		t.Error("zero spawner reports a budget; want disabled")
	}
}

// TestSpawner_SeatBudget_ConcurrentReloadAndSpawn pins the B3 lock: a live
// config reload (SetSeatBudget from the yaml watcher) racing spawns must
// not trip -race. The unlocked struct read this replaces passed every
// scoped suite because nothing hammered both sides concurrently — drive
// them together through the production AcceptWork path.
func TestSpawner_SeatBudget_ConcurrentReloadAndSpawn(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 8,
		SeatBudget:            SeatBudget{CPUs: 2, Mode: "best-effort"},
		StdoutPrefixWriter:    PrefixWriterFunc(func(_, _ string) {}),
		StderrPrefixWriter:    PrefixWriterFunc(func(_, _ string) {}),
	})
	s.Resume()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			s.SetSeatBudget(SeatBudget{CPUs: 2 + i%3, Mode: "best-effort"})
		}
	}()
	for i := 0; i < 8; i++ {
		spec := SessionSpec{SessionID: "race-reload-spawn-" + string(rune('a'+i)), Repository: "github.com/a/b"}
		if _, err := s.AcceptWork(spec); err != nil {
			// At-capacity rejections are fine: the race window is the
			// admission snapshot itself, which every call exercises
			// whether it spawns or not.
			if !strings.Contains(err.Error(), "at capacity") && !strings.Contains(err.Error(), "already") {
				t.Errorf("accept: %v", err)
			}
		}
	}
	<-done
}

// newSeatReportDaemon builds a daemon whose shim registry is open on a temp
// directory and whose current seat budget (8 cpu best-effort) deliberately
// differs from every launch record the tests write, so a report that read the
// current configuration could not hide. readLimits is the seat read-back.
func newSeatReportDaemon(t *testing.T, readLimits func(scope string, userScope bool) (seatbudget.SeatLimits, bool)) (*Daemon, *sessionshim.Registry) {
	t.Helper()
	d := New(Options{SkipRegistration: true})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 4,
		SeatBudget:            SeatBudget{CPUs: 8, MemoryMB: 8192, Mode: "best-effort"},
	})
	registry, err := sessionshim.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	d.shims.registry = registry
	d.shimScope = shimScopeProbe{goos: "linux", userScope: func() bool { return true }, readLimits: readLimits}
	return d, registry
}

func putSeatLaunch(t *testing.T, registry *sessionshim.Registry, id sessionshim.Identity, epoch uint64, mode string, cpus, memoryMB int) string {
	t.Helper()
	scope := seatBudgetShimScopeName(id.OrgID, id.SessionID, epoch)
	if err := registry.PutSeatLaunch(sessionshim.NewSeatLaunch(id, epoch, scope, mode, cpus, memoryMB, 0, time.Now())); err != nil {
		t.Fatalf("PutSeatLaunch: %v", err)
	}
	return scope
}

func noLiveScope(string, bool) (seatbudget.SeatLimits, bool) { return seatbudget.SeatLimits{}, false }

// TestSeatBudgetHandleReport_RecoverySurfaces pins the adopted-handle rule
// through the production handle path: sessions the daemon adopts at startup
// (empty published handle) or quarantines report the limits read back from
// the seat's own scope, falling back to the launch record written beside the
// discovery record — never this daemon's current configuration. A seat with
// no launch record reports none with the reason named.
func TestSeatBudgetHandleReport_RecoverySurfaces(t *testing.T) {
	d, registry := newSeatReportDaemon(t, noLiveScope)
	quarantinedID := sessionshim.Identity{OrgID: "o", SessionID: "quarantined-lineage"}
	quarantinedScope := putSeatLaunch(t, registry, quarantinedID, 1, sessionshim.SeatModeEnforced, 2, 1024)
	d.shims.quarantined = append(d.shims.quarantined, sessionshim.QuarantinedSession{
		OrgID: quarantinedID.OrgID, SessionID: quarantinedID.SessionID, ShimID: "shim-1", ProcessEpoch: 1,
	})
	adoptedID := sessionshim.Identity{OrgID: "o", SessionID: "adopted-at-startup"}
	d.shims.adopted[adoptedID] = adoptedShim{
		shimID:   "shim-2",
		adoption: SessionShimAdoptionEvidence{Identity: adoptedID, ShimID: "shim-2", ProcessEpoch: 1},
	}
	byID := map[string]SessionHandle{}
	for _, h := range d.sessionShimHandles() {
		byID[h.SessionID] = h
	}
	h, ok := byID["quarantined-lineage"]
	if !ok || h.SeatBudget == nil {
		t.Fatalf("quarantined handle = %+v; want one carrying the launched limits", h)
	}
	if h.SeatBudget.Mode != "enforced" || h.SeatBudget.CPUs != 2 || h.SeatBudget.MemoryMB != 1024 {
		t.Errorf("quarantined SeatBudget = %+v; want the launch record's enforced 2 cpu / 1024MiB, not the current 8-cpu share", h.SeatBudget)
	}
	if !strings.Contains(h.SeatBudget.Detail, quarantinedScope) {
		t.Errorf("quarantined detail = %q; want the owning scope named", h.SeatBudget.Detail)
	}
	a, ok := byID["adopted-at-startup"]
	if !ok || a.SeatBudget == nil || a.SeatBudget.Mode != "none" || !strings.Contains(a.SeatBudget.Detail, "no seat launch record") {
		t.Errorf("adopted-without-record SeatBudget = %+v; want none with the missing record named (never the current share)", a.SeatBudget)
	}
	// Held recovery projections stamp the same share. The projection
	// below is the minimal journal shape hold() reads: envelope session,
	// source, and initial event.
	proj := localqueue.SessionProjection{
		Admission: localqueue.AdmissionRecord{Envelope: localqueue.AdmissionEnvelope{
			Source:       localqueue.GitHubSource{OwnerRepo: "a/b"},
			InitialEvent: localqueue.LifecycleEvent{RecordedAt: time.Now().UTC().Format(time.RFC3339)},
		}},
	}
	proj.Admission.Envelope.Session.SessionID = "held-recovery"
	l := &localRuntime{daemon: d, held: map[string]SessionHandle{}, holdReasons: map[string]string{}}
	l.hold(proj, "test")
	held := l.heldSessions()
	if len(held) != 1 || held[0].SeatBudget == nil {
		t.Fatalf("held handle = %+v; want the current share stamped", held)
	}
	if held[0].SeatBudget.Mode != "best-effort" || held[0].SeatBudget.CPUs != 8 {
		t.Errorf("held SeatBudget = %+v; want the current best-effort 8 cpu share", held[0].SeatBudget)
	}
	// Budgeting off reads as mode none, never nil-shaped drift.
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 4,
	})
	if rep := d.seatBudgetHandleReport(); rep == nil || rep.Mode != "none" {
		t.Errorf("disabled handle report = %+v; want mode none", rep)
	}
}

// TestShimSeatReportReadsBackOnceOutsideTheLock pins how the handle path
// reads a seat back: the live scope wins over the launch record, each launch
// incarnation is read once (adoption never changes a running seat's limits,
// and the read execs systemctl), the read never runs under the shim-state
// lock, and a seat that leaves the adopted and quarantined sets leaves the
// cache with it.
func TestShimSeatReportReadsBackOnceOutsideTheLock(t *testing.T) {
	var d *Daemon
	calls := map[string]int{}
	readLimits := func(scope string, userScope bool) (seatbudget.SeatLimits, bool) {
		calls[scope]++
		if !userScope {
			t.Error("read-back used the system bus; want the launch's bus decision")
		}
		if !d.shims.mu.TryLock() {
			t.Error("seat read-back ran while the shim-state lock was held")
		} else {
			d.shims.mu.Unlock()
		}
		return seatbudget.SeatLimits{CPUs: 3, MemoryMB: 768}, true
	}
	d, registry := newSeatReportDaemon(t, readLimits)
	id := sessionshim.Identity{OrgID: "o", SessionID: "live-seat"}
	scope := putSeatLaunch(t, registry, id, 1, sessionshim.SeatModeEnforced, 2, 1024)
	d.shims.quarantined = append(d.shims.quarantined, sessionshim.QuarantinedSession{
		OrgID: id.OrgID, SessionID: id.SessionID, ShimID: "shim-1", ProcessEpoch: 1,
	})
	for round := 0; round < 3; round++ {
		handles := d.sessionShimHandles()
		if len(handles) != 1 || handles[0].SeatBudget == nil {
			t.Fatalf("round %d handles = %+v; want the one quarantined seat", round, handles)
		}
		if got := handles[0].SeatBudget; got.Mode != "enforced" || got.CPUs != 3 || got.MemoryMB != 768 {
			t.Errorf("round %d SeatBudget = %+v; want the live 3 cpu / 768MiB over the record's 2/1024", round, got)
		}
	}
	if calls[scope] != 1 {
		t.Errorf("live scope read %d times across three status reads; want once per incarnation", calls[scope])
	}
	d.shims.quarantined = nil
	_ = d.sessionShimHandles()
	d.shimSeatReports.mu.Lock()
	cached := len(d.shimSeatReports.reports)
	d.shimSeatReports.mu.Unlock()
	if cached != 0 {
		t.Errorf("cache holds %d reports after the seat left; want none", cached)
	}
}

// TestReadShimSeatReportNeverReadsCurrentConfig pins the report helper behind
// every shim handle: a launch record stands in when the live scope cannot be
// read, a best-effort record stays best-effort even in a limit-free live
// scope, and a seat whose record is missing is still read back through the
// scope its launch identity names. None of it reads the daemon's current
// configuration.
func TestReadShimSeatReportNeverReadsCurrentConfig(t *testing.T) {
	d, registry := newSeatReportDaemon(t, noLiveScope)
	recorded := sessionshim.Identity{OrgID: "o", SessionID: "recorded"}
	putSeatLaunch(t, registry, recorded, 2, sessionshim.SeatModeEnforced, 4, 2048)
	if rep := d.readShimSeatReport(registry, recorded, 2); rep.Mode != "enforced" || rep.CPUs != 4 || rep.MemoryMB != 2048 {
		t.Errorf("record fallback = %+v; want the record's 4 cpu / 2048MiB, not the current 8-cpu share", rep)
	}
	if rep := d.readShimSeatReport(registry, recorded, 3); rep.Mode != "none" {
		t.Errorf("another epoch without a record = %+v; want none", rep)
	}

	d.shimScope.readLimits = func(string, bool) (seatbudget.SeatLimits, bool) { return seatbudget.SeatLimits{}, true }
	bestEffort := sessionshim.Identity{OrgID: "o", SessionID: "best-effort"}
	putSeatLaunch(t, registry, bestEffort, 1, sessionshim.SeatModeBestEffort, 2, 0)
	if rep := d.readShimSeatReport(registry, bestEffort, 1); rep.Mode != "best-effort" || rep.CPUs != 2 {
		t.Errorf("best-effort seat in a limit-free scope = %+v; want best-effort 2 cpu", rep)
	}

	unrecorded := sessionshim.Identity{OrgID: "o", SessionID: "unrecorded"}
	derived := seatBudgetShimScopeName(unrecorded.OrgID, unrecorded.SessionID, 1)
	d.shimScope.readLimits = func(scope string, _ bool) (seatbudget.SeatLimits, bool) {
		if scope != derived {
			return seatbudget.SeatLimits{}, false
		}
		return seatbudget.SeatLimits{CPUs: 1, MemoryMB: 256}, true
	}
	if rep := d.readShimSeatReport(registry, unrecorded, 1); rep.Mode != "enforced" || rep.CPUs != 1 || !strings.Contains(rep.Detail, derived) {
		t.Errorf("seat without a record = %+v; want the live 1 cpu read back through %s", rep, derived)
	}
	d.shimScope.readLimits = func(scope string, _ bool) (seatbudget.SeatLimits, bool) {
		t.Errorf("read-back called with %q for an invalid identity; no scope may be derived from it", scope)
		return seatbudget.SeatLimits{}, false
	}
	if rep := d.readShimSeatReport(nil, sessionshim.Identity{}, 0); rep.Mode != "none" {
		t.Errorf("invalid identity = %+v; want none, with no scope derived", rep)
	}
}

// TestReadShimSeatReportReadsOnlyItsOwnScope pins the read-back to the scope
// the launch identity names. A launch record is written only by the launching
// daemon and always names the scope derived from (identity, epoch); a record
// that names any other unit — even a well-formed seat scope that passes the
// shape check, such as a different seat's — is corrupt, and querying that unit
// would attribute another seat's limits to this one. The read-back must query
// only the derived scope and must not report the record's facts.
func TestReadShimSeatReportReadsOnlyItsOwnScope(t *testing.T) {
	d, registry := newSeatReportDaemon(t, noLiveScope)
	id := sessionshim.Identity{OrgID: "o", SessionID: "own-seat"}
	own := seatBudgetShimScopeName(id.OrgID, id.SessionID, 1)
	foreign := seatBudgetShimScopeName("o", "other-seat", 1)
	if !seatbudget.ValidScopeName(foreign) {
		t.Fatalf("fixture scope %q must pass the shape check, or this test proves nothing", foreign)
	}
	if err := registry.PutSeatLaunch(sessionshim.NewSeatLaunch(id, 1, foreign, sessionshim.SeatModeEnforced, 16, 65536, 0, time.Now())); err != nil {
		t.Fatalf("PutSeatLaunch: %v", err)
	}
	var queried []string
	d.shimScope.readLimits = func(scope string, _ bool) (seatbudget.SeatLimits, bool) {
		queried = append(queried, scope)
		if scope == foreign {
			return seatbudget.SeatLimits{CPUs: 16, MemoryMB: 65536}, true
		}
		return seatbudget.SeatLimits{}, false
	}
	rep := d.readShimSeatReport(registry, id, 1)
	for _, scope := range queried {
		if scope != own {
			t.Errorf("read-back queried %q; only the launch identity's own scope %q may reach systemctl", scope, own)
		}
	}
	if rep.CPUs == 16 || rep.MemoryMB == 65536 || strings.Contains(rep.Detail, foreign) {
		t.Errorf("report = %+v; it carries the foreign scope's or the corrupt record's facts", rep)
	}
}

// TestResolveSeatBudget_SingleSeatKeepsHost pins the throughput rule at the
// daemon boundary: a lone seat is never throttled by defaults. A zero
// authored block means budgeting is off (CPUs 0, disabled) — the daemon
// must not invent limits the operator never asked for.
func TestResolveSeatBudget_SingleSeatKeepsHost(t *testing.T) {
	if got := resolveSeatBudget(SeatBudgetConfig{}, 1, 0, 0); !got.Disabled() {
		t.Errorf("zero block resolves to %+v; want disabled (budgeting off)", got)
	}
	// An authored mode with no numbers derives the whole host for one seat.
	if n := resolveSeatBudget(SeatBudgetConfig{Mode: "auto"}, 1, 16, 32768).CPUs; n != 16 {
		t.Errorf("single-seat CPUs = %d; want whole host 16", n)
	}
	got := resolveSeatBudget(SeatBudgetConfig{CPUs: 2}, 4, 32, 32768)
	if got.CPUs != 2 || got.MemoryMB != 8192 {
		t.Errorf("authored CPUs lost: %+v", got)
	}
	if err := validateSeatBudget(SeatBudgetConfig{CPUs: -1}); err == nil {
		t.Error("negative CPUs validate; want error")
	}
	if err := validateSeatBudget(SeatBudgetConfig{Mode: "hard"}); err == nil {
		t.Error("unknown mode validates; want error")
	}
}

// TestSeatBudget_EnvKnobsMatchEnforcement pins the two knob sets together:
// every variable the spawner injects is one the enforcement package
// declares, so the report cannot claim a cap the seat does not carry.
func TestSeatBudget_EnvKnobsMatchEnforcement(t *testing.T) {
	env := applySeatBudgetToEnv(nil, seatbudget.Budget{CPUs: 2, Mode: "best-effort"}, true)
	declared := map[string]struct{}{}
	for _, k := range seatbudget.WorkerCapKeys() {
		declared[k] = struct{}{}
	}
	for _, kv := range env {
		k := kv[:strings.IndexByte(kv, '=')]
		if _, ok := declared[k]; !ok {
			t.Errorf("spawner injected undeclared knob %q", k)
		}
	}
	if len(env) != len(declared) {
		t.Errorf("spawner injected %d knobs; enforcement declares %d", len(env), len(declared))
	}
}

// TestWantsEnforcementForGOOS_AutoMode pins the default-mode decision at
// the wrap level: `auto` is the documented default and resolveSeatBudget
// stores it verbatim, so the branch that confines default-configured Linux
// seats (wrap) versus degrades them (best-effort report) needs its own pin.
// EffectiveModeFor is pinned elsewhere but answers the reporter's question,
// not the spawner's — INVERTing the auto clause here (linux <-> non-linux)
// must fail this test, or default Linux seats ship unconfined while status
// still renders an enforced-shape line.
func TestWantsEnforcementForGOOS_AutoMode(t *testing.T) {
	cases := []struct {
		mode seatbudget.Mode
		goos string
		want bool
	}{
		{seatbudget.ModeEnforced, "linux", true},
		{seatbudget.ModeEnforced, "darwin", true},
		{"auto", "linux", true},
		{"auto", "darwin", false},
		{seatbudget.ModeBestEffort, "linux", false},
		{seatbudget.ModeBestEffort, "darwin", false},
		{seatbudget.ModeNone, "linux", false},
		{"", "linux", false},
	}
	for _, tc := range cases {
		if got := wantsEnforcementForGOOS(seatbudget.Budget{Mode: tc.mode}, tc.goos); got != tc.want {
			t.Errorf("wantsEnforcementForGOOS(%q, %q) = %v; want %v", tc.mode, tc.goos, got, tc.want)
		}
	}
}

// TestApplySeatBudgetToCmdForBusGOOS_AutoWrapAgreement pins the wrap/report
// agreement for the default mode on both platforms: an auto seat on a
// systemd Linux host wraps AND reports enforced; without a backend it runs
// unwrapped and reports none; off Linux it never wraps and reports
// best-effort. A wrap without an enforced report (or vice versa) is the
// false-confinement shape this package exists to prevent.
func TestApplySeatBudgetToCmdForBusGOOS_AutoWrapAgreement(t *testing.T) {
	auto := seatbudget.Budget{CPUs: 2, MemoryMB: 1024, Mode: "auto"}
	cmd := []string{"/bin/worker", "agent", "run"}
	// Linux + systemd: enforced-shape wrap with the per-session scope.
	wrapped := applySeatBudgetToCmdForBusGOOS(cmd, "auto-seat", auto, true, seatbudget.PlacementSystemd, false, "linux")
	if joined := strings.Join(wrapped, " "); !strings.Contains(joined, "donmai-seat-") || !strings.Contains(joined, "--unit") || !strings.Contains(joined, "CPUQuota=200%") {
		t.Errorf("auto/linux/systemd wrap = %q; want the enforced-shape scope argv", joined)
	}
	if rep := sessionSeatBudgetReportForGOOS(auto, true, seatbudget.PlacementSystemd, "linux"); rep.Mode != "enforced" {
		t.Errorf("auto/linux/systemd report mode = %q; want enforced to match the wrap", rep.Mode)
	}
	// Linux + no backend: no wrap, report none with the reason.
	if got := applySeatBudgetToCmdForBusGOOS(cmd, "auto-seat", auto, true, seatbudget.PlacementNone, false, "linux"); strings.Join(got, " ") != strings.Join(cmd, " ") {
		t.Errorf("auto/linux/none wrap changed the command: %q", strings.Join(got, " "))
	}
	if rep := sessionSeatBudgetReportForGOOS(auto, true, seatbudget.PlacementNone, "linux"); rep.Mode != "none" {
		t.Errorf("auto/linux/none report mode = %q; want none for the unwrapped seat", rep.Mode)
	}
	// Off Linux: never wraps (no backend exists there), reports best-effort.
	if got := applySeatBudgetToCmdForBusGOOS(cmd, "auto-seat", auto, true, seatbudget.PlacementSystemd, false, "darwin"); strings.Join(got, " ") != strings.Join(cmd, " ") {
		t.Errorf("auto/darwin wrap changed the command: %q", strings.Join(got, " "))
	}
	if rep := sessionSeatBudgetReportForGOOS(auto, true, seatbudget.PlacementSystemd, "darwin"); rep.Mode != "best-effort" {
		t.Errorf("auto/darwin report mode = %q; want best-effort for the cooperative seat", rep.Mode)
	}
}

// TestSeatBudgetForSpawn_ClampsNegativeLimits pins the embedder-composed
// boundary: a spawner built directly with negative memory or IO weight
// (bypassing config validation) reads back as uncapped, never as a
// negative number travelling into scope argv or reports.
func TestSeatBudgetForSpawn_ClampsNegativeLimits(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 1,
		SeatBudget:            SeatBudget{CPUs: 2, MemoryMB: -512, IOWeight: -5, Mode: "enforced"},
	})
	b, ok := s.seatBudgetForSpawn()
	if !ok {
		t.Fatal("seatBudgetForSpawn disabled; want the clamped share")
	}
	if b.MemoryMB != 0 {
		t.Errorf("MemoryMB = %d; want 0 (uncapped), not a negative cap", b.MemoryMB)
	}
	if b.IOWeight != 0 {
		t.Errorf("IOWeight = %d; want 0 (backend default), not a negative weight", b.IOWeight)
	}
	if b.CPUs != 2 {
		t.Errorf("CPUs = %d; want the authored 2", b.CPUs)
	}
}

var (
	_ = os.Getenv
	_ = exec.Command
)

// installFakeSystemdRun puts a fake systemd-run on PATH that records the argv
// it was invoked with and execs its tail command, and returns where the argv
// lands. The record is written to a temporary name and renamed into place, so
// a reader polling for the file never sees a half-written argv.
func installFakeSystemdRun(t *testing.T, dir string) string {
	t.Helper()
	fakes := filepath.Join(dir, "fakes")
	if err := os.MkdirAll(fakes, 0o700); err != nil {
		t.Fatal(err)
	}
	argvPath := filepath.Join(dir, "systemd-run-argv")
	fake := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$FAKE_SYSTEMD_RUN_ARGV.tmp\"\n" +
		"mv \"$FAKE_SYSTEMD_RUN_ARGV.tmp\" \"$FAKE_SYSTEMD_RUN_ARGV\"\n" +
		"while [ $# -gt 0 ]; do\n" +
		"  if [ \"$1\" = \"--unit\" ]; then shift 2; break; fi\n" +
		"  shift\n" +
		"done\n" +
		"exec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(fakes, "systemd-run"), []byte(fake), 0o700); err != nil { //nolint:gosec // G306: test-owned fake binary; the exec bit is the point (LookPath + exec need +x)
		t.Fatal(err)
	}
	t.Setenv("PATH", fakes+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_SYSTEMD_RUN_ARGV", argvPath)
	return argvPath
}

// waitForFile polls for a file another process writes after Start returns.
func waitForFile(t *testing.T, path, what string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			return raw
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared at %s: %v", what, path, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForShimMarker waits until the launched child's marker reaches its
// per-session log, proving the launch ran its worker through the wrap.
func waitForShimMarker(t *testing.T, registryDir string, identity sessionshim.Identity, marker string) {
	t.Helper()
	logPath := shimChildLogPath(registryDir, identity)
	deadline := time.Now().Add(5 * time.Second)
	for {
		content, readErr := os.ReadFile(logPath)
		if readErr == nil && strings.Contains(string(content), marker) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("shim child marker %q missing from %s; the wrap swallowed the launch", marker, logPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// linuxSystemdShimScope is the probe for a test that drives the Linux scope
// path against a fake systemd-run: the platform reads as Linux with the
// systemd placement on the system bus, and nothing reads a live scope.
func linuxSystemdShimScope() shimScopeProbe {
	return shimScopeProbe{
		goos:       "linux",
		placement:  func() seatbudget.Placement { return seatbudget.PlacementSystemd },
		userScope:  func() bool { return false },
		readLimits: noLiveScope,
	}
}

// startShimForScopeTest launches one shim through the PRODUCTION entry with
// the given budget under probe, and returns the launch and its registry.
func startShimForScopeTest(t *testing.T, dir, sessionID, marker string, budget SeatBudget, probe shimScopeProbe, env []string) (sessionshim.Launch, *sessionshim.Registry) {
	t.Helper()
	d := &Daemon{shimScope: probe}
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		SeatBudget:            budget,
		WorkerCommand:         []string{"/bin/sh", "-c", "echo " + marker + "; exit 0"},
		WorktreeParentDir:     dir,
	})
	launch := sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "test-org", SessionID: sessionID},
		RegistryDir:  dir + "/registry",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 1,
	}
	started, err := d.startShimProcess(SessionSpec{SessionID: sessionID}, launch, append([]string{"PATH=" + os.Getenv("PATH")}, env...))
	if err != nil {
		t.Fatalf("startShimProcess: %v", err)
	}
	if started.PID == 0 {
		t.Fatal("startShimProcess returned pid 0")
	}
	registry, err := sessionshim.NewRegistry(launch.RegistryDir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return launch, registry
}

// requireSeatLaunch reads the launch record startShimProcess wrote. It is
// written synchronously before the process starts, so it is already there
// when startShimProcess returns — no polling.
func requireSeatLaunch(t *testing.T, registry *sessionshim.Registry, launch sessionshim.Launch) sessionshim.SeatLaunch {
	t.Helper()
	rec, found, err := registry.SeatLaunch(launch.Identity, launch.ProcessEpoch)
	if err != nil || !found {
		t.Fatalf("seat launch record = found %v, err %v; startShimProcess must write it before the shim starts", found, err)
	}
	return rec
}

// TestStartShimProcess_WrapsEnforcedSeatInSystemdScope drives the PRODUCTION
// shim launch entry (startShimProcess) with an enforced seat budget against a
// fake systemd-run, and asserts both halves of the launch: the shim starts
// inside the transient scope carrying the seat's quota, weight and memory
// ceiling under the launch's digest unit, and the secret-free launch record
// beside the discovery record states that scope and those limits. Reverting
// startShimProcess to the bare worker command leaves no argv record; dropping
// the launch-record write leaves no record.
func TestStartShimProcess_WrapsEnforcedSeatInSystemdScope(t *testing.T) {
	dir := t.TempDir()
	argvPath := installFakeSystemdRun(t, dir)
	const sessionID, marker = "shim-budget-wrap", "shim-budget-wrap-marker"
	launch, registry := startShimForScopeTest(t, dir, sessionID, marker,
		SeatBudget{CPUs: 2, MemoryMB: 256, Mode: "enforced"}, linuxSystemdShimScope(),
		[]string{"FAKE_SYSTEMD_RUN_ARGV=" + argvPath})
	wantUnit := seatBudgetShimScopeName("test-org", sessionID, 1)

	rec := requireSeatLaunch(t, registry, launch)
	if rec.Scope != wantUnit || rec.Mode != sessionshim.SeatModeEnforced || rec.CPUs != 2 || rec.MemoryMB != 256 {
		t.Errorf("seat launch record = %+v; want enforced 2 cpu / 256MiB in %s", rec, wantUnit)
	}
	joined := strings.Join(strings.Split(strings.TrimSpace(string(waitForFile(t, argvPath, "fake systemd-run argv"))), "\n"), " ")
	for _, want := range []string{"--scope", "--collect", "CPUQuota=200%", "CPUWeight=200", "OOMPolicy=continue", "MemoryMax=268435456", "--unit " + wantUnit, "/bin/sh"} {
		if !strings.Contains(joined, want) {
			t.Errorf("shim scope argv %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "AllowedCPUs") {
		t.Errorf("shim scope argv %q pins cores; want quota+weight only", joined)
	}
	waitForShimMarker(t, launch.RegistryDir, launch.Identity, marker)
}

// TestStartShimProcess_BudgetlessSeatStillScopes drives the PRODUCTION shim
// launch entry with budgeting OFF and asserts the shim still launches inside
// its own transient scope — with no limit properties — and that the launch
// record names that scope with posture none. Reverting the production wrap to
// budget-only launches this seat bare, which is the restart-kills-the-seat
// shape this change exists to remove.
func TestStartShimProcess_BudgetlessSeatStillScopes(t *testing.T) {
	dir := t.TempDir()
	argvPath := installFakeSystemdRun(t, dir)
	const sessionID, marker = "shim-budgetless-scope", "shim-budgetless-scope-marker"
	launch, registry := startShimForScopeTest(t, dir, sessionID, marker, SeatBudget{}, linuxSystemdShimScope(),
		[]string{"FAKE_SYSTEMD_RUN_ARGV=" + argvPath})
	wantUnit := seatBudgetShimScopeName("test-org", sessionID, 1)

	rec := requireSeatLaunch(t, registry, launch)
	if rec.Scope != wantUnit || rec.Mode != sessionshim.SeatModeNone || rec.CPUs != 0 || rec.MemoryMB != 0 {
		t.Errorf("seat launch record = %+v; want posture none in %s", rec, wantUnit)
	}
	joined := strings.Join(strings.Split(strings.TrimSpace(string(waitForFile(t, argvPath, "fake systemd-run argv"))), "\n"), " ")
	for _, want := range []string{"--scope", "--collect", "OOMPolicy=continue", "--unit " + wantUnit, "/bin/sh"} {
		if !strings.Contains(joined, want) {
			t.Errorf("budgetless shim scope argv %q missing %q", joined, want)
		}
	}
	for _, banned := range []string{"CPUQuota=", "CPUWeight=", "MemoryMax=", "IOWeight="} {
		if strings.Contains(joined, banned) {
			t.Errorf("budgetless shim scope argv %q carries %q; want no limit properties", joined, banned)
		}
	}
	waitForShimMarker(t, launch.RegistryDir, launch.Identity, marker)
}

// TestShimSeatLaunchRecordStatesWhatTheSeatGot pins the launch record against
// the direct lane's report for the same budget and placement: enforced only
// where the scope carries the limits, best-effort for cooperative caps, none
// when budgeting is off — and off Linux never a scope and never enforced, so
// a macOS shim's handle cannot claim a transient systemd scope.
func TestShimSeatLaunchRecordStatesWhatTheSeatGot(t *testing.T) {
	id := sessionshim.Identity{OrgID: "org", SessionID: "seat"}
	scope := seatBudgetShimScopeName(id.OrgID, id.SessionID, 1)
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name     string
		scope    string
		budget   seatbudget.Budget
		ok       bool
		goos     string
		wantMode string
		wantCPUs int
		wantIO   int
	}{
		{name: "linux enforced", scope: scope, budget: seatbudget.Budget{CPUs: 2, MemoryMB: 256, IOWeight: 300, Mode: "enforced"}, ok: true, goos: "linux", wantMode: sessionshim.SeatModeEnforced, wantCPUs: 2, wantIO: 300},
		{name: "linux auto", scope: scope, budget: seatbudget.Budget{CPUs: 2, Mode: "auto"}, ok: true, goos: "linux", wantMode: sessionshim.SeatModeEnforced, wantCPUs: 2},
		{name: "linux best-effort", scope: scope, budget: seatbudget.Budget{CPUs: 2, IOWeight: 300, Mode: "best-effort"}, ok: true, goos: "linux", wantMode: sessionshim.SeatModeBestEffort, wantCPUs: 2},
		{name: "linux budgetless", scope: scope, goos: "linux", wantMode: sessionshim.SeatModeNone},
		{name: "darwin enforced", budget: seatbudget.Budget{CPUs: 2, Mode: "enforced"}, ok: true, goos: "darwin", wantMode: sessionshim.SeatModeNone},
		{name: "darwin auto", budget: seatbudget.Budget{CPUs: 2, Mode: "auto"}, ok: true, goos: "darwin", wantMode: sessionshim.SeatModeBestEffort, wantCPUs: 2},
		{name: "darwin budgetless", goos: "darwin", wantMode: sessionshim.SeatModeNone},
	}
	for _, tc := range cases {
		rec := shimSeatLaunchRecord(id, 1, tc.scope, tc.budget, tc.ok, tc.goos, now)
		if err := rec.Validate(); err != nil {
			t.Errorf("%s: launch record does not validate: %v", tc.name, err)
		}
		if rec.Scope != tc.scope || rec.Mode != tc.wantMode || rec.CPUs != tc.wantCPUs || rec.IOWeight != tc.wantIO {
			t.Errorf("%s: launch record = %+v; want scope %q, mode %s, %d cpu, io %d", tc.name, rec, tc.scope, tc.wantMode, tc.wantCPUs, tc.wantIO)
		}
	}
}

// TestStartShimProcess_OffLinuxNeverClaimsAScope drives the PRODUCTION launch
// entry off Linux with an enforced budget: the bare worker runs, the launch
// record names no scope, and the fresh handle's report is never enforced via
// a transient systemd scope that does not exist there.
func TestStartShimProcess_OffLinuxNeverClaimsAScope(t *testing.T) {
	dir := t.TempDir()
	const sessionID, marker = "shim-off-linux", "shim-off-linux-marker"
	probe := shimScopeProbe{goos: "darwin", placement: func() seatbudget.Placement { return seatbudget.PlacementNone }, readLimits: noLiveScope}
	launch, registry := startShimForScopeTest(t, dir, sessionID, marker, SeatBudget{CPUs: 2, MemoryMB: 256, Mode: "enforced"}, probe, nil)
	rec := requireSeatLaunch(t, registry, launch)
	if rec.Scope != "" || rec.Mode == sessionshim.SeatModeEnforced {
		t.Errorf("off-Linux launch record = %+v; want no scope and no enforced claim", rec)
	}
	d := &Daemon{shimScope: probe}
	if rep := d.readShimSeatReport(registry, launch.Identity, launch.ProcessEpoch); rep.Mode == "enforced" || strings.Contains(rep.Detail, "systemd") {
		t.Errorf("off-Linux handle report = %+v; want no enforced transient-scope claim", rep)
	}
	waitForShimMarker(t, launch.RegistryDir, launch.Identity, marker)
}

// TestStartShimProcess_RefusesWithoutScopeBackend pins the no-scope gate at
// the PRODUCTION launch entry: on Linux with no systemd placement the launch
// is REFUSED — nothing launches unscoped, because an unscoped shim dies with
// its daemon while its handle claims a survivorship it does not have — and it
// leaves no launch record behind.
func TestStartShimProcess_RefusesWithoutScopeBackend(t *testing.T) {
	dir := t.TempDir()
	d := &Daemon{shimScope: shimScopeProbe{goos: "linux", placement: func() seatbudget.Placement { return seatbudget.PlacementNone }, readLimits: noLiveScope}}
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		SeatBudget:            SeatBudget{CPUs: 2, MemoryMB: 256, Mode: "enforced"},
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		WorktreeParentDir:     dir,
	})
	launch := sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "test-org", SessionID: "no-scope-seat"},
		RegistryDir:  dir + "/registry",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 1,
	}
	if _, err := d.startShimProcess(SessionSpec{SessionID: "no-scope-seat"}, launch, nil); err == nil {
		t.Fatal("startShimProcess without a scope backend succeeded; want refusal (nothing launches unscoped)")
	} else if !strings.Contains(err.Error(), "transient-scope") {
		t.Errorf("refusal %q does not name the missing transient-scope backend", err)
	}
	if registry, err := sessionshim.NewRegistry(launch.RegistryDir); err == nil {
		if _, found, _ := registry.SeatLaunch(launch.Identity, 1); found {
			t.Error("a refused launch left a seat launch record behind")
		}
	}
}

// TestStartShimProcess_FailedStartRemovesTheLaunchRecord pins the launch
// record's failure path: the record is written before the process starts, so
// a start that fails must remove it again — nothing else will ever dispose of
// a record for a seat that never existed.
func TestStartShimProcess_FailedStartRemovesTheLaunchRecord(t *testing.T) {
	dir := t.TempDir()
	d := &Daemon{shimScope: shimScopeProbe{goos: "darwin", readLimits: noLiveScope}}
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{filepath.Join(dir, "no-such-worker")},
		WorktreeParentDir:     dir,
	})
	launch := sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "test-org", SessionID: "failed-start"},
		RegistryDir:  dir + "/registry",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 1,
	}
	if _, err := d.startShimProcess(SessionSpec{SessionID: "failed-start"}, launch, nil); err == nil {
		t.Fatal("startShimProcess with a missing worker succeeded; want a start failure")
	}
	registry, err := sessionshim.NewRegistry(launch.RegistryDir)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if _, found, err := registry.SeatLaunch(launch.Identity, 1); err != nil || found {
		t.Errorf("seat launch record after a failed start = found %v, err %v; want it removed", found, err)
	}
}
