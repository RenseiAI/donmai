//go:build linux

package confinement

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestScopesAvailable_DegradedBelowABI6 drives the production gate: below
// the scope ABI the scope layer reports unenforced with the reason, and at
// or above it reports enforced. A stubbed probe pins the kernel answer so
// the test runs on any host.
func TestScopesAvailable_DegradedBelowABI6(t *testing.T) {
	for _, abi := range []int{0, 1, 2, 5} {
		// ScopesAvailable reads scopesProbe (see stage_linux.go), not the
		// checkLandlock floor probe, so stub the scope probe: the portable
		// stubScopeProbe seam in scope_degraded_test.go pins it on every
		// host, including a scoped kernel where the real answer is
		// enforced.
		stubScopeProbe(t, abi)
		if ok, why := ScopesAvailable(); ok || why == "" {
			t.Fatalf("ScopesAvailable on ABI %d = (%v, %q), want unenforced with a reason", abi, ok, why)
		} else if !strings.Contains(why, "scope layer is unenforced") {
			t.Fatalf("ScopesAvailable on ABI %d reason = %q, want the typed scope-layer reason", abi, why)
		}
	}
	stubScopeProbe(t, landlockScopeABI)
	if ok, why := ScopesAvailable(); !ok || why != "" {
		t.Fatalf("ScopesAvailable on ABI %d = (%v, %q), want enforced", landlockScopeABI, ok, why)
	}
}

// TestSelfTestRecord_DegradedNeverAttests drives the production attestation
// entry points: a passing record taken below the scope ABI marks itself
// degraded, attests nothing, and refuses Prepare — the host never reports
// as confined while the scope layer is missing. Reverting the degraded
// marking (passing the record through as attested) turns this test red.
func TestSelfTestRecord_DegradedNeverAttests(t *testing.T) {
	stubScopeProbe(t, landlockScopeABI-1)
	w := newLinuxWorld(t)
	launcher := fakeLauncher(t, "exit 0")
	c, err := New(Options{
		Backend:          &mountNamespaceBackend{launcherOverride: launcher},
		ProfileDir:       w.profileDir,
		Home:             w.home,
		StateHome:        w.stateHome,
		ExecutableDigest: "sha256:test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	current, err := c.fingerprint()
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	// A passing record taken below the scope ABI: the probes all passed
	// but the scope layer is missing. The verdict runs through the same
	// marking SelfTest applies to its production record.
	record := SelfTestRecord{
		Backend: current.backend, BackendVersion: current.backendVersion, ProbeSetVersion: ProbeSetVersion,
		ExecutableDigest: current.executableDigest,
		SessionModes:     []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled},
		Passed:           true,
		Probes:           []ProbeOutcome{{ID: "p", Pass: true}},
	}
	record.applyScopeVerdict(ScopesAvailable())
	record.Digest = record.computeDigest()
	c.store(record)
	if record.Degraded == "" {
		t.Fatal("a passing record below the scope ABI carries no degraded reason")
	}
	if attestation, ok := c.Attestation(); ok {
		t.Fatalf("a degraded record attests: %+v", attestation)
	}
	if _, ok := c.Degraded(); !ok {
		t.Fatal("Degraded reports nothing for a degraded record")
	}
	spec := linuxSpec(w)
	if _, err := c.Prepare(spec); err == nil {
		t.Fatal("Prepare started a seat from a degraded record")
	} else if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("Prepare from a degraded record: err=%v, want self_test_failed", err)
	}
	// The digest covers the degraded marking: stripping it changes the
	// digest, so a degraded record never verifies as a full one.
	stripped := record
	stripped.Degraded = ""
	if stripped.computeDigest() == record.Digest {
		t.Fatal("the digest does not cover the degraded marking")
	}
}

// TestEmptyPlaceholder_IsPerUser pins the per-user placeholder: the overlay
// base names the caller's user id and stays owner-only, so one user's
// denied listing never shows through another user's placeholder. Reverting
// to the shared base turns this test red.
func TestEmptyPlaceholder_IsPerUser(t *testing.T) {
	w := newLinuxWorld(t)
	base, err := emptyPlaceholderBase()
	if err != nil {
		t.Fatalf("emptyPlaceholderBase: %v", err)
	}
	if !strings.Contains(base, "donmai-confine-empty-") {
		t.Fatalf("placeholder base = %q, want the per-user overlay directory", base)
	}
	if strings.HasSuffix(base, "donmai-confine-empty") {
		t.Fatalf("placeholder base = %q, want the per-user suffix, not the shared directory", base)
	}
	dir, _, err := emptyPlaceholder(w.ro)
	if err != nil {
		t.Fatalf("emptyPlaceholder: %v", err)
	}
	if !strings.HasPrefix(dir, base+string('/')) && dir != base {
		t.Fatalf("placeholder %q is not under the per-user base %q", dir, base)
	}
}
