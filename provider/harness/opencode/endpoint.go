package opencode

import (
	"fmt"
	"sort"

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
	if ep.Model != "" {
		spec.Model = ep.Model
	}

	// Merge the cell's resolved credential values onto spec.Env (a copy — the
	// input spec.Env is never mutated) and mirror the API key onto
	// DONMAI_OC_KEY for the injected config's {env:...} substitution.
	env := make(map[string]string, len(spec.Env)+len(ep.Env)+1)
	for k, v := range spec.Env {
		env[k] = v
	}
	for k, v := range ep.Env {
		if v != "" {
			env[k] = v
		}
	}
	if _, ok := env[OCKeyEnvVar]; !ok {
		if key := pickAPIKey(ep.Env); key != "" {
			env[OCKeyEnvVar] = key
		}
	}
	spec.Env = env
	return spec, nil
}

// pickAPIKey selects the cell's API key value from the binding env by
// preferring well-known key names, falling back to the first non-empty value
// (deterministic via sorted iteration).
func pickAPIKey(envVals map[string]string) string {
	for _, name := range []string{OCKeyEnvVar, "OPENAI_API_KEY", "OPENAI_COMPAT_API_KEY", "API_KEY"} {
		if v := envVals[name]; v != "" {
			return v
		}
	}
	keys := make([]string, 0, len(envVals))
	for k := range envVals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if envVals[k] != "" {
			return envVals[k]
		}
	}
	return ""
}
