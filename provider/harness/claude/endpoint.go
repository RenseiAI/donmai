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

	// EnvAuthToken carries the session credential for a translating-gateway
	// cell. The CLI documents ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN as
	// the gateway pair (its own changelog names exactly this pair for
	// sessions that reach it through a gateway with no first-party
	// account): unlike ANTHROPIC_API_KEY, which raises an interactive
	// "Detected a custom API key" approval the moment a TTY is present,
	// the token pair is accepted unattended on both the headless and the
	// interactive lanes. Name only — never a value; see applyEndpoint.
	EnvAuthToken = "ANTHROPIC_AUTH_TOKEN" //nolint:gosec // G101: env-var NAME, never credential bytes.

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
//	gateway       ANTHROPIC_BASE_URL=<BaseURL> + ANTHROPIC_AUTH_TOKEN=<key>;
//	              the modal-generating ANTHROPIC_API_KEY name is kept out of
//	              the child env (see projectGatewayCredential)
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
	case agent.HostGateway:
		// Translating-gateway cell: the harness drives the gateway's
		// loopback surface over the anthropic-messages dialect, presenting
		// the session bearer through the CLI's own unattended token pair.
		// The manifest's DrivesHosts gates admission upstream
		// (validateReceiptCell); this read site only projects.
		return projectGatewayCredential(spec, ep)
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

// projectGatewayCredential projects a translating-gateway binding onto the
// spec the CLI subprocess sees: the gateway's loopback surface becomes the
// base URL and the session bearer rides the CLI's unattended token pair.
//
// The bearer is selected exactly once, from the binding's own cell key first
// (Endpoint.Env, the resolved credential the control plane bound for this
// session) and otherwise from the trusted Spec.Env layer the platform's
// credential fan-out already populated — never from the ambient parent
// process. A gateway cell with no bearer anywhere fails loudly: silently
// spawning against the default host would mis-bill and mis-route, and an
// unattended session parked on a login prompt is the defect this closes.
//
// The modal-generating key name (ANTHROPIC_API_KEY) is kept OUT of the
// child env: with a TTY present the CLI answers that name with an
// interactive "Detected a custom API key" approval no unattended session
// can answer, and without one the session degrades to ambient host login.
// Only the token name the CLI honors unattended rides the child.
func projectGatewayCredential(spec agent.Spec, ep *agent.EndpointBinding) (agent.Spec, error) {
	if ep.Model != "" {
		spec.Model = ep.Model
	}
	if err := agent.ValidateEndpointBindingBaseURL(ep.BaseURL); err != nil {
		return spec, fmt.Errorf("gateway endpoint: %w", err)
	}
	key := ""
	if ep.Env != nil {
		key = pickGatewayBearer(ep.Env)
	}
	if key == "" && spec.Env != nil {
		key = pickGatewayBearer(spec.Env)
	}
	if key == "" {
		return spec, fmt.Errorf("gateway endpoint serves %q but carries no session bearer", ep.BaseURL)
	}
	env := make(map[string]string)
	for k, v := range spec.Env {
		if k == gatewayModalKeyName {
			continue
		}
		env[k] = v
	}
	for k, v := range ep.Env {
		if v == "" || k == gatewayModalKeyName {
			continue
		}
		env[k] = v
	}
	env[EnvBaseURL] = ep.BaseURL
	env[EnvAuthToken] = key
	spec.Env = env
	return spec, nil
}

// gatewayModalKeyName is the credential name the CLI answers with an
// interactive approval on a TTY. It must never ride an unattended session's
// child env; the token pair carries the same bearer without the modal.
const gatewayModalKeyName = "ANTHROPIC_API_KEY"

// pickGatewayBearer selects the session bearer from a credential layer: the
// gateway token name first, then the modal-generating key name (a binding
// may file the same bearer under either spelling). Only these two spellings
// are read — a generic fallback would risk presenting an unrelated key as
// the session credential. Empty when the layer carries no bearer.
func pickGatewayBearer(layer map[string]string) string {
	if v := layer[EnvAuthToken]; v != "" {
		return v
	}
	return layer[gatewayModalKeyName]
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
