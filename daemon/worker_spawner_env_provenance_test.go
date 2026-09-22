package daemon

import (
	"strings"
	"testing"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// TestComposeEnv_RefusesRunnerOnlyFromCallerMaps pins the PROVENANCE of the
// injection declaration.
//
// SessionSpec.Env is copied verbatim from the orchestrator's poll work item, so
// without this filter a work item carrying
// DONMAI_INJECTED_ENV_KEYS=ANTHROPIC_API_KEY would re-admit the DAEMON's own
// operator-exported provider key into a harness child — a leak no value of
// item.Env could reach before the declaration existed. OnPreSpawn runs after
// this composition and remains the only author.
func TestComposeEnv_RefusesRunnerOnlyFromCallerMaps(t *testing.T) {
	t.Parallel()

	refused := []string{
		runtimeenv.InjectedEnvKeysVar,
		runtimeenv.GatewayUpstreamEnvKeysVar,
		"ATTACH_TOKEN",
		"ATTACH_TOKEN_FILE",
		"ATTACH_URL",
		"DONMAI_SESSION_SHIM_REGISTRY_DIR",
	}

	for _, key := range refused {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			// Every caller-supplied layer, not just spec.Env: BaseEnv is the
			// embedder's static map and daemonOwnedEnv is the daemon's own, and
			// neither is the right place to author a runner-owned control.
			for layer, parts := range map[string][]map[string]string{
				"spec.Env":       {nil, {key: "hostile", "KEEP": "yes"}, nil},
				"BaseEnv":        {{key: "hostile", "KEEP": "yes"}, nil, nil},
				"daemonOwnedEnv": {nil, nil, {key: "hostile", "KEEP": "yes"}},
			} {
				got := composeEnv(parts...)
				if envHasKey(got, key) {
					t.Errorf("%s layer: %q reached the composed worker env", layer, key)
				}
				if !envHasEntry(got, "KEEP=yes") {
					t.Errorf("%s layer: ordinary entries must still pass through (got %v)", layer, got)
				}
			}
		})
	}
}

// TestComposeEnv_OrdinaryEntriesStillMerge is the control: the filter must not
// disturb the layering it was inserted into.
func TestComposeEnv_OrdinaryEntriesStillMerge(t *testing.T) {
	t.Parallel()

	got := composeEnv(
		map[string]string{"A": "base", "B": "base"},
		map[string]string{"B": "spec"},
		map[string]string{"C": "daemon"},
	)
	for _, want := range []string{"A=base", "B=spec", "C=daemon"} {
		if !envHasEntry(got, want) {
			t.Errorf("composeEnv() = %v, missing %q", got, want)
		}
	}
}

func envHasKey(entries []string, key string) bool {
	prefix := key + "="
	for _, entry := range entries {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func envHasEntry(entries []string, want string) bool {
	for _, entry := range entries {
		if entry == want {
			return true
		}
	}
	return false
}
