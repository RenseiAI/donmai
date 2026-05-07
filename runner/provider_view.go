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

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// ProviderView wraps a *Registry and satisfies daemon.ProviderRegistry.
// Construct via NewProviderView. Read-only and safe for concurrent use.
type ProviderView struct {
	reg *Registry
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
