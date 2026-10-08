package daemon

import (
	"os"
	"os/exec"
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
	for _, want := range []string{"systemd-run", "--scope", "AllowedCPUs=0-1", "CPUQuota=200%", "donmai-seat-abc-123.scope", "/bin/sh"} {
		if !strings.Contains(joined, want) {
			t.Errorf("wrapped command %q missing %q", joined, want)
		}
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

var (
	_ = os.Getenv
	_ = exec.Command
)
