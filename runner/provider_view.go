// Package runner provider_view.go — adapter that exposes the in-process
// AgentRuntime registry as the read-only daemon.ProviderRegistry view
// consumed by the /api/daemon/providers* HTTP handler. Wave 9 / A1.
//
// The adapter lives in the runner package (not daemon) so daemon stays
// free of a runner import — daemon.ProviderRegistry is the interface,
// runner.NewProviderView builds the concrete view from a *Registry.
package runner

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// ProviderView wraps a *Registry and satisfies daemon.ProviderRegistry.
// Construct via NewProviderView. Read-only and safe for concurrent use.
type ProviderView struct {
	reg *Registry
	// decorate is the embedder's additional-extension decorator
	// (afcli.Config.AgentSpecExtensionDecorator). PreflightExecution forwards
	// it into compilePreparedHarness so the daemon's persisted
	// ToolLifecycleReceipt reflects the SAME decorated AdditionalExtensions
	// the real spawn's decorated Provider will apply — see
	// ReconcileAdditionalExtensions's doc comment for why leaving this only
	// to the spawn lane surfaces as an undiagnosable
	// *agent.ToolLifecycleDriftError instead of an admission-time truth. nil
	// preserves the historical undecorated behavior.
	decorate                      agent.ExtensionDecorator
	realizations                  *agent.CapabilityRealizationRegistry
	preparedCapabilities          capabilityRealizationResolver
	configRequirements            ExecutionPreflightConfigRequirementResolver
	protectedRuntimeMCPSelector   ProtectedRuntimeMCPSelector
	protectedRuntimeMCPV2Selector ProtectedRuntimeMCPV2Selector
}

// ExecutionPreflightConfigRequirementContext is the secret-free, fully
// admitted context passed to a trusted process-registered resolver.
type ExecutionPreflightConfigRequirementContext struct {
	SessionID                string
	OperationalEnvironment   map[string]string
	OperationalPayloadDigest string
	RuntimeBinding           executioncell.RuntimeBinding
	AdmissionReceipt         executioncell.AdmissionReceipt
	ClaimReceipt             *executioncell.ClaimReceipt
	EffectiveCell            executioncell.ResolvedExecutionCell
	CompiledReceipt          executioncell.HostAdaptationReceipt
}

// ExecutionPreflightConfigRequirementResolver returns closed common-config requirements.
type ExecutionPreflightConfigRequirementResolver func(ExecutionPreflightConfigRequirementContext) ([]executioncell.PreflightConfigRequirementV1, error)

const protectedRuntimeMCPRequirementID = "protected-runtime-mcp/v1"

const protectedRuntimeMCPRequirementIDV2 = "protected-runtime-mcp/v2"

// ProtectedRuntimeMCPSelector identifies one exact registered capability
// realization. The zero value disables protected runtime MCP acknowledgement.
// It is process configuration, never session or wire input.
type ProtectedRuntimeMCPSelector struct {
	CapabilityID     string
	HarnessID        agent.HarnessName
	AdapterProfileID string
	Mode             agent.PromptSessionMode
}

// ProtectedRuntimeMCPV2Selector identifies one exact registered realization
// and the common private-file requirement it must join. It is process-only;
// no session, queue, or authored MCP input can configure it.
type ProtectedRuntimeMCPV2Selector struct {
	CapabilityID        string
	HarnessID           agent.HarnessName
	AdapterProfileID    string
	Mode                agent.PromptSessionMode
	ConfigRequirementID string
}

func (s ProtectedRuntimeMCPV2Selector) configured() bool {
	return s != (ProtectedRuntimeMCPV2Selector{})
}

func (s ProtectedRuntimeMCPV2Selector) realizationSelector() ProtectedRuntimeMCPSelector {
	return ProtectedRuntimeMCPSelector{
		CapabilityID: s.CapabilityID, HarnessID: s.HarnessID,
		AdapterProfileID: s.AdapterProfileID, Mode: s.Mode,
	}
}

func validateProtectedRuntimeMCPV2Selector(selector ProtectedRuntimeMCPV2Selector, realizations *agent.CapabilityRealizationRegistry) error {
	if !selector.configured() {
		return nil
	}
	if err := validateProtectedRuntimeMCPSelector(selector.realizationSelector(), realizations); err != nil {
		return err
	}
	probe := executioncell.ProtectedRuntimeMCPConfigRequirementV2{
		ContractVersion: executioncell.ProtectedRuntimeMCPConfigContractVersionV2,
		RequirementID:   protectedRuntimeMCPRequirementIDV2, AuthorityBindingDigest: strings.Repeat("0", 64),
		OperationalPayloadDigest: strings.Repeat("0", 64), ServerName: "protected-runtime-mcp",
		Transport: executioncell.ProtectedRuntimeMCPTransportHTTP, EndpointDigest: strings.Repeat("0", 64),
		Headers: []executioncell.ProtectedRuntimeMCPHeaderV1{{Name: "Authorization", ValueDigest: strings.Repeat("0", 64)}},
		AuthorizationSource: executioncell.ProtectedRuntimeMCPAuthorizationSourceV2{
			Kind: executioncell.PreflightConfigSourceSessionMCPBearerFile, ConfigRequirementID: selector.ConfigRequirementID,
			TargetEnv: executioncell.PreflightConfigSessionMCPBearerFileTarget, Mode: executioncell.PreflightConfigPrivateFileMode,
		},
	}
	if err := executioncell.ValidateProtectedRuntimeMCPConfigRequirementV2(probe); err != nil {
		return fmt.Errorf("runner: protected runtime MCP v2 selector config requirement: %w", err)
	}
	return nil
}

