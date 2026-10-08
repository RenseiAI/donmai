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
// the PRODUCTION entry point: seatBudgetShimCommandForLaunch is what
// startShimProcess calls, keyed by the full launch identity. A budgetless
// seat still wraps (no limit properties, still its own scope); a budgeted
// seat carries the quota, weight and memory ceiling. Reverting the
// production wrap to the bare worker command keeps every helper-level suite
// green but launches no scope here.
func TestSeatBudgetShimCommandMatchesDirectWrap(t *testing.T) {
	b := seatbudget.Budget{CPUs: 2, MemoryMB: 1024, Mode: "enforced"}
	got, ok := seatBudgetShimCommandForLaunch([]string{"/bin/worker", "agent", "run"}, "test-org", "seat-shim-1", 1, b, true, seatbudget.PlacementSystemd, false, "linux")
	if !ok {
		t.Fatal("shim wrap refused on a systemd host; want the scope argv")
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{"systemd-run", "--scope", "--collect", "CPUQuota=200%", "CPUWeight=200", "OOMPolicy=continue", "MemoryMax=1073741824", "/bin/worker"} {
		if !strings.Contains(joined, want) {
			t.Errorf("shim command %q missing %q", joined, want)
		}
	}
	if !strings.HasPrefix(unitOf(got), "donmai-seat-") || !strings.HasSuffix(unitOf(got), ".scope") {
		t.Errorf("shim command %q missing the per-launch scope unit", joined)
	}
	// The digest name is stable for one launch identity: same inputs, same
	// scope. Two launches of one session differ only by epoch.
	again, _ := seatBudgetShimCommandForLaunch([]string{"/bin/worker"}, "test-org", "seat-shim-1", 1, b, true, seatbudget.PlacementSystemd, false, "linux")
	if unitOf(got) != unitOf(again) {
		t.Errorf("scope unit %q != %q for the same launch; want a stable name", unitOf(got), unitOf(again))
	}
	next, _ := seatBudgetShimCommandForLaunch([]string{"/bin/worker"}, "test-org", "seat-shim-1", 2, b, true, seatbudget.PlacementSystemd, false, "linux")
	if unitOf(got) == unitOf(next) {
		t.Errorf("epoch 1 and 2 share scope unit %q; want distinct scopes per incarnation", unitOf(got))
	}
	// A budgetless seat still gets its own scope, with no limit
	// properties: the scope owns the seat's cgroup past a daemon restart.
	plain := []string{"/bin/worker", "agent", "run"}
	unbudgeted, ok := seatBudgetShimCommandForLaunch(plain, "test-org", "s", 1, seatbudget.Budget{}, false, seatbudget.PlacementSystemd, false, "linux")
	if !ok {
		t.Fatal("budgetless shim wrap refused on a systemd host; want a limit-free scope")
	}
	unjoined := strings.Join(unbudgeted, " ")
	if !strings.Contains(unjoined, "systemd-run") || !strings.Contains(unjoined, "--scope") {
		t.Errorf("budgetless shim command %q is unwrapped; want a scope with no limits", unjoined)
	}
	for _, banned := range []string{"CPUQuota=", "CPUWeight=", "MemoryMax="} {
		if strings.Contains(unjoined, banned) {
			t.Errorf("budgetless shim command %q carries %q; want no limit properties", unjoined, banned)
		}
	}
	// No systemd placement: the launch is refused, never unwrapped. An
	// unscoped shim dies with its daemon while its handle claims a
	// survivorship it does not have.
	if _, ok := seatBudgetShimCommandForLaunch(plain, "test-org", "s", 1, b, true, seatbudget.PlacementNone, false, "linux"); ok {
		t.Error("shim wrap succeeded with no backend; want refusal (nothing launches unscoped)")
	}
	if _, ok := seatBudgetShimCommandForLaunch(plain, "test-org", "s", 1, b, true, seatbudget.PlacementCgroupFS, false, "linux"); ok {
		t.Error("shim wrap succeeded on the cgroupfs placement; want refusal (nothing launches unscoped)")
	}
	if _, ok := seatBudgetShimCommandForLaunch(plain, "test-org", "s", 1, b, true, seatbudget.PlacementSystemd, false, "darwin"); !ok {
		t.Error("shim wrap refused off Linux; want the bare command (no scope exists there)")
	}
	if bare, _ := seatBudgetShimCommandForLaunch(plain, "test-org", "s", 1, b, true, seatbudget.PlacementSystemd, false, "darwin"); strings.Join(bare, " ") != strings.Join(plain, " ") {
		t.Errorf("off-Linux shim command %q != bare %q", strings.Join(bare, " "), strings.Join(plain, " "))
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

// TestSeatBudgetHandleReport_RecoverySurfaces pins the adopted-handle rule
// through the production handle paths: sessions the daemon adopts at startup
// (empty published handle) or quarantines report the limits read back from
// the seat's own cgroup, falling back to the launch record — never this
// daemon's current configuration. A quarantined entry whose record carries
// the launched limits reports them even after the operator reconfigures the
// host; an entry with no seat facts reports none with the reason named.
func TestSeatBudgetHandleReport_RecoverySurfaces(t *testing.T) {
	d := New(Options{SkipRegistration: true})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 4,
		// Deliberately DIFFERENT from the record's launched limits below:
		// the handles must report the seat's own facts, not this share.
		SeatBudget: SeatBudget{CPUs: 8, MemoryMB: 8192, Mode: "best-effort"},
	})
	// A quarantined entry whose discovery record carried the launched limits
	// reports those limits — not the daemon's current 8-CPU share.
	d.shims.quarantined = append(d.shims.quarantined, sessionshim.QuarantinedSession{
		OrgID: "o", SessionID: "quarantined-lineage",
		ShimID: "shim-1", ProcessEpoch: 1,
		SeatScope: "donmai-seat-abc-1.scope", SeatCPUs: 2, SeatMemoryMB: 1024,
	})
	// Adopted-at-startup with no controller and no seat facts: the honest
	// report is none with the reason named, never the current share.
	d.shims.adopted[sessionshim.Identity{OrgID: "o", SessionID: "adopted-at-startup"}] = adoptedShim{}
	byID := map[string]SessionHandle{}
	for _, h := range d.sessionShimHandles() {
		byID[h.SessionID] = h
	}
	h, ok := byID["quarantined-lineage"]
	if !ok {
		t.Fatal("no handle projected for quarantined-lineage")
	}
	if h.SeatBudget == nil {
		t.Fatal("quarantined handle carries no SeatBudget; want the launched limits")
	}
	if h.SeatBudget.Mode != "enforced" || h.SeatBudget.CPUs != 2 || h.SeatBudget.MemoryMB != 1024 {
		t.Errorf("quarantined SeatBudget = %+v; want the record's enforced 2 cpu / 1024MiB, not the current 8-cpu share", h.SeatBudget)
	}
	if !strings.Contains(h.SeatBudget.Detail, "donmai-seat-abc-1.scope") {
		t.Errorf("quarantined detail = %q; want the owning scope named", h.SeatBudget.Detail)
	}
	a, ok := byID["adopted-at-startup"]
	if !ok {
		t.Fatal("no handle projected for adopted-at-startup")
	}
	if a.SeatBudget == nil || a.SeatBudget.Mode != "none" {
		t.Errorf("adopted-without-facts SeatBudget = %+v; want mode none (never the current share)", a.SeatBudget)
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

// shimLaunchEnvProbe is a WorkerCommand that records the exact environment
// the launched shim process starts with, then runs the tail command. The
// fake systemd-run prefix (see the startShimProcess tests) execs its tail,
// so chaining this probe BEHIND it captures the launch-contract half of
// startShimProcess: the scope unit and launched limits stamped into the
// child env. Deleting the launch.SeatScope/limits stamp keeps the scope
// wrap green but leaves this probe's record without the seat keys — the
// mutation B3 names.
func shimLaunchEnvProbe(t *testing.T, dir string) (workerCommand []string, envPath string) {
	t.Helper()
	envPath = filepath.Join(dir, "shim-launch-env")
	// The probe appends its own environ to envPath (one KEY=VALUE per
	// line), then execs the tail command unchanged. "$@" preserves the
	// worker argv startShimProcess configured; the env record lands before
	// the tail replaces this shell.
	probe := "env > \"$SHIM_LAUNCH_ENV_PATH\"; exec \"$@\""
	return []string{"/bin/sh", "-c", probe, "shim-launch-env-probe"}, envPath
}

// readShimLaunchEnvRecord polls for the probe's env record the same way the
// scope-argv tests poll for the fake systemd-run argv: Start returns before
// the child runs, so the record lands asynchronously.
func readShimLaunchEnvRecord(t *testing.T, envPath string) map[string]string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, readErr := os.ReadFile(envPath)
		if readErr == nil {
			byKey := map[string]string{}
			for _, line := range strings.Split(string(raw), "\n") {
				if k, v, ok := strings.Cut(line, "="); ok {
					byKey[k] = v
				}
			}
			return byKey
		}
		if time.Now().After(deadline) {
			t.Fatalf("shim launch env probe never ran (no env record): %v — the launch never reached its worker", readErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// assertShimLaunchEnvCarriesSeatFacts pins the launch-record half of
// startShimProcess: the child env carries the digest scope unit plus the
// launched limits, so the shim republishes them into its discovery record
// and a restarted daemon falls back to them — never to empty facts.
func assertShimLaunchEnvCarriesSeatFacts(t *testing.T, byKey map[string]string, wantScope string, wantCPUs, wantMemoryMB int) {
	t.Helper()
	if got := byKey[sessionshim.EnvSeatScope]; got != wantScope {
		t.Errorf("launched env %s = %q; want the digest scope %q", sessionshim.EnvSeatScope, got, wantScope)
	}
	if got := byKey[sessionshim.EnvSeatCPUs]; got != strconv.Itoa(wantCPUs) {
		t.Errorf("launched env %s = %q; want %d", sessionshim.EnvSeatCPUs, got, wantCPUs)
	}
	if got := byKey[sessionshim.EnvSeatMemoryMB]; got != strconv.Itoa(wantMemoryMB) {
		t.Errorf("launched env %s = %q; want %d", sessionshim.EnvSeatMemoryMB, got, wantMemoryMB)
	}
}

// TestStartShimProcess_WrapsEnforcedSeatInSystemdScope drives the PRODUCTION
// shim launch entry (startShimProcess) with an enforced seat budget and a
// stubbed systemd placement, and asserts the shim actually launches inside
// the transient scope. A fake systemd-run on PATH records the argv it was
// invoked with and execs the tail worker command, so the test observes both
// halves: the scope argv carries the seat's quota, weight, memory ceiling
// and per-session unit, and the shim child still starts (its marker lands
// in the per-session log). Reverting startShimProcess to the bare worker
// command keeps every other suite green but leaves no argv record here —
// the parity helper test alone cannot catch a dropped production wrap.
func TestStartShimProcess_WrapsEnforcedSeatInSystemdScope(t *testing.T) {
	dir := t.TempDir()
	fakes := filepath.Join(dir, "fakes")
	if err := os.MkdirAll(fakes, 0o700); err != nil {
		t.Fatal(err)
	}
	argvPath := filepath.Join(dir, "systemd-run-argv")
	fake := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$FAKE_SYSTEMD_RUN_ARGV\"\n" +
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

	oldPlacement, oldUserScope := seatBudgetLaunchPlacement, seatBudgetLaunchUserScope
	seatBudgetLaunchPlacement = func() seatbudget.Placement { return seatbudget.PlacementSystemd }
	seatBudgetLaunchUserScope = func() bool { return false }
	oldScopeGOOS := shimScopeGOOS
	shimScopeGOOS = "linux"
	defer func() {
		seatBudgetLaunchPlacement, seatBudgetLaunchUserScope = oldPlacement, oldUserScope
		shimScopeGOOS = oldScopeGOOS
	}()

	const sessionID = "shim-budget-wrap"
	const marker = "shim-budget-wrap-marker"
	probeCmd, envPath := shimLaunchEnvProbe(t, dir)
	d := &Daemon{}
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		SeatBudget:            SeatBudget{CPUs: 2, MemoryMB: 256, Mode: "enforced"},
		WorkerCommand:         append(append([]string(nil), probeCmd...), "/bin/sh", "-c", "echo "+marker+"; exit 0"),
		WorktreeParentDir:     dir,
	})
	identity := sessionshim.Identity{OrgID: "test-org", SessionID: sessionID}
	launch := sessionshim.Launch{
		Identity:     identity,
		RegistryDir:  dir + "/registry",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 1,
	}
	started, err := d.startShimProcess(SessionSpec{SessionID: sessionID}, launch, []string{
		"PATH=" + os.Getenv("PATH"),
		"FAKE_SYSTEMD_RUN_ARGV=" + argvPath,
		"SHIM_LAUNCH_ENV_PATH=" + envPath,
	})
	if err != nil {
		t.Fatalf("startShimProcess: %v", err)
	}
	if started.PID == 0 {
		t.Fatal("startShimProcess returned pid 0")
	}
	// The fake records its argv asynchronously after Start returns: poll
	// for the record the same way the stdio test polls for the child log.
	var raw []byte
	argvDeadline := time.Now().Add(5 * time.Second)
	for {
		var readErr error
		raw, readErr = os.ReadFile(argvPath)
		if readErr == nil {
			break
		}
		if time.Now().After(argvDeadline) {
			t.Fatalf("fake systemd-run never invoked (no argv record): %v — the shim launched unwrapped", readErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	joined := strings.Join(strings.Split(strings.TrimSpace(string(raw)), "\n"), " ")
	for _, want := range []string{"--scope", "--collect", "CPUQuota=200%", "CPUWeight=200", "OOMPolicy=continue", "MemoryMax=268435456", "/bin/sh"} {
		if !strings.Contains(joined, want) {
			t.Errorf("shim scope argv %q missing %q", joined, want)
		}
	}
	// The unit is the digest scope for this launch identity (org, session,
	// epoch 1) — never a truncation of the session id.
	wantUnit := seatBudgetShimScopeName("test-org", sessionID, 1)
	if !strings.Contains(joined, wantUnit) {
		t.Errorf("shim scope argv %q missing digest unit %q", joined, wantUnit)
	}
	if strings.Contains(joined, "donmai-seat-shim-budget-wrap.scope") {
		t.Errorf("shim scope argv %q carries the legacy truncated unit; want the digest scope", joined)
	}
	if strings.Contains(joined, "AllowedCPUs") {
		t.Errorf("shim scope argv %q pins cores; want quota+weight only", joined)
	}
	// The launch-record half: the child env carries the same digest scope
	// plus the launched limits, so the shim republishes them into its
	// discovery record. Deleting the startShimProcess stamp keeps the wrap
	// above green but empties these — the silent-restart-regression shape.
	assertShimLaunchEnvCarriesSeatFacts(t, readShimLaunchEnvRecord(t, envPath), wantUnit, 2, 256)
	// The shim child ran through the wrap: its marker reaches the log.
	logPath := shimChildLogPath(launch.RegistryDir, identity)
	deadline := time.Now().Add(5 * time.Second)
	for {
		content, readErr := os.ReadFile(logPath)
		if readErr == nil && strings.Contains(string(content), marker) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shim child marker %q missing from %s; the wrap swallowed the launch", marker, logPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStartShimProcess_BudgetlessSeatStillScopes drives the PRODUCTION shim
// launch entry (startShimProcess) with budgeting OFF and a stubbed systemd
// placement, and asserts the shim still launches inside its own transient
// scope — with no limit properties. Reverting the production wrap to
// budget-only keeps every other suite green but launches this seat bare,
// which is the restart-kills-the-seat shape this slice exists to remove.
func TestStartShimProcess_BudgetlessSeatStillScopes(t *testing.T) {
	dir := t.TempDir()
	fakes := filepath.Join(dir, "fakes")
	if err := os.MkdirAll(fakes, 0o700); err != nil {
		t.Fatal(err)
	}
	argvPath := filepath.Join(dir, "systemd-run-argv")
	fake := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$FAKE_SYSTEMD_RUN_ARGV\"\n" +
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

	oldPlacement, oldUserScope := seatBudgetLaunchPlacement, seatBudgetLaunchUserScope
	seatBudgetLaunchPlacement = func() seatbudget.Placement { return seatbudget.PlacementSystemd }
	seatBudgetLaunchUserScope = func() bool { return false }
	oldScopeGOOS := shimScopeGOOS
	shimScopeGOOS = "linux"
	defer func() {
		seatBudgetLaunchPlacement, seatBudgetLaunchUserScope = oldPlacement, oldUserScope
		shimScopeGOOS = oldScopeGOOS
	}()

	const sessionID = "shim-budgetless-scope"
	const marker = "shim-budgetless-scope-marker"
	probeCmd, envPath := shimLaunchEnvProbe(t, dir)
	d := &Daemon{}
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		WorkerCommand:         append(append([]string(nil), probeCmd...), "/bin/sh", "-c", "echo "+marker+"; exit 0"),
		WorktreeParentDir:     dir,
	})
	identity := sessionshim.Identity{OrgID: "test-org", SessionID: sessionID}
	launch := sessionshim.Launch{
		Identity:     identity,
		RegistryDir:  dir + "/registry",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 1,
	}
	started, err := d.startShimProcess(SessionSpec{SessionID: sessionID}, launch, []string{
		"PATH=" + os.Getenv("PATH"),
		"FAKE_SYSTEMD_RUN_ARGV=" + argvPath,
		"SHIM_LAUNCH_ENV_PATH=" + envPath,
	})
	if err != nil {
		t.Fatalf("startShimProcess without a budget: %v", err)
	}
	if started.PID == 0 {
		t.Fatal("startShimProcess returned pid 0")
	}
	var raw []byte
	argvDeadline := time.Now().Add(5 * time.Second)
	for {
		var readErr error
		raw, readErr = os.ReadFile(argvPath)
		if readErr == nil {
			break
		}
		if time.Now().After(argvDeadline) {
			t.Fatalf("fake systemd-run never invoked (no argv record): %v — the budgetless seat launched bare", readErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	joined := strings.Join(strings.Split(strings.TrimSpace(string(raw)), "\n"), " ")
	for _, want := range []string{"--scope", "--collect", "OOMPolicy=continue", "/bin/sh"} {
		if !strings.Contains(joined, want) {
			t.Errorf("budgetless shim scope argv %q missing %q", joined, want)
		}
	}
	for _, banned := range []string{"CPUQuota=", "CPUWeight=", "MemoryMax=", "IOWeight="} {
		if strings.Contains(joined, banned) {
			t.Errorf("budgetless shim scope argv %q carries %q; want no limit properties", joined, banned)
		}
	}
	if wantUnit := seatBudgetShimScopeName("test-org", sessionID, 1); !strings.Contains(joined, wantUnit) {
		t.Errorf("budgetless shim scope argv %q missing digest unit %q", joined, wantUnit)
	}
	// The budgetless seat still stamps its scope: zero limits ride as "0"
	// so the shim republishes the scope into its record and a restarted
	// daemon recovers the owning unit instead of an empty one.
	assertShimLaunchEnvCarriesSeatFacts(t, readShimLaunchEnvRecord(t, envPath), seatBudgetShimScopeName("test-org", sessionID, 1), 0, 0)
	logPath := shimChildLogPath(launch.RegistryDir, identity)
	deadline := time.Now().Add(5 * time.Second)
	for {
		content, readErr := os.ReadFile(logPath)
		if readErr == nil && strings.Contains(string(content), marker) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shim child marker %q missing from %s; the wrap swallowed the launch", marker, logPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStartShimProcess_RefusesWithoutScopeBackend pins the no-scope gate at
// the PRODUCTION launch entry: on Linux with no systemd placement the launch
// is REFUSED — nothing launches unscoped, because an unscoped shim dies with
// its daemon while its handle claims a survivorship it does not have.
// Reverting the gate to the old unwrap-and-run keeps every other suite green
// but lets this launch succeed bare here.
func TestStartShimProcess_RefusesWithoutScopeBackend(t *testing.T) {
	dir := t.TempDir()
	oldPlacement, oldUserScope := seatBudgetLaunchPlacement, seatBudgetLaunchUserScope
	seatBudgetLaunchPlacement = func() seatbudget.Placement { return seatbudget.PlacementNone }
	seatBudgetLaunchUserScope = func() bool { return false }
	oldScopeGOOS := shimScopeGOOS
	shimScopeGOOS = "linux"
	defer func() {
		seatBudgetLaunchPlacement, seatBudgetLaunchUserScope = oldPlacement, oldUserScope
		shimScopeGOOS = oldScopeGOOS
	}()

	d := &Daemon{}
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		SeatBudget:            SeatBudget{CPUs: 2, MemoryMB: 256, Mode: "enforced"},
		WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
		WorktreeParentDir:     dir,
	})
	identity := sessionshim.Identity{OrgID: "test-org", SessionID: "no-scope-seat"}
	launch := sessionshim.Launch{
		Identity:     identity,
		RegistryDir:  dir + "/registry",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 1,
	}
	if _, err := d.startShimProcess(SessionSpec{SessionID: "no-scope-seat"}, launch, nil); err == nil {
		t.Fatal("startShimProcess without a scope backend succeeded; want refusal (nothing launches unscoped)")
	} else if !strings.Contains(err.Error(), "transient-scope") {
		t.Errorf("refusal %q does not name the missing transient-scope backend", err)
	}
}

// TestShimScopeGateMatrix pins the platform gate: only Linux with the systemd
// placement may launch shims. Off Linux the shim launches bare (no scope
// exists there); on Linux without systemd nothing launches at all.
func TestShimScopeGateMatrix(t *testing.T) {
	if !shimScopeAvailable(seatbudget.PlacementSystemd, "linux") {
		t.Error("systemd/linux must allow shim launches")
	}
	for _, placement := range []seatbudget.Placement{seatbudget.PlacementNone, seatbudget.PlacementCgroupFS} {
		if shimScopeAvailable(placement, "linux") {
			t.Errorf("linux/%s must refuse shim launches (nothing launches unscoped)", placement)
		}
	}
	if shimScopeAvailable(seatbudget.PlacementSystemd, "darwin") {
		t.Error("darwin must not take the scope path (no scope exists there)")
	}
	if err := shimScopeGateError(seatbudget.PlacementNone); err == nil || !strings.Contains(err.Error(), "nothing launches unscoped") {
		t.Errorf("gate error = %v; want the unscoped-launch refusal named", err)
	}
}

// TestAdoptedHandleReportsRecordLimitsNotCurrentConfig pins the adopted-handle
// rule through the PRODUCTION report helper: a started-at-startup handle whose
// record carries the launched limits reports THOSE limits — even though this
// daemon's current configuration resolves a different share. Reverting the
// helper to the current-config report keeps every other suite green but
// reports the wrong numbers here.
func TestAdoptedHandleReportsRecordLimitsNotCurrentConfig(t *testing.T) {
	d := New(Options{SkipRegistration: true})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 4,
		// Best-effort on purpose: the current-config report reads non-none
		// for it on any host, so a reverted helper cannot hide behind the
		// no-backend downgrade.
		SeatBudget: SeatBudget{CPUs: 8, MemoryMB: 8192, Mode: "best-effort"},
	})
	// A controller-less adopted entry with no seat facts reports none —
	// never the current 8-CPU best-effort share.
	entry := adoptedShim{}
	rep := d.seatBudgetAdoptedReport(entry.controller)
	if rep == nil || rep.Mode != "none" {
		t.Fatalf("adopted report without facts = %+v; want mode none, never the current best-effort share", rep)
	}
	if rep.CPUs != 0 || rep.MemoryMB != 0 {
		t.Errorf("adopted report without facts = %+v; want no values", rep)
	}
	// A quarantined entry reports its own record facts through the same
	// production helper the handle path uses.
	q := sessionshim.QuarantinedSession{
		OrgID: "o", SessionID: "q-1", ShimID: "shim-9", ProcessEpoch: 2,
		SeatScope: "donmai-seat-q-2.scope", SeatCPUs: 4, SeatMemoryMB: 2048,
	}
	qrep := d.seatBudgetQuarantinedReport(q)
	if qrep.Mode != "enforced" || qrep.CPUs != 4 || qrep.MemoryMB != 2048 {
		t.Errorf("quarantined report = %+v; want the entry's 4 cpu / 2048MiB, not the current 8-cpu share", qrep)
	}
}
