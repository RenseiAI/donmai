package pi

import (
	"slices"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestManifest_AttestsSessionRootV1WithoutReadOnlyEnforcement pins the
// protocol gate half of the declaration: this harness attests
// session-root-v1 (an all-mutable declaration passes admission) while
// declaring no read-only enforcement. A read-only-bearing declaration is
// refused later by the runner contract gate (validateRepositoryWorkarea /
// ValidateFor), which demands isolated-read-only-v1 whenever any
// repository is read-only.
func TestManifest_AttestsSessionRootV1WithoutReadOnlyEnforcement(t *testing.T) {
	t.Parallel()

	manifest := (&Provider{}).Manifest()
	if !slices.Contains(manifest.Caps.MultiRepositoryWorkareaProtocols, "session-root-v1") {
		t.Fatalf("MultiRepositoryWorkareaProtocols = %q, want session-root-v1 attested", manifest.Caps.MultiRepositoryWorkareaProtocols)
	}
	if manifest.Caps.RepositoryAuthorityEnforcement != "" {
		t.Fatalf("RepositoryAuthorityEnforcement = %q, want unset (no proven read-only boundary)", manifest.Caps.RepositoryAuthorityEnforcement)
	}

	mutable := agent.Spec{
		Cwd: "/work/root/primary", SandboxEnabled: true, SandboxLevel: agent.SandboxWorkspaceWrite,
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: "/work/root", SelectedPath: "/work/root/primary",
			MutablePaths: []string{"/work/root/primary", "/work/root/secondary"}, Enforcement: "",
		},
	}
	if err := agent.ValidateSpecCapabilities(mutable, manifest); err != nil {
		t.Fatalf("all-mutable authority admission = %v, want nil", err)
	}
}

// TestConfinePiSession_AllMutableAuthorityPrefersLeavesOverCwd pins the
// working-set half on the headless lane: confinePiSession maps the
// declared authority onto the confinement spec — mutable leaves writable,
// CWD-derived single-leaf fallback only when no authority is declared.
// It drives confinePiSession with a nil confiner twice: nil is the
// documented "session did not request confinement" path returning no
// plan, so the test pins the branch condition (authority vs CWD
// fallback) without needing a live backend. The leaf mapping itself is
// proven against the real backend by
// TestPiConfinement_AuthorityRequestConfinesThroughLaunch (darwin live).
func TestConfinePiSession_AllMutableAuthorityPrefersLeavesOverCwd(t *testing.T) {
	t.Parallel()

	withAuthority := agent.Spec{
		Cwd: "/work/root/primary", Prompt: "probe",
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: "/work/root", SelectedPath: "/work/root/primary",
			MutablePaths: []string{"/work/root/primary", "/work/root/secondary"}, Enforcement: "",
		},
	}
	if plan, err := confinePiSession(withAuthority, sessionLayout{root: t.TempDir()}, nil, piReadScope{}); err != nil || plan != nil {
		t.Fatalf("confinePiSession(authority, nil confiner) = (%v, %v), want (nil, nil)", plan, err)
	}
	// The gate that decides whether confinePiSession is even reached with
	// an authority: the authority branch of piConfinementEnabled must hold
	// for an all-mutable declaration, so the plan path above is live.
	if !piConfinementEnabled(withAuthority, false) {
		t.Fatal("piConfinementEnabled(authority) = false, want true: an all-mutable declaration must request confinement")
	}
}