func (s ProtectedRuntimeMCPSelector) configured() bool {
	return s != (ProtectedRuntimeMCPSelector{})
}

func validateProtectedRuntimeMCPSelector(selector ProtectedRuntimeMCPSelector, realizations *agent.CapabilityRealizationRegistry) error {
	if !selector.configured() {
		return nil
	}
	if selector.CapabilityID == "" || selector.HarnessID == "" || selector.AdapterProfileID == "" || selector.Mode == "" {
		return fmt.Errorf("runner: protected runtime MCP realization selector is incomplete")
	}
	if realizations == nil {
		return fmt.Errorf("runner: protected runtime MCP realization registry is unavailable")
	}
	if _, ok := realizations.Resolve(selector.CapabilityID, selector.HarnessID, selector.AdapterProfileID, selector.Mode); !ok {
		return fmt.Errorf("runner: protected runtime MCP realization is not registered exactly")
	}
	return nil
}

func protectedRuntimeMCPTargetsSession(qw QueuedWork, selection harnessSelection, selector ProtectedRuntimeMCPSelector) bool {
	if !selector.configured() {
		return false
	}
	mode := sessionPromptMode(qw, selection.effectiveCell)
	return agent.HarnessName(selection.Harness.ID) == selector.HarnessID && mode == selector.Mode
}

func protectedRuntimeMCPApplies(qw QueuedWork, selection harnessSelection, selector ProtectedRuntimeMCPSelector) (bool, error) {
	if !protectedRuntimeMCPTargetsSession(qw, selection, selector) {
		return false, nil
	}
	mode := selector.Mode
	harness, ok := selection.Provider.(agent.HarnessProvider)
	if !ok {
		return false, fmt.Errorf("runner: protected runtime MCP target has no exact harness manifest")
	}
	manifest := harness.Manifest()
	if manifest.Name != selector.HarnessID {
		return false, fmt.Errorf("runner: protected runtime MCP target harness identity changed")
	}
	profile, ok := manifest.ToolLifecycleProfileByID(selector.AdapterProfileID, mode)
	if !ok || profile.ID != selector.AdapterProfileID {
		return false, fmt.Errorf("runner: protected runtime MCP target adapter profile changed")
	}
	return true, nil
}

func digestProtectedRuntimeMCPAuthority(binding agent.CapabilityRealizationBinding) (string, error) {
	raw, err := executioncell.CanonicalJSON(binding)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum[:]), nil
}

