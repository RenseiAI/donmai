package confinement

import (
	"os"
	"testing"
)

// TestMain lets the test binary stand in for the harness: re-executed with
// ProbeEnv set, it runs the probe instead of the tests.
func TestMain(m *testing.M) {
	if handled, code := RunLandlockStageFromEnv(); handled {
		os.Exit(code)
	}
	if handled, code := RunProbeFromEnv(); handled {
		os.Exit(code)
	}
	if handled, code := runNestedCheckFromEnv(); handled {
		os.Exit(code)
	}
	if handled, code := runSeatCheckFromEnv(); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
