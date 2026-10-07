package codex

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// argvSecretSentinel is the canary credential this test injects on Spec.Env.
// It must reach the codex child through its ENVIRONMENT and must never
// appear in the child's argv — any local user can read another process's
// command line via ps or /proc/<pid>/cmdline, so a credential placed there
// is readable host-wide. (MCP-server secrets already have their own
// secret-free argv proof in
// TestBuildInteractiveLaunch_MixedMCPIsDeterministicSemanticAndSecretFree;
// this file covers the session-level credential the runner fans out on
// Spec.Env.)
const argvSecretSentinel = "sentinel-credential-must-not-ride-argv"

// TestInteractiveLaunch_SessionCredentialRidesEnvNotArgv drives the
// production interactive-launch entry point (the argv+env pair ptycli.Spawn
// hands to the PTY child) with a sentinel session credential and asserts
// the argv carries no trace of it while the env does. The headless lane
// needs no separate argv proof: startLocked execs the construction-time
// p.opts.Args verbatim (never derived from Spec.Env) and composes the
// session credential into cmd.Env via mergeEnv — both halves pinned below.
//
// RED proof: interpolate any Spec.Env value into the launch argv (for
// example by appending KEY=value entries in buildInteractiveLaunchEnv) and
// the argv assertion fails with the sentinel quoted in the failure output.
func TestInteractiveLaunch_SessionCredentialRidesEnvNotArgv(t *testing.T) {
	t.Parallel()

	spec := agent.Spec{
		Cwd:    t.TempDir(),
		Prompt: "prove argv carries no credential",
		Model:  "test-model",
		Env:    map[string]string{"SENTINEL_API_KEY": argvSecretSentinel},
	}
	launch, err := buildInteractiveLaunch(spec)
	if err != nil {
		t.Fatalf("buildInteractiveLaunch: %v", err)
	}
	for _, a := range launch.argv {
		if strings.Contains(a, argvSecretSentinel) {
			t.Fatalf("interactive launch argv carries the sentinel credential: %q (argv=%q)", a, launch.argv)
		}
	}
	if launch.env["SENTINEL_API_KEY"] != argvSecretSentinel {
		t.Fatalf("sentinel credential did not reach the interactive child env: %#v", launch.env)
	}
}

// TestHeadlessAppServerEnv_SessionCredentialRidesEnv pins the headless
// lane's composition: mergeEnv — the exact function startLocked uses for
// the app-server child's cmd.Env — carries the session credential into the
// child environment. The argv half needs no sentinel assertion because the
// app-server argv is the construction-time p.opts.Args, which is never
// derived from per-session Spec.Env; what this test locks is that the
// credential's only path into the child is the env layer.
func TestHeadlessAppServerEnv_SessionCredentialRidesEnv(t *testing.T) {
	t.Parallel()

	ownedHome := t.TempDir()
	got := mergeEnv(nil, map[string]string{"SENTINEL_API_KEY": argvSecretSentinel}, ownedHome)
	found := false
	for _, entry := range got {
		if entry == "SENTINEL_API_KEY="+argvSecretSentinel {
			found = true
		}
		if strings.HasPrefix(entry, "SENTINEL_API_KEY=") && entry != "SENTINEL_API_KEY="+argvSecretSentinel {
			t.Fatalf("session credential mangled in app-server env: %q", entry)
		}
	}
	if !found {
		t.Fatal("session credential missing from the app-server child env")
	}
}
