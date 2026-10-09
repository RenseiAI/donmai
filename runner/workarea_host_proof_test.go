package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	claudeprovider "github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// errNestedSandboxProof stands for a host whose pi confinement self-test
// cannot pass, e.g. a daemon already inside an outer sandbox profile.
var errNestedSandboxProof = errors.New("pi confinement self-test: confinement unavailable: nested_sandbox")

// piManifestOnlyProvider carries pi's real manifest without offering its
// host proof, as a wrapper that does not forward it would.
type piManifestOnlyProvider struct{ piWorkareaRunProvider }

func (*piManifestOnlyProvider) ProveWorkareaHost() {} // not agent.WorkareaHostProver

// TestWorkareaExecutorCapabilitiesPublishesPiOnlyWhereItsHostProofPasses
// pins the registration half of the host gate: pi's workarea attestation
// (session-root-v1 and isolated-read-only-v1) is published only when its
// host proof passes. A failing proof, a provider value that hides the
// proof, and a decorated provider over a failing proof all publish nothing
// for pi, while a harness whose manifest needs no proof (claude) is
// published either way.
func TestWorkareaExecutorCapabilitiesPublishesPiOnlyWhereItsHostProofPasses(t *testing.T) {
	t.Parallel()
	decorate := func(agent.Spec) []agent.ExtensionDelivery { return nil }
	cases := []struct {
		name   string
		pi     agent.Provider
		wantPi bool
	}{
		{name: "proof passes", pi: &piWorkareaRunProvider{}, wantPi: true},
		{name: "proof fails", pi: &piWorkareaRunProvider{proof: errNestedSandboxProof}},
		{name: "proof hidden by the provider value", pi: &piManifestOnlyProvider{}},
		{name: "decorated, proof passes", pi: agent.DecorateProvider(&piWorkareaRunProvider{}, decorate), wantPi: true},
		{name: "decorated, proof fails", pi: agent.DecorateProvider(&piWorkareaRunProvider{proof: errNestedSandboxProof}, decorate)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := NewRegistry()
			if err := reg.Register(tc.pi); err != nil {
				t.Fatal(err)
			}
			if err := reg.Register(&claudeprovider.Provider{}); err != nil {
				t.Fatal(err)
			}
			byHarness := map[string]workarea.ExecutorCapabilityAttestation{}
			for _, attestation := range NewProviderView(reg).WorkareaExecutorCapabilities() {
				byHarness[attestation.HarnessID] = attestation
			}
			if _, ok := byHarness[string(agent.HarnessClaudeCode)]; !ok {
				t.Fatalf("attestations = %+v, want claude published (its manifest needs no host proof)", byHarness)
			}
			pi, ok := byHarness[string(agent.HarnessPi)]
			if ok != tc.wantPi {
				t.Fatalf("pi published = %v (%+v), want %v", ok, pi, tc.wantPi)
			}
			if !tc.wantPi {
				return
			}
			if len(pi.MultiRepositoryWorkareaProtocols) != 1 || pi.MultiRepositoryWorkareaProtocols[0] != workarea.ProtocolSessionRootV1 ||
				pi.RepositoryAuthorityEnforcement != workarea.RepositoryAuthorityIsolatedReadOnlyV1 {
				t.Fatalf("pi attestation = %+v, want session-root-v1 with isolated-read-only-v1", pi)
			}
		})
	}
}

