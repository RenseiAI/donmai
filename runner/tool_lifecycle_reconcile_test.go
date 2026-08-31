package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

// additionalExtensionDeliveryForTest returns a structurally well-formed
// inline ExtensionDelivery — the shape an embedder's decorator
// (afcli.Config.AgentSpecExtensionDecorator) appends for kit-provided tools
// or a2a tool registration, the real-world source of the "additional-
// extensions" ToolLifecycleReceipt entry this file's tests reconcile.
func additionalExtensionDeliveryForTest(id, content string) agent.ExtensionDelivery {
	sum := sha256.Sum256([]byte(content))
	return agent.ExtensionDelivery{
		ID: id, Kind: agent.ExtensionDeliveryInline, Source: []byte(content),
		Basename: id + ".js", Digest: hex.EncodeToString(sum[:]), Required: true,
	}
}

// TestPreflightAndSpawnAgreeForHumanControlledPiWithAdditionalExtensionDecorator
// reproduces the reported production incident against pi's REAL, shipped
// manifest (piManifestForTest — unlike repository_sandbox_reconcile_test.go's
// synthetic manifest, no fixture mutation is needed: pi/interactive/
// tool-lifecycle-v3 already declares ToolPluginDelivery:
// agent.ToolDeliveryPiAdditionalExtension). Reported shape: an interactive,
// human-controlled pi session under the session shim failed at spawn ~2s in
// with "agent: spawn failed: agent: tool/lifecycle application differs from
// host adaptation receipt (fields: entries)". The daemon's preflight had
// admitted a ToolLifecycleReceipt with exactly ONE entry (id
// "disallowed-tools", channel "disallowed_tools", delivery
// "pi_interactive_local_tool_policy", profile
// "pi/interactive/tool-lifecycle-v3", decision "ready"); at spawn, the
// recomputed entries differed.
//
// Root cause: an embedder's additional-extension decorator
// (afcli.Config.AgentSpecExtensionDecorator, the seam kit-provided tools and
// a2a tool registration ride onto Spec.AdditionalExtensions —
// donmai-architecture 002-provider-base-contract.md §E) is applied via
// agent.DecorateProvider ONLY inside a Provider's own wrapped Spawn/Resume
// call (agent/spec_decorator.go) — the ONE place a registered decorator ever
// actually runs. Neither compile site that computes a ToolLifecycleReceipt
// from a Spec — the daemon's own preflight compiler (ProviderView.
// PreflightExecution, via compilePreparedHarness) nor Runner's own
// prepared-source authority self-check (runLoop, via the SAME
// buildPreparedSourceSpec) — ever calls Provider.Spawn, because
// CompilePreparedHarness/ApplyPreparedHarness must stay side-effect-free
// (PreparedHarness is a secret-free, digest-only authority). So
// Spec.AdditionalExtensions was invisible to both compile sites: the daemon
// persisted a plan with no "additional-extensions" entry, while the real
// spawn's Provider — resolved from a registry a decorator DID wrap — recomputed
// one WITH it, because pi's ToolPluginDelivery ADMITS a populated
// AdditionalExtensions batch rather than denying it, on both its headless and
// interactive profiles. AdditionalExtensions is also deliberately excluded
// from harnessAuthorityProjection (agent/prepared_harness.go), so the
// mismatch skips straight past the AuthorityDigest check and surfaces as the
// undiagnosable *agent.ToolLifecycleDriftError{Fields:["entries"]} instead of
// a receipt that already told the truth.
func TestPreflightAndSpawnAgreeForHumanControlledPiWithAdditionalExtensionDecorator(t *testing.T) {
	provider := &selectorFakeProvider{name: agent.ProviderPi, harness: agent.HarnessPi}
	providerWithManifest := &manifestSelectorProvider{
		selectorFakeProvider: provider, manifest: piManifestForTest(), capabilities: piCapabilitiesForTest(),
	}
	registry := NewRegistry()
	if err := registry.Register(providerWithManifest); err != nil {
		t.Fatal(err)
	}

	const model = "gpt-additional-extension-model"
	delivery := additionalExtensionDeliveryForTest("kit-tool-pack", "register a kit-provided tool")
	// decorate mirrors agent.DecorateProvider's wrapped Spawn/Resume: it is
	// applied unconditionally to every registered provider (afcli's
	// decorateRegistryProviders), appending one real ExtensionDelivery — the
	// shape a kit-provided tool pack or a2a tool registration takes.
	decorate := func(agent.Spec) []agent.ExtensionDelivery {
		return []agent.ExtensionDelivery{delivery}
	}

	buildBaseQW := func() QueuedWork {
		qw := QueuedWork{}
		qw.SessionID = "host-preflight-pi-additional-extension"
		qw.Mode = interactiveRunMode
		qw.InitialPrompt = "actual human-controlled initial turn"
		// The exact reported receipt shape: ONE entry, "disallowed-tools" —
		// no AllowedTools/PermissionConfig/MCPServers/MCPToolNames, so the
		// only entry legacyToolRequirements produces from THIS Spec's own
		// fields (before any decorator-appended additional-extensions entry
		// joins it) is this one.
		qw.DisallowedTools = []string{"Bash"}
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

	compileHostPlan := func(t *testing.T, viewDecorate agent.ExtensionDecorator) agent.PreparedHarness {
		t.Helper()
		receipt, err := NewProviderViewWithDecorator(registry, viewDecorate).PreflightExecution(rawJSONForRunner(t, detail))
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

	// simulateSpawnSpec reproduces the spawn lane's final materialized Spec —
	// the argument agent.PrepareHarness receives inside a provider's own
	// prepare() (e.g. pi.Provider.prepare, called from Spawn). It builds the
	// undecorated Spec buildPreparedSourceSpec produces (decorate is
	// deliberately nil here: the real spawn lane's Spec is built via
	// translateSpec + applyPreparedSourceAuthority, which never touches
	// AdditionalExtensions — runner/prepared_harness.go's field list), then
	// applies decorate via agent.ApplyExtensionDecorator — the exact mutation
	// agent.DecorateProvider's wrapped Spawn/Resume performs on the REAL spec
	// immediately before delegating to the underlying provider.
	simulateSpawnSpec := func(t *testing.T, plan agent.PreparedHarness) agent.Spec {
		t.Helper()
		spawnQW := buildBaseQW()
		spawnQW.AdmissionReceipt, spawnQW.ClaimReceipt, spawnQW.EffectiveCell = baseQW.AdmissionReceipt, baseQW.ClaimReceipt, baseQW.EffectiveCell
		spawnQW.ExecutionRuntimeBinding, spawnQW.OperationalPayload = baseQW.ExecutionRuntimeBinding, baseQW.OperationalPayload
		spawnQW.WorkerID = baseQW.WorkerID

		source, _, err := buildPreparedSourceSpec(spawnQW, harnessSelection{
			Provider: providerWithManifest, receipt: mustAdmissionReceipt(t, spawnQW.AdmissionReceipt),
			effectiveCell: receiptCell(),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		planCopy := plan
		source.PreparedHarness = &planCopy
		source = agent.ApplyExtensionDecorator(source, decorate)
		return source
	}

	t.Run("preflight and spawn agree once both reconcile the additional-extension decorator", func(t *testing.T) {
		plan := compileHostPlan(t, decorate)
		source := simulateSpawnSpec(t, plan)

		if _, err := agent.PrepareHarness(source, providerWithManifest.Manifest()); err != nil {
			t.Fatalf("ApplyPreparedHarness must pass once preflight and spawn reconcile the same additional-extension decorator: %v", err)
		}
		if provider.spawnCalls.Load() != 0 {
			t.Fatalf("provider spawned during host compile: %d", provider.spawnCalls.Load())
		}
	})

	t.Run("control: daemon registered no decorator while the real spawn's registry IS decorated", func(t *testing.T) {
		// This is a GENUINE, unreconciled mismatch — a deployment where the
		// daemon's own registry and the `agent run` registry disagree on
		// whether AgentSpecExtensionDecorator is wired at all (the
		// afcli-level half of this fix keeps both call sites passing the
		// SAME cfg.AgentSpecExtensionDecorator value; this sub-test proves
		// the runner-level reconciliation does NOT — and must not — paper
		// over a case where they genuinely still disagree). Must keep
		// refusing, both before and after this fix.
		plan := compileHostPlan(t, nil)
		source := simulateSpawnSpec(t, plan)

		_, err := agent.PrepareHarness(source, providerWithManifest.Manifest())
		if err == nil {
			t.Fatal("expected a tool-lifecycle drift error for the genuinely unreconciled decorator mismatch, got nil")
		}
		var toolDrift *agent.ToolLifecycleDriftError
		if !errors.As(err, &toolDrift) {
			t.Fatalf("expected *agent.ToolLifecycleDriftError (the reported \"tool/lifecycle application differs\" text), got %T: %v", err, err)
		}
		found := false
		for _, field := range toolDrift.Fields {
			if field == "entries" {
				found = true
			}
		}
		if !found {
			t.Fatalf("Fields = %v, want it to name %q", toolDrift.Fields, "entries")
		}
	})
}
