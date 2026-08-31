package runner

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// declaredRepositoryPiManifestForTest is a SYNTHETIC manifest: it mirrors
// piManifestForTest (resolved_profile_reconcile_test.go) but additionally
// attests the session-root-v1 multi-repository workarea protocol pi's
// actually-shipped manifest does NOT declare — at v0.72.8 (this file's base
// commit), grep confirms `MultiRepositoryWorkareaProtocols` is set ONLY by
// provider/harness/codex/manifest.go; provider/harness/pi/manifest.go never
// sets it. That means a REAL pi session carrying a RepositoryDeclaration
// would fail workarea.ExecutorWorkareaCapabilities.ValidateFor
// (runtime/workarea/declaration.go, ReasonProtocolUnsupported) inside
// resolveRepositoryWorkarea BEFORE ever reaching the sandbox override this
// file's fix reconciles — a "protocol unsupported" refusal, not the
// "tool/lifecycle application differs" text the reported production
// incident showed.
//
// This test proves the RECONCILIATION LOGIC is correct for any harness that
// DOES declare the protocol (codex, today) and exercises it against pi's
// manifest only because the original incident report named pi and this
// file's test names/fixtures (piReceiptCell, piManifestForTest,
// resolved_profile_reconcile_test.go's sibling test) already existed for
// it — it does NOT prove pi's production manifest can reach this code
// path. Whether the reported pi incident's exact failing authority field
// is this file's fix, a version/manifest mismatch, or something else
// entirely is UNPROVEN — see this repo's PR description for the disclosed
// gap. The bounded, redacting shim-child log capture
// (session_shim_spawn.go) added alongside this fix is what will capture
// the ACTUAL failing field directly from a live pi session on its next
// occurrence, closing that gap without guessing here.
//
// Do not "fix" this by adding MultiRepositoryWorkareaProtocols to pi's real
// manifest just to make this test's premise true — that is a capability
// grant with its own review (workarea protocol + authority-enforcement
// attestation, ADR-scoped), not a side effect of a test fixture.
func declaredRepositoryPiManifestForTest() agent.HarnessManifest {
	manifest := piManifestForTest()
	manifest.Caps.MultiRepositoryWorkareaProtocols = []string{string(workarea.ProtocolSessionRootV1)}
	return manifest
}