func resolveProtectedRuntimeMCPRequirement(
	qw QueuedWork,
	selection harnessSelection,
	realizations *agent.CapabilityRealizationRegistry,
	selector ProtectedRuntimeMCPSelector,
	host executioncell.HostAdaptationReceipt,
) (*executioncell.ProtectedRuntimeMCPConfigRequirementV1, error) {
	if !selector.configured() {
		return nil, nil
	}
	if err := validateProtectedRuntimeMCPSelector(selector, realizations); err != nil {
		return nil, err
	}
	if !protectedRuntimeMCPTargetsSession(qw, selection, selector) {
		return nil, nil
	}
	count := 0
	for _, capability := range selection.effectiveCell.GrantedCapabilities {
		if capability.Name == selector.CapabilityID {
			count++
		}
	}
	if count == 0 {
		return nil, nil
	}
	if count != 1 {
		return nil, fmt.Errorf("runner: protected runtime MCP capability %q must be granted exactly once", selector.CapabilityID)
	}
	applies, err := protectedRuntimeMCPApplies(qw, selection, selector)
	if err != nil {
		return nil, err
	}
	if !applies {
		return nil, nil
	}
	harness, ok := selection.Provider.(agent.HarnessProvider)
	if !ok {
		return nil, fmt.Errorf("runner: protected runtime MCP capability requires an exact harness manifest")
	}
	mode := selector.Mode
	manifest := harness.Manifest()
	profile, ok := manifest.ToolLifecycleProfileByID(selector.AdapterProfileID, mode)
	if !ok || profile.ID != selector.AdapterProfileID {
		return nil, fmt.Errorf("runner: protected runtime MCP target adapter profile changed")
	}
	compiled, ok := realizations.Resolve(selector.CapabilityID, selector.HarnessID, selector.AdapterProfileID, selector.Mode)
	if !ok {
		return nil, fmt.Errorf("runner: protected runtime MCP capability %q has no exact registered realization", selector.CapabilityID)
	}
	expectedBinding := agent.BindCapabilityRealization(compiled)
	var toolReceipt agent.ToolLifecycleReceipt
	if err := json.Unmarshal(host.ToolLifecycleReceipt, &toolReceipt); err != nil {
		return nil, fmt.Errorf("runner: decode protected runtime MCP tool receipt: %w", err)
	}
	matches := 0
	for _, result := range toolReceipt.CapabilityRealizations {
		if result.CapabilityID != selector.CapabilityID {
			continue
		}
		matches++
		if result.Decision != "artifact_bound" || !reflect.DeepEqual(result.CapabilityRealizationBinding, expectedBinding) {
			return nil, fmt.Errorf("runner: protected runtime MCP capability evidence does not match the registered realization")
		}
	}
	if matches != 1 {
		return nil, fmt.Errorf("runner: protected runtime MCP capability requires one artifact-bound receipt result")
	}
	server, err := protectedRuntimeMCPServer(qw, selection.Provider, mode)
	if err != nil {
		return nil, err
	}
	authorityDigest, err := digestProtectedRuntimeMCPAuthority(expectedBinding)
	if err != nil {
		return nil, err
	}
	endpoint := sha256.Sum256([]byte(server.URL))
	headers := make([]executioncell.ProtectedRuntimeMCPHeaderV1, 0, len(server.Headers))
	for name, value := range server.Headers {
		digest := sha256.Sum256([]byte(value))
		headers = append(headers, executioncell.ProtectedRuntimeMCPHeaderV1{Name: name, ValueDigest: fmt.Sprintf("%x", digest[:])})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].Name < headers[j].Name })
	requirement := executioncell.ProtectedRuntimeMCPConfigRequirementV1{
		ContractVersion: executioncell.ProtectedRuntimeMCPConfigContractVersion,
		RequirementID:   protectedRuntimeMCPRequirementID, AuthorityBindingDigest: authorityDigest,
		OperationalPayloadDigest: selection.receipt.Value().OperationalPayloadDigest,
		ServerName:               server.Name, Transport: server.Type, EndpointDigest: fmt.Sprintf("%x", endpoint[:]), Headers: headers,
	}
	if err := executioncell.ValidateProtectedRuntimeMCPConfigRequirement(requirement); err != nil {
		return nil, err
	}
	return &requirement, nil
}

func resolveProtectedRuntimeMCPRequirementV2(
	qw QueuedWork,
	selection harnessSelection,
	realizations *agent.CapabilityRealizationRegistry,
	selector ProtectedRuntimeMCPV2Selector,
	host executioncell.HostAdaptationReceipt,
) (*executioncell.ProtectedRuntimeMCPConfigRequirementV2, error) {
	base, err := resolveProtectedRuntimeMCPRequirement(qw, selection, realizations, selector.realizationSelector(), host)
	if err != nil || base == nil {
		return nil, err
	}
	harness, ok := selection.Provider.(agent.HarnessProvider)
	if !ok {
		return nil, errors.New("runner: protected runtime MCP v2 requires an exact harness manifest")
	}
	profile, ok := harness.Manifest().ToolLifecycleProfileByID(selector.AdapterProfileID, selector.Mode)
	if !ok || profile.EvidenceTier != "native_verified" || !profile.ProductionEligible {
		return nil, errors.New("runner: protected runtime MCP v2 requires a native-verified production profile")
	}
	requirement := executioncell.ProtectedRuntimeMCPConfigRequirementV2{
		ContractVersion:        executioncell.ProtectedRuntimeMCPConfigContractVersionV2,
		RequirementID:          protectedRuntimeMCPRequirementIDV2,
		AuthorityBindingDigest: base.AuthorityBindingDigest, OperationalPayloadDigest: base.OperationalPayloadDigest,
		ServerName: base.ServerName, Transport: base.Transport, EndpointDigest: base.EndpointDigest,
		Headers: append([]executioncell.ProtectedRuntimeMCPHeaderV1(nil), base.Headers...),
		AuthorizationSource: executioncell.ProtectedRuntimeMCPAuthorizationSourceV2{
			Kind:                executioncell.PreflightConfigSourceSessionMCPBearerFile,
			ConfigRequirementID: selector.ConfigRequirementID,
			TargetEnv:           executioncell.PreflightConfigSessionMCPBearerFileTarget,
			Mode:                executioncell.PreflightConfigPrivateFileMode,
		},
	}
	if err := executioncell.ValidateProtectedRuntimeMCPConfigRequirementV2(requirement); err != nil {
		return nil, err
	}
	return &requirement, nil
}

