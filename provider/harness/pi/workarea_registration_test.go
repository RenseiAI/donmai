package pi_test

import (
	"os"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/pi"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// registeredAttestations runs registration's per-harness workarea
// attestation over providers, keyed by harness id.
func registeredAttestations(t *testing.T, providers ...agent.Provider) map[string]workarea.ExecutorCapabilityAttestation {
	t.Helper()
	reg := runner.NewRegistry()
	for _, provider := range providers {
		if err := reg.Register(provider); err != nil {
			t.Fatal(err)
		}
	}
	out := map[string]workarea.ExecutorCapabilityAttestation{}
	for _, attestation := range runner.NewProviderView(reg).WorkareaExecutorCapabilities() {
		out[attestation.HarnessID] = attestation
	}
	return out
}

// TestRegistration_WithholdsPiWorkareaAttestationWithoutConfinementBackend
// runs registration with the REAL pi provider on a host whose confinement
// self-test cannot pass (no backend, as on a Linux host without the
// namespace tooling): pi's manifest still declares session-root-v1 and
// isolated-read-only-v1, and registration publishes neither.
func TestRegistration_WithholdsPiWorkareaAttestationWithoutConfinementBackend(t *testing.T) {
	pi.WithoutConfinementBackendForTest(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := pi.NewHostProofProviderForTest(t, exe)
	if caps := provider.Manifest().Caps; len(caps.MultiRepositoryWorkareaProtocols) == 0 || caps.RepositoryAuthorityEnforcement == "" {
		t.Fatalf("pi manifest declares %q/%q; this test needs the declaration it withholds", caps.MultiRepositoryWorkareaProtocols, caps.RepositoryAuthorityEnforcement)
	}
	if got, ok := registeredAttestations(t, provider)[string(agent.HarnessPi)]; ok {
		t.Fatalf("registration published pi workarea attestation %+v on a host whose pi confinement self-test cannot pass", got)
	}
}
