package runner

import (
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestApplySeatBudget(t *testing.T) {
	t.Run("nil budget leaves env untouched", func(t *testing.T) {
		env := map[string]string{"PATH": "/bin"}
		got := applySeatBudget(env, nil)
		if got["PATH"] != "/bin" || len(got) != 1 {
			t.Errorf("applySeatBudget(nil) = %v; want unchanged", got)
		}
	})
	t.Run("mode none leaves env untouched", func(t *testing.T) {
		env := map[string]string{"PATH": "/bin"}
		got := applySeatBudget(env, &SeatBudget{Mode: "none", CPUs: 4})
		if len(got) != 1 {
			t.Errorf("applySeatBudget(none) = %v; want unchanged", got)
		}
	})
	t.Run("caps land at the seat share", func(t *testing.T) {
		got := applySeatBudget(map[string]string{}, &SeatBudget{Mode: "best-effort", CPUs: 2})
		// Five verified knobs only: JS runners take no seat env entry
		// (vitest reads config/CLI, Next.js reads config/CLI), so both
		// must stay absent — an entry the tool ignores would claim a
		// cap that does nothing.
		want := map[string]string{
			"GOMAXPROCS":                 "2",
			"MAKEFLAGS":                  "-j2",
			"CMAKE_BUILD_PARALLEL_LEVEL": "2",
			"NINJAFLAGS":                 "-j2",
			"CARGO_BUILD_JOBS":           "2",
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q; want %q", k, got[k], v)
			}
		}
		if len(got) != len(want) {
			t.Errorf("applySeatBudget injected %d knobs; want exactly the %d verified controls", len(got), len(want))
		}
		for _, dead := range []string{"VITEST_MAX_WORKERS", "NEXT_BUILD_WORKERS"} {
			if _, ok := got[dead]; ok {
				t.Errorf("applySeatBudget injected %q; no such tool control exists", dead)
			}
		}
	})
	t.Run("explicit values win", func(t *testing.T) {
		env := map[string]string{"GOMAXPROCS": "16", "PATH": "/bin"}
		got := applySeatBudget(env, &SeatBudget{Mode: "enforced", CPUs: 2})
		if got["GOMAXPROCS"] != "16" {
			t.Errorf("GOMAXPROCS = %q; want operator value 16", got["GOMAXPROCS"])
		}
		if got["MAKEFLAGS"] != "-j2" {
			t.Errorf("MAKEFLAGS = %q; want -j2", got["MAKEFLAGS"])
		}
	})
	t.Run("zero cpus clamp to one", func(t *testing.T) {
		got := applySeatBudget(nil, &SeatBudget{Mode: "best-effort"})
		if got["GOMAXPROCS"] != "1" {
			t.Errorf("GOMAXPROCS = %q; want 1", got["GOMAXPROCS"])
		}
	})
}

func TestSeatBudgetResultReport(t *testing.T) {
	res := &Result{}
	budget := &SeatBudget{Mode: "best-effort", CPUs: 4, MemoryMB: 8192, Detail: "caps"}
	// Mirror the loop.go stamp: the result carries what the seat got.
	if budget != nil && !budget.disabled() {
		res.SeatBudget = &agent.SeatBudgetReport{
			Mode:     budget.Mode,
			CPUs:     budget.CPUs,
			MemoryMB: budget.MemoryMB,
			Detail:   budget.Detail,
		}
	}
	if res.SeatBudget == nil {
		t.Fatal("SeatBudget is nil; want the stamped share")
	}
	if res.SeatBudget.Mode != "best-effort" || res.SeatBudget.CPUs != 4 || res.SeatBudget.MemoryMB != 8192 {
		t.Errorf("SeatBudget = %+v; want best-effort 4 cpu 8192MB", res.SeatBudget)
	}
	if (&SeatBudget{Mode: "none"}).disabled() != true {
		t.Error("mode none is not disabled")
	}
	if (*SeatBudget)(nil).disabled() != true {
		t.Error("nil budget is not disabled")
	}
}

func TestWorkerCapEnvMatchesDaemonKnobs(t *testing.T) {
	// The runner's knob set must stay identical to the daemon's: a knob
	// present on one side but missing on the other means the report claims
	// a cap the seat does not carry, or vice versa.
	env := workerCapEnv(3)
	wantKeys := []string{
		"GOMAXPROCS", "MAKEFLAGS", "CMAKE_BUILD_PARALLEL_LEVEL",
		"NINJAFLAGS", "CARGO_BUILD_JOBS",
	}
	if len(env) != len(wantKeys) {
		t.Fatalf("workerCapEnv has %d knobs; want %d", len(env), len(wantKeys))
	}
	for _, k := range wantKeys {
		if _, ok := env[k]; !ok {
			t.Errorf("workerCapEnv missing knob %q", k)
		}
	}
}

// TestRun_SeatBudgetCapsAndReports drives the PRODUCTION run path (Run)
// with a seat budget and asserts both halves: the harness Spec.Env carries
// the worker caps, and the session result reports the posture with the
// values. The scripted provider captures Spec.Env at Spawn.
func TestRun_SeatBudgetCapsAndReports(t *testing.T) {
	res, provider := runScriptedSession(t, scriptedSession{
		workType:   "development",
		seatBudget: &SeatBudget{Mode: "best-effort", CPUs: 2, MemoryMB: 4096, Detail: "caps"},
		turns: []verdictScriptTurn{
			{text: "Opened " + followUpPR + "\nWORK_RESULT: passed"},
		},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	if provider.lastSpecEnv()["GOMAXPROCS"] != "2" {
		t.Errorf("harness GOMAXPROCS = %q; want 2", provider.lastSpecEnv()["GOMAXPROCS"])
	}
	if provider.lastSpecEnv()["MAKEFLAGS"] != "-j2" {
		t.Errorf("harness MAKEFLAGS = %q; want -j2", provider.lastSpecEnv()["MAKEFLAGS"])
	}
	// The unverified JS spellings must stay absent from the harness env:
	// a seat carrying them would report caps its tools ignore.
	for _, dead := range []string{"VITEST_MAX_WORKERS", "NEXT_BUILD_WORKERS"} {
		if _, ok := provider.lastSpecEnv()[dead]; ok {
			t.Errorf("harness %s = %q; want absent (no such tool control)", dead, provider.lastSpecEnv()[dead])
		}
	}
	if res.SeatBudget == nil {
		t.Fatal("SeatBudget is nil; want the seat posture on the result")
	}
	if res.SeatBudget.Mode != "best-effort" || res.SeatBudget.CPUs != 2 || res.SeatBudget.MemoryMB != 4096 {
		t.Errorf("SeatBudget = %+v; want best-effort 2 cpu 4096MB", res.SeatBudget)
	}
}

// TestRun_NoSeatBudgetReportsNothing pins the off path through Run: no
// budget means no caps on the harness env and no report on the result.
func TestRun_NoSeatBudgetReportsNothing(t *testing.T) {
	res, provider := runScriptedSession(t, scriptedSession{
		workType: "development",
		turns: []verdictScriptTurn{
			{text: "Opened " + followUpPR + "\nWORK_RESULT: passed"},
		},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); want completed", res.Status, res.FailureMode, res.Error)
	}
	if _, ok := provider.lastSpecEnv()["GOMAXPROCS"]; ok {
		t.Errorf("harness GOMAXPROCS = %q; want absent with no budget", provider.lastSpecEnv()["GOMAXPROCS"])
	}
	if res.SeatBudget != nil {
		t.Errorf("SeatBudget = %+v; want nil with no budget", res.SeatBudget)
	}
}