type hostAdaptationReceipt struct {
	ContractVersion string                       `json:"contractVersion"`
	RequestID       string                       `json:"requestId"`
	WorkerID        string                       `json:"workerId"`
	PlacementID     string                       `json:"placementId"`
	ClaimID         string                       `json:"claimId,omitempty"`
	Decision        string                       `json:"decision"`
	Plan            *agent.PreparedHarness       `json:"plan,omitempty"`
	PlanDigest      string                       `json:"planDigest,omitempty"`
	Prompt          *agent.PromptDeliveryReceipt `json:"promptReceipt,omitempty"`
	ToolLifecycle   *agent.ToolLifecycleReceipt  `json:"toolLifecycleReceipt,omitempty"`
	Denial          string                       `json:"denial,omitempty"`
}

type providerViewPreflightWire struct {
	SessionID               string          `json:"sessionId"`
	WorkerID                string          `json:"workerId"`
	PlatformURL             string          `json:"platformUrl"`
	MCPAuthToken            string          `json:"mcpAuthToken"`
	AdmissionReceipt        json.RawMessage `json:"admissionReceipt"`
	ClaimReceipt            json.RawMessage `json:"claimReceipt"`
	EffectiveCell           json.RawMessage `json:"effectiveCell"`
	ExecutionRuntimeBinding json.RawMessage `json:"executionRuntimeBinding"`
	OperationalPayload      json.RawMessage `json:"operationalPayload"`
	ModelProfile            json.RawMessage `json:"modelProfile"`
	ResolvedProfile         json.RawMessage `json:"resolvedProfile"`
	StageBudget             json.RawMessage `json:"stageBudget"`
}

func decodeProviderViewPreflightWork(detailJSON json.RawMessage) (QueuedWork, executioncell.RuntimeBinding, error) {
	var wire providerViewPreflightWire
	if err := json.Unmarshal(detailJSON, &wire); err != nil {
		return QueuedWork{}, executioncell.RuntimeBinding{}, err
	}
	var qw QueuedWork
	if err := json.Unmarshal(wire.OperationalPayload, &qw); err != nil {
		return QueuedWork{}, executioncell.RuntimeBinding{}, fmt.Errorf("decode host operational payload: %w", err)
	}
	qw.SessionID, qw.WorkerID = wire.SessionID, wire.WorkerID
	qw.PlatformURL, qw.McpAuthToken = wire.PlatformURL, wire.MCPAuthToken
	qw.AdmissionReceipt, qw.ClaimReceipt, qw.EffectiveCell = wire.AdmissionReceipt, wire.ClaimReceipt, wire.EffectiveCell
	qw.ExecutionRuntimeBinding, qw.OperationalPayload = wire.ExecutionRuntimeBinding, wire.OperationalPayload
	var err error
	qw, err = ReconcileResolvedProfile(qw, wire.ModelProfile, wire.ResolvedProfile)
	if err != nil {
		return QueuedWork{}, executioncell.RuntimeBinding{}, fmt.Errorf("decode host resolved profile: %w", err)
	}
	qw, err = ReconcileStageBudget(qw, wire.StageBudget)
	if err != nil {
		return QueuedWork{}, executioncell.RuntimeBinding{}, fmt.Errorf("decode host stage budget: %w", err)
	}
	binding, err := executioncell.DecodeRuntimeBinding(wire.ExecutionRuntimeBinding)
	if err != nil {
		return QueuedWork{}, executioncell.RuntimeBinding{}, err
	}
	return qw, binding, nil
}

