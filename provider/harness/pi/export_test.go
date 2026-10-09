package pi

import (
	"testing"

	"github.com/RenseiAI/donmai/runtime/confinement"
)

// This file exposes test seams to the external pi_test package, whose
// registration tests compose the real pi provider with the runner (which
// this package cannot import without a cycle).

// WithoutConfinementBackendForTest makes this host report no confinement
// backend until t ends. It writes a package variable, so its caller must
// not run in parallel.
func WithoutConfinementBackendForTest(t *testing.T) {
	t.Helper()
	old := piConfinementBackend
	piConfinementBackend = func() confinement.Backend { return nil }
	t.Cleanup(func() { piConfinementBackend = old })
}

// NewHostProofProviderForTest returns a process-spawning pi provider whose
// binary is bin and whose confinement host directories are fresh throwaway
// directories: the provider registration holds, minus the version probe.
func NewHostProofProviderForTest(t *testing.T, bin string) *Provider {
	t.Helper()
	return &Provider{binary: bin, opts: Options{confinementDirs: &piConfinementDirs{
		profileDir: t.TempDir(), home: t.TempDir(), stateHome: t.TempDir(), scratchDir: t.TempDir(),
	}}}
}
