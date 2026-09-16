package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/codeintelbridge"
	"github.com/RenseiAI/donmai/internal/codeintelcontract"
	"github.com/RenseiAI/donmai/internal/kit"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

type capturingManifestProvider struct {
	*manifestSelectorProvider
	spawned    agent.Spec
	spawnCalls int
}

func (p *capturingManifestProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	p.spawned = spec
	p.spawnCalls++
	return nil, errors.New("capture provider stops before process spawn")
}

func TestCodeIntelDeliveryRouteControlsAllThreeProducers(t *testing.T) {
	ci := &prompt.CodeIntelWork{Tools: []string{"af_code_search_symbols", "af_code_get_repo_map"}}
	native := codeIntelBindingFixture(t, `{"codeIntel":{"tools":["af_code_search_symbols","af_code_get_repo_map"]}}`, agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous, true)
	route, err := resolveCodeIntelDeliveryRoute(ci, []ResolvedCapabilityParameterBinding{native})
	if err != nil || route.Route != codeIntelDeliveryNative {
		t.Fatalf("native route=%v err=%v", route, err)
	}
	partial := injectCodeIntelPartialForDelivery("base", mcpCaps(), ci, route)
	if strings.Contains(partial, "mcp__") || strings.Contains(partial, " code ") || !strings.Contains(partial, "af_code_get_repo_map") {
		t.Fatalf("native partial=%q", partial)
	}
	qw := QueuedWork{}
	qw.CodeIntel = ci
	if servers := defaultMCPServersForHarness(qw, "/abs/worktree", mcpDeliveringHarness(), agent.PromptModeAutonomous, route.Route); len(servers) != 0 {
		t.Fatalf("native route retained code-intelligence MCP: %+v", servers)
	}
	spec := translateSpec(qw, mcpCaps(), SpecInputs{CodeIntelDeliveryRoute: route.Route})
	if len(spec.MCPToolNames) != 0 {
		t.Fatalf("native route retained FQ names: %v", spec.MCPToolNames)
	}

	mcp := codeIntelBindingFixture(t, `{"codeIntel":{"tools":["af_code_search_symbols","af_code_get_repo_map"]}}`, agent.HarnessCodex, "codex/test/mcp-v1", agent.PromptModeAutonomous, false)
	route, err = resolveCodeIntelDeliveryRoute(ci, []ResolvedCapabilityParameterBinding{mcp})
	if err != nil || route.Route != codeIntelDeliveryMCP {
		t.Fatalf("MCP route=%v err=%v", route, err)
	}
	if servers := defaultMCPServersForHarness(qw, "/abs/worktree", mcpDeliveringHarness(), agent.PromptModeAutonomous, route.Route); len(servers) != 1 || servers[0].Name != codeIntelServerName {
		t.Fatalf("MCP route servers=%+v", servers)
	}
	if got := translateSpec(qw, mcpCaps(), SpecInputs{CodeIntelDeliveryRoute: route.Route}).MCPToolNames; len(got) != 2 || !strings.HasPrefix(got[0], codeIntelFQPrefix) {
		t.Fatalf("MCP route names=%v", got)
	}
}

