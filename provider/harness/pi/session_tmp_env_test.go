package pi

// Confined-run scratch merge: a session that already carries an
// executor-owned session scratch keeps the confinement's own tmp binding,
// bound exactly once. Both spawn paths compose through confinePiEnv, so
// this drives that production seam: a real (empty) plan proves the prior
// executor entries are replaced, and a nil plan proves the passthrough.

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/runtime/confinement"
)

// TestConfinePiEnv_ReplacesExecutorTmpBinding pins AC-6 on the headless
// composition: with a plan, prior TMPDIR/TMP/TEMP entries are replaced —
// never shadowed by duplicates — while every other entry survives.
func TestConfinePiEnv_ReplacesExecutorTmpBinding(t *testing.T) {
	t.Parallel()
	var plan confinement.Plan
	childEnv := []string{
		"PATH=/usr/bin",
		"TMPDIR=/executor/seat-9",
		"TMP=/executor/seat-9",
		"TEMP=/executor/seat-9",
		"DONMAI_SESSION_TMPDIR=/executor/seat-9",
	}
	got := confinePiEnv(childEnv, &plan)
	for _, kv := range got {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if key == "TMPDIR" || key == "TMP" || key == "TEMP" {
			t.Fatalf("executor entry %q survives a confined composition (env %v): the plan binding must win exactly once", kv, got)
		}
	}
	for _, want := range []string{"PATH=/usr/bin", "DONMAI_SESSION_TMPDIR=/executor/seat-9"} {
		found := false
		for _, kv := range got {
			found = found || kv == want
		}
		if !found {
			t.Fatalf("entry %q lost in the confined composition (env %v)", want, got)
		}
	}
}

// TestConfinePiEnv_NilPlanPassesThrough pins the unconfined composition:
// without a plan the child environment — including an executor-owned
// scratch binding — is byte-identical.
func TestConfinePiEnv_NilPlanPassesThrough(t *testing.T) {
	t.Parallel()
	childEnv := []string{"TMPDIR=/executor/seat-9", "TMP=/executor/seat-9", "TEMP=/executor/seat-9", "PATH=/usr/bin"}
	got := confinePiEnv(childEnv, nil)
	if strings.Join(got, "\n") != strings.Join(childEnv, "\n") {
		t.Fatalf("confinePiEnv without a plan = %v, want the input unchanged %v", got, childEnv)
	}
}
