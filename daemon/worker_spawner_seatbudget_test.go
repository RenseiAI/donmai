package daemon

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/daemon/seatbudget"
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
	if byKey["VITEST_MAX_WORKERS"] != "3" {
		t.Errorf("VITEST_MAX_WORKERS in seat env = %q; want 3", byKey["VITEST_MAX_WORKERS"])
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
		if strings.HasPrefix(kv, "GOMAXPROCS=") || strings.HasPrefix(kv, "VITEST_MAX_WORKERS=") {
			t.Errorf("disabled budget leaked cap %q into seat env", kv)
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
	// Cgroupfs placement without the systemd wrapper: env caps still ride,
	// command unwrapped, report names cgroupfs.
	rep = sessionSeatBudgetReport(b, true, seatbudget.PlacementCgroupFS)
	if rep.Mode != "enforced" || !strings.Contains(rep.Detail, "cgroupfs") {
		t.Errorf("report = %+v; want enforced via cgroupfs", rep)
	}
	best := seatbudget.Budget{CPUs: 2, Mode: "best-effort"}
	if got := applySeatBudgetToCmd(cmd, "s1", best, true, seatbudget.PlacementSystemd); strings.Join(got, " ") != strings.Join(cmd, " ") {
		t.Errorf("best-effort wrap changed the command: %q", got)
	}
	if got := applySeatBudgetToCmd(cmd, "s1", b, false, seatbudget.PlacementSystemd); strings.Join(got, " ") != strings.Join(cmd, " ") {
		t.Errorf("disabled-budget wrap changed the command: %q", got)
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
