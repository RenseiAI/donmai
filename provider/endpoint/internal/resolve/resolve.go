// Package resolve holds the shared, pure (no-network) resolution helper used
// by every company model-endpoint package (provider/endpoint/<company>). It is
// module-private (under internal/) and OSS-safe: it copies env-var VALUES only
// when the caller already provided them in EndpointRequest.EnvProvided (it
// never reads process env), and it never logs values.
//
// Keeping this in one place keeps the five endpoints' Resolve() implementations
// byte-identical in behavior — the only per-company input is the manifest.
package resolve

import (
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// FromManifest constructs the resolved EndpointBinding for req.Host without
// dialing. It looks up the requested host in m.Hosts, templates the base URL
// with req.Region (also recorded on the binding for harness env/path
// mapping), copies the host's declared env keys from req.EnvProvided, and
// returns the binding. Returns an error for an unknown host.
func FromManifest(m agent.ModelEndpointManifest, req agent.EndpointRequest) (agent.EndpointBinding, error) {
	var host *agent.HostDesc
	for i := range m.Hosts {
		if m.Hosts[i].Host == req.Host {
			host = &m.Hosts[i]
			break
		}
	}
	if host == nil {
		return agent.EndpointBinding{}, fmt.Errorf("%s endpoint: unknown serving host %q", m.Company, req.Host)
	}
	return fromHost(m.Company, *host, req), nil
}

// FromProtocolManifest selects exactly one declared host/protocol row. It is
// used by opt-in providers; the public helper also validates the declaration
// and resulting binding before handing it to a caller.
func FromProtocolManifest(m agent.ModelEndpointManifest, req agent.EndpointRequest, protocol agent.WireProtocol) (agent.EndpointBinding, error) {
	if protocol == "" {
		return agent.EndpointBinding{}, agent.ErrEndpointProtocolRequired
	}
	declared := false
	for _, candidate := range m.Speaks {
		if candidate == protocol {
			declared = true
			break
		}
	}
	if !declared {
		return agent.EndpointBinding{}, agent.ErrEndpointProtocolUnsupported
	}
	seen := make(map[[2]string]bool, len(m.Hosts))
	var selected *agent.HostDesc
	for i := range m.Hosts {
		row := &m.Hosts[i]
		key := [2]string{string(row.Host), string(row.Protocol)}
		if row.Host == "" || row.Protocol == "" || seen[key] {
			return agent.EndpointBinding{}, agent.ErrEndpointHostProtocolAmbiguous
		}
		seen[key] = true
		if row.Host != req.Host || row.Protocol != protocol {
			continue
		}
		selected = row
	}
	if selected == nil {
		return agent.EndpointBinding{}, agent.ErrEndpointProtocolUnsupported
	}
	allowed := false
	for _, mode := range selected.AuthModes {
		if mode == req.Auth {
			allowed = true
			break
		}
	}
	if !allowed {
		return agent.EndpointBinding{}, agent.ErrEndpointProtocolUnsupported
	}
	return fromHost(m.Company, *selected, req), nil
}

func fromHost(company agent.Company, host agent.HostDesc, req agent.EndpointRequest) agent.EndpointBinding {
	env := make(map[string]string, len(host.EnvKeys))
	for _, k := range host.EnvKeys {
		if v, ok := req.EnvProvided[k]; ok {
			env[k] = v
		}
	}

	return agent.EndpointBinding{
		Company:       company,
		Model:         req.Model,
		BaseURL:       TemplateBaseURL(host.BaseURLTmpl, req.Region),
		Protocol:      host.Protocol,
		Host:          host.Host,
		Mechanism:     req.Mechanism,
		Auth:          req.Auth,
		Region:        req.Region,
		CostModel:     host.CostModel,
		BringsOwnAuth: host.BringsOwnAuth,
		Env:           env,
	}
}

// TemplateBaseURL substitutes the {region} placeholder in a host's
// BaseURLTmpl. Empty templates (host-login cells) are returned unchanged.
func TemplateBaseURL(tmpl, region string) string {
	if tmpl == "" {
		return ""
	}
	return strings.ReplaceAll(tmpl, "{region}", region)
}