// PreflightExecution is the host-process compiler used by daemon before its
// credential hook. It consumes the same raw operational projection and closed
// execution-cell contracts as the child runner.
func (v *ProviderView) PreflightExecution(detailJSON json.RawMessage) (json.RawMessage, error) {
	qw, binding, err := decodeProviderViewPreflightWork(detailJSON)
	if err != nil {
		return nil, err
	}
	qw = BindProtectedRuntimeMCPV2ProfileIntent(qw, v.protectedRuntimeMCPV2Selector)
	receipt := hostAdaptationReceipt{
		ContractVersion: executioncell.HostAdaptationContractVersion, RequestID: binding.RequestID,
		WorkerID: binding.WorkerID, PlacementID: binding.PlacementID,
		ClaimID: binding.ClaimID, Decision: "denied",
	}
	encode := func(cause error) (json.RawMessage, error) {
		if cause != nil {
			receipt.Denial = cause.Error()
		}
		raw, marshalErr := json.Marshal(receipt)
		if marshalErr != nil {
			return nil, marshalErr
		}
		return raw, cause
	}
	admission, err := v.reg.preflightAdmissionReceipt(qw, false, v.realizations)
	if err != nil {
		return encode(err)
	}
	if admission == nil || admission.selection.Provider == nil {
		return encode(fmt.Errorf("host adaptation requires explicit receipt admission"))
	}
	// repositoryDeclaration is forwarded into compilePreparedHarness so
	// ReconcileRepositorySandbox (runner/repository_sandbox_reconcile.go)
	// applies the identical repository-authority-driven sandbox mutation
	// the spawn lane applies (runner/loop.go) — mirroring how #482 forwarded
	// the sibling resolved profile via daemon.go's preflightInput. Before
	// this, the resolved declaration was computed here only to validate the
	// workarea contract and then discarded, so the host-compiled receipt's
	// SandboxEnabled/SandboxLevel never reflected it even though the spawn
	// lane's final Spec always did for a declared repository.
	repositoryDeclaration, _, err := resolveRepositoryWorkarea(qw, admission.selection.Provider)
	if err != nil {
		return encode(err)
	}
	plan, _, err := compilePreparedHarness(qw, admission.selection, repositoryDeclaration, v.decorate, v.preparedCapabilities)
	if plan != nil {
		receipt.Plan = plan
		receipt.PlanDigest = agent.DigestPreparedHarness(plan)
		receipt.Prompt = &plan.PromptReceipt
		receipt.ToolLifecycle = &plan.ToolLifecycleReceipt
	}
	if err != nil {
		return encode(err)
	}
	if err := agent.ValidatePreparedHarness(plan, admission.receipt.Value().OperationalPayloadDigest); err != nil {
		return encode(err)
	}
	receipt.Decision = "ready"
	return encode(nil)
}

// ResolveExecutionPreflightConfigRequirements invokes one process-registered,
// trusted resolver only after revalidating the exact admission, claim,
// effective cell, and compiled receipt. A nil resolver preserves host-v1.
func (v *ProviderView) ResolveExecutionPreflightConfigRequirements(detailJSON json.RawMessage, compiledReceipt json.RawMessage) ([]executioncell.PreflightConfigRequirementV1, error) {
	if v == nil || v.reg == nil || v.configRequirements == nil {
		return nil, nil
	}
	qw, binding, err := decodeProviderViewPreflightWork(detailJSON)
	if err != nil {
		return nil, err
	}
	qw = BindProtectedRuntimeMCPV2ProfileIntent(qw, v.protectedRuntimeMCPV2Selector)
	host, err := executioncell.DecodeHostAdaptationReceipt(compiledReceipt)
	if err != nil {
		return nil, err
	}
	if (host.ContractVersion != executioncell.HostAdaptationContractVersion && host.ContractVersion != executioncell.HostAdaptationV2ContractVersion && host.ContractVersion != executioncell.HostAdaptationV3ContractVersion) || host.Decision != "ready" {
		return nil, fmt.Errorf("runner: config requirements require a ready host adaptation receipt")
	}
	if err := v.ValidateRetainedExecution(detailJSON, compiledReceipt); err != nil {
		return nil, err
	}
	admission, err := v.reg.preflightAdmissionReceipt(qw, false, v.realizations)
	if err != nil || admission == nil {
		return nil, fmt.Errorf("runner: config requirements require exact admission: %w", err)
	}
	admitted := admission.receipt.Value()
	var operational struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(qw.OperationalPayload, &operational); err != nil {
		return nil, fmt.Errorf("runner: decode config requirement operational environment: %w", err)
	}
	environment := make(map[string]string, len(operational.Env))
	for name, value := range operational.Env {
		environment[name] = value
	}
	context := ExecutionPreflightConfigRequirementContext{
		SessionID: qw.SessionID, OperationalEnvironment: environment,
		OperationalPayloadDigest: admitted.OperationalPayloadDigest, RuntimeBinding: binding,
		AdmissionReceipt: admitted, EffectiveCell: admission.selection.effectiveCell, CompiledReceipt: host,
	}
	if claim := admission.selection.claimReceipt.Value(); claim.ContractVersion != "" {
		context.ClaimReceipt = &claim
	}
	resolved, err := v.configRequirements(context)
	if err != nil {
		return nil, err
	}
	for i := range resolved {
		if err := executioncell.ValidatePreflightConfigRequirement(resolved[i]); err != nil {
			return nil, err
		}
		if resolved[i].OperationalPayloadDigest != admitted.OperationalPayloadDigest {
			return nil, fmt.Errorf("runner: config requirement operational payload changed")
		}
		if i > 0 && resolved[i-1].RequirementID >= resolved[i].RequirementID {
			return nil, fmt.Errorf("runner: config requirements must be unique and sorted")
		}
	}
	return append([]executioncell.PreflightConfigRequirementV1(nil), resolved...), nil
}

