package afcli

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/statehome"
	"github.com/RenseiAI/donmai/runtime/worktree"
	"github.com/spf13/cobra"
)

func TestAgentRunProtectedRuntimeMCPSelectorReachesChildOptions(t *testing.T) {
	const capability = "example.protected-mcp/v1"
	const platformMCPServerName = "example-platform"
	realizations := receiptCapabilityRealizationsForTest(t, capability, agent.HarnessPi, agent.PromptModeHumanControlled)
	selector := runner.ProtectedRuntimeMCPSelector{
		CapabilityID: capability, HarnessID: agent.HarnessPi,
		AdapterProfileID: "pi/interactive/tool-lifecycle-v6", Mode: agent.PromptModeHumanControlled,
	}
	commandOptions := agentRunOptions(Config{
		CapabilityRealizations:      realizations,
		ProtectedRuntimeMCPSelector: selector,
		PlatformMCPServerName:       platformMCPServerName,
	}, "embedder")
	var child runner.Options
	applyAgentRunCapabilityOptions(&child, commandOptions)
	if child.CapabilityRealizations != realizations || child.ProtectedRuntimeMCPSelector != selector || child.PlatformMCPServerName != platformMCPServerName {
		t.Fatalf("child capability options = registry %p selector %+v serverName %q", child.CapabilityRealizations, child.ProtectedRuntimeMCPSelector, child.PlatformMCPServerName)
	}
}

func TestAgentRunProtectedRuntimeMCPV2SelectorReachesChildOptions(t *testing.T) {
	const capability = "example.protected-mcp/v2"
	realizations := receiptCapabilityRealizationsForTest(t, capability, agent.HarnessPi, agent.PromptModeHumanControlled)
	selector := runner.ProtectedRuntimeMCPV2Selector{
		CapabilityID: capability, HarnessID: agent.HarnessPi,
		AdapterProfileID: "pi/interactive/tool-lifecycle-v6", Mode: agent.PromptModeHumanControlled,
		ConfigRequirementID: "example.session-config/v1",
	}
	commandOptions := agentRunOptions(Config{CapabilityRealizations: realizations, ProtectedRuntimeMCPV2Selector: selector}, "embedder")
	var child runner.Options
	applyAgentRunCapabilityOptions(&child, commandOptions)
	if child.CapabilityRealizations != realizations || child.ProtectedRuntimeMCPV2Selector != selector {
		t.Fatalf("child capability options = registry %p selector %+v", child.CapabilityRealizations, child.ProtectedRuntimeMCPV2Selector)
	}
}

func TestAgentRunProtectedRuntimeMCPDefaultStaysLegacyAndMalformedUsesRunnerValidation(t *testing.T) {
	commandOptions := agentRunOptions(Config{}, "embedder")
	child := runner.Options{
		Registry: runner.NewRegistry(), WorktreeManager: &worktree.Manager{}, Poster: &result.Poster{},
	}
	applyAgentRunCapabilityOptions(&child, commandOptions)
	if child.CapabilityRealizations != nil || child.ProtectedRuntimeMCPSelector != (runner.ProtectedRuntimeMCPSelector{}) || child.ProtectedRuntimeMCPV2Selector != (runner.ProtectedRuntimeMCPV2Selector{}) {
		t.Fatalf("default child capability options = registry %p selector %+v", child.CapabilityRealizations, child.ProtectedRuntimeMCPSelector)
	}
	if _, err := runner.New(child); err != nil {
		t.Fatalf("legacy child options rejected: %v", err)
	}

	malformed := agentRunOptions(Config{ProtectedRuntimeMCPSelector: runner.ProtectedRuntimeMCPSelector{CapabilityID: "incomplete"}}, "embedder")
	applyAgentRunCapabilityOptions(&child, malformed)
	if _, err := runner.New(child); err == nil {
		t.Fatal("runner accepted malformed protected runtime MCP selector")
	}
}