func TestRunnerNewConstructsPreparedCapabilityResolver(t *testing.T) {
	realizations, _ := codeIntelRealizationFixture(t, agent.HarnessCodex, "codex/test/mcp-v1", agent.PromptModeAutonomous, false)
	binder, _ := NewCodeIntelParameterBinder(testCodeIntelBinderDigest)
	binders, _ := NewCapabilityParameterBinderRegistry(binder)
	server := mockPlatformServer(t)
	t.Cleanup(server.Close)
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{PlatformURL: server.URL, WorkerID: "worker", AuthToken: "token", HTTPClient: server.Client(), BaseDelay: 1})
	if err != nil {
		t.Fatal(err)
	}
	run, err := New(Options{Registry: NewRegistry(), WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(), CapabilityRealizations: realizations, CapabilityParameterBinders: binders, SkipBackstop: true, SkipSteering: true, SkipPostSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if run.capabilityRealizations != realizations || run.capabilityParameterBinders != binders {
		t.Fatal("Runner.New did not preserve process-owned static/binder registries")
	}
	if _, ok := run.preparedCapabilities.(*parameterBoundCapabilityResolver); !ok {
		t.Fatalf("prepared resolver type = %T", run.preparedCapabilities)
	}
	if _, err := New(Options{Registry: NewRegistry(), WorktreeManager: manager, Poster: poster, CapabilityParameterBinders: binders}); err == nil {
		t.Fatal("Runner.New accepted binders without static realization rows")
	}
}

func TestRunnerRunUsesSupportedBinderConstructionBeforeSpawn(t *testing.T) {
	manifest := codexManifestForTest()
	for i := range manifest.ToolLifecycle {
		if manifest.ToolLifecycle[i].Mode == agent.PromptModeAutonomous {
			manifest.ToolLifecycle[i].ToolPluginDelivery = agent.ToolDeliveryPiAdditionalExtension
			manifest.ToolLifecycle[i].NativeToolPolicyDelivery = agent.ToolDeliveryPiInjectedBoundary
			manifest.ToolLifecycle[i].NamedExtensionEntries = true
		}
	}
	profile, _ := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	realizations, _ := codeIntelRealizationFixtureWithTools(t, "example.code-intelligence/v1", agent.HarnessCodex, profile.ID, agent.PromptModeAutonomous, true, []string{"af_code_get_repo_map", "af_code_search_symbols"})
	binder, _ := NewCodeIntelParameterBinder(testCodeIntelBinderDigest)
	binders, _ := NewCapabilityParameterBinderRegistry(binder)
	caps := codexCapabilitiesForTest()
	caps.AcceptsAllowedToolsList = true
	baseProvider := &manifestSelectorProvider{selectorFakeProvider: &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}, manifest: manifest, capabilities: caps}
	provider := &capturingManifestProvider{manifestSelectorProvider: baseProvider}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	server := mockPlatformServer(t)
	t.Cleanup(server.Close)
	qw := exactReceiptQueuedWork("native-runner-construction")
	qw.Body = "exercise actual runner construction"
	qw.PlatformURL = server.URL
	qw.AuthToken = "token"
	qw.CodeIntel = &prompt.CodeIntelWork{}
	qw.Skills = []prompt.SkillSpec{{ID: "native-policy", Body: "policy", DisallowedTools: []string{"mcp__" + codeintelcontract.ServerName + "__" + codeintelcontract.ToolSearchSymbols}}}
	operational, _ := CanonicalOperationalPayload(qw)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(operational, &fields)
	parametersDigest, _ := executioncell.DigestCapabilityParameters(fields["codeIntel"])
	cell := exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: "example.code-intelligence/v1", ParametersDigest: parametersDigest}})
	qw = attachAdmittedExecutionCell(t, qw, cell)
	qw.OperationalPayload = operational
	detail := map[string]any{"sessionId": qw.SessionID, "workerId": qw.WorkerID, "admissionReceipt": qw.AdmissionReceipt, "effectiveCell": qw.EffectiveCell, "executionRuntimeBinding": qw.ExecutionRuntimeBinding, "operationalPayload": qw.OperationalPayload}
	view, _ := NewProviderViewWithOptions(registry, ProviderViewOptions{CapabilityRealizations: realizations, CapabilityParameterBinders: binders})
	host, err := view.PreflightExecution(rawJSONForRunner(t, detail))
	if err != nil {
		t.Fatal(err)
	}
	qw.HostAdaptationReceipt = host
	manager, _ := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	poster, _ := result.NewPoster(result.Options{PlatformURL: server.URL, WorkerID: qw.WorkerID, AuthToken: "token", HTTPClient: server.Client(), BaseDelay: 1})
	run, err := New(Options{Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(), CapabilityRealizations: realizations, CapabilityParameterBinders: binders, SkipBackstop: true, SkipSteering: true, SkipPostSession: true, MaxSessionDuration: -1})
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := run.Run(context.Background(), qw)
	if provider.spawned.ToolLifecyclePlan == nil {
		t.Fatalf("actual run loop never reached provider with prepared authority: result=%+v err=%v", result, runErr)
	}
	for _, mcpServer := range provider.spawned.MCPServers {
		if mcpServer.Name == codeIntelServerName {
			t.Fatalf("actual run loop retained code-intelligence MCP: %+v", provider.spawned.MCPServers)
		}
	}
	if len(provider.spawned.MCPToolNames) != 0 || len(provider.spawned.AdditionalExtensions) != 1 || len(provider.spawned.CapabilityRuntimeMaterializations) != 1 || !strings.Contains(provider.spawned.SystemPromptAppend, "native code-intelligence") {
		t.Fatalf("actual run loop route drift: mcpNames=%d extensions=%d materializations=%d nativePrompt=%v", len(provider.spawned.MCPToolNames), len(provider.spawned.AdditionalExtensions), len(provider.spawned.CapabilityRuntimeMaterializations), strings.Contains(provider.spawned.SystemPromptAppend, "native code-intelligence"))
	}
	if !strings.Contains(provider.spawned.SystemPromptAppend, "af_code_get_repo_map") || !strings.Contains(provider.spawned.SystemPromptAppend, "af_code_search_symbols") || strings.Contains(provider.spawned.SystemPromptAppend, "af_code_search_code") {
		t.Fatalf("native prompt did not use exact declared default surface: %q", provider.spawned.SystemPromptAppend)
	}
	wantAllowed := append(defaultAllowedTools(), codeintelcontract.ToolGetRepoMap, codeintelcontract.ToolSearchSymbols)
	if !slices.Equal(provider.spawned.AllowedTools, wantAllowed) {
		t.Fatalf("actual run native allowlist=%v want=%v", provider.spawned.AllowedTools, wantAllowed)
	}
	if !slices.Contains(provider.spawned.DisallowedTools, codeintelcontract.ToolSearchSymbols) {
		t.Fatalf("actual run omitted normalized inline-card deny: %v", provider.spawned.DisallowedTools)
	}
}

