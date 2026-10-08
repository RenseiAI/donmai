package seatbudget

import (
	"fmt"
	"strings"
)

// macOS is best effort: there is no core affinity on Apple Silicon, so no
// OS primitive can pin a seat to a set of cores. The seat instead gets the
// cooperative worker caps (WorkerCapEnv) composed with the installed service
// process-priority mode: the service installer already demotes the daemon
// and every child it spawns (background band via launchd ProcessType or
// taskpolicy/nice), and the caps keep each tool's own fan-out at the seat
// share.
//
// The caps ride the seat as environment, so they cross the daemon -> worker
// -> harness -> tool chain without any new IPC: every process in the seat
// inherits them, and the tools read them where they already look.

// ComposeEnv overlays the worker caps for cpus onto base (the already
// composed seat environment) and returns the seat environment. Base wins on
// collision: an operator or session that set GOMAXPROCS explicitly keeps
// its value — the budget is a default, not an override. The returned slice
// is a copy; base is never mutated.
func ComposeEnv(base []string, cpus int) []string {
	caps := WorkerCapEnv(cpus)
	present := make(map[string]struct{}, len(base))
	for _, kv := range base {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			present[kv[:i]] = struct{}{}
		}
	}
	out := append([]string(nil), base...)
	for _, k := range WorkerCapKeys() {
		if _, ok := present[k]; ok {
			continue
		}
		out = append(out, k+"="+caps[k])
	}
	return out
}

// DescribeBestEffort renders the macOS report detail: the CPU share and the
// knob count, so an operator reading status can tell what "best-effort"
// concretely means for this seat.
func DescribeBestEffort(cpus, knobCount int) string {
	if cpus < 1 {
		cpus = 1
	}
	return fmt.Sprintf("%d shared CPUs via %d worker-cap env knobs; no core pinning on this OS", cpus, knobCount)
}