func (v *ProviderView) protectedRuntimeMCPPreflightContext(detailJSON json.RawMessage, compiledReceipt json.RawMessage) (QueuedWork, harnessSelection, executioncell.HostAdaptationReceipt, error) {
	if v == nil || v.reg == nil {
		return QueuedWork{}, harnessSelection{}, executioncell.HostAdaptationReceipt{}, fmt.Errorf("runner: protected runtime MCP provider view is unavailable")
	}
	qw, _, err := decodeProviderViewPreflightWork(detailJSON)
	if err != nil {
		return QueuedWork{}, harnessSelection{}, executioncell.HostAdaptationReceipt{}, err
	}
	qw = BindProtectedRuntimeMCPV2ProfileIntent(qw, v.protectedRuntimeMCPV2Selector)
	host, err := executioncell.DecodeHostAdaptationReceipt(compiledReceipt)
	if err != nil {
		return QueuedWork{}, harnessSelection{}, executioncell.HostAdaptationReceipt{}, err
	}
	if host.Decision != "ready" {
		return QueuedWork{}, harnessSelection{}, executioncell.HostAdaptationReceipt{}, fmt.Errorf("runner: protected runtime MCP requirements require a ready host adaptation receipt")
	}
	if err := v.ValidateRetainedExecution(detailJSON, compiledReceipt); err != nil {
		return QueuedWork{}, harnessSelection{}, executioncell.HostAdaptationReceipt{}, err
	}
	admission, err := v.reg.preflightAdmissionReceipt(qw, false, v.realizations)
	if err != nil || admission == nil {
		return QueuedWork{}, harnessSelection{}, executioncell.HostAdaptationReceipt{}, fmt.Errorf("runner: protected runtime MCP requirements require exact admission: %w", err)
	}
	return qw, admission.selection, host, nil
}

// ResolveExecutionPreflightProtectedRuntimeMCPRequirements resolves the sole
// process-configured v1 realization only after exact admission and retained-plan
// validation. An unselected harness, mode, or capability returns no requirements.
func (v *ProviderView) ResolveExecutionPreflightProtectedRuntimeMCPRequirements(detailJSON json.RawMessage, compiledReceipt json.RawMessage) ([]executioncell.ProtectedRuntimeMCPConfigRequirementV1, error) {
	if v == nil || v.reg == nil || !v.protectedRuntimeMCPSelector.configured() {
		return nil, nil
	}
	qw, selection, host, err := v.protectedRuntimeMCPPreflightContext(detailJSON, compiledReceipt)
	if err != nil {
		return nil, err
	}
	requirement, err := resolveProtectedRuntimeMCPRequirement(qw, selection, v.realizations, v.protectedRuntimeMCPSelector, host)
	if err != nil {
		return nil, err
	}
	if requirement == nil {
		return nil, nil
	}
	return []executioncell.ProtectedRuntimeMCPConfigRequirementV1{*requirement}, nil
}

// ResolveExecutionPreflightProtectedRuntimeMCPRequirementsV2 resolves the sole
// process-configured v2 realization and its exact common-config requirement.
// The selector is not wired into any default or child runtime in this unit.
func (v *ProviderView) ResolveExecutionPreflightProtectedRuntimeMCPRequirementsV2(detailJSON json.RawMessage, compiledReceipt json.RawMessage) ([]executioncell.ProtectedRuntimeMCPConfigRequirementV2, error) {
	if v == nil || v.reg == nil || !v.protectedRuntimeMCPV2Selector.configured() {
		return nil, nil
	}
	qw, selection, host, err := v.protectedRuntimeMCPPreflightContext(detailJSON, compiledReceipt)
	if err != nil {
		return nil, err
	}
	requirement, err := resolveProtectedRuntimeMCPRequirementV2(qw, selection, v.realizations, v.protectedRuntimeMCPV2Selector, host)
	if err != nil {
		return nil, err
	}
	if requirement == nil {
		return nil, nil
	}
	return []executioncell.ProtectedRuntimeMCPConfigRequirementV2{*requirement}, nil
}

// ValidateRetainedExecution verifies current sibling mirrors against the exact
// retained plan through the same reconciliation, selection and preparation
// path used by fresh preflight. It never calls CompilePreparedHarness and never
// returns replacement receipt bytes.
func (v *ProviderView) ValidateRetainedExecution(detailJSON json.RawMessage, receipt json.RawMessage) error {
	if v == nil || v.reg == nil {
		return fmt.Errorf("runner: retained execution validator is unavailable")
	}
	qw, _, err := decodeProviderViewPreflightWork(detailJSON)
	if err != nil {
		return err
	}
	qw = BindProtectedRuntimeMCPV2ProfileIntent(qw, v.protectedRuntimeMCPV2Selector)
	qw.HostAdaptationReceipt = append(json.RawMessage(nil), receipt...)
	admission, err := v.reg.preflightAdmissionReceipt(qw, true, v.realizations)
	if err != nil {
		return err
	}
	if admission == nil || admission.selection.Provider == nil {
		return fmt.Errorf("runner: retained execution requires explicit receipt admission")
	}
	repositoryDeclaration, _, err := resolveRepositoryWorkarea(qw, admission.selection.Provider)
	if err != nil {
		return err
	}
	source, _, err := buildPreparedSourceSpec(qw, admission.selection, v.decorate, v.preparedCapabilities)
	if err != nil {
		return err
	}
	source = ReconcileRepositorySandbox(source, repositoryDeclaration)
	plan, err := preparedHarnessFromWork(qw)
	if err != nil {
		return err
	}
	source.PreparedHarness = plan
	harness, ok := admission.selection.Provider.(agent.HarnessProvider)
	if !ok {
		return fmt.Errorf("runner: selected provider has no exact harness manifest")
	}
	_, err = agent.ApplyPreparedHarness(source, harness.Manifest())
	return err
}