// TestPreflightAndSpawnAgreeForHumanControlledPiWithDeclaredRepository
// reproduces the production defect this file's sibling function
// (ReconcileRepositorySandbox, repository_sandbox_reconcile.go) fixes: a
// receipt-bearing, human-controlled pi session that declares a repository
// and requests the fully-autonomous full-access permission profile.
//
// Before this fix, runner/loop.go's spawn lane forced SandboxEnabled/
// SandboxLevel to workspace-write ONLY at the very end of the spawn lane
// (immediately before provider.Spawn), invisibly to the daemon preflight
// compiler (ProviderView.PreflightExecution), which never learned about the
// resolved repository declaration at all. The host-compiled PreparedHarness
// plan therefore carried the caller's ORIGINAL full-access sandbox level,
// while the spawn lane's actual materialized Spec carried the
// authority-forced workspace-write level — a genuine, silent drift between
// what preflight persisted and what the child (via the provider's own
// agent.PrepareHarness call, e.g. pi.Provider.prepare) tried to apply.
// Reported production shape: a shim-owned interactive pi session failing at
// spawn with "agent: spawn failed: agent: tool/lifecycle application
// differs from host adaptation receipt".
//
// This test does not go through a full Runner.Run (no real git/worktree
// needed): ReconcileRepositorySandbox and resolveRepositoryWorkarea are both
// pure, so simulating the spawn lane's final Spec — buildPreparedSourceSpec
// plus the exact same ReconcileRepositorySandbox call runner/loop.go's spawn
// lane makes immediately before provider.Spawn — reproduces the real
// byte-for-byte Spec without provisioning a worktree.
//
// IMPORTANT — the manifest below is SYNTHETIC: pi's real, shipped manifest
// does not declare the multi-repository workarea protocol this scenario
// needs (see declaredRepositoryPiManifestForTest's doc comment). This test
// proves ReconcileRepositorySandbox's reconciliation is correct for any
// harness that DOES declare it; it does not prove the reported pi incident
// actually reaches this exact code path.
func TestPreflightAndSpawnAgreeForHumanControlledPiWithDeclaredRepository(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderPi, harness: agent.HarnessPi}
	providerWithManifest := &manifestSelectorProvider{
		selectorFakeProvider: provider, manifest: declaredRepositoryPiManifestForTest(), capabilities: piCapabilitiesForTest(),
	}
	registry := NewRegistry()
	if err := registry.Register(providerWithManifest); err != nil {
		t.Fatal(err)
	}

	const model = "gpt-declared-repository-model"
	const repoURL = "https://example.invalid/declared-repository-primary.git"
	buildBaseQW := func() QueuedWork {
		qw := QueuedWork{}
		qw.SessionID = "host-preflight-pi-declared-repository"
		qw.Mode = interactiveRunMode
		qw.InitialPrompt = "actual human-controlled initial turn"
		qw.Repository = repoURL
		qw.ResolvedProfile = ResolvedProfile{
			Harness: string(agent.HarnessPi), Model: model,
			Endpoint: &agent.EndpointBinding{
				Company: agent.CompanyOpenAI, Model: model, Protocol: agent.ProtoOpenAIChat, Host: agent.HostDirect,
				EndpointID: "endpoint:openai/direct", EndpointOperator: "vercel", EndpointRevision: "2026-08-06", ModelAuthor: "openai",
				AuthBindingID: "auth-binding:byok", AuthAuthority: "openai", AuthCommercialMode: string(executioncell.CommercialUsageBilled),
				AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
				AuthDelivery: string(executioncell.DeliveryEnvironment), Mechanism: agent.AuthAPIKey,
			},
		}
		// PermissionProfileAutonomous requests agent.SandboxFullAccess
		// (spec_translation.go's resolveSandboxLevel) — the caller preference
		// the repository authority declaration must override. Leaving this at
		// its default (workspace-write) would make the override a no-op and
		// hide the drift this test pins.
		qw.PermissionProfile = PermissionProfileAutonomous
		qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
			Protocol: workarea.ProtocolSessionRootV1,
			Repositories: []workarea.DeclaredRepositoryV1{{
				Source: workarea.RepositorySource{Repository: repoURL}, Name: "primary",
				Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
			}},
		}
		return qw
	}
	receiptCell := func() executioncell.ResolvedExecutionCell {
		return piReceiptCell("harness/v2", model, executioncell.SessionHumanControlled, nil)
	}

	baseQW := buildBaseQW()
	baseQW = attachAdmittedExecutionCell(t, baseQW, receiptCell())
	operational, err := CanonicalOperationalPayload(baseQW)
	if err != nil {
		t.Fatal(err)
	}
	baseQW.OperationalPayload = operational

	detail := map[string]any{
		"sessionId": baseQW.SessionID, "workerId": baseQW.WorkerID, "admissionReceipt": baseQW.AdmissionReceipt,
		"effectiveCell": baseQW.EffectiveCell, "executionRuntimeBinding": baseQW.ExecutionRuntimeBinding,
		"operationalPayload": baseQW.OperationalPayload,
	}

	compileHostPlan := func(t *testing.T) agent.PreparedHarness {
		t.Helper()
		receipt, err := NewProviderView(registry, nil).PreflightExecution(rawJSONForRunner(t, detail))
		if err != nil {
			t.Fatalf("PreflightExecution: %v receipt=%s", err, receipt)
		}
		host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
		if err != nil {
			t.Fatal(err)
		}
		var plan agent.PreparedHarness
		if err := json.Unmarshal(host.Plan, &plan); err != nil {
			t.Fatal(err)
		}
		if plan.Mode != agent.PromptModeHumanControlled || plan.Harness != string(agent.HarnessPi) {
			t.Fatalf("host compiled wrong identity: %+v", plan)
		}
		return plan
	}

	// simulateSpawnSpec reproduces the spawn lane's final materialized Spec
	// for a receipt-bearing session with a declared repository: the SAME
	// reconstruction loop.go performs (buildPreparedSourceSpec) followed by
	// the SAME repository-authority-driven sandbox mutation loop.go's spawn
	// lane applies immediately before provider.Spawn.
	simulateSpawnSpec := func(t *testing.T, plan agent.PreparedHarness) agent.Spec {
		t.Helper()
		spawnQW := buildBaseQW()
		spawnQW.AdmissionReceipt, spawnQW.ClaimReceipt, spawnQW.EffectiveCell = baseQW.AdmissionReceipt, baseQW.ClaimReceipt, baseQW.EffectiveCell
		spawnQW.ExecutionRuntimeBinding, spawnQW.OperationalPayload = baseQW.ExecutionRuntimeBinding, baseQW.OperationalPayload
		spawnQW.WorkerID = baseQW.WorkerID

		repositoryDeclaration, _, err := resolveRepositoryWorkarea(spawnQW, providerWithManifest)
		if err != nil {
			t.Fatalf("resolveRepositoryWorkarea: %v", err)
		}
		if repositoryDeclaration == nil {
			t.Fatal("expected a resolved repository declaration for this declared-repository session")
		}

		source, _, err := buildPreparedSourceSpec(spawnQW, harnessSelection{
			Provider: providerWithManifest, receipt: mustAdmissionReceipt(t, spawnQW.AdmissionReceipt),
			effectiveCell: receiptCell(),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		source = ReconcileRepositorySandbox(source, repositoryDeclaration)
		if !source.SandboxEnabled || source.SandboxLevel != agent.SandboxWorkspaceWrite {
			t.Fatalf("spawn lane sandbox = enabled=%v level=%q, want workspace-write (the repository authority override)",
				source.SandboxEnabled, source.SandboxLevel)
		}
		planCopy := plan
		source.PreparedHarness = &planCopy
		return source
	}

	t.Run("preflight and spawn agree once both reconcile the declared-repository sandbox authority", func(t *testing.T) {
		plan := compileHostPlan(t)
		source := simulateSpawnSpec(t, plan)

		if _, err := agent.PrepareHarness(source, providerWithManifest.Manifest()); err != nil {
			t.Fatalf("ApplyPreparedHarness must pass once preflight and spawn reconcile the same repository-authority sandbox override: %v", err)
		}
		if provider.spawnCalls.Load() != 0 {
			t.Fatalf("provider spawned during host compile: %d", provider.spawnCalls.Load())
		}
	})

	t.Run("control: a genuine model authority difference at spawn still refuses and names model", func(t *testing.T) {
		plan := compileHostPlan(t)
		source := simulateSpawnSpec(t, plan)
		// A GENUINE authority drift, unrelated to the repository-sandbox
		// reconciliation: the resolved model swaps between preflight and
		// spawn (e.g. a routing decision that changed independently). This
		// must never be normalized away by the fix above.
		source.Model = "different-model-swapped-after-preflight"

		_, err := agent.PrepareHarness(source, providerWithManifest.Manifest())
		if err == nil {
			t.Fatal("expected an authority drift error for the swapped model, got nil")
		}
		var driftErr *agent.AuthorityDriftError
		if !errors.As(err, &driftErr) {
			t.Fatalf("expected *agent.AuthorityDriftError, got %T: %v", err, err)
		}
		found := false
		for _, field := range driftErr.Fields {
			if field == "model" {
				found = true
			}
		}
		if !found {
			t.Fatalf("Fields = %v, want it to name %q", driftErr.Fields, "model")
		}
		if !strings.Contains(err.Error(), "model") {
			t.Fatalf("Error() = %q must name the drifting field", err.Error())
		}
	})
}

// TestPreflightAndSpawnAgreeForHumanControlledCodexWithDeclaredRepository is
// the NON-synthetic counterpart to the pi-based test above: it drives the
// exact same reconciliation scenario through codex's REAL, unmutated
// manifest (codexManifestForTest / codexCapabilitiesForTest,
// executioncell_adaptation_test.go) — codex is the one harness that
// genuinely declares MultiRepositoryWorkareaProtocols at this file's base
// commit (provider/harness/codex/manifest.go). Where the pi-based test
// above proves the reconciliation logic is correct in principle, this test
// proves it is correct against a real, currently-shipped harness capable of
// reaching this code path in production today.
func TestPreflightAndSpawnAgreeForHumanControlledCodexWithDeclaredRepository(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	providerWithManifest := &manifestSelectorProvider{
		selectorFakeProvider: provider, manifest: codexManifestForTest(), capabilities: codexCapabilitiesForTest(),
	}
	registry := NewRegistry()
	if err := registry.Register(providerWithManifest); err != nil {
		t.Fatal(err)
	}

	const model = "gpt-declared-repository-codex-model"
	const repoURL = "https://example.invalid/declared-repository-codex-primary.git"
	buildBaseQW := func() QueuedWork {
		qw := QueuedWork{}
		qw.SessionID = "host-preflight-codex-declared-repository"
		qw.Mode = interactiveRunMode
		qw.InitialPrompt = "actual human-controlled initial turn"
		qw.Repository = repoURL
		qw.ResolvedProfile = ResolvedProfile{
			Harness: string(agent.HarnessCodex), Model: model,
			Endpoint: &agent.EndpointBinding{
				Company: agent.CompanyOpenAI, Model: model, Protocol: agent.ProtoOpenAIResponses, Host: agent.HostDirect,
				EndpointID: "openai-direct", EndpointOperator: "openai", EndpointRevision: "2026-08-06", ModelAuthor: "openai",
				AuthBindingID: "auth_test", AuthAuthority: "openai", AuthCommercialMode: string(executioncell.CommercialUsageBilled),
				AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
				AuthDelivery: string(executioncell.DeliveryEnvironment), Mechanism: agent.AuthAPIKey,
			},
		}
		// Same lever as the pi-based test: PermissionProfileAutonomous
		// requests agent.SandboxFullAccess, the caller preference the
		// repository authority declaration must override.
		qw.PermissionProfile = PermissionProfileAutonomous
		qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
			Protocol: workarea.ProtocolSessionRootV1,
			Repositories: []workarea.DeclaredRepositoryV1{{
				Source: workarea.RepositorySource{Repository: repoURL}, Name: "primary",
				Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
			}},
		}
		return qw
	}
	receiptCell := func() executioncell.ResolvedExecutionCell {
		return exactReceiptCell("harness/v2", model, executioncell.SessionHumanControlled, nil)
	}

	baseQW := buildBaseQW()
	baseQW = attachAdmittedExecutionCell(t, baseQW, receiptCell())
	operational, err := CanonicalOperationalPayload(baseQW)
	if err != nil {
		t.Fatal(err)
	}
	baseQW.OperationalPayload = operational

	detail := map[string]any{
		"sessionId": baseQW.SessionID, "workerId": baseQW.WorkerID, "admissionReceipt": baseQW.AdmissionReceipt,
		"effectiveCell": baseQW.EffectiveCell, "executionRuntimeBinding": baseQW.ExecutionRuntimeBinding,
		"operationalPayload": baseQW.OperationalPayload,
	}

	receipt, err := NewProviderView(registry, nil).PreflightExecution(rawJSONForRunner(t, detail))
	if err != nil {
		t.Fatalf("PreflightExecution: %v receipt=%s", err, receipt)
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var plan agent.PreparedHarness
	if err := json.Unmarshal(host.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Mode != agent.PromptModeHumanControlled || plan.Harness != string(agent.HarnessCodex) {
		t.Fatalf("host compiled wrong identity: %+v", plan)
	}

	spawnQW := buildBaseQW()
	spawnQW.AdmissionReceipt, spawnQW.ClaimReceipt, spawnQW.EffectiveCell = baseQW.AdmissionReceipt, baseQW.ClaimReceipt, baseQW.EffectiveCell
	spawnQW.ExecutionRuntimeBinding, spawnQW.OperationalPayload = baseQW.ExecutionRuntimeBinding, baseQW.OperationalPayload
	spawnQW.WorkerID = baseQW.WorkerID

	repositoryDeclaration, _, err := resolveRepositoryWorkarea(spawnQW, providerWithManifest)
	if err != nil {
		t.Fatalf("resolveRepositoryWorkarea: %v (codex's REAL manifest must attest session-root-v1 for this test's premise to hold)", err)
	}
	if repositoryDeclaration == nil {
		t.Fatal("expected a resolved repository declaration for this declared-repository session")
	}

	source, _, err := buildPreparedSourceSpec(spawnQW, harnessSelection{
		Provider: providerWithManifest, receipt: mustAdmissionReceipt(t, spawnQW.AdmissionReceipt),
		effectiveCell: receiptCell(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	source = ReconcileRepositorySandbox(source, repositoryDeclaration)
	if !source.SandboxEnabled || source.SandboxLevel != agent.SandboxWorkspaceWrite {
		t.Fatalf("spawn lane sandbox = enabled=%v level=%q, want workspace-write (the repository authority override)",
			source.SandboxEnabled, source.SandboxLevel)
	}
	source.PreparedHarness = &plan

	if _, err := agent.PrepareHarness(source, providerWithManifest.Manifest()); err != nil {
		t.Fatalf("ApplyPreparedHarness must pass once preflight and spawn reconcile the same repository-authority sandbox override: %v", err)
	}
	if provider.spawnCalls.Load() != 0 {
		t.Fatalf("provider spawned during host compile: %d", provider.spawnCalls.Load())
	}
}
