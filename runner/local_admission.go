package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/matrix"
	"github.com/RenseiAI/donmai/provider/endpoint/anthropic"
	"github.com/RenseiAI/donmai/provider/endpoint/openai"
)

// LocalModelCatalogEntry is an explicit operator-owned catalog declaration.
// It is never synthesized as fallback from a rejected built-in model. This
// declaration describes a route, not online entitlement to use the model.
type LocalModelCatalogEntry struct {
	Model    executioncell.ModelRef `json:"model"`
	Host     agent.ServingHost      `json:"host"`
	Revision string                 `json:"revision"`
}

// LocalAdmissionConfig belongs to the trusted local controller, never issue
// text. The controller must construct the registered provider with BinaryPath
// and validate its actual host-login reference in CheckHostAuth. The callback
// must neither deliver credentials nor start the harness. No credential values
// belong in this configuration or its evidence.
type LocalAdmissionConfig struct {
	ScopeID               string
	WorkerID              string
	PlacementID           string
	ConfigurationRevision string
	Harness               agent.HarnessName
	Model                 executioncell.ModelRef
	ModelCatalogEntry     *LocalModelCatalogEntry
	EndpointID            string
	AuthBindingID         string
	BinaryPath            string
	CheckHostAuth         func(context.Context) error
}

// LocalAdmissionRequest supplies one authored headless development payload.
// All execution identity, endpoint, credentials and receipt sidecars are owned
// by the controller and must be absent from Work. IDs come from the local
// authority, not the tracker. The result is evidence, not start permission.
type LocalAdmissionRequest struct {
	SessionID            string
	ReceiptID            string
	PreflightChallengeID string
	Work                 QueuedWork
}

// LocalAdmission is a secret-free candidate for atomic local-store admission.
// Persist the session, intent, receipt, lifecycle event and dispatch outbox in
// one transaction before invoking Daemon.AcceptWorkWithDetail. Its host compiler
// and durable start-registration gates must run again at dispatch.
type LocalAdmission struct {
	ScopeID            string
	Session            executioncell.SessionRef
	Intent             executioncell.DispatchIntent
	Receipt            json.RawMessage
	EffectiveCell      json.RawMessage
	RuntimeBinding     json.RawMessage
	OperationalPayload json.RawMessage
	HostPreflight      json.RawMessage
	// Evidence contains the actual canonical config/catalog/runtime observations
	// from which the receipt digests were derived. No executable path or secret.
	Evidence json.RawMessage
}

// LocalAdmissionProducer admits only autonomous development on the exact local
// host using an explicitly selected Claude Code or Codex host-login route.
// It grants no optional harness or session lifecycle capabilities. Further
// modes/capabilities require a separate producer policy with measured adapters.
type LocalAdmissionProducer struct {
	registry *Registry
	config   LocalAdmissionConfig
}

// NewLocalAdmissionProducer freezes controller configuration. It never reads
// ambient environment or creates a process, session, or workarea.
func NewLocalAdmissionProducer(registry *Registry, config LocalAdmissionConfig) (*LocalAdmissionProducer, error) {
	if registry == nil || config.CheckHostAuth == nil || !filepath.IsAbs(config.BinaryPath) {
		return nil, errors.New("local admission requires registry, runtime binary and host-auth checker")
	}
	if !isLocalRuntimeTransport(registry.runtimeTransport) {
		return nil, errors.New("local admission requires explicit local runtime transport")
	}
	for _, value := range []string{config.ScopeID, config.WorkerID, config.PlacementID, config.ConfigurationRevision, config.EndpointID, config.AuthBindingID, config.Model.ID, config.Model.Author} {
		if !localAdmissionRef(value) {
			return nil, errors.New("local admission has an invalid configured reference")
		}
	}
	if config.Harness != agent.HarnessCodex && config.Harness != agent.HarnessClaudeCode {
		return nil, errors.New("local admission supports only explicitly selected Codex or Claude Code")
	}
	if config.ModelCatalogEntry != nil {
		copyEntry := *config.ModelCatalogEntry
		config.ModelCatalogEntry = &copyEntry
	}
	return &LocalAdmissionProducer{registry: registry, config: config}, nil
}

func localAdmissionRef(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.:/", r)) {
			return false
		}
	}
	return true
}