// TestRunPiSiblingEnvStaysLegacyWhereHostProofFails pins the sibling half of
// the host gate: on a host whose pi proof fails, a plain work item's sibling
// env keeps its legacy flat placement and the session runs, instead of
// turning into a declared read-only leaf whose confined spawn the host
// would refuse.
func TestRunPiSiblingEnvStaysLegacyWhereHostProofFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	provider := &piWorkareaRunProvider{proof: errNestedSandboxProof}
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	t.Setenv(siblingReposEnv, "")
	qw := h.queuedWork("PI-SIBLING-UNPROVEN")
	qw.WorkType = "acceptance"
	qw.ResolvedProfile.Harness = string(agent.HarnessPi)
	qw.Env = map[string]string{siblingReposEnv: namedBareRepo(t, "pi-corpus")}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil || res.Status != "completed" {
		t.Fatalf("Run = status %q (%s: %s), %v; want completed", res.Status, res.FailureMode, res.Error, err)
	}
	if res.WorkareaRoot != "" && res.WorkareaRoot != res.WorktreePath {
		t.Fatalf("WorkareaRoot = %q WorktreePath = %q; want the legacy flat layout on a host without the pi proof", res.WorkareaRoot, res.WorktreePath)
	}
	provider.mu.Lock()
	policy := provider.observed.RepositoryAuthority
	provider.mu.Unlock()
	if policy != nil {
		t.Fatalf("spawn carried repository authority %+v; want none on a host without the pi proof", policy)
	}
	if _, err := os.Stat(filepath.Join(res.WorktreePath, "..", "pi-corpus", ".git")); err != nil {
		t.Fatalf("legacy sibling placement ../pi-corpus is not a clone: %v", err)
	}
}

// TestRunPiDeclarationRefusedWhereHostProofFails pins the bind half of the
// host gate: a declared session is refused at bind, before provisioning,
// with the protocol-unsupported contract reason, on a host whose pi proof
// fails. The same declaration on a proven host provisions the nested layout
// (TestRunPiAttestedDeclarationProvisionsNestedLayoutAndBindsAuthority).
func TestRunPiDeclarationRefusedWhereHostProofFails(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	h := newRunnerHarness(t)
	provider := &piWorkareaRunProvider{proof: errNestedSandboxProof}
	if err := h.runner.registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	qw := h.queuedWork("PI-DECLARED-UNPROVEN")
	qw.WorkType = "acceptance"
	qw.ResolvedProfile.Harness = string(agent.HarnessPi)
	qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
		Protocol: workarea.ProtocolSessionRootV1,
		Repositories: []workarea.DeclaredRepositoryV1{
			{Source: workarea.RepositorySource{Repository: qw.Repository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
			{Source: workarea.RepositorySource{Repository: makeBareRepo(t)}, Name: "secondary", Role: workarea.RepositoryRoleSecondary, Authority: workarea.RepositoryMutable},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err == nil {
		t.Fatalf("Run = status %q root %q; want a bind refusal on a host without the pi proof", res.Status, res.WorkareaRoot)
	}
	if !strings.Contains(err.Error(), "workarea_protocol_unsupported") {
		t.Fatalf("Run error = %v; want workarea_protocol_unsupported", err)
	}
	provider.mu.Lock()
	spawned := provider.observed.Cwd != ""
	provider.mu.Unlock()
	if spawned {
		t.Fatal("the provider was spawned for a declaration its host cannot hold")
	}
}

// TestExecutorAttestsSiblingContextFollowsTheHostProof pins the sibling
// reader on its own: it reports a read-only context capability for pi only
// where pi's host proof passes. (On the Run path the bind-time check would
// also drop a derived declaration the proof does not cover; this keeps the
// derivation from being attempted at all.)
func TestExecutorAttestsSiblingContextFollowsTheHostProof(t *testing.T) {
	t.Parallel()
	if !executorAttestsSiblingContext(&piWorkareaRunProvider{}) {
		t.Fatal("executorAttestsSiblingContext(pi, proof passes) = false, want true")
	}
	if executorAttestsSiblingContext(&piWorkareaRunProvider{proof: errNestedSandboxProof}) {
		t.Fatal("executorAttestsSiblingContext(pi, proof fails) = true, want false")
	}
	if executorAttestsSiblingContext(&piManifestOnlyProvider{}) {
		t.Fatal("executorAttestsSiblingContext(pi, proof hidden) = true, want false")
	}
}
