package pi

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/confinement"
)

// confinePiSessionForTest drives the production mapping with a recording
// prepare function: it builds the confinement spec exactly as production
// does, captures it, and returns no plan — no live OS backend, no
// self-test record.
func confinePiSessionForTest(spec agent.Spec, layout sessionLayout, recording *workareaRootRecordingConfiner, reads piReadScope) (*confinement.Plan, error) {
	confiner := &confinement.Confiner{}
	return confinePiSessionWithPreparer(spec, layout, confiner, reads, func(_ *confinement.Confiner, cspec confinement.Spec) (*confinement.Plan, error) {
		recording.append(cspec)
		return nil, nil
	})
}

// workareaRootRecordingConfiner captures the Spec confinePiSession builds.
type workareaRootRecordingConfiner struct {
	mu    sync.Mutex
	specs []confinement.Spec
}

func (b *workareaRootRecordingConfiner) append(spec confinement.Spec) {
	b.mu.Lock()
	b.specs = append(b.specs, spec)
	b.mu.Unlock()
}

func (b *workareaRootRecordingConfiner) lastSpec() (confinement.Spec, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.specs) == 0 {
		return confinement.Spec{}, false
	}
	return b.specs[len(b.specs)-1], true
}

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
// CWD-derived single-leaf fallback only when no authority is declared. It
// drives the production mapping (confinePiSessionWithPreparer) with a
// recording prepare function that captures the built Spec, so a regression
// that maps the wrong leaves (or none) changes the captured MutableLeaves
// and goes RED. The gate control pins that only the authority-bearing spec
// requests confinement on this provider, and the nil-confiner control pins
// the documented "session did not request confinement" path (no plan).
// The rendered leaf mapping is additionally proven against the real
// backend by TestPiConfinement_AuthorityRequestConfinesThroughLaunch
// (darwin live).
func TestConfinePiSession_AllMutableAuthorityPrefersLeavesOverCwd(t *testing.T) {
	t.Parallel()

	withAuthority := agent.Spec{
		Cwd: "/work/root/primary", Prompt: "probe",
		RepositoryAuthority: &agent.RepositoryAuthorityPolicy{
			Protocol: "session-root-v1", WorkareaRoot: "/work/root", SelectedPath: "/work/root/primary",
			MutablePaths: []string{"/work/root/primary", "/work/root/secondary"}, Enforcement: "",
		},
	}
	bare := agent.Spec{Cwd: "/work/root/primary", Prompt: "probe"}

	// Gate control through the production entry point: on a provider with
	// no host requirement only the authority-bearing spec requests
	// confinement, so the two inputs cannot collapse onto one branch.
	oldBackend := piConfinementBackend
	piConfinementBackend = func() confinement.Backend { return nil }
	t.Cleanup(func() { piConfinementBackend = oldBackend })
	gate := &Provider{binary: "/bin/sh", opts: Options{RequireConfinement: false}}
	if confiner, err := gate.confinerForSession(context.Background(), bare); err != nil || confiner != nil {
		t.Fatalf("confinerForSession(bare) = (%v, %v), want (nil, nil): no authority and no host requirement requests nothing", confiner, err)
	}
	if confiner, err := gate.confinerForSession(context.Background(), withAuthority); err == nil || confiner != nil {
		t.Fatalf("confinerForSession(authority) = (%v, %v), want a backend_absent refusal: the authority must request confinement", confiner, err)
	} else if reason, ok := confinement.ReasonOf(err); !ok || reason != confinement.ReasonBackendAbsent {
		t.Fatalf("confinerForSession(authority) error = %v, want the backend_absent reason", err)
	}

	// Leaf-mapping control through the production mapping with a
	// recording prepare function: the captured Spec must carry the
	// declared mutable pair for the authority spec and the CWD fallback
	// for the bare spec.
	recording := &workareaRootRecordingConfiner{}
	p := &Provider{binary: "/bin/sh", opts: Options{RequireConfinement: false}}
	capture := func(spec agent.Spec) confinement.Spec {
		t.Helper()
		layout := sessionLayout{root: t.TempDir()}
		if _, err := confinePiSessionForTest(spec, layout, recording, piReadScope{}); err != nil {
			t.Fatalf("confinePiSession(%q): %v", spec.Cwd, err)
		}
		got, ok := recording.lastSpec()
		if !ok {
			t.Fatal("recording confiner captured no spec")
		}
		return got
	}
	gotAuthority := capture(withAuthority)
	if gotAuthority.WorkareaRoot != "/work/root" || !slices.Equal(gotAuthority.MutableLeaves, []string{"/work/root/primary", "/work/root/secondary"}) {
		t.Fatalf("authority confinement spec = root %q leaves %q, want root /work/root with the declared mutable pair", gotAuthority.WorkareaRoot, gotAuthority.MutableLeaves)
	}
	gotBare := capture(bare)
	if gotBare.WorkareaRoot != "/work/root" || !slices.Equal(gotBare.MutableLeaves, []string{"/work/root/primary"}) {
		t.Fatalf("bare confinement spec = root %q leaves %q, want the CWD fallback", gotBare.WorkareaRoot, gotBare.MutableLeaves)
	}

	// Nil-confiner control: the documented no-request path returns no
	// plan without touching the authority.
	layout := sessionLayout{root: t.TempDir()}
	if plan, err := p.confineSession(bare, layout, nil); err != nil || plan != nil {
		t.Fatalf("confineSession(bare, nil confiner) = (%v, %v), want (nil, nil)", plan, err)
	}
	if skip := filepath.Dir(filepath.Clean(bare.Cwd)); skip != "/work/root" {
		t.Fatalf("CWD fallback root = %q, want /work/root", skip)
	}
}