func TestRunnerReceiptBoundNativePathDoesNotLoadPrecomputedKitSkills(t *testing.T) {
	manifest := codexManifestForTest()
	for i := range manifest.ToolLifecycle {
		if manifest.ToolLifecycle[i].Mode == agent.PromptModeAutonomous {
			manifest.ToolLifecycle[i].ToolPluginDelivery = agent.ToolDeliveryPiAdditionalExtension
			manifest.ToolLifecycle[i].NativeToolPolicyDelivery = agent.ToolDeliveryPiInjectedBoundary
			manifest.ToolLifecycle[i].NamedExtensionEntries = true
		}
	}
	profile, _ := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	realizations, _ := codeIntelRealizationFixtureWithTools(t, "example.code-intelligence/v1", agent.HarnessCodex, profile.ID, agent.PromptModeAutonomous, true, []string{codeintelcontract.ToolGetRepoMap, codeintelcontract.ToolSearchSymbols})
	binder, _ := NewCodeIntelParameterBinder(testCodeIntelBinderDigest)
	binders, _ := NewCapabilityParameterBinderRegistry(binder)
	caps := codexCapabilitiesForTest()
	caps.AcceptsAllowedToolsList = true
	baseProvider := &manifestSelectorProvider{selectorFakeProvider: &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}, manifest: manifest, capabilities: caps}
	provider := &capturingManifestProvider{manifestSelectorProvider: baseProvider}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	server := mockPlatformServer(t)
	t.Cleanup(server.Close)
	qw := exactReceiptQueuedWork("native-late-kit-policy")
	qw.Body = "exercise late kit policy refusal"
	qw.PlatformURL = server.URL
	qw.AuthToken = "token"
	qw.CodeIntel = &prompt.CodeIntelWork{}
	operational, _ := CanonicalOperationalPayload(qw)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(operational, &fields)
	parametersDigest, _ := executioncell.DigestCapabilityParameters(fields["codeIntel"])
	cell := exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: "example.code-intelligence/v1", ParametersDigest: parametersDigest}})
	qw = attachAdmittedExecutionCell(t, qw, cell)
	qw.OperationalPayload = operational
	detail := map[string]any{"sessionId": qw.SessionID, "workerId": qw.WorkerID, "admissionReceipt": qw.AdmissionReceipt, "effectiveCell": qw.EffectiveCell, "executionRuntimeBinding": qw.ExecutionRuntimeBinding, "operationalPayload": qw.OperationalPayload}
	view, _ := NewProviderViewWithOptions(registry, ProviderViewOptions{CapabilityRealizations: realizations, CapabilityParameterBinders: binders})
	host, err := view.PreflightExecution(rawJSONForRunner(t, detail))
	if err != nil {
		t.Fatal(err)
	}
	qw.HostAdaptationReceipt = host

	kitDir := t.TempDir()
	writeSkillMD(t, kitDir, "skills/native-policy/SKILL.md", "---\ntools:\n  disallow:\n    - shell: \"af_code_search_symbols\"\n---\n\n# Native policy\n")
	kitSources := []kit.KitSkillSource{{ID: "fixture/native-policy", Priority: 10, ManifestPath: filepath.Join(kitDir, "fixture.kit.toml"), SkillFiles: []string{"skills/native-policy/SKILL.md"}}}
	manager, _ := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	poster, _ := result.NewPoster(result.Options{PlatformURL: server.URL, WorkerID: qw.WorkerID, AuthToken: "token", HTTPClient: server.Client(), BaseDelay: 1})
	run, err := New(Options{Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(), CapabilityRealizations: realizations, CapabilityParameterBinders: binders, KitSkillSources: kitSources, SkipBackstop: true, SkipSteering: true, SkipPostSession: true, MaxSessionDuration: -1})
	if err != nil {
		t.Fatal(err)
	}
	got, runErr := run.Run(context.Background(), qw)
	if runErr == nil || !strings.Contains(runErr.Error(), "capture provider stops") {
		t.Fatalf("run error=%T %v result=%+v, want capture-provider sentinel", runErr, runErr, got)
	}
	if provider.spawnCalls != 1 {
		t.Fatalf("provider spawn calls=%d want=1", provider.spawnCalls)
	}
	if slices.Contains(provider.spawned.DisallowedTools, codeintelcontract.ToolSearchSymbols) {
		t.Fatalf("receipt-bound path unexpectedly loaded precomputed kit policy: %v", provider.spawned.DisallowedTools)
	}
}

