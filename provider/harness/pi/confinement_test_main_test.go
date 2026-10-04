package pi

import (
	"os"
	"testing"

	"github.com/RenseiAI/donmai/runtime/confinement"
)

// testMainCleanups run once after every test in the package has finished.
// Package-scoped fixtures shared across tests (the live confinement host
// directories, whose self-tested confiner every live test reuses) register
// their removal here instead of on a single test's t.Cleanup.
var testMainCleanups []func()

// TestMain lets the pi test binary stand in for a harness process:
// re-executed with the confinement probe variable set, it runs the probe
// instead of the tests. The live confinement tests drive the probe through
// the real spawn paths this way, exactly as the production binary's main
// does.
func TestMain(m *testing.M) {
	if handled, code := confinement.RunProbeFromEnv(); handled {
		os.Exit(code)
	}
	code := m.Run()
	for _, cleanup := range testMainCleanups {
		cleanup()
	}
	os.Exit(code)
}
