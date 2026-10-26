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

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

func init() {
	// Neutral fixture for the embedder-owned namespace; the shared
	// constant lives in launcher_env_test.go (same package).
	runtimeenv.RegisterEngineControlPrefix("EMBEDDER_SEATBELT_")
}

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

// TestPlanApplyToEnv_CarriesBoundaryEnv pins that the merge carries the
// boundary's own environment (the Linux stage policy Applied.Environ
// holds) alongside the tmp and cache bindings: dropping it would run the
// seat with no read policy. The marker stands in for the stage policy the
// Linux backend renders.
//
// RED: append only the tmp/cache bindings (not the full Environment) and
// the marker assertion fails.
func TestPlanApplyToEnv_CarriesBoundaryEnv(t *testing.T) {
	t.Parallel()
	plan := &Plan{
		env: []string{"TMPDIR=/plan/tmp", "TMP=/plan/tmp", "TEMP=/plan/tmp"},
		applied: Applied{Environ: func() []string {
			return []string{"STAGE_POLICY=x"}
		}},
	}
	got := plan.ApplyToEnv([]string{"HOME=/h", "TMPDIR=/executor/seat-9"})
	want := []string{"HOME=/h", "TMPDIR=/plan/tmp", "TMP=/plan/tmp", "TEMP=/plan/tmp", "STAGE_POLICY=x"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ApplyToEnv = %v, want %v: the boundary env the plan carries must survive the merge", got, want)
	}
}

// TestPlanApplyToEnv_StripsEngineControlsBeforeTrustedAppend pins the final
// child boundary: inherited or session-supplied engine re-exec controls are
// stripped from childEnv, while safe near-misses survive and the
// producer-owned bindings appended last (session-tmp plus the boundary's
// stage policy) still reach the child.
//
// RED: remove the ApplyToEnv engine filter and the evil entries below
// survive in the merged env.
func TestPlanApplyToEnv_StripsEngineControlsBeforeTrustedAppend(t *testing.T) {
	t.Parallel()
	plan := &Plan{
		env: []string{"TMPDIR=/plan/tmp", "TMP=/plan/tmp", "TEMP=/plan/tmp"},
		applied: Applied{Environ: func() []string {
			return []string{"DONMAI_CONFINEMENT_LANDLOCK_STAGE=trusted-policy"}
		}},
	}
	childEnv := []string{
		"PATH=/usr/bin",
		"DONMAI_CONFINEMENT_PROBE=evil-probe",
		"DONMAI_CONFINEMENT_LANDLOCK_STAGE=evil-stage",
		"DONMAI_CONFINEMENT_PROBE_DECOY=evil-decoy",
		"DONMAI_CONFINEMENT_PROBE_REMOUNT=evil-remount",
		"EMBEDDER_SEATBELT_ENFORCE=evil-seatbelt",
		"EVIL_PROBE=evil-suffix",
		"FOO_PROBE_EXTRA=near-miss",
		"DONMAI_CONFINEMENT=bare-prefix",
		"TMPDIR=/executor/seat-9",
	}
	got := plan.ApplyToEnv(childEnv)
	for _, kv := range got {
		if strings.Contains(kv, "evil") {
			t.Fatalf("untrusted engine control survived the final boundary: %v", got)
		}
	}
	// The trusted stage policy appended last survives exactly once, even
	// though the child carried an evil copy of the same name. Moving the
	// trusted append before the filter strips it as well, so this
	// assertion is the trusted-last RED.
	stage := 0
	for _, kv := range got {
		if kv == "DONMAI_CONFINEMENT_LANDLOCK_STAGE=trusted-policy" {
			stage++
		}
		if kv == "DONMAI_CONFINEMENT_LANDLOCK_STAGE=evil-stage" {
			t.Fatalf("evil stage copy survived alongside the trusted one: %v", got)
		}
	}
	if stage != 1 {
		t.Fatalf("trusted stage policy present %d times, want exactly once: %v", stage, got)
	}
	for _, want := range []string{
		"PATH=/usr/bin",
		"FOO_PROBE_EXTRA=near-miss",
		"DONMAI_CONFINEMENT=bare-prefix",
		"TMPDIR=/plan/tmp",
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
