package pi

import "testing"

// NewLiveHostProofProviderForTest returns a process-spawning pi provider
// whose binary is bin over the live confinement host directories every
// live test in this binary shares, so its host proof runs the real
// self-test on this host.
func NewLiveHostProofProviderForTest(t *testing.T, bin string) *Provider {
	t.Helper()
	return &Provider{binary: bin, opts: Options{confinementDirs: liveConfinementDirs(t)}}
}
