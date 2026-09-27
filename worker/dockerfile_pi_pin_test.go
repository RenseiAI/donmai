package worker

import (
	"os"
	"regexp"
	"testing"

	"github.com/RenseiAI/donmai/provider/harness/pi"
)

// piPinRe extracts the @earendil-works/pi-coding-agent@<version> pin off an
// `npm i -g` line in a worker image Dockerfile.
var piPinRe = regexp.MustCompile(`@earendil-works/pi-coding-agent@([0-9]+(?:\.[0-9]+)+)`)

// TestPiPin_MatchesProbePinnedVersion guards worker/Dockerfile and
// worker/e2b/e2b.Dockerfile's @earendil-works/pi-coding-agent npm pin against
// drifting from provider/harness/pi/probe.go's PinnedVersion — the single
// source of truth the pi harness enforces at construction time
// (checkVersionPin denies any binary below MinVersion). A worker image that
// installs a pi release the harness itself would refuse to run boots broken.
func TestPiPin_MatchesProbePinnedVersion(t *testing.T) {
	for _, tc := range workerImageDockerfiles {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.name, err)
			}

			m := piPinRe.FindStringSubmatch(string(raw))
			if m == nil {
				t.Fatalf("%s: no @earendil-works/pi-coding-agent@<version> pin found", tc.name)
			}
			got := m[1]

			if got != pi.PinnedVersion {
				t.Errorf("%s pins pi at %s, want %s (provider/harness/pi/probe.go PinnedVersion)",
					tc.name, got, pi.PinnedVersion)
			}
		})
	}
}
