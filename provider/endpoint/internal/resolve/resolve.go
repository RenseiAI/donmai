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
// with req.Region, copies the host's declared env keys from req.EnvProvided,
// and returns the binding. Returns an error for an unknown host.
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

	env := make(map[string]string, len(host.EnvKeys))
	for _, k := range host.EnvKeys {
		if v, ok := req.EnvProvided[k]; ok {
			env[k] = v
		}
	}

	return agent.EndpointBinding{
		Company:       m.Company,
		Model:         req.Model,
		BaseURL:       TemplateBaseURL(host.BaseURLTmpl, req.Region),
		Protocol:      host.Protocol,
		Host:          host.Host,
		Auth:          req.Auth,
		CostModel:     host.CostModel,
		BringsOwnAuth: host.BringsOwnAuth,
		Env:           env,
	}, nil
}

// TemplateBaseURL substitutes the {region} placeholder in a host's
// BaseURLTmpl. Empty templates (host-login cells) are returned unchanged.
func TemplateBaseURL(tmpl, region string) string {
	if tmpl == "" {
		return ""
	}
	return strings.ReplaceAll(tmpl, "{region}", region)
}
