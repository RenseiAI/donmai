package daemon

import (
	"os/exec"
	"strings"
	"testing"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// daemonEngineFixturePrefix is the neutral fixture standing in for an
// embedder-owned control namespace. It is registered with the shared
// classifier so the daemon spawn test proves the registration seam is
// honored on the worker path alongside the built-in confinement prefix
// and probe suffix.
const daemonEngineFixturePrefix = "EMBEDDER_SEATBELT_"

func init() {
	runtimeenv.RegisterEngineControlPrefix(daemonEngineFixturePrefix)
}

// TestSessionEnv_StripsEngineControlsFromEveryLayer pins that the worker
// spawn environment carries no engine re-exec control from any untrusted
// or inherited layer: the daemon's own parent environment, BaseEnv, or the
// work item's SessionSpec.Env. A surviving copy would make the spawned
// worker re-execute as a probe or stage instead of running the session.
// The test runs a real child on the composed env, so the answer is what
// the worker process actually observes after exec resolves duplicates.
//
// RED: drop the engine filter from composeEnv's explicit-map loop or from
// filterSpawnerParentEnv and an evil below reaches the child.
func TestSessionEnv_StripsEngineControlsFromEveryLayer(t *testing.T) {
	// Not parallel: stages engine controls in the process environment,
	// which is the daemon's inherited parent layer.
	t.Setenv("DONMAI_CONFINEMENT_PROBE", "parent-evil-probe")
	t.Setenv("DONMAI_CONFINEMENT_LANDLOCK_STAGE", "parent-evil-stage")
	t.Setenv("DONMAI_CONFINEMENT_PROBE_DECOY", "parent-evil-decoy")
	t.Setenv("DONMAI_CONFINEMENT_PROBE_REMOUNT", "parent-evil-remount")
	t.Setenv("EMBEDDER_SEATBELT_ENFORCE", "parent-evil-seatbelt")
	t.Setenv("EVIL_PROBE", "parent-evil-suffix")
	t.Setenv("FOO_PROBE_EXTRA", "parent-near-miss")
	t.Setenv("DONMAI_CONFINEMENT", "parent-bare-prefix")

	spec := SessionSpec{SessionID: "sess-engine-deny", Env: map[string]string{
		"DONMAI_CONFINEMENT_PROBE":       "spec-evil-probe",
		"DONMAI_CONFINEMENT_PROBE_DECOY": "spec-evil-decoy",
		"EMBEDDER_SEATBELT_FOO":          "spec-evil-seatbelt",
		"BAR_PROBE":                      "spec-evil-suffix",
		"FOO_PROBE_EXTRA":                "spec-near-miss",
		"DONMAI_CONFINEMENT":             "spec-bare-prefix",
		"KEEP":                           "yes",
	}}
	s := NewWorkerSpawner(SpawnerOptions{BaseEnv: map[string]string{
		"DONMAI_CONFINEMENT_LANDLOCK_STAGE": "base-evil-stage",
		"QUX_PROBE":                         "base-evil-suffix",
		"EMBEDDER_SEATBELT_X":               "base-evil-seatbelt",
		"BASE_KEEP":                         "yes",
	}})
	env := s.sessionEnv(spec, nil)
	for _, entry := range env {
		key := entry
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key = entry[:i]
		}
		// Report the key only: values stay out of the log and a
		// malformed attacker-controlled name is never echoed.
		if strings.Contains(entry, "evil") {
			t.Fatalf("engine control %q reached the worker env", key)
		}
		if runtimeenv.IsEngineControlEnv(key) {
			t.Fatalf("engine control %q survived the worker composition", key)
		}
	}
	if !envHasEntry(env, "KEEP=yes") {
		t.Fatal("ordinary work-item entries must still pass through")
	}
	if !envHasEntry(env, "BASE_KEEP=yes") {
		t.Fatal("ordinary BaseEnv entries must still pass through")
	}
	if !envHasEntry(env, "DONMAI_SESSION_ID=sess-engine-deny") {
		t.Fatal("daemon-owned session id must still reach the worker")
	}

	// Real child: the worker observes the composed env after exec.
	cmd := exec.Command("/bin/sh", "-c", `printf '%s|%s|%s|%s' "${DONMAI_CONFINEMENT_PROBE-unset}" "${EMBEDDER_SEATBELT_ENFORCE-unset}" "${EVIL_PROBE-unset}" "${FOO_PROBE_EXTRA-unset}"`) //nolint:gosec // G204: fixed shell probe of the composed worker env.
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	// The near-miss rides the explicit copy (last wins); every control
	// reads unset.
	if got, want := string(out), "unset|unset|unset|spec-near-miss"; got != want {
		t.Errorf("worker child observes %q, want %q (probe|seatbelt|suffix|near-miss)", got, want)
	}
}
