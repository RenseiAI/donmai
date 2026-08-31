package afcli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/runner"
)

// testFakeDecoratorHarnessName/testFakeDecoratorProviderName reuse the
// real, canonical "stub" harness/provider identity (agent.HarnessStub /
// agent.ProviderStub) — harness admission validates the harness id against
// the matrix package's fixed canonical/legacy-alias set, so an invented name
// denies before ever reaching this test's fake manifest. This registry
// carries ONLY this file's fake provider under that name (never the real
// provider/harness/stub package, whose manifest declares no PromptDelivery
// profile at all and so can never reach a successful compile), so nothing
// here exercises the real stub implementation.
const (
	testFakeDecoratorHarnessName  = agent.HarnessStub
	testFakeDecoratorProviderName = agent.ProviderStub
)

// daemonDecoratorFakeProvider is a minimal agent.HarnessProvider whose
// manifest declares a full, always-admittable PromptDelivery + ToolLifecycle
// profile (an autonomous/headless session, an admitted — not Unsupported —
// ToolPluginDelivery) so TestDaemonProviderViewAppliesConfiguredDecorator can
// drive a real, successful CompilePreparedHarness compile deterministically.
// Spawn/Resume are never called by this test (PreflightExecution never
// spawns) and error loudly if they ever are.
type daemonDecoratorFakeProvider struct{}

func (daemonDecoratorFakeProvider) Name() agent.ProviderName { return testFakeDecoratorProviderName }

func (daemonDecoratorFakeProvider) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (daemonDecoratorFakeProvider) Shutdown(context.Context) error   { return nil }
func (daemonDecoratorFakeProvider) Spawn(context.Context, agent.Spec) (agent.Handle, error) {
	return nil, errors.New("daemonDecoratorFakeProvider: Spawn must never be called by a preflight-only test")
}

func (daemonDecoratorFakeProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, errors.New("daemonDecoratorFakeProvider: Resume must never be called by a preflight-only test")
}

func (daemonDecoratorFakeProvider) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{
		Name: testFakeDecoratorHarnessName, HumanLabel: "afcli decorator test fake",
		Family: agent.FamilyHarness, ContractABI: "harness/v2",
		Caps: agent.HarnessCaps{
			SupportsOneShot: true,
			Drives:          []agent.WireProtocol{agent.ProtoStub},
			DrivesHosts:     []agent.ServingHost{agent.HostLocal},
			Transport:       agent.TransportDirectAPI,
		},
		PromptDelivery: []agent.PromptDeliveryProfile{{
			ID: "afcli-decorator-test-fake/prompt-v1", Mode: agent.PromptModeAutonomous,
			SystemDelivery: agent.PromptDeliveryUnsupported, BaseAppendDelivery: agent.PromptDeliveryUnsupported,
			BaseReplaceDelivery: agent.PromptDeliveryUnsupported, ContextDelivery: agent.PromptDeliveryUnsupported,
			UserDelivery: agent.PromptDeliveryPiRPCPrompt, AmendmentDelivery: agent.PromptDeliveryUnsupported,
		}},
		ToolLifecycle: []agent.ToolLifecycleProfile{{
			ID: "afcli-decorator-test-fake/tool-v1", Mode: agent.PromptModeAutonomous,
			// Every delivery is admitted (agent.ToolDeliveryStubOracle,
			// mirroring provider/harness/stub's own all-on ToolLifecycle
			// profile — the proven-well-formed shape) so nothing this
			// fake's compile happens to inject (allowed-tools, MCP, …)
			// fatally denies; the test's only interest is whether an
			// additional-extensions entry appears, which requires
			// ToolPluginDelivery specifically to be admitted, not
			// Unsupported.
			ToolPluginDelivery: agent.ToolDeliveryStubOracle, MCPDelivery: agent.ToolDeliveryStubOracle,
			NativeToolPolicyDelivery: agent.ToolDeliveryStubOracle, PermissionConfigDelivery: agent.ToolDeliveryStubOracle,
			MCPToolPolicyDelivery: agent.ToolDeliveryStubOracle, ToolHookDelivery: agent.ToolDeliveryStubOracle,
			LifecycleDelivery: agent.ToolDeliveryStubOracle, LifecycleFidelity: agent.EvidenceStructured, LifecycleEvents: []agent.EventKind{agent.EventInit, agent.EventResult},
			ReplayDelivery: agent.ToolDeliveryStubOracle, ReplayFidelity: agent.EvidenceStructured, ReplayEvents: []agent.EventKind{agent.EventInit, agent.EventResult},
			CleanupDelivery: agent.ToolDeliveryStubOracle, EvidenceTier: "unit_verified",
		}},
	}
}