func TestPreparedSourceSelectsNativeBeforeAllProducers(t *testing.T) {
	manifest := codexManifestForTest()
	for i := range manifest.ToolLifecycle {
		if manifest.ToolLifecycle[i].Mode == agent.PromptModeAutonomous {
			manifest.ToolLifecycle[i].ToolPluginDelivery = agent.ToolDeliveryPiAdditionalExtension
			manifest.ToolLifecycle[i].NativeToolPolicyDelivery = agent.ToolDeliveryPiInjectedBoundary
			manifest.ToolLifecycle[i].NamedExtensionEntries = true
		}
	}
	profile, _ := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	realizations, _ := codeIntelRealizationFixtureWithTools(t, "example.code-intelligence/v1", agent.HarnessCodex, profile.ID, agent.PromptModeAutonomous, true, []string{"af_code_get_repo_map", "af_code_search_symbols"})
	binder, _ := NewCodeIntelParameterBinder(testCodeIntelBinderDigest)
	binders, _ := NewCapabilityParameterBinderRegistry(binder)
	resolver, err := newPreparedCapabilityResolver(realizations, binders)
	if err != nil {
		t.Fatal(err)
	}
	qw := exactReceiptQueuedWork("native-prepared-source")
	qw.Body = "exercise native capability routing"
	qw.CodeIntel = &prompt.CodeIntelWork{Repo: "example/repo"}
	operational, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(operational, &fields)
	parametersDigest, _ := executioncell.DigestCapabilityParameters(fields["codeIntel"])
	cell := exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: "example.code-intelligence/v1", ParametersDigest: parametersDigest}})
	qw = attachAdmittedExecutionCell(t, qw, cell)
	qw.OperationalPayload = operational
	caps := codexCapabilitiesForTest()
	caps.AcceptsAllowedToolsList = true
	provider := &manifestSelectorProvider{selectorFakeProvider: &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}, manifest: manifest, capabilities: caps}
	selection := harnessSelection{Provider: provider, receipt: mustAdmissionReceipt(t, qw.AdmissionReceipt), effectiveCell: cell}
	spec, runtimeNames, err := buildPreparedSourceSpec(qw, selection, nil, resolver)
	if err != nil {
		t.Fatal(err)
	}
	for _, server := range spec.MCPServers {
		if server.Name == codeIntelServerName {
			t.Fatalf("native prepared source retained code-intelligence MCP: %+v", spec.MCPServers)
		}
	}
	if len(spec.MCPToolNames) != 0 || strings.Contains(spec.SystemPromptAppend, "mcp__") || !strings.Contains(spec.SystemPromptAppend, "native code-intelligence") {
		t.Fatalf("native prepared source retained MCP producer: servers=%+v names=%v runtime=%v prompt=%q", spec.MCPServers, spec.MCPToolNames, runtimeNames, spec.SystemPromptAppend)
	}
	if !strings.Contains(spec.SystemPromptAppend, "af_code_get_repo_map") || !strings.Contains(spec.SystemPromptAppend, "af_code_search_symbols") || strings.Contains(spec.SystemPromptAppend, "af_code_search_code") {
		t.Fatalf("prepared native prompt did not use exact declared default surface: %q", spec.SystemPromptAppend)
	}
	if len(spec.AdditionalExtensions) != 1 || len(spec.CapabilityRuntimeMaterializations) != 1 || spec.ToolLifecyclePlan == nil || len(spec.ToolLifecyclePlan.CapabilityRealizations) != 1 {
		t.Fatalf("native prepared authority incomplete: %+v", spec)
	}
	plan, _, err := compilePreparedHarness(qw, selection, nil, nil, resolver)
	if err != nil || plan.ToolLifecycleReceipt.Decision != "ready" {
		t.Fatalf("compiled native plan=%+v err=%v", plan, err)
	}
	extraSource := []byte("export default function activate() {}\n")
	extraDigest := sha256.Sum256(extraSource)
	decorator := func(agent.Spec) []agent.ExtensionDelivery {
		return []agent.ExtensionDelivery{{ID: "downstream-advisory", Kind: agent.ExtensionDeliveryInline, Source: extraSource, Basename: "downstream.ts", Digest: hex.EncodeToString(extraDigest[:])}}
	}
	decorated, _, err := buildPreparedSourceSpec(qw, selection, decorator, resolver)
	if err != nil || len(decorated.AdditionalExtensions) != 2 {
		t.Fatalf("decorated prepared source extensions=%d err=%v", len(decorated.AdditionalExtensions), err)
	}
	applied := applyPreparedSourceAuthority(agent.Spec{}, decorated, plan)
	if len(applied.AdditionalExtensions) != 1 || applied.AdditionalExtensions[0].ID != codeintelbridge.DeliveryID {
		t.Fatalf("prepared authority copied downstream decorator delivery: %+v", applied.AdditionalExtensions)
	}
}

