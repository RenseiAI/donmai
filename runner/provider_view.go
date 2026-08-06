// Package runner provider_view.go — adapter that exposes the in-process
// AgentRuntime registry as the read-only daemon.ProviderRegistry view
// consumed by the /api/daemon/providers* HTTP handler. Wave 9 / A1.
//
// The adapter lives in the runner package (not daemon) so daemon stays
// free of a runner import — daemon.ProviderRegistry is the interface,
// runner.NewProviderView builds the concrete view from a *Registry.
package runner

import (
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
)

// ProviderView wraps a *Registry and satisfies daemon.ProviderRegistry.
// Construct via NewProviderView. Read-only and safe for concurrent use.
type ProviderView struct {
	reg *Registry
}

type hostAdaptationReceipt struct {
	ContractVersion string                       `json:"contractVersion"`
	RequestID       string                       `json:"requestId"`
	WorkerID        string                       `json:"workerId"`
	PlacementID     string                       `json:"placementId"`
	ClaimID         string                       `json:"claimId,omitempty"`
	Decision        string                       `json:"decision"`
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
	harness, ok := admission.selection.Provider.(agent.HarnessProvider)
	if !ok {
		return encode(fmt.Errorf("selected provider has no closed harness manifest"))
	}
	spec := translateSpec(qw, admission.selection.Provider.Capabilities(), SpecInputs{
		Autonomous: true, MCPServers: qw.McpServers,
		ProviderName: string(admission.selection.Provider.Name()),
	})
	spec, err = bindAdmissionToolLifecyclePlan(spec, admission.receipt, admission.selection.claimReceipt)
	if err != nil {
		return encode(err)
	}
	spec.OnPromptAdapted = func(value agent.PromptDeliveryReceipt) error {
		snapshot := value
		receipt.Prompt = &snapshot
		return nil
	}
	spec.OnToolLifecycleAdapted = func(value agent.ToolLifecycleReceipt) error {
		snapshot := value
		receipt.ToolLifecycle = &snapshot
		return nil
	}
	if _, err = agent.PrepareHarness(spec, harness.Manifest()); err != nil {
		return encode(err)
	}
	if receipt.Prompt == nil || receipt.Prompt.Decision != "ready" || receipt.ToolLifecycle == nil || receipt.ToolLifecycle.Decision != "ready" {
		return encode(fmt.Errorf("host adaptation did not produce complete ready receipts"))
	}
	receipt.Decision = "ready"
	return encode(nil)
}

// NewProviderView returns a ProviderView backed by reg. Pass the result
// to daemon.Options.ProviderRegistry to expose the runner's registered
// AgentRuntime providers via the daemon's HTTP control API.
func NewProviderView(reg *Registry) *ProviderView {
	return &ProviderView{reg: reg}
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
