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
	for _, want := range []string{"systemd-run", "--scope", "CPUQuota=200%", "CPUWeight=200", "OOMPolicy=continue", "MemoryMax=1073741824", "donmai-seat-abc-123.scope", "/bin/sh"} {
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
	for _, want := range []string{"systemd-run", "--user", "--scope", "donmai-seat-abc-123.scope", "/bin/sh"} {
		if !strings.Contains(userJoined, want) {
			t.Errorf("user-bus wrapped command %q missing %q", userJoined, want)
		}
	}
}

// TestSeatBudgetShimCommandMatchesDirectWrap pins the F1 parity: the shim
// launch path wraps its worker command with the same systemd-scope argv
// the direct spawn path applies, and stamps the same enforced handle. A
// divergence here is a seat that runs unconfined while its handle claims
// a confinement it did not get (or vice versa).
func TestSeatBudgetShimCommandMatchesDirectWrap(t *testing.T) {
	b := seatbudget.Budget{CPUs: 2, MemoryMB: 1024, Mode: "enforced"}
	want := applySeatBudgetToCmdForBus([]string{"/bin/worker", "agent", "run"}, "seat-shim-1", b, true, seatbudget.PlacementSystemd, false)
	got := seatBudgetShimCommand([]string{"/bin/worker", "agent", "run"}, "seat-shim-1", b, true, seatbudget.PlacementSystemd, false)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("shim command %q != direct wrap %q", strings.Join(got, " "), strings.Join(want, " "))
	}
	if !strings.Contains(strings.Join(got, " "), "donmai-seat-seat-shim-1.scope") {
		t.Errorf("shim command %q missing the per-session scope", strings.Join(got, " "))
	}
	// Disabled budgets leave the shim command byte-identical.
	plain := []string{"/bin/worker", "agent", "run"}
	if got := seatBudgetShimCommand(plain, "s", seatbudget.Budget{}, false, seatbudget.PlacementSystemd, false); strings.Join(got, " ") != strings.Join(plain, " ") {
		t.Errorf("disabled-budget shim wrap changed the command: %q", got)
	}
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

// TestSeatBudgetHandleReport_RecoverySurfaces pins F1 through the
// production handle paths: sessions the daemon adopts at startup (empty
// published handle), quarantines, or holds for recovery re-report the
// daemon's CURRENT resolved share instead of going stale at nil. A fresh
// direct/shim launch stamps the share at spawn; these three sites have no
// launch-time spec, so they snapshot the same helper the launch paths use.
func TestSeatBudgetHandleReport_RecoverySurfaces(t *testing.T) {
	d := New(Options{SkipRegistration: true})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/a/b"}},
		MaxConcurrentSessions: 4,
		SeatBudget:            SeatBudget{CPUs: 2, MemoryMB: 1024, Mode: "best-effort"},
	})
	// Quarantined + startup-adopted (empty handle) entries project the
	// current share.
	d.shims.adopted[sessionshim.Identity{OrgID: "o", SessionID: "adopted-at-startup"}] = adoptedShim{}
	d.shims.quarantined = append(d.shims.quarantined, sessionshim.QuarantinedSession{OrgID: "o", SessionID: "quarantined-lineage"})
	byID := map[string]SessionHandle{}
	for _, h := range d.sessionShimHandles() {
		byID[h.SessionID] = h
	}
	for _, id := range []string{"adopted-at-startup", "quarantined-lineage"} {
		h, ok := byID[id]
		if !ok {
			t.Fatalf("no handle projected for %q", id)
		}
		if h.SeatBudget == nil {
			t.Fatalf("%s handle carries no SeatBudget; want the current share", id)
		}
		if h.SeatBudget.Mode != "best-effort" || h.SeatBudget.CPUs != 2 {
			t.Errorf("%s SeatBudget = %+v; want best-effort 2 cpu", id, h.SeatBudget)
		}
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
	if held[0].SeatBudget.Mode != "best-effort" || held[0].SeatBudget.CPUs != 2 {
		t.Errorf("held SeatBudget = %+v; want best-effort 2 cpu", held[0].SeatBudget)
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
	if joined := strings.Join(wrapped, " "); !strings.Contains(joined, "donmai-seat-auto-seat.scope") || !strings.Contains(joined, "CPUQuota=200%") {
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
	defer func() {
		seatBudgetLaunchPlacement, seatBudgetLaunchUserScope = oldPlacement, oldUserScope
	}()

	const sessionID = "shim-budget-wrap"
	const marker = "shim-budget-wrap-marker"
	d := &Daemon{}
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		SeatBudget:            SeatBudget{CPUs: 2, MemoryMB: 256, Mode: "enforced"},
		WorkerCommand:         []string{"/bin/sh", "-c", "echo " + marker + "; exit 0"},
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
	for _, want := range []string{"--scope", "--collect", "CPUQuota=200%", "CPUWeight=200", "OOMPolicy=continue", "MemoryMax=268435456", "donmai-seat-shim-budget-wrap.scope", "/bin/sh"} {
		if !strings.Contains(joined, want) {
			t.Errorf("shim scope argv %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "AllowedCPUs") {
		t.Errorf("shim scope argv %q pins cores; want quota+weight only", joined)
	}
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
