package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

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
// The manifest below is pi's REAL, unmutated manifest: pi attests the
// session-root-v1 protocol this scenario needs, so this test proves
// ReconcileRepositorySandbox's reconciliation against the production premise
// a real pi session reaches — the same standing as the codex counterpart.
func TestPreflightAndSpawnAgreeForHumanControlledPiWithDeclaredRepository(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderPi, harness: agent.HarnessPi}
	providerWithManifest := &manifestSelectorProvider{
		selectorFakeProvider: provider, manifest: piManifestForTest(), capabilities: piCapabilitiesForTest(),
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
		receipt, err := NewProviderView(registry).PreflightExecution(rawJSONForRunner(t, detail))
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

// TestPreflightAndSpawnAgreeForHumanControlledCodexWithDeclaredRepository
// drives the exact same reconciliation scenario through codex's REAL,
// unmutated manifest (codexManifestForTest / codexCapabilitiesForTest,
// executioncell_adaptation_test.go) — codex and pi both attest the
// session-root-v1 protocol (provider/harness/codex/manifest.go,
// provider/harness/pi/manifest.go), and this test pins the reconciliation
// against codex's production premise alongside the pi-based test above.
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

	receipt, err := NewProviderView(registry).PreflightExecution(rawJSONForRunner(t, detail))
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

type repositoryAuthorityRunProvider struct {
	mu         sync.Mutex
	spawnCalls int
	spawnSpec  agent.Spec
}

func (*repositoryAuthorityRunProvider) Name() agent.ProviderName { return agent.ProviderCodex }
func (*repositoryAuthorityRunProvider) Capabilities() agent.Capabilities {
	return codexCapabilitiesForTest()
}

func (*repositoryAuthorityRunProvider) Manifest() agent.HarnessManifest {
	return codexManifestForTest()
}

func (p *repositoryAuthorityRunProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	applied, err := agent.PrepareHarness(spec, p.Manifest())
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.spawnCalls++
	p.spawnSpec = applied
	p.mu.Unlock()
	events := make(chan agent.Event, 3)
	events <- agent.InitEvent{SessionID: "repository-authority-run"}
	if applied.RepositoryAuthority == nil {
		events <- agent.AssistantTextEvent{Text: "WORK_RESULT:passed"}
	} else {
		repositories := []agent.TurnManifestRepository{{
			Name: "primary", Verdict: "passed", PullRequestURL: "https://github.com/example/repository/pull/1",
		}}
		manifest, marshalErr := json.Marshal(agent.TurnManifest{
			SchemaVersion: ManifestSchemaVersion,
			Verdict:       "passed",
			Repositories:  &repositories,
		})
		if marshalErr != nil {
			return nil, marshalErr
		}
		events <- agent.AssistantTextEvent{Text: "Intended manifest: " + string(manifest) + "\nWORK_RESULT:passed"}
	}
	events <- agent.ResultEvent{Success: true, Message: "controlled repository authority run completed"}
	close(events)
	return &repositoryAuthorityRunHandle{events: events}, nil
}

func (p *repositoryAuthorityRunProvider) Resume(ctx context.Context, _ string, spec agent.Spec) (agent.Handle, error) {
	return p.Spawn(ctx, spec)
}
func (*repositoryAuthorityRunProvider) Shutdown(context.Context) error { return nil }

func (p *repositoryAuthorityRunProvider) observation() (int, agent.Spec) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawnCalls, p.spawnSpec
}

type repositoryAuthorityRunHandle struct {
	events <-chan agent.Event
}

func (*repositoryAuthorityRunHandle) SessionID() string            { return "repository-authority-run" }
func (h *repositoryAuthorityRunHandle) Events() <-chan agent.Event { return h.events }
func (*repositoryAuthorityRunHandle) Inject(context.Context, string) error {
	return agent.ErrUnsupported
}
func (*repositoryAuthorityRunHandle) Stop(context.Context) error { return nil }

