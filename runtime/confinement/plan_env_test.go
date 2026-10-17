package confinement

// Tests for Plan.ApplyToEnv: the plan's session-tmp binding wins exactly
// once over any prior TMPDIR/TMP/TEMP entry (for example an executor-owned
// session scratch bound before the worker started), and a child environment
// with no prior binding sees only the append.
//
// RED: stop replacing the prior entries (plain append) and the
// bound-once assertions fail.

import (
	"strings"
	"testing"
)

// TestPlanApplyToEnv_ReplacesPriorTmpBindings pins the confined-run merge:
// each of TMPDIR/TMP/TEMP appears exactly once, bound to session_tmp,
// while every other entry — including cache bindings already present —
// passes through untouched.
func TestPlanApplyToEnv_ReplacesPriorTmpBindings(t *testing.T) {
	t.Parallel()
	plan := &Plan{env: []string{
		"TMPDIR=/plan/tmp",
		"TMP=/plan/tmp",
		"TEMP=/plan/tmp",
		"GOCACHE=/plan/cache/go-build",
	}}
	childEnv := []string{
		"PATH=/usr/bin",
		"TMPDIR=/executor/seat-9",
		"TMP=/executor/seat-9",
		"TEMP=/executor/seat-9",
		"DONMAI_SESSION_TMPDIR=/executor/seat-9",
		"GOCACHE=/operator/cache",
	}
	got := plan.ApplyToEnv(childEnv)
	counts := map[string]int{}
	values := map[string][]string{}
	for _, kv := range got {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		counts[key]++
		values[key] = append(values[key], kv)
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if counts[key] != 1 {
			t.Fatalf("%s bound %d times (%v), want exactly once", key, counts[key], values[key])
		}
		if want := key + "=/plan/tmp"; values[key][0] != want {
			t.Fatalf("%s = %q, want the plan binding %q", key, values[key][0], want)
		}
	}
	for _, want := range []string{
		"PATH=/usr/bin",
		"DONMAI_SESSION_TMPDIR=/executor/seat-9",
		"GOCACHE=/operator/cache",
		"GOCACHE=/plan/cache/go-build",
	} {
		found := false
		for _, kv := range got {
			found = found || kv == want
		}
		if !found {
			t.Fatalf("entry %q lost in the merge (env %v)", want, got)
		}
	}
}

// TestPlanApplyToEnv_AppendsWithoutPriorBinding pins the append half: with
// no prior temp binding the plan's environment lands whole and nothing else
// moves.
func TestPlanApplyToEnv_AppendsWithoutPriorBinding(t *testing.T) {
	t.Parallel()
	plan := &Plan{env: []string{"TMPDIR=/plan/tmp", "TMP=/plan/tmp", "TEMP=/plan/tmp"}}
	childEnv := []string{"PATH=/usr/bin", "HOME=/root"}
	got := plan.ApplyToEnv(childEnv)
	want := []string{"PATH=/usr/bin", "HOME=/root", "TMPDIR=/plan/tmp", "TMP=/plan/tmp", "TEMP=/plan/tmp"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ApplyToEnv = %v, want %v", got, want)
	}
}

// TestPlanApplyToEnv_DropsBareKeys pins that a bare TMPDIR with no '='
// cannot shadow the plan binding either: it names the variable without
// setting it, so it is dropped like any other prior entry.
func TestPlanApplyToEnv_DropsBareKeys(t *testing.T) {
	t.Parallel()
	plan := &Plan{env: []string{"TMPDIR=/plan/tmp", "TMP=/plan/tmp", "TEMP=/plan/tmp"}}
	got := plan.ApplyToEnv([]string{"TMPDIR", "TMP=x", "KEEP=1"})
	for _, kv := range got {
		if kv == "TMPDIR" || kv == "TMP=x" {
			t.Fatalf("shadowed entry %q survives the merge (env %v)", kv, got)
		}
	}
}