// fakeDecoratorReceiptCell mirrors runner's own exactReceiptCell/piReceiptCell
// test fixtures (runner/executioncell_adaptation_test.go,
// runner/resolved_profile_reconcile_test.go), rebuilt here from exported
// executioncell/agent APIs only (those helpers are unexported to the runner
// package's own test binary), naming testFakeDecoratorHarnessName.
func fakeDecoratorReceiptCell() executioncell.ResolvedExecutionCell {
	return executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion,
		Harness:         executioncell.HarnessRef{ID: string(testFakeDecoratorHarnessName), Version: "harness/v2"},
		Model:           executioncell.ModelRef{ID: "fake-model", Author: "fake"},
		Endpoint: executioncell.ServingEndpointRef{
			ID: "fake-endpoint", Protocol: string(agent.ProtoStub), Operator: "fake", Revision: "2026-08-06",
		},
		AuthBinding: executioncell.AuthBindingRef{
			ID: "fake-auth", Mechanism: executioncell.AuthAPIKey, CommercialMode: executioncell.CommercialUsageBilled,
			Authority: "fake", BindingScope: executioncell.ScopeProcess, Portability: executioncell.Portable, Delivery: executioncell.DeliveryEnvironment,
		},
		Placement:           executioncell.PlacementRef{ID: "host_test", Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact},
		SessionMode:         executioncell.SessionAutonomous,
		GrantedCapabilities: []executioncell.CapabilityRequirement{},
		EvidenceTier:        executioncell.EvidenceUnitVerified,
		CompatibilityDigest: strings.Repeat("3", 64), RuntimeInventoryDigest: strings.Repeat("4", 64),
	}
}

