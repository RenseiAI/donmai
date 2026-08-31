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
	"fmt"
	"sort"

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
	decorate agent.ExtensionDecorator
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

// PreflightExecution is the host-process compiler used by daemon before its
// credential hook. It consumes the same raw operational projection and closed
// execution-cell contracts as the child runner.
func (v *ProviderView) PreflightExecution(detailJSON json.RawMessage) (json.RawMessage, error) {
	var wire struct {
		SessionID               string          `json:"sessionId"`
		WorkerID                string          `json:"workerId"`
		AdmissionReceipt        json.RawMessage `json:"admissionReceipt"`
		ClaimReceipt            json.RawMessage `json:"claimReceipt"`
		EffectiveCell           json.RawMessage `json:"effectiveCell"`
		ExecutionRuntimeBinding json.RawMessage `json:"executionRuntimeBinding"`
		OperationalPayload      json.RawMessage `json:"operationalPayload"`
		// ModelProfile and ResolvedProfile are the daemon SessionDetail's
		// sibling profile fields (never embedded in OperationalPayload — see
		// daemon.go's preflightInput). Reconciled into qw.ResolvedProfile
		// below via the SAME logic the spawned child applies
		// (afcli.detailToQueuedWork → runner.ReconcileResolvedProfile), so
		// the plan this function compiles is over the identical Model/
		// Effort/ProviderConfig/Endpoint the child will materialize.
		ModelProfile    json.RawMessage `json:"modelProfile"`
		ResolvedProfile json.RawMessage `json:"resolvedProfile"`
	}
	if err := json.Unmarshal(detailJSON, &wire); err != nil {
		return nil, err
	}
	var qw QueuedWork
	if err := json.Unmarshal(wire.OperationalPayload, &qw); err != nil {
		return nil, fmt.Errorf("decode host operational payload: %w", err)
	}
	qw.SessionID, qw.WorkerID = wire.SessionID, wire.WorkerID
	qw.AdmissionReceipt, qw.ClaimReceipt, qw.EffectiveCell = wire.AdmissionReceipt, wire.ClaimReceipt, wire.EffectiveCell
	qw.ExecutionRuntimeBinding, qw.OperationalPayload = wire.ExecutionRuntimeBinding, wire.OperationalPayload
	qw, err := ReconcileResolvedProfile(qw, wire.ModelProfile, wire.ResolvedProfile)
	if err != nil {
		return nil, fmt.Errorf("decode host resolved profile: %w", err)
	}
	binding, err := executioncell.DecodeRuntimeBinding(wire.ExecutionRuntimeBinding)
	if err != nil {
		return nil, err
	}
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
	admission, err := v.reg.preflightAdmissionReceipt(qw, false)
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
	plan, _, err := compilePreparedHarness(qw, admission.selection, repositoryDeclaration, v.decorate)
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

// NewProviderView returns a ProviderView backed by reg, with no additional-
// extension decorator applied — the historical, source-compatible
// constructor. Pass the result to daemon.Options.ProviderRegistry to expose
// the runner's registered AgentRuntime providers via the daemon's HTTP
// control API.
//
// This signature is part of the OSS embed surface and stays exactly as it
// was: an embedder building a ProviderView with no
// Config.AgentSpecExtensionDecorator registered keeps compiling unchanged.
// An embedder that DOES register a decorator must call
// NewProviderViewWithDecorator instead — see its doc comment for why.
func NewProviderView(reg *Registry) *ProviderView {
	return &ProviderView{reg: reg}
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
	return &ProviderView{reg: reg, decorate: decorate}
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
