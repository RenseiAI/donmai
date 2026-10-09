package agycli

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// argvSecretSentinel is the canary credential this test injects on Spec.Env.
// It must reach the `agy` child through its ENVIRONMENT and must never
// appear in the child's argv — any local user can read another process's
// command line via ps or /proc/<pid>/cmdline, so a credential placed there
// is readable host-wide. `agy` authenticates via its own host OAuth; the
// provider injects no API key, which is exactly why any Spec.Env value
// showing up in argv would be a regression rather than a feature.
//
// RED proof: interpolate any Spec.Env value into buildArgs (for example by
// appending KEY=value entries to the argv) and the assertion fails with the
// sentinel quoted in the failure output.
func TestBuildArgs_CarryNoCredentialValue(t *testing.T) {
	t.Parallel()

	for _, spec := range []agent.Spec{
		{
			Prompt: "prove argv carries no credential",
			Cwd:    "/tmp/work",
			Env:    map[string]string{"SENTINEL_API_KEY": argvSecretSentinel},
		},
		{
			Prompt: "prove argv carries no credential",
			Cwd:    "/tmp/work",
			Env: map[string]string{
				"SENTINEL_API_KEY":  argvSecretSentinel,
				"ANTHROPIC_API_KEY": argvSecretSentinel,
			},
		},
	} {
		argv := buildArgs(spec, false)
		for _, a := range argv {
			if strings.Contains(a, argvSecretSentinel) {
				t.Fatalf("agy argv carries the sentinel credential: %q (argv=%q)", a, argv)
			}
		}
		env := composeEnv(nil, spec.Env)
		found := false
		for _, entry := range env {
			if entry == "SENTINEL_API_KEY="+argvSecretSentinel {
				found = true
			}
		}
		if !found {
			t.Fatalf("sentinel credential missing from the agy child env: %q", env)
		}
	}
}

const argvSecretSentinel = "sentinel-credential-must-not-ride-argv"