// ProviderViewOptions is the complete immutable construction surface.
type ProviderViewOptions struct {
	Decorator                     agent.ExtensionDecorator
	CapabilityRealizations        *agent.CapabilityRealizationRegistry
	CapabilityParameterBinders    *CapabilityParameterBinderRegistry
	ConfigRequirements            ExecutionPreflightConfigRequirementResolver
	ProtectedRuntimeMCPSelector   ProtectedRuntimeMCPSelector
	ProtectedRuntimeMCPV2Selector ProtectedRuntimeMCPV2Selector
}

// NewProviderViewWithOptions constructs a complete read-only provider view.
func NewProviderViewWithOptions(reg *Registry, opts ProviderViewOptions) (*ProviderView, error) {
	if opts.ProtectedRuntimeMCPSelector.configured() && opts.ProtectedRuntimeMCPV2Selector.configured() {
		return nil, fmt.Errorf("runner: protected runtime MCP selector version is ambiguous")
	}
	if err := validateProtectedRuntimeMCPSelector(opts.ProtectedRuntimeMCPSelector, opts.CapabilityRealizations); err != nil {
		return nil, err
	}
	if err := validateProtectedRuntimeMCPV2Selector(opts.ProtectedRuntimeMCPV2Selector, opts.CapabilityRealizations); err != nil {
		return nil, err
	}
	prepared, err := newPreparedCapabilityResolver(opts.CapabilityRealizations, opts.CapabilityParameterBinders)
	if err != nil {
		return nil, err
	}
	return &ProviderView{
		reg: reg, decorate: opts.Decorator, realizations: opts.CapabilityRealizations,
		preparedCapabilities: prepared, configRequirements: opts.ConfigRequirements,
		protectedRuntimeMCPSelector:   opts.ProtectedRuntimeMCPSelector,
		protectedRuntimeMCPV2Selector: opts.ProtectedRuntimeMCPV2Selector,
	}, nil
}

// NewProviderView returns the historical default provider view.
func NewProviderView(reg *Registry) *ProviderView {
	view, _ := NewProviderViewWithOptions(reg, ProviderViewOptions{})
	return view
}

// NewProviderViewWithDecorator is NewProviderView plus one more step: decorate
// is the SAME agent.ExtensionDecorator (afcli.Config.AgentSpecExtensionDecorator)
// an embedder registers for the `agent run` subcommand's own registry
// (afcli/agent_run.go's decorateRegistryProviders) — pass it here too so
// PreflightExecution's persisted plan and the real spawn agree on
// Spec.AdditionalExtensions (see ReconcileAdditionalExtensions). Any embedder
// that registers Config.AgentSpecExtensionDecorator for its own daemon-side
// registry construction must build its ProviderView through this
// constructor, not NewProviderView — this repo's own afcli/daemon_run.go
// (daemonProviderView) is the reference wiring. nil is a legitimate value,
// equivalent to NewProviderView.
func NewProviderViewWithDecorator(reg *Registry, decorate agent.ExtensionDecorator) *ProviderView {
	view, _ := NewProviderViewWithOptions(reg, ProviderViewOptions{Decorator: decorate})
	return view
}

// NewProviderViewWithDecoratorAndRealizations binds the same immutable realization snapshot used by child recomputation.
func NewProviderViewWithDecoratorAndRealizations(reg *Registry, decorate agent.ExtensionDecorator, realizations *agent.CapabilityRealizationRegistry) *ProviderView {
	view, _ := NewProviderViewWithOptions(reg, ProviderViewOptions{Decorator: decorate, CapabilityRealizations: realizations})
	return view
}

// NewProviderViewWithDecoratorRealizationsAndConfigRequirements adds the
// trusted common-config requirement resolver while preserving older constructors.
func NewProviderViewWithDecoratorRealizationsAndConfigRequirements(reg *Registry, decorate agent.ExtensionDecorator, realizations *agent.CapabilityRealizationRegistry, resolver ExecutionPreflightConfigRequirementResolver) *ProviderView {
	view, _ := NewProviderViewWithOptions(reg, ProviderViewOptions{Decorator: decorate, CapabilityRealizations: realizations, ConfigRequirements: resolver})
	return view
}

