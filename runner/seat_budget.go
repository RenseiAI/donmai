package runner

import (
	"sort"
	"strconv"
)

// SeatBudget is the runner-visible per-seat resource budget threaded from
// the daemon's SessionDetail: the resolved share plus the mode. The daemon
// stamps it at dispatch; the runner applies the cooperative worker caps to
// the harness environment and reports what the seat actually got on the
// session result.
//
// Nil means budgeting is off — the seat spawns exactly as before. The
// Linux cgroup placement is applied by the daemon at spawn (it owns the
// child process); the worker only carries the cooperative caps, which must
// ride the harness Spec.Env to reach the tools that fan out.
type SeatBudget struct {
	// Mode is enforced | best-effort | none.
	Mode string `json:"mode,omitempty"`
	// CPUs is the whole-core seat share.
	CPUs int `json:"cpus,omitempty"`
	// MemoryMB is the seat memory ceiling in mebibytes. Zero means no cap.
	MemoryMB int `json:"memoryMb,omitempty"`
	// Detail is the short human line the daemon stamped.
	Detail string `json:"detail,omitempty"`
}

// Clone returns a defensive copy of the budget. Nil stays nil so a
// disabled budget never gains a projection entry by copying.
func (b *SeatBudget) Clone() *SeatBudget {
	if b == nil {
		return nil
	}
	out := *b
	return &out
}

// disabled reports whether the budget carries no constraint.
func (b *SeatBudget) disabled() bool {
	if b == nil {
		return true
	}
	if b.Mode == "" || b.Mode == "none" {
		return true
	}
	return false
}

// workerCapEnv returns the cooperative worker-cap entries for cpus: the env
// knobs core-fanning tools actually read (Go runtime, make, cmake, ninja,
// cargo). The set mirrors the daemon's enforcement package; the runner
// re-renders it here (rather than importing the daemon) because the daemon
// package must stay independent of the runner — `donmai agent run`
// constructs its runner from the opaque SessionDetail payload.
func workerCapEnv(cpus int) map[string]string {
	if cpus < 1 {
		cpus = 1
	}
	n := strconv.Itoa(cpus)
	return map[string]string{
		"GOMAXPROCS":                 n,
		"MAKEFLAGS":                  "-j" + n,
		"CMAKE_BUILD_PARALLEL_LEVEL": n,
		"NINJAFLAGS":                 "-j" + n,
		"CARGO_BUILD_JOBS":           n,
	}
}

// applySeatBudget overlays the seat's cooperative worker caps onto the
// already-composed harness Spec.Env. Operator/session values win: an
// explicitly set knob is never overridden — the budget is a default, not
// an override. A disabled budget returns env unchanged.
func applySeatBudget(env map[string]string, budget *SeatBudget) map[string]string {
	if budget.disabled() {
		return env
	}
	caps := workerCapEnv(budget.CPUs)
	keys := make([]string, 0, len(caps))
	for k := range caps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := env
	if out == nil {
		out = make(map[string]string, len(caps))
	}
	for _, k := range keys {
		if _, ok := out[k]; !ok {
			out[k] = caps[k]
		}
	}
	return out
}
