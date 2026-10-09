package env

import "testing"

// TestValidEnvKey pins which map keys can be serialized as one environment
// entry naming exactly that key.
func TestValidEnvKey(t *testing.T) {
	t.Parallel()
	for key, want := range map[string]bool{
		"PATH":                   true,
		"DONMAI_PI_CONFINEMENT":  true,
		"":                       false,
		"DONMAI_PI_CONFINEMENT=": false,
		"A=B":                    false,
		"BAD\x00KEY":             false,
	} {
		if got := ValidEnvKey(key); got != want {
			t.Errorf("ValidEnvKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// TestMapFilters_DropMalformedKeys pins that both map filters drop a key
// that would name a different variable once serialized, on their own: a
// filter that compared names only would pass {"<refused>=x": ""} through.
func TestMapFilters_DropMalformedKeys(t *testing.T) {
	t.Parallel()
	in := map[string]string{
		PiConfinementEnv + "=off":   "",
		PiConfinementEnv + "=":      "off",
		InjectedEnvKeysVar + "=KEY": "",
		"":                          "empty",
		"BAD\x00KEY":                "nul",
		"KEEP":                      "yes",
	}
	for name, filter := range map[string]func(map[string]string) map[string]string{
		"FilterRunnerOnlyMap": FilterRunnerOnlyMap,
		"FilterHostOwnedMap":  FilterHostOwnedMap,
	} {
		out := filter(in)
		if len(out) != 1 || out["KEEP"] != "yes" {
			t.Errorf("%s = %q, want only KEEP=yes", name, out)
		}
	}
}

// TestFilterHostOwnedMap_DropsTheConfinementSettings pins that a work item
// cannot set, loosen or widen the host's pi confinement: the requirement,
// the read scope and the declared read paths are all host-owned.
func TestFilterHostOwnedMap_DropsTheConfinementSettings(t *testing.T) {
	t.Parallel()
	in := map[string]string{
		PiConfinementEnv:          "off",
		PiConfinementReadEnv:      "host",
		PiConfinementReadPathsEnv: "/",
		"KEEP":                    "yes",
	}
	out := FilterHostOwnedMap(in)
	if len(out) != 1 || out["KEEP"] != "yes" {
		t.Fatalf("FilterHostOwnedMap = %q, want only KEEP=yes", out)
	}
	for _, key := range []string{PiConfinementEnv, PiConfinementReadEnv, PiConfinementReadPathsEnv} {
		if !IsHostOwned(key) {
			t.Errorf("%s is not host-owned", key)
		}
	}
	if IsHostOwned("PATH") {
		t.Error("PATH is host-owned")
	}
}

// TestFilterHostOwnedMap_DropsKeepFailedWorktree pins that a work item
// cannot set the host's failed-worktree recovery answer: the operator's
// keep (or teardown) policy for its own session is not the session's to
// override.
func TestFilterHostOwnedMap_DropsKeepFailedWorktree(t *testing.T) {
	t.Parallel()
	if !IsHostOwned(KeepFailedWorktreeEnv) {
		t.Fatalf("%s is not host-owned", KeepFailedWorktreeEnv)
	}
	in := map[string]string{KeepFailedWorktreeEnv: "1", "KEEP": "yes"}
	out := FilterHostOwnedMap(in)
	if len(out) != 1 || out["KEEP"] != "yes" {
		t.Fatalf("FilterHostOwnedMap = %q, want only KEEP=yes", out)
	}
}
