package runner

import (
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// ReconcileRepositorySandbox applies the repository-authority-driven sandbox
// posture a receipt-bearing session with a declared repository must carry:
// the authority declaration is stronger than a caller's autonomous
// full-access preference (once a repository is declared, only its declared
// mutable paths may be writable), so a non-nil repositoryDeclaration always
// forces workspace-write sandboxing regardless of what
// QueuedWork.PermissionProfile otherwise requested. A nil declaration leaves
// spec untouched.
//
// Same doctrine as ReconcileResolvedProfile (resolved_profile_reconcile.go —
// see its doc comment): this function is called IDENTICALLY from both the
// daemon preflight compiler (ProviderView.PreflightExecution, via
// compilePreparedHarness) and the runner's spawn lane (Runner.run), so a
// receipt-bearing session with a declared repository gets byte-identical
// SandboxEnabled/SandboxLevel authority in the host-compiled PreparedHarness
// plan and the child's materialized Spec. Before this, only the spawn lane
// applied this mutation — AFTER the early prepared-source authority check —
// so the host-compiled receipt was compiled without it: a receipt-bearing
// human-controlled session whose PermissionProfile requested full access
// (PermissionProfileAutonomous) got downgraded to workspace-write only at
// the very end of the spawn lane, invisibly to the plan the daemon preflight
// had already persisted. agent.ApplyPreparedHarness (reached from the
// provider's own Spawn, e.g. pi.Provider.prepare) then refused with either
// "agent: tool/lifecycle application differs from host adaptation receipt"
// or an *agent.AuthorityDriftError naming "sandboxLevel", depending on which
// projection field the recompute actually disagreed on.
func ReconcileRepositorySandbox(spec agent.Spec, repositoryDeclaration *workarea.NormalizedDeclaration) agent.Spec {
	if repositoryDeclaration == nil {
		return spec
	}
	spec.SandboxEnabled = true
	spec.SandboxLevel = agent.SandboxWorkspaceWrite
	return spec
}