func compileRepositoryAuthorityHostReceipt(t *testing.T, registry *Registry, qw QueuedWork) json.RawMessage {
	t.Helper()
	detail := map[string]any{
		"sessionId":               qw.SessionID,
		"workerId":                qw.WorkerID,
		"admissionReceipt":        qw.AdmissionReceipt,
		"effectiveCell":           qw.EffectiveCell,
		"executionRuntimeBinding": qw.ExecutionRuntimeBinding,
		"operationalPayload":      qw.OperationalPayload,
		"resolvedProfile":         qw.ResolvedProfile,
	}
	receipt, err := NewProviderView(registry).PreflightExecution(rawJSONForRunner(t, detail))
	if err != nil {
		t.Fatalf("PreflightExecution: %v receipt=%s", err, receipt)
	}
	return receipt
}

func TestRunAdmittedReconcilesAutonomousRepositoryAuthorityBeforeEarlyApply(t *testing.T) {
	tests := []struct {
		name     string
		declared bool
	}{
		{name: "declared repository overrides autonomous full access", declared: true},
		{name: "no declaration retains autonomous full access", declared: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			provider := &repositoryAuthorityRunProvider{}
			if err := h.runner.registry.Register(provider); err != nil {
				t.Fatalf("register exact provider: %v", err)
			}

			qw := h.queuedWork("AUTONOMOUS-REPOSITORY-AUTHORITY")
			qw.McpAuthToken = "sess_test"
			qw.ResolvedProfile = exactReceiptQueuedWork(qw.SessionID).ResolvedProfile
			qw.PermissionProfile = PermissionProfileAutonomous
			if test.declared {
				qw.RepositoryDeclaration = &workarea.RepositoryDeclarationV1{
					Protocol: workarea.ProtocolSessionRootV1,
					Repositories: []workarea.DeclaredRepositoryV1{{
						Source: workarea.RepositorySource{Repository: h.bareRepo}, Name: "primary",
						Role: workarea.RepositoryRolePrimary, Authority: workarea.RepositoryMutable,
					}},
				}
			}
			qw = attachAdmittedExecutionCell(t, qw, exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil))
			operational, err := CanonicalOperationalPayload(qw)
			if err != nil {
				t.Fatalf("CanonicalOperationalPayload: %v", err)
			}
			qw.OperationalPayload = operational
			qw.HostAdaptationReceipt = compileRepositoryAuthorityHostReceipt(t, h.runner.registry, qw)
			admission, err := h.runner.registry.PreflightHarness(qw)
			if err != nil {
				t.Fatalf("PreflightHarness: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			result, err := h.runner.RunAdmitted(ctx, qw, admission)
			spawnCalls, applied := provider.observation()
			if err != nil {
				t.Fatalf("RunAdmitted: %v (spawnCalls=%d)", err, spawnCalls)
			}
			if result.Status != "completed" || spawnCalls != 1 {
				t.Fatalf("RunAdmitted result=%#v spawnCalls=%d, want completed/1", result, spawnCalls)
			}

			if !test.declared {
				if applied.SandboxLevel != agent.SandboxFullAccess || applied.RepositoryAuthority != nil {
					t.Fatalf("undeclared autonomous spec sandbox=%q authority=%#v, want full-access/nil", applied.SandboxLevel, applied.RepositoryAuthority)
				}
				return
			}

			authority := applied.RepositoryAuthority
			if authority == nil || !applied.SandboxEnabled || applied.SandboxLevel != agent.SandboxWorkspaceWrite ||
				authority.Protocol != string(workarea.ProtocolSessionRootV1) || authority.WorkareaRoot != result.WorkareaRoot ||
				authority.SelectedPath != result.WorktreePath || len(authority.MutablePaths) != 1 || authority.MutablePaths[0] != result.WorktreePath ||
				len(authority.ReadOnlyPaths) != 0 || authority.Enforcement != string(workarea.RepositoryAuthorityIsolatedReadOnlyV1) ||
				applied.Cwd != result.WorktreePath {
				t.Fatalf("declared autonomous spec=%#v result=%#v", applied, result)
			}
		})
	}
}