func TestProviderViewOptionsDriveActualParameterizedPreflight(t *testing.T) {
	manifest := codexManifestForTest()
	for i := range manifest.ToolLifecycle {
		if manifest.ToolLifecycle[i].Mode == agent.PromptModeAutonomous {
			manifest.ToolLifecycle[i].ToolPluginDelivery = agent.ToolDeliveryPiAdditionalExtension
			manifest.ToolLifecycle[i].NativeToolPolicyDelivery = agent.ToolDeliveryPiInjectedBoundary
			manifest.ToolLifecycle[i].NamedExtensionEntries = true
		}
	}
	profile, _ := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	realizations, _ := codeIntelRealizationFixture(t, agent.HarnessCodex, profile.ID, agent.PromptModeAutonomous, true)
	binder, _ := NewCodeIntelParameterBinder(testCodeIntelBinderDigest)
	binders, _ := NewCapabilityParameterBinderRegistry(binder)
	caps := codexCapabilitiesForTest()
	caps.AcceptsAllowedToolsList = true
	provider := &manifestSelectorProvider{selectorFakeProvider: &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}, manifest: manifest, capabilities: caps}
	registry := NewRegistry()
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	qw := exactReceiptQueuedWork("native-provider-view")
	qw.Body = "exercise native preflight"
	qw.CodeIntel = &prompt.CodeIntelWork{Tools: []string{"af_code_get_repo_map"}}
	qw.Skills = []prompt.SkillSpec{{ID: "native-policy", Body: "policy", DisallowedTools: []string{"mcp__" + codeintelcontract.ServerName + "__" + codeintelcontract.ToolGetRepoMap}}}
	operational, _ := CanonicalOperationalPayload(qw)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(operational, &fields)
	parametersDigest, _ := executioncell.DigestCapabilityParameters(fields["codeIntel"])
	cell := exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, []executioncell.CapabilityRequirement{{Name: "example.code-intelligence/v1", ParametersDigest: parametersDigest}})
	qw = attachAdmittedExecutionCell(t, qw, cell)
	qw.OperationalPayload = operational
	detail := map[string]any{"sessionId": qw.SessionID, "workerId": qw.WorkerID, "admissionReceipt": qw.AdmissionReceipt, "effectiveCell": qw.EffectiveCell, "executionRuntimeBinding": qw.ExecutionRuntimeBinding, "operationalPayload": qw.OperationalPayload}
	view, err := NewProviderViewWithOptions(registry, ProviderViewOptions{CapabilityRealizations: realizations, CapabilityParameterBinders: binders})
	if err != nil {
		t.Fatal(err)
	}
	rawReceipt, err := view.PreflightExecution(rawJSONForRunner(t, detail))
	if err != nil {
		t.Fatalf("preflight: %v receipt=%s", err, rawReceipt)
	}
	host, err := executioncell.DecodeHostAdaptationReceipt(rawReceipt)
	if err != nil {
		t.Fatal(err)
	}
	var plan agent.PreparedHarness
	if err := json.Unmarshal(host.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.ToolLifecycleReceipt.CapabilityRealizations) != 1 || plan.ToolLifecycleReceipt.CapabilityRealizations[0].ParameterBinding == nil {
		t.Fatalf("preflight omitted parameterized realization: %+v", plan.ToolLifecycleReceipt)
	}
	wantAllowed := append(defaultAllowedTools(), codeintelcontract.ToolGetRepoMap)
	rawAllowed, _ := json.Marshal(wantAllowed)
	allowedSum := sha256.Sum256(rawAllowed)
	if got, want := plan.AuthorityFieldDigests["allowedTools"], hex.EncodeToString(allowedSum[:]); got != want {
		t.Fatalf("preflight allowedTools digest=%s want=%s", got, want)
	}
	wantDisallowed := append(defaultDisallowedTools(), codeintelcontract.ToolGetRepoMap)
	rawDisallowed, _ := json.Marshal(wantDisallowed)
	disallowedSum := sha256.Sum256(rawDisallowed)
	if got, want := plan.AuthorityFieldDigests["disallowedTools"], hex.EncodeToString(disallowedSum[:]); got != want {
		t.Fatalf("preflight disallowedTools digest=%s want=%s", got, want)
	}
	if _, err := NewProviderViewWithOptions(registry, ProviderViewOptions{CapabilityParameterBinders: binders}); err == nil {
		t.Fatal("provider view accepted binders without static realizations")
	}
}