// attachAdmittedExecutionCellForTest mirrors runner's own
// attachAdmittedExecutionCell (executioncell_adaptation_test.go), rebuilt
// from exported runner/executioncell APIs only.
func attachAdmittedExecutionCellForTest(t *testing.T, qw runner.QueuedWork, cell executioncell.ResolvedExecutionCell) runner.QueuedWork {
	t.Helper()
	payloadDigest, err := runner.DigestOperationalPayload(qw)
	if err != nil {
		t.Fatalf("digest operational payload: %v", err)
	}
	receipt := executioncell.AdmissionReceipt{
		ContractVersion:          executioncell.ContractVersion,
		ReceiptID:                "admission_" + qw.SessionID,
		RequestID:                qw.SessionID,
		Decision:                 executioncell.AdmissionAdmitted,
		IntentDigest:             strings.Repeat("1", 64),
		OperationalPayloadDigest: payloadDigest,
		Cell:                     &cell,
		ResolverDecisions: []executioncell.ResolverDecision{{
			Kind: executioncell.DecisionExplicit, Field: "harness", SelectedRef: "harness:" + cell.Harness.ID + "@" + cell.Harness.Version, Reason: "test receipt pins the exact harness",
		}},
		RecordedAt: "2026-08-06T12:00:00Z",
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal admission receipt: %v", err)
	}
	immutable, err := executioncell.DecodeAdmissionReceipt(raw)
	if err != nil {
		t.Fatalf("validate admission receipt: %v", err)
	}
	effective, err := executioncell.CanonicalJSON(cell)
	if err != nil {
		t.Fatalf("canonical effective cell: %v", err)
	}
	qw.AdmissionReceipt = immutable.Bytes()
	qw.EffectiveCell = effective
	qw.WorkerID = "worker_test"
	binding, err := json.Marshal(executioncell.RuntimeBinding{
		ContractVersion: executioncell.RuntimeBindingContractVersion,
		RequestID:       qw.SessionID, WorkerID: qw.WorkerID, PlacementID: cell.Placement.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	qw.ExecutionRuntimeBinding = binding
	return qw
}

// stubAdditionalExtensionDeliveryForTest returns a structurally well-formed
// inline ExtensionDelivery — the shape Config.AgentSpecExtensionDecorator
// appends for kit-provided tools or a2a tool registration. Mirrors
// runner.additionalExtensionDeliveryForTest (runner/tool_lifecycle_reconcile_test.go),
// rebuilt here because that helper is unexported to the runner package's own
// test binary.
func stubAdditionalExtensionDeliveryForTest(id, content string) agent.ExtensionDelivery {
	sum := sha256.Sum256([]byte(content))
	return agent.ExtensionDelivery{
		ID: id, Kind: agent.ExtensionDeliveryInline, Source: []byte(content),
		Basename: id + ".js", Digest: hex.EncodeToString(sum[:]), Required: true,
	}
}

// TestDaemonProviderViewAppliesConfiguredDecorator is the N1 regression seam
// pinning that Config.AgentSpecExtensionDecorator actually reaches the
// daemon-side ProviderView (daemonProviderView, newDaemonRunCmd's own
// construction site) — not just that runner.ReconcileAdditionalExtensions
// works in isolation (runner/tool_lifecycle_reconcile_test.go already pins
// that in the runner package). Before daemonProviderView existed, reverting
// the lines that thread cfg.AgentSpecExtensionDecorator into
// runner.NewProviderViewWithDecorator left every existing test green,
// because none of them called the daemon's own registry-construction path —
// they either drove PreflightExecution against a hand-built fake registry
// (runner package tests) or drove runAgentRun's per-session registry
// (afcli's own decorator tests, agent_run_spec_decorator_test.go). This test
// drives daemonProviderView directly and asserts BEHAVIORALLY that the
// compiled ToolLifecycleReceipt differs based on cfg.AgentSpecExtensionDecorator
// alone — a revert of either decorator argument inside daemonProviderView
// fails it.
//
// daemonRegistryBuilder is substituted with a fake, always-successful
// registry (daemonDecoratorFakeProvider) rather than driving the real
// BuildDecoratedAgentRunRegistry: that function's real ctor list probes
// claude/codex/pi/... binaries this test environment cannot assume are
// installed. The registry-construction half of the production wiring
// (BuildDecoratedAgentRunRegistry itself, and whether newDaemonRunCmd calls
// it with cfg.AgentSpecExtensionDecorator) is covered separately by
// TestDecorateProvider_* (agent package) and this repo's build — the part
// unique to daemonProviderView, and the part this file's fix actually
// depends on for the reported incident, is the ProviderView.decorate field
// this test exercises: PreflightExecution never calls Provider.Spawn, so
// registry-level provider wrapping is inert for its own compile path (see
// runner.ReconcileAdditionalExtensions's doc comment) — only the decorator
// threaded into compilePreparedHarness via ProviderView.decorate changes
// what gets compiled, and that is exactly what daemonRegistryBuilder's
// substitution isolates.
func TestDaemonProviderViewAppliesConfiguredDecorator(t *testing.T) {
	original := daemonRegistryBuilder
	t.Cleanup(func() { daemonRegistryBuilder = original })
	daemonRegistryBuilder = func(_ *slog.Logger, decorate agent.ExtensionDecorator) *runner.Registry {
		reg := runner.NewRegistry()
		provider := agent.Provider(daemonDecoratorFakeProvider{})
		if decorate != nil {
			provider = agent.DecorateProvider(provider, decorate)
		}
		if err := reg.Register(provider); err != nil {
			t.Fatalf("register fake decorator provider: %v", err)
		}
		return reg
	}

	const sessionID = "host-daemon-provider-view-decorator"
	delivery := stubAdditionalExtensionDeliveryForTest("kit-tool-pack", "register a kit-provided tool")
	decorate := func(agent.Spec) []agent.ExtensionDelivery {
		return []agent.ExtensionDelivery{delivery}
	}

	compile := func(t *testing.T, cfg Config) agent.PreparedHarness {
		t.Helper()
		qw := runner.QueuedWork{}
		qw.SessionID = sessionID
		qw.IssueIdentifier = "TEST-1"
		qw.ResolvedProfile = runner.ResolvedProfile{
			Harness: string(testFakeDecoratorHarnessName), Model: "fake-model",
			Endpoint: &agent.EndpointBinding{
				Company: "fake", Model: "fake-model", Protocol: agent.ProtoStub, Host: agent.HostLocal,
				EndpointID: "fake-endpoint", EndpointOperator: "fake", EndpointRevision: "2026-08-06", ModelAuthor: "fake",
				AuthBindingID: "fake-auth", AuthAuthority: "fake", AuthCommercialMode: string(executioncell.CommercialUsageBilled),
				AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
				AuthDelivery: string(executioncell.DeliveryEnvironment), Mechanism: agent.AuthAPIKey,
			},
		}
		qw = attachAdmittedExecutionCellForTest(t, qw, fakeDecoratorReceiptCell())
		operational, err := runner.CanonicalOperationalPayload(qw)
		if err != nil {
			t.Fatal(err)
		}
		qw.OperationalPayload = operational
		detail := map[string]any{
			"sessionId": qw.SessionID, "workerId": qw.WorkerID, "admissionReceipt": qw.AdmissionReceipt,
			"effectiveCell": qw.EffectiveCell, "executionRuntimeBinding": qw.ExecutionRuntimeBinding,
			"operationalPayload": qw.OperationalPayload,
		}
		raw, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		view := daemonProviderView(cfg, quietLogger())
		receipt, err := view.PreflightExecution(raw)
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
		if plan.Harness != string(testFakeDecoratorHarnessName) || plan.ToolLifecycleReceipt.Decision != "ready" {
			t.Fatalf("host did not compile a ready plan for the fake harness: %+v", plan)
		}
		return plan
	}

	hasAdditionalExtensionsEntry := func(plan agent.PreparedHarness) bool {
		for _, entry := range plan.ToolLifecycleReceipt.Entries {
			if entry.ID == "additional-extensions" {
				return true
			}
		}
		return false
	}

	t.Run("no decorator configured: daemon compiles without an additional-extensions entry", func(t *testing.T) {
		plan := compile(t, Config{})
		if hasAdditionalExtensionsEntry(plan) {
			t.Fatalf("unconfigured daemon compiled an additional-extensions entry it should not know about: %+v", plan.ToolLifecycleReceipt.Entries)
		}
	})

	t.Run("decorator configured: daemonProviderView's compiled receipt reflects it", func(t *testing.T) {
		plan := compile(t, Config{AgentSpecExtensionDecorator: decorate})
		if !hasAdditionalExtensionsEntry(plan) {
			t.Fatalf("Config.AgentSpecExtensionDecorator did not reach daemonProviderView's compiled receipt: entries=%+v", plan.ToolLifecycleReceipt.Entries)
		}
	})
}
