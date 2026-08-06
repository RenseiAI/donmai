package opencode

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// applyEndpoint projects a resolved Spec.Endpoint binding onto the spec the
// opencode session actually runs under, making opencode the second harness
// after claude/gemini with a real endpoint read site (07 §9).
//
// Unlike claude (which sets CLI env knobs) opencode routes entirely through the
// injected opencode.json baseURL (config.go resolvedBaseURL) — so applyEndpoint's
// job is (a) fail loudly on a company/host this harness cannot drive, (b) honor
// Endpoint.Model over Spec.Model, and (c) surface the resolved cell credentials
// on spec.Env so the config's "{env:DONMAI_OC_KEY}" indirection resolves in the
// child process (keys ride env, never disk).
//
// Routable surface = the opencode manifest's declared cells: openai-chat over
// direct / local / oauth-cli, for the OpenAI and Google companies (google via
// its local /v1 openai-chat host) plus the local + stub companies used by the
// north-star cell and tests. Anything else (anthropic company, bedrock/vertex/
// azure host) is not routable — silently spawning against a default would
// mis-bill and mis-route.
func applyEndpoint(spec agent.Spec) (agent.Spec, error) {
	ep := spec.Endpoint
	if ep == nil {
		return spec, nil
	}
	if ep.Protocol != agent.ProtoOpenAIChat {
		return spec, fmt.Errorf("endpoint protocol %q is not routable by the opencode harness (requires %q)", ep.Protocol, agent.ProtoOpenAIChat)
	}
	if ep.Model == "" || ep.Model != strings.TrimSpace(ep.Model) {
		return spec, fmt.Errorf("endpoint model must be exact and non-empty")
	}
	parsed, err := url.Parse(ep.BaseURL)
	if err != nil || ep.BaseURL != strings.TrimSpace(ep.BaseURL) || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return spec, fmt.Errorf("endpoint base URL %q must be an absolute HTTP(S) URL", ep.BaseURL)
	}
	switch ep.Company {
	case "", agent.CompanyOpenAI, agent.CompanyGoogle, agent.CompanyLocal, agent.CompanyStub:
		// routable
	default:
		return spec, fmt.Errorf("endpoint company %q is not routable by the opencode harness (drives openai-chat only)", ep.Company)
	}
	switch ep.Host {
	case "", agent.HostDirect, agent.HostLocal, agent.HostOAuthCLI:
		// routable
	default:
		return spec, fmt.Errorf("serving host %q is not routable by the opencode harness", ep.Host)
	}
	spec.Model = ep.Model

	// Endpoint-bound sessions never inherit an ambient route, model, or key.
	// Copy unrelated session env, then inject only the credential explicitly
	// bound for this exact session.
	env := make(map[string]string, len(spec.Env)+1)
	for k, v := range spec.Env {
		if isEndpointControlEnv(k) {
			continue
		}
		env[k] = v
	}
	switch ep.Mechanism {
	case agent.AuthNone:
		// Deliberately no credential env. buildConfig likewise omits apiKey.
	case agent.AuthAPIKey:
		key, err := exactBoundAPIKey(ep.Env)
		if err != nil {
			return spec, err
		}
		env[OCKeyEnvVar] = key
	default:
		return spec, fmt.Errorf("endpoint auth mechanism %q is not supported by the opencode harness", ep.Mechanism)
	}
	spec.Env = env
	return spec, nil
}

func exactBoundAPIKey(env map[string]string) (string, error) {
	key := ""
	for _, value := range env {
		if value == "" {
			continue
		}
		if key != "" {
			return "", fmt.Errorf("endpoint auth mechanism %q requires exactly one non-empty bound session key", agent.AuthAPIKey)
		}
		key = value
	}
	if key == "" {
		return "", fmt.Errorf("endpoint auth mechanism %q requires exactly one non-empty bound session key", agent.AuthAPIKey)
	}
	return key, nil
}

// isEndpointControlEnv identifies ambient knobs that could override or leak
// the exact binding OpenCode receives through its session config.
func isEndpointControlEnv(key string) bool {
	switch key {
	case OCConfigEnvVar, OCKeyEnvVar, EnvEndpoint, EnvAPIKey,
		"OPENAI_API_KEY", "OPENAI_COMPAT_API_KEY", "OPENAI_BASE_URL",
		"OPENAI_MODEL", "OPENCODE_MODEL", "API_KEY":
		return true
	default:
		return false
	}
}