func TestRunAgentRunProtectedRuntimeMCPSelectorReachesChildValidation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	originalRegistryBuilder := buildRegistryForAgentRun
	buildRegistryForAgentRun = func(*slog.Logger, agentRunCtorHints, string) *runner.Registry {
		registry := runner.NewRegistry()
		if err := registry.Register(daemonDecoratorFakeProvider{}); err != nil {
			t.Fatal(err)
		}
		return registry
	}
	t.Cleanup(func() { buildRegistryForAgentRun = originalRegistryBuilder })
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(platform.Close)

	const capability = "example.protected-mcp/v1"
	detail, realizations := protectedMCPStubDetailForTest(t, platform.URL, capability)
	selector := runner.ProtectedRuntimeMCPSelector{
		CapabilityID: capability, HarnessID: agent.HarnessStub,
		AdapterProfileID: "afcli-decorator-test-fake/tool-v1", Mode: agent.PromptModeAutonomous,
	}
	daemonServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/daemon/sessions/") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(detail) //nolint:gosec // fixed test-only bearer
	}))
	t.Cleanup(daemonServer.Close)

	opts := agentRunOptions(Config{
		CapabilityRealizations: realizations, ProtectedRuntimeMCPSelector: selector,
	}, "embedder")
	opts.sessionID, opts.daemonURL, opts.worktree = detail.SessionID, daemonServer.URL, t.TempDir()
	if err := runAgentRun(t.Context(), &cobra.Command{}, opts); err == nil || !strings.Contains(err.Error(), "Spawn must never be called") {
		t.Fatalf("runAgentRun protected child validation: %v", err)
	}
}

