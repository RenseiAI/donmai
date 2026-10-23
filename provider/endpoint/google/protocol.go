package google

import (
	"context"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/endpoint/internal/resolve"
)

var _ agent.ProtocolEndpointResolver = (*Endpoint)(nil)

// ProtocolManifest projects the active wire surfaces from the existing
// declaration. The retired OAuth CLI tuple remains in the byte-compatible v1
// manifest only; it is not a supported protocol in the qualified view.
func (e *Endpoint) ProtocolManifest() agent.ModelEndpointManifest {
	manifest := e.Manifest()
	manifest.ContractABI = "model-endpoint/v2"
	hosts := make([]agent.HostDesc, 0, len(manifest.Hosts)-1)
	for _, host := range manifest.Hosts {
		if host.Host == agent.HostOAuthCLI && host.Protocol == agent.ProtoAntigravityOAuth {
			continue
		}
		hosts = append(hosts, host)
	}
	manifest.Hosts = hosts
	return manifest
}

// ResolveProtocol resolves only an active, explicitly declared tuple.
func (e *Endpoint) ResolveProtocol(_ context.Context, req agent.EndpointRequest, protocol agent.WireProtocol) (agent.EndpointBinding, error) {
	return resolve.FromProtocolManifest(e.ProtocolManifest(), req, protocol)
}