func TestCodeIntelLegacyRoutePreservesExistingBytes(t *testing.T) {
	ci := &prompt.CodeIntelWork{Repo: "example/repo"}
	qw := QueuedWork{}
	qw.CodeIntel = ci
	legacyServers := defaultMCPServersForHarness(qw, "/abs/worktree", mcpDeliveringHarness(), agent.PromptModeAutonomous)
	explicitServers := defaultMCPServersForHarness(qw, "/abs/worktree", mcpDeliveringHarness(), agent.PromptModeAutonomous, codeIntelDeliveryLegacy)
	if len(legacyServers) != len(explicitServers) || legacyServers[0].Name != explicitServers[0].Name || injectCodeIntelPartial("base", mcpCaps(), ci) != injectCodeIntelPartial("base", mcpCaps(), ci, codeIntelDeliveryLegacy) {
		t.Fatal("explicit legacy route changed existing behavior")
	}
}

func TestCodeIntelDeliveryRouteRejectsConflictingSelectedSets(t *testing.T) {
	first := codeIntelBindingFixture(t, `{"codeIntel":{"tools":["af_code_get_repo_map"]}}`, agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous, true)
	second := codeIntelBindingFixture(t, `{"codeIntel":{"tools":["af_code_search_symbols"]}}`, agent.HarnessPi, "pi/test/native-v1", agent.PromptModeAutonomous, true)
	if _, err := resolveCodeIntelDeliveryRoute(&prompt.CodeIntelWork{}, []ResolvedCapabilityParameterBinding{first, second}); err == nil {
		t.Fatal("same-contract realizations with conflicting selected sets routed")
	}
}