func protectedMCPStubDetailForTest(t *testing.T, platformURL, capability string) (daemon.SessionDetail, *agent.CapabilityRealizationRegistry) {
	t.Helper()
	provider := daemonDecoratorFakeProvider{}
	harness := agent.HarnessProvider(provider)
	profile, ok := harness.Manifest().ToolLifecycleProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("stub autonomous tool lifecycle profile is unavailable")
	}
	serverName := statehome.Brand() + "-platform"
	entryID, err := agent.MCPServerCapabilityEntryID(serverName)
	if err != nil {
		t.Fatal(err)
	}
	surface := []agent.CapabilitySurfaceIdentity{
		{Kind: agent.CapabilitySurfaceMCPServer, ID: serverName},
		{Kind: agent.CapabilitySurfaceMCPTool, ID: "example_call"},
	}
	inputDigest := agent.MCPRuntimeServerCapabilityInputDigest(agent.MCPServerConfig{Name: serverName, Type: "http"})
	declaration, err := agent.NewCapabilityRealization(agent.CapabilityRealizationInput{
		CapabilityID: capability, HarnessID: agent.HarnessStub, AdapterVersion: profile.ID, Mode: agent.PromptModeAutonomous,
		RecipeID: "example/protected-mcp/v1", DeclaredSurface: surface,
		Entries: []agent.CapabilityRecipeEntry{{EntryID: entryID, Channel: agent.ToolChannelMCPServer, Required: true, InputDigest: inputDigest, SurfaceRefs: surface}},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := agent.NewCapabilityFixtureObservation(agent.CapabilityFixtureObservationInput{
		Declaration: declaration, FixtureID: "protected-mcp-stub", BinaryDigest: strings.Repeat("b", 64),
		AppliedArtifacts: []agent.CapabilityAppliedArtifact{{EntryID: entryID, Channel: agent.ToolChannelMCPServer, InputDigest: inputDigest}},
		ObservedSurface:  surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := agent.CompileCapabilityRealization(declaration, observation)
	if err != nil {
		t.Fatal(err)
	}
	realizations, err := agent.NewCapabilityRealizationRegistry([]agent.CompiledCapabilityRealization{compiled})
	if err != nil {
		t.Fatal(err)
	}

	const sessionID = "protected-mcp-command-path"
	qw := runner.QueuedWork{
		QueuedWork:  prompt.QueuedWork{SessionID: sessionID, IssueIdentifier: "TEST-1", Body: "protected MCP command path"},
		PlatformURL: platformURL, McpAuthToken: "fixture-session-bearer",
		ResolvedProfile: runner.ResolvedProfile{
			Harness: string(agent.HarnessStub), Provider: agent.ProviderStub, Model: "fake-model",
			Endpoint: &agent.EndpointBinding{
				Company: agent.CompanyStub, Model: "fake-model", Protocol: agent.ProtoStub, Host: agent.HostLocal,
				EndpointID: "fake-endpoint", EndpointOperator: "fake", EndpointRevision: "2026-08-06", ModelAuthor: "fake",
				AuthBindingID: "fake-auth", AuthAuthority: "fake", AuthCommercialMode: string(executioncell.CommercialUsageBilled),
				AuthBindingScope: string(executioncell.ScopeProcess), AuthPortability: string(executioncell.Portable),
				AuthDelivery: string(executioncell.DeliveryEnvironment), Mechanism: agent.AuthAPIKey,
			},
		},
	}
	cell := fakeDecoratorReceiptCell()
	cell.GrantedCapabilities = []executioncell.CapabilityRequirement{{Name: capability}}
	qw = attachAdmittedExecutionCellForTest(t, qw, cell)
	operational, err := runner.CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	qw.OperationalPayload = operational
	hostRegistry := runner.NewRegistry()
	if err := hostRegistry.Register(provider); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{
		"sessionId": qw.SessionID, "workerId": qw.WorkerID, "platformUrl": qw.PlatformURL, "mcpAuthToken": qw.McpAuthToken,
		"admissionReceipt": qw.AdmissionReceipt, "effectiveCell": qw.EffectiveCell,
		"executionRuntimeBinding": qw.ExecutionRuntimeBinding, "operationalPayload": qw.OperationalPayload,
	})
	if err != nil {
		t.Fatal(err)
	}
	selector := runner.ProtectedRuntimeMCPSelector{
		CapabilityID: capability, HarnessID: agent.HarnessStub,
		AdapterProfileID: profile.ID, Mode: agent.PromptModeAutonomous,
	}
	view, err := runner.NewProviderViewWithProtectedRuntimeMCP(hostRegistry, nil, realizations, nil, selector)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := view.PreflightExecution(input)
	if err != nil {
		t.Fatalf("stub host preflight: %v", err)
	}
	requirements, err := view.ResolveExecutionPreflightProtectedRuntimeMCPRequirements(input, receipt)
	if err != nil || len(requirements) != 1 {
		t.Fatalf("protected requirements=%+v err=%v", requirements, err)
	}
	requirement := requirements[0]
	materialization := executioncell.ProtectedRuntimeMCPConfigMaterializationV1{
		ContractVersion: requirement.ContractVersion, RequirementID: requirement.RequirementID,
		AuthorityBindingDigest: requirement.AuthorityBindingDigest, OperationalPayloadDigest: requirement.OperationalPayloadDigest,
		ServerName: requirement.ServerName, Transport: requirement.Transport, EndpointDigest: requirement.EndpointDigest,
		Headers: append([]executioncell.ProtectedRuntimeMCPHeaderV1(nil), requirement.Headers...),
	}
	materialization.ConfigReferenceDigest, err = executioncell.DigestProtectedRuntimeMCPConfigReference(materialization)
	if err != nil {
		t.Fatal(err)
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	host.ContractVersion = executioncell.HostAdaptationV3ContractVersion
	host.ProtectedRuntimeMCPConfigs = []executioncell.ProtectedRuntimeMCPConfigMaterializationV1{materialization}
	hostRaw, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	return daemon.SessionDetail{
		SessionID: qw.SessionID, WorkerID: qw.WorkerID, PlatformURL: qw.PlatformURL, McpAuthToken: qw.McpAuthToken,
		AdmissionReceipt: qw.AdmissionReceipt, EffectiveCell: qw.EffectiveCell,
		ExecutionRuntimeBinding: qw.ExecutionRuntimeBinding, OperationalPayload: qw.OperationalPayload,
		HostAdaptationReceipt: hostRaw,
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Harness: string(agent.HarnessStub), Provider: string(agent.ProviderStub), Model: qw.ResolvedProfile.Model,
			Endpoint: &daemon.SessionEndpointBinding{
				Company: string(qw.ResolvedProfile.Endpoint.Company), Model: qw.ResolvedProfile.Endpoint.Model,
				Protocol: string(qw.ResolvedProfile.Endpoint.Protocol), Host: string(qw.ResolvedProfile.Endpoint.Host),
				EndpointID: qw.ResolvedProfile.Endpoint.EndpointID, EndpointOperator: qw.ResolvedProfile.Endpoint.EndpointOperator,
				EndpointRevision: qw.ResolvedProfile.Endpoint.EndpointRevision, ModelAuthor: qw.ResolvedProfile.Endpoint.ModelAuthor,
				AuthBindingID: qw.ResolvedProfile.Endpoint.AuthBindingID, AuthAuthority: qw.ResolvedProfile.Endpoint.AuthAuthority,
				AuthCommercialMode: qw.ResolvedProfile.Endpoint.AuthCommercialMode, AuthBindingScope: qw.ResolvedProfile.Endpoint.AuthBindingScope,
				AuthPortability: qw.ResolvedProfile.Endpoint.AuthPortability, AuthDelivery: qw.ResolvedProfile.Endpoint.AuthDelivery,
				Mechanism: string(qw.ResolvedProfile.Endpoint.Mechanism),
			},
		},
	}, realizations
}
