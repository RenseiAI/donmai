package pi

import (
	"os"
	"testing"

	"github.com/RenseiAI/donmai/runtime/confinement"
)

// TestMain lets the pi test binary stand in for a harness process:
// re-executed with the confinement probe variable set, it runs the probe
// instead of the tests. The live confinement test below drives the probe
// through the real spawn paths this way.
func TestMain(m *testing.M) {
	if handled, code := confinement.RunProbeFromEnv(); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