// Admit compiles evidence using current registered manifests, published matrix
// compatibility, executable bytes, host-auth availability and native host
// preflight. Online authentication/entitlement is deliberately not claimed.
func (p *LocalAdmissionProducer) Admit(ctx context.Context, request LocalAdmissionRequest) (*LocalAdmission, error) {
	if p == nil || p.registry == nil || p.config.CheckHostAuth == nil {
		return nil, errors.New("local admission producer is not configured")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, value := range []string{request.SessionID, request.ReceiptID, request.PreflightChallengeID} {
		if !localAdmissionRef(value) {
			return nil, errors.New("local admission has an invalid request reference")
		}
	}
	if err := p.registry.validateRuntimeTransport(request.Work); err != nil {
		return nil, err
	}
	if err := validateLocalAdmissionWork(request.Work); err != nil {
		return nil, err
	}
	selection, err := p.registry.selectExplicitHarness(ResolvedProfile{Harness: string(p.config.Harness)})
	if err != nil {
		return nil, err
	}
	manifest := selection.Provider.(agent.HarnessProvider).Manifest()
	endpoint, catalog, err := p.resolveEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	compatibility, err := localAdmissionCompatibility(manifest, endpoint)
	if err != nil {
		return nil, err
	}
	binaryDigest, err := localAdmissionBinaryDigest(p.config.BinaryPath)
	if err != nil {
		return nil, err
	}
	if err = p.config.CheckHostAuth(ctx); err != nil {
		return nil, fmt.Errorf("local admission host authentication reference unavailable: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	configuration := struct {
		ScopeID, WorkerID, PlacementID, Revision, EndpointID, AuthBindingID string
		Harness                                                             agent.HarnessName
		Model                                                               executioncell.ModelRef
		Catalog                                                             any
		RuntimeTransportMode                                                RuntimeTransportMode
	}{p.config.ScopeID, p.config.WorkerID, p.config.PlacementID, p.config.ConfigurationRevision, p.config.EndpointID, p.config.AuthBindingID, p.config.Harness, p.config.Model, catalog, p.registry.runtimeTransport}
	configDigest, err := executioncell.DigestContractValue(configuration)
	if err != nil {
		return nil, err
	}
	endpoint.EndpointRevision = configDigest
	inventory := struct {
		Manifest             agent.HarnessManifest `json:"manifest"`
		BinarySHA256         string                `json:"binarySha256"`
		ConfigurationDigest  string                `json:"configurationDigest"`
		AuthReferencePresent bool                  `json:"authReferencePresent"`
	}{manifest, binaryDigest, configDigest, true}
	inventoryDigest, err := executioncell.DigestContractValue(inventory)
	if err != nil {
		return nil, err
	}
	compatibilityDigest, err := executioncell.DigestContractValue(compatibility)
	if err != nil {
		return nil, err
	}
	cell := executioncell.ResolvedExecutionCell{
		ContractVersion: executioncell.ContractVersion, Harness: selection.Harness, Model: p.config.Model,
		Endpoint:    executioncell.ServingEndpointRef{ID: endpoint.EndpointID, Protocol: string(endpoint.Protocol), Operator: endpoint.EndpointOperator, Revision: endpoint.EndpointRevision},
		AuthBinding: executioncell.AuthBindingRef{ID: endpoint.AuthBindingID, Mechanism: endpoint.Mechanism, CommercialMode: executioncell.CommercialSubscription, Authority: endpoint.AuthAuthority, BindingScope: executioncell.ScopeHost, Portability: executioncell.HostBound, Delivery: executioncell.DeliveryHostCLIHomeReference},
		Placement:   executioncell.PlacementRef{ID: p.config.PlacementID, Kind: executioncell.PlacementHost, Resolution: executioncell.PlacementExact}, SessionMode: executioncell.SessionAutonomous,
		GrantedCapabilities: []executioncell.CapabilityRequirement{}, EvidenceTier: executioncell.EvidenceImplemented, CompatibilityDigest: compatibilityDigest, RuntimeInventoryDigest: inventoryDigest,
	}
	intent := executioncell.DispatchIntent{ContractVersion: executioncell.ContractVersion, RequestID: request.SessionID, Harness: &cell.Harness, Model: cell.Model, Endpoint: &cell.Endpoint, AuthBinding: &cell.AuthBinding, Placement: &cell.Placement, SessionMode: cell.SessionMode, RequiredCapabilities: []executioncell.CapabilityRequirement{}, OptionalCapabilities: []executioncell.CapabilityRequirement{}, FallbackAlternatives: executioncell.FallbackPolicy{}}
	selectors := &executioncell.ExecutionSelectorRegistry{HarnessVersions: map[string][]string{cell.Harness.ID: {cell.Harness.Version}}, Models: []string{cell.Model.Author + "/" + cell.Model.ID}, Endpoints: []string{cell.Endpoint.ID}, AuthBindings: []string{cell.AuthBinding.ID}, Placements: []string{cell.Placement.ID}, Capabilities: []string{}}
	intentRaw, err := json.Marshal(intent)
	if err != nil {
		return nil, err
	}
	intent, err = executioncell.DecodeDispatchIntent(intentRaw, selectors)
	if err != nil {
		return nil, err
	}
	work := request.Work
	work.SessionID = request.SessionID
	work.OrganizationID = p.config.ScopeID
	work.WorkerID = p.config.WorkerID
	work.ResolvedProfile = ResolvedProfile{Harness: cell.Harness.ID, Model: cell.Model.ID, Endpoint: &endpoint}
	payload, err := CanonicalOperationalPayload(work)
	if err != nil {
		return nil, err
	}
	payloadDigest, err := executioncell.DigestOperationalPayload(payload)
	if err != nil {
		return nil, err
	}
	intentDigest, err := executioncell.DigestContractValue(intent)
	if err != nil {
		return nil, err
	}
	decisions := []executioncell.ResolverDecision{}
	for _, axis := range []struct{ field, ref string }{{"harness", "harness:" + cell.Harness.ID + "@" + cell.Harness.Version}, {"model", "model:" + cell.Model.Author + "/" + cell.Model.ID}, {"endpoint", "endpoint:" + cell.Endpoint.ID}, {"authBinding", "auth-binding:" + cell.AuthBinding.ID}, {"placement", "placement:" + cell.Placement.ID}} {
		decisions = append(decisions, executioncell.ResolverDecision{Kind: executioncell.DecisionExplicit, Field: axis.field, SelectedRef: axis.ref, SourceRef: "local-config:" + configDigest, Reason: "Explicit local operator configuration; no fallback or legacy import."})
	}
	receipt := executioncell.AdmissionReceipt{ContractVersion: executioncell.ContractVersion, ReceiptID: request.ReceiptID, RequestID: request.SessionID, Decision: executioncell.AdmissionAdmitted, IntentDigest: intentDigest, OperationalPayloadDigest: payloadDigest, Cell: &cell, ResolverDecisions: decisions, RecordedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	receiptRaw, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	immutable, err := executioncell.DecodeAdmissionReceipt(receiptRaw)
	if err != nil {
		return nil, err
	}
	if err = executioncell.AssertAdmissionProvenance(intent, immutable); err != nil {
		return nil, err
	}
	effective, err := executioncell.CanonicalJSON(cell)
	if err != nil {
		return nil, err
	}
	binding := executioncell.RuntimeBinding{ContractVersion: executioncell.RuntimeBindingV2ContractVersion, RequestID: request.SessionID, WorkerID: p.config.WorkerID, PlacementID: p.config.PlacementID, PreflightRegistration: &executioncell.PreflightRegistrationRef{ContractVersion: executioncell.PreflightRegistrationContractVersion, Required: true, ChallengeID: request.PreflightChallengeID}}
	bindingRaw, err := json.Marshal(binding)
	if err != nil {
		return nil, err
	}
	if _, err = executioncell.DecodeRuntimeBinding(bindingRaw); err != nil {
		return nil, err
	}
	detail := map[string]any{"sessionId": request.SessionID, "workerId": p.config.WorkerID, "admissionReceipt": json.RawMessage(immutable.Bytes()), "effectiveCell": json.RawMessage(effective), "executionRuntimeBinding": json.RawMessage(bindingRaw), "operationalPayload": json.RawMessage(payload), "resolvedProfile": work.ResolvedProfile}
	detailRaw, err := json.Marshal(detail)
	if err != nil {
		return nil, err
	}
	host, err := NewProviderView(p.registry).PreflightExecution(detailRaw)
	if err != nil {
		return nil, fmt.Errorf("local admission host preflight: %w", err)
	}
	work.OperationalPayload = payload
	work.AdmissionReceipt = immutable.Bytes()
	work.EffectiveCell = effective
	work.ExecutionRuntimeBinding = bindingRaw
	work.HostAdaptationReceipt = host
	if _, err = p.registry.PreflightHarness(work); err != nil {
		return nil, fmt.Errorf("local admission child preflight: %w", err)
	}
	session := executioncell.SessionRef{ContractVersion: executioncell.ContractVersion, SessionID: request.SessionID, AdmissionReceiptID: request.ReceiptID, Mode: executioncell.SessionAutonomous}
	sessionRaw, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}
	if _, err = executioncell.DecodeSessionRef(sessionRaw); err != nil {
		return nil, err
	}
	evidence, err := executioncell.CanonicalJSON(map[string]any{"configuration": configuration, "inventory": inventory, "compatibility": compatibility})
	if err != nil {
		return nil, err
	}
	return &LocalAdmission{ScopeID: p.config.ScopeID, Session: session, Intent: intent, Receipt: immutable.Bytes(), EffectiveCell: effective, RuntimeBinding: bindingRaw, OperationalPayload: payload, HostPreflight: host, Evidence: evidence}, nil
}

func validateLocalAdmissionWork(work QueuedWork) error {
	if strings.TrimSpace(work.Body) == "" && strings.TrimSpace(work.PromptContext) == "" && strings.TrimSpace(work.IssueIdentifier) == "" {
		return errors.New("local admission requires authored issue context")
	}
	if work.Mode != "" || work.WorkType != "development" || work.TerminalWorkareaLease != nil {
		return errors.New("local admission requires unleased headless development")
	}
	if work.SessionID != "" || work.OrganizationID != "" || work.WorkerID != "" || work.AuthToken != "" || work.McpAuthToken != "" || work.PlatformURL != "" || len(work.Env) > 0 || len(work.Capabilities) > 0 {
		return errors.New("local admission work cannot select authority, credentials or runtime capabilities")
	}
	if len(work.AdmissionReceipt) > 0 || len(work.ClaimReceipt) > 0 || len(work.EffectiveCell) > 0 || len(work.ExecutionRuntimeBinding) > 0 || len(work.HostAdaptationReceipt) > 0 || len(work.OperationalPayload) > 0 {
		return errors.New("local admission work cannot supply execution evidence")
	}
	profile, err := json.Marshal(work.ResolvedProfile)
	if err != nil {
		return err
	}
	if string(profile) != "{}" {
		return errors.New("local admission work cannot select an execution profile")
	}
	return nil
}

func (p *LocalAdmissionProducer) resolveEndpoint(ctx context.Context) (agent.EndpointBinding, any, error) {
	var provider agent.ModelEndpointProvider
	switch p.config.Harness {
	case agent.HarnessCodex:
		provider = openai.New()
	case agent.HarnessClaudeCode:
		provider = anthropic.New()
	}
	manifest := provider.Manifest()
	if p.config.Model.Author != string(manifest.Company) {
		return agent.EndpointBinding{}, nil, errors.New("local admission model author differs from configured first-party endpoint")
	}
	var catalog any
	if entry := p.config.ModelCatalogEntry; entry != nil {
		if entry.Model != p.config.Model || entry.Host != agent.HostOAuthCLI || !localAdmissionRef(entry.Revision) {
			return agent.EndpointBinding{}, nil, errors.New("local admission operator model catalog entry does not match explicit route")
		}
		catalog = struct {
			Kind  string
			Entry LocalModelCatalogEntry
		}{"operator", *entry}
	} else {
		for _, model := range manifest.Models {
			if model.ID == p.config.Model.ID && slices.Contains(model.Hosts, agent.HostOAuthCLI) {
				catalog = struct {
					Kind            string
					Model           agent.ModelDesc
					ManifestVersion string
				}{"builtin", model, manifest.ContractABI}
				break
			}
		}
		if catalog == nil {
			return agent.EndpointBinding{}, nil, errors.New("local admission model-host pair is absent from built-in catalog; explicit operator catalog required")
		}
	}
	binding, err := provider.Resolve(ctx, agent.EndpointRequest{Model: p.config.Model.ID, Host: agent.HostOAuthCLI, Mechanism: agent.AuthCLISession, Auth: agent.AuthHostSession})
	if err != nil {
		return agent.EndpointBinding{}, nil, err
	}
	binding.EndpointID = p.config.EndpointID
	binding.EndpointOperator = string(manifest.Company)
	binding.ModelAuthor = p.config.Model.Author
	binding.AuthBindingID = p.config.AuthBindingID
	binding.AuthAuthority = string(manifest.Company)
	binding.AuthCommercialMode = string(executioncell.CommercialSubscription)
	binding.AuthBindingScope = string(executioncell.ScopeHost)
	binding.AuthPortability = string(executioncell.HostBound)
	binding.AuthDelivery = string(executioncell.DeliveryHostCLIHomeReference)
	return binding, catalog, nil
}

func localAdmissionCompatibility(manifest agent.HarnessManifest, endpoint agent.EndpointBinding) (any, error) {
	for _, cell := range matrix.ValidCells() {
		if cell.Harness == manifest.Name && cell.Endpoint == endpoint.Company && cell.Host == endpoint.Host && cell.Protocol == endpoint.Protocol && cell.OneShot && slices.Contains(cell.AuthModes, endpoint.Auth) && containsWireProtocol(manifest.Caps.Drives, endpoint.Protocol) && containsServingHost(manifest.Caps.DrivesHosts, endpoint.Host) {
			return struct {
				Cell     matrix.HarnessEndpointCell
				Manifest agent.HarnessManifest
			}{cell, manifest}, nil
		}
	}
	return nil, errors.New("local admission has no published headless harness/endpoint/host/auth compatibility cell")
}

func localAdmissionBinaryDigest(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("local admission runtime binary unavailable: %w", err)
	}
	root, err := os.OpenRoot(filepath.Dir(resolved))
	if err != nil {
		return "", fmt.Errorf("local admission runtime directory unavailable: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(resolved))
	if err != nil {
		return "", fmt.Errorf("local admission runtime binary unavailable: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("local admission runtime binary is not a regular executable")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("local admission runtime inventory: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