// NewProviderViewWithProtectedRuntimeMCP adds one immutable process-owned
// exact-realization selector to the complete preflight view. The child Runner
// must be built with the same selector and realization registry.
func NewProviderViewWithProtectedRuntimeMCP(
	reg *Registry,
	decorate agent.ExtensionDecorator,
	realizations *agent.CapabilityRealizationRegistry,
	resolver ExecutionPreflightConfigRequirementResolver,
	selector ProtectedRuntimeMCPSelector,
) (*ProviderView, error) {
	return NewProviderViewWithOptions(reg, ProviderViewOptions{Decorator: decorate, CapabilityRealizations: realizations, ConfigRequirements: resolver, ProtectedRuntimeMCPSelector: selector})
}

// Names returns the sorted list of registered provider names as plain
// strings (the daemon.ProviderRegistry contract). The underlying
// Registry.Names() returns []agent.ProviderName which is just a typed
// alias; we widen the wire shape here.
func (v *ProviderView) Names() []string {
	if v == nil || v.reg == nil {
		return nil
	}
	src := v.reg.Names()
	out := make([]string, len(src))
	for i, n := range src {
		out[i] = string(n)
	}
	return out
}

// Capabilities returns the typed capability struct serialised to a
// flat map[string]any for the named provider, or (nil, false) when the
// provider is not registered. The map shape matches the JSON encoding
// of agent.Capabilities so the wire shape on /api/daemon/providers
// satisfies the contract in afclient/provider_types.go.
func (v *ProviderView) Capabilities(name string) (map[string]any, bool) {
	if v == nil || v.reg == nil {
		return nil, false
	}
	p, err := v.reg.Resolve(agent.ProviderName(name))
	if err != nil {
		return nil, false
	}
	caps := p.Capabilities()
	// Round-trip through json so the keys we expose match the JSON tags
	// on agent.Capabilities exactly. This decouples the wire shape from
	// any future refactor of the Go field names.
	data, err := json.Marshal(caps)
	if err != nil {
		return map[string]any{}, true
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{}, true
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, true
}

// WorkareaExecutorCapabilities returns exact harness-scoped attestations for
// registration. Only positive protocol declarations are emitted; zero-value
// harnesses remain registrable for the legacy singular path without being
// mistaken for session-root-v1 executors.
func (v *ProviderView) WorkareaExecutorCapabilities() []workarea.ExecutorCapabilityAttestation {
	if v == nil || v.reg == nil {
		return nil
	}
	var attestations []workarea.ExecutorCapabilityAttestation
	for _, name := range v.reg.Names() {
		provider, err := v.reg.Resolve(name)
		if err != nil {
			continue
		}
		harness, ok := provider.(agent.HarnessProvider)
		if !ok {
			continue
		}
		manifest := harness.Manifest()
		if len(manifest.Caps.MultiRepositoryWorkareaProtocols) == 0 {
			continue
		}
		protocols := make([]workarea.Protocol, 0, len(manifest.Caps.MultiRepositoryWorkareaProtocols))
		for _, protocol := range manifest.Caps.MultiRepositoryWorkareaProtocols {
			protocols = append(protocols, workarea.Protocol(protocol))
		}
		modeSet := make(map[string]struct{}, len(manifest.PromptDelivery))
		for _, profile := range manifest.PromptDelivery {
			if profile.Mode != "" {
				modeSet[string(profile.Mode)] = struct{}{}
			}
		}
		modes := make([]string, 0, len(modeSet))
		for mode := range modeSet {
			modes = append(modes, mode)
		}
		sort.Strings(modes)
		manifestBytes, err := json.Marshal(manifest)
		if err != nil {
			continue
		}
		manifestDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(manifestBytes))
		attestations = append(attestations, workarea.ExecutorCapabilityAttestation{
			HarnessID: string(manifest.Name), AdapterVersion: manifest.ContractABI,
			ManifestDigest: manifestDigest, SessionModes: modes,
			SupportsReadOnlySelectedCWD: manifest.Caps.SupportsReadOnlySelectedCWD,
			ExecutorWorkareaCapabilities: workarea.ExecutorWorkareaCapabilities{
				MultiRepositoryWorkareaProtocols: protocols,
				RepositoryAuthorityEnforcement:   workarea.RepositoryAuthorityEnforcement(manifest.Caps.RepositoryAuthorityEnforcement),
			},
		})
	}
	sort.Slice(attestations, func(i, j int) bool {
		if attestations[i].HarnessID != attestations[j].HarnessID {
			return attestations[i].HarnessID < attestations[j].HarnessID
		}
		return attestations[i].AdapterVersion < attestations[j].AdapterVersion
	})
	return attestations
}
