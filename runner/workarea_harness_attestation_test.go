package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	claudeprovider "github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// manifestAttestingProvider wraps a real harness manifest's workarea
// attestation behind the test gate provider's spawn: the runner binds the
// harness by name, so the declaration is validated against a manifest that
// carries the exact production attestation of the named harness while
// spawn itself stays hermetic.
type manifestAttestingProvider struct {
	*workareaGateProvider
	manifest agent.HarnessManifest
}

func (p *manifestAttestingProvider) Manifest() agent.HarnessManifest { return p.manifest }

// TestRunAllMutableDeclarationAdmittedOnClaudeAndPi is the revert-RED gate
// for the session-root-v1 attestation: an all-mutable two-repository
// declaration must provision the nested layout under each harness's real
// production attestation, binding the selected leaf as the harness CWD
// and the complete authority partition on the spec. Removing either
// manifest's attestation makes its subtest fail with
// workarea_protocol_unsupported before provisioning.
func TestRunAllMutableDeclarationAdmittedOnClaudeAndPi(t *testing.T) {
	providers := []struct {
		name     string
		manifest agent.HarnessManifest
	}{
		{name: "claude", manifest: (&claudeprovider.Provider{}).Manifest()},
		{name: "pi", manifest: piManifestForTest()},
	}
	for _, harness := range providers {
		t.Run(harness.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			provider := &manifestAttestingProvider{workareaGateProvider: &workareaGateProvider{supportsReadOnlySelected: true}, manifest: harness.manifest}
			if err := h.runner.registry.Register(provider); err != nil {
				t.Fatal(err)
			}
			qw := h.queuedWork("ALL-MUTABLE-" + harness.name)
			qw.WorkType = "acceptance"
			secondary := makeBareRepo(t)
			qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
				Protocol: workarea.ProtocolSessionRootV1,
				Repositories: []workarea.DeclaredRepositoryV1{
					{Source: workarea.RepositorySource{Repository: qw.Repository}, Name: "primary", Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable},
					{Source: workarea.RepositorySource{Repository: secondary}, Name: "secondary", Role: workarea.RepositoryRoleSecondary, Authority: workarea.RepositoryMutable},
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			res, err := h.runner.Run(ctx, qw)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.WorkareaRoot == "" || res.WorkareaRoot == res.WorktreePath {
				t.Fatalf("result paths = root %q cwd %q, want nested layout", res.WorkareaRoot, res.WorktreePath)
			}
			if filepath.Base(res.WorktreePath) != "primary" || filepath.Dir(res.WorktreePath) != res.WorkareaRoot {
				t.Fatalf("selected leaf = %q under %q, want primary under root", res.WorktreePath, res.WorkareaRoot)
			}
			for _, leaf := range []string{"primary", "secondary"} {
				if _, err := os.Stat(filepath.Join(res.WorkareaRoot, leaf, ".git")); err != nil {
					t.Fatalf("leaf %s has no clone: %v", leaf, err)
				}
			}
			provider.mu.Lock()
			policy := provider.observed.RepositoryAuthority
			cwd := provider.observed.Cwd
			provider.mu.Unlock()
			if policy == nil || len(policy.MutablePaths) != 2 || len(policy.ReadOnlyPaths) != 0 {
				t.Fatalf("authority partition = %#v, want two mutable and no read-only", policy)
			}
			if cwd != res.WorktreePath {
				t.Fatalf("harness CWD = %q, want selected leaf %q", cwd, res.WorktreePath)
			}
		})
	}
}
