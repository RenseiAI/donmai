package pi_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/pi"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// outerProfileProbeEnv makes the test binary, re-executed inside an outer
// sandbox profile, print what registration publishes instead of testing.
const outerProfileProbeEnv = "DONMAI_TEST_PI_REGISTRATION_IN_OUTER_PROFILE"

const outerProfileMarker = "PI-REGISTRATION-ATTESTATIONS:"

// TestRegistration_PublishesPiWorkareaAttestationWhereSelfTestPasses is the
// positive control: on this host, outside any outer profile, the real pi
// provider's confinement self-test passes, so registration publishes
// session-root-v1 with isolated-read-only-v1 for pi — directly and through
// the extension decorator embedders wrap providers in.
func TestRegistration_PublishesPiWorkareaAttestationWhereSelfTestPasses(t *testing.T) {
	if os.Getenv(outerProfileProbeEnv) != "" {
		t.Skip("running as the outer-profile probe")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	decorate := func(agent.Spec) []agent.ExtensionDelivery { return nil }
	for name, provider := range map[string]agent.Provider{
		"direct":    pi.NewLiveHostProofProviderForTest(t, exe),
		"decorated": agent.DecorateProvider(pi.NewLiveHostProofProviderForTest(t, exe), decorate),
	} {
		got, ok := registeredAttestations(t, provider)[string(agent.HarnessPi)]
		if !ok {
			t.Fatalf("%s: registration published no pi workarea attestation on a host whose pi self-test passes", name)
		}
		if len(got.MultiRepositoryWorkareaProtocols) != 1 || got.MultiRepositoryWorkareaProtocols[0] != workarea.ProtocolSessionRootV1 ||
			got.RepositoryAuthorityEnforcement != workarea.RepositoryAuthorityIsolatedReadOnlyV1 {
			t.Fatalf("%s: pi attestation = %+v, want session-root-v1 with isolated-read-only-v1", name, got)
		}
	}
}

// TestRegistration_WithholdsPiWorkareaAttestationInsideOuterSandbox
// re-executes this test binary inside an outer sandbox profile — the state
// of a daemon running under a composing binary's own profile — and runs
// registration there with the real pi provider: the self-test refuses with
// nested_sandbox, and registration publishes no pi workarea attestation.
func TestRegistration_WithholdsPiWorkareaAttestationInsideOuterSandbox(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv(outerProfileProbeEnv) != "" {
		raw, err := json.Marshal(registeredAttestations(t, pi.NewLiveHostProofProviderForTest(t, exe)))
		if err != nil {
			t.Fatal(err)
		}
		t.Log(outerProfileMarker + string(raw))
		return
	}
	launcher := "/usr/bin/sandbox-exec"
	if _, err := os.Stat(launcher); err != nil {
		t.Skipf("no macOS profile launcher: %v", err)
	}
	cmd := exec.Command(launcher, "-p", "(version 1)(allow default)", exe, //nolint:gosec // G204: fixed launcher; the test binary re-executes itself.
		"-test.run", "^TestRegistration_WithholdsPiWorkareaAttestationInsideOuterSandbox$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), outerProfileProbeEnv+"=1")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("outer-profile probe: %v\n%s", err, output.String())
	}
	_, payload, found := strings.Cut(output.String(), outerProfileMarker)
	if !found {
		t.Fatalf("outer-profile probe printed no attestations:\n%s", output.String())
	}
	payload, _, _ = strings.Cut(payload, "\n")
	var got map[string]workarea.ExecutorCapabilityAttestation
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("outer-profile attestations %q: %v", payload, err)
	}
	if attestation, ok := got[string(agent.HarnessPi)]; ok {
		t.Fatalf("inside an outer sandbox profile, registration published pi workarea attestation %+v", attestation)
	}
	if !strings.Contains(output.String(), "nested_sandbox") {
		t.Fatalf("the outer-profile probe withheld pi for another reason (want nested_sandbox):\n%s", output.String())
	}
}
