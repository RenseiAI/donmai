package runner

import "github.com/RenseiAI/donmai/agent"

// ReconcileAdditionalExtensions applies the embedder's additional-extension
// decorator (afcli.Config.AgentSpecExtensionDecorator) to
// spec.AdditionalExtensions before either compile site computes a
// ToolLifecycleReceipt from it.
//
// Same doctrine as ReconcileResolvedProfile (resolved_profile_reconcile.go)
// and ReconcileRepositorySandbox (repository_sandbox_reconcile.go): this
// function is called IDENTICALLY from every site that builds the
// prepared-source Spec buildPreparedSourceSpec compiles a ToolLifecycleReceipt
// from — the daemon's preflight compiler (ProviderView.PreflightExecution,
// via compilePreparedHarness) and Runner's own prepared-source authority
// self-check (runner/loop.go's runLoop, via the SAME buildPreparedSourceSpec
// call) — so both computations see the identical decorated
// AdditionalExtensions the real spawn will carry.
//
// Before this, Spec.AdditionalExtensions was invisible to BOTH compile sites
// whenever an embedder registered a decorator: agent.DecorateProvider's
// wrapped Spawn/Resume is the ONLY place a registered ExtensionDecorator ever
// runs (agent/spec_decorator.go), and neither compile site calls
// Provider.Spawn — CompilePreparedHarness/ApplyPreparedHarness must stay
// side-effect-free (PreparedHarness is a secret-free, digest-only authority).
// So the daemon persisted a ToolLifecycleReceipt with no additional-extensions
// entry, while the real spawn's Provider — resolved from a registry a
// decorator DID wrap — recomputed one WITH it whenever the exact harness
// profile admits ToolPluginDelivery rather than denying it (pi's
// pi/interactive/tool-lifecycle-v3 and pi/headless/tool-lifecycle-v2 profiles
// both do). agent.ApplyPreparedHarness then found the recomputed
// ToolLifecycleReceipt disagreed with the host-persisted one even though the
// harnessAuthorityProjection digest still matched (AdditionalExtensions is
// deliberately excluded from that projection — CompilePreparedHarness has no
// side-effecting way to invoke a caller's decorator), surfacing as an
// undiagnosable *agent.ToolLifecycleDriftError{Fields:["entries"]} at spawn
// instead of a receipt that already told the truth.
//
// A nil decorate is a no-op, matching agent.DecorateProvider's own
// nil-decorate passthrough contract.
func ReconcileAdditionalExtensions(spec agent.Spec, decorate agent.ExtensionDecorator) agent.Spec {
	return agent.ApplyExtensionDecorator(spec, decorate)
}
