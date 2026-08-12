package claude

import (
	"fmt"

	"github.com/RenseiAI/donmai/agent"
)

// Env-var NAMES (never values) the Claude Code CLI consumes for endpoint
// routing. These are the CLI's documented knobs: the claude binary itself
// performs the per-host wire translation (SigV4 for Bedrock, OAuth for
// Vertex) once the flag/env surface below is set.
const (
	// EnvBaseURL points the CLI at a non-default Anthropic-Messages base
	// URL (regional mirrors, proxies, httptest fakes).
	EnvBaseURL = "ANTHROPIC_BASE_URL"

	// EnvUseBedrock flips the CLI onto the AWS Bedrock serving host.
	EnvUseBedrock = "CLAUDE_CODE_USE_BEDROCK"

	// EnvUseVertex flips the CLI onto the Google Vertex serving host.
	EnvUseVertex = "CLAUDE_CODE_USE_VERTEX"

	// EnvAWSRegion is the AWS region the Bedrock host serves from.
	EnvAWSRegion = "AWS_REGION"

	// EnvVertexRegion is the GCP region the Vertex host serves from.
	EnvVertexRegion = "CLOUD_ML_REGION"
)

// applyEndpoint projects a resolved Spec.Endpoint binding onto the spec the
// CLI subprocess actually sees, making the declared claude-code × anthropic
// matrix cells (direct / bedrock / vertex) actually route:
//
//	host          env effect
//	────────────  ──────────────────────────────────────────────────────────
//	(nil) / ""    none — today's behavior (host login / env defaults)
//	oauth-cli     none — BringsOwnAuth, the CLI's own login session routes
//	direct        ANTHROPIC_BASE_URL=<BaseURL> (when set) + binding env
//	bedrock       CLAUDE_CODE_USE_BEDROCK=1 + AWS_REGION=<Region> + binding env
//	vertex        CLAUDE_CODE_USE_VERTEX=1 + CLOUD_ML_REGION=<Region> + binding env
//
// Merge precedence (most → least specific): host-derived routing vars >
// Endpoint.Env (the deliberately resolved cell credentials) > Spec.Env (the
// broader session env). Region vars are only derived when the binding's env
// did not already provide them, so an explicitly configured region wins.
// Endpoint.Model wins over Spec.Model when set (the binding is the resolved
// cell — same rule as the one-shot lane). spec.Env is never mutated; a
// merged copy is returned.
//
// A binding for a company this harness cannot route (anything non-Anthropic)
// or an unknown serving host fails loudly: silently spawning against the
// default host would mis-bill and mis-route.
func applyEndpoint(spec agent.Spec) (agent.Spec, error) {
	ep := spec.Endpoint
	if ep == nil {
		return spec, nil
	}
	if ep.Company != "" && ep.Company != agent.CompanyAnthropic {
		return spec, fmt.Errorf("endpoint company %q is not routable by the claude harness", ep.Company)
	}
	if ep.Model != "" {
		spec.Model = ep.Model
	}

	switch ep.Host {
	case "", agent.HostOAuthCLI:
		// Host-login cell: the CLI's own session brings auth + routing.
		return spec, nil
	case agent.HostDirect, agent.HostBedrock, agent.HostVertex:
		// fall through to the env projection below
	default:
		return spec, fmt.Errorf("serving host %q is not routable by the claude harness", ep.Host)
	}

	// No capacity hint: a Go map grows on demand, so pre-sizing buys nothing
	// here, and summing len()s as an allocation size is exactly the shape a
	// static scanner (go/allocation-size-overflow) flags as a potential overflow.
	env := make(map[string]string)
	for k, v := range spec.Env {
		env[k] = v
	}
	for k, v := range ep.Env {
		if v != "" {
			env[k] = v
		}
	}

	switch ep.Host {
	case agent.HostDirect:
		if ep.BaseURL != "" {
			env[EnvBaseURL] = ep.BaseURL
		}
	case agent.HostBedrock:
		env[EnvUseBedrock] = "1"
		setIfAbsent(env, EnvAWSRegion, ep.Region)
	case agent.HostVertex:
		env[EnvUseVertex] = "1"
		setIfAbsent(env, EnvVertexRegion, ep.Region)
	}

	spec.Env = env
	return spec, nil
}

// setIfAbsent sets env[key]=value unless value is empty or the key already
// carries an explicitly configured value.
func setIfAbsent(env map[string]string, key, value string) {
	if value == "" {
		return
	}
	if _, ok := env[key]; ok {
		return
	}
	env[key] = value
}
