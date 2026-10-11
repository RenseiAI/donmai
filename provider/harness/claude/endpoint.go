package claude

import (
	"fmt"
	"os"

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

// routingPinEnvNames are the CLI routing DESTINATION knobs the session
// launcher pins AFTER repo-local harness config is applied, so a repo
// setting never redirects the session bearer to an attacker endpoint. The
// pin rides the --settings JSON string donmai itself passes (stophook.go /
// cli_args.go / oneshot.go): the CLI resolves that layer above the process
// environment, which in turn sits above repo settings files. The pin states
// the session's own destinations (see pinnedRoutingEnv) plus explicit
// "" blanks for the alternate serving-host and proxy names the session
// does not set (see pinnedFallbackRoutingEnv), so a repo flipping a
// provider switch or naming a proxy loses to the pin. A repo setting naming
// one of the session's own values is likewise overridden — every layer the
// CLI honors points at the session's own endpoint.
//
// The pin covers only these declarative routing destinations. Repo config
// that runs code (hooks, apiKeyHelper and other helpers, stdio MCP
// servers, the agent's own tools) still runs with the session environment
// once the repo path is trusted, exactly as on the headless lane — that
// execution surface is the accepted model for a trusted repo, not something
// the pin closes. See the file-level trust doc in trust.go.
//
// Credential VALUES are deliberately NOT pinned: the bearer rides the
// process environment (never argv), and --settings travels on the process
// listing — pinning the token there would publish the session bearer to
// every same-user process. Pinning the destination alone closes the
// redirect: with the URL fixed, a repo naming its own token only breaks its
// own auth loudly (the foreign token is invalid at the pinned endpoint),
// and the session bearer still goes to — and only to — the pinned URL.
// Name only — never a value; see pinnedRoutingEnv.
var routingPinEnvNames = map[string]struct{}{
	EnvBaseURL:      {},
	EnvUseBedrock:   {},
	EnvAWSRegion:    {},
	EnvUseVertex:    {},
	EnvVertexRegion: {},
}

// pinnedFallbackRoutingEnv are the extra routing-adjacent env names the
// launcher pins to the CLI's winning --settings layer alongside the
// session's own routing. A gateway session carries only its own base URL,
// so without these entries a repo-local settings file could flip an
// alternate serving host on (or point the request at an attacker proxy) and
// redirect the session bearer there: the pin states every name the CLI
// consults for where the request goes, with "" restoring the ambient
// default. Values ride the effective child layer — spec.Env over the parent
// process — so a host that genuinely proxies keeps its own proxy, while a
// repo naming one loses to the pin. These ride only when the session
// carries routing of its own (see pinnedRoutingEnv), so host-login sessions
// stay byte-identical. The names here and in resumePinEnvNames must stay in
// step; the proxy/provider-switch tests fail if one side drops a name.
var pinnedFallbackRoutingEnv = []string{
	// Alternate serving-host switches the session never sets: pinned off so
	// a repo cannot flip the request onto a non-default provider surface.
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
	// Alternate base URLs for those hosts: pinned empty so a repo cannot
	// arm a redirect the switches would then use.
	"ANTHROPIC_BEDROCK_BASE_URL",
	"ANTHROPIC_VERTEX_BASE_URL",
	"ANTHROPIC_FOUNDRY_BASE_URL",
	// Plaintext request proxies: pinned empty so a repo cannot observe the
	// session bearer in transit by naming an attacker listener.
	"HTTP_PROXY",
	"HTTPS_PROXY",
	"ALL_PROXY",
	"http_proxy",
	"https_proxy",
	"all_proxy",
}

// pinnedRoutingEnv returns the routing DESTINATION knobs the session
// authoritatively carries, for the --settings pin: the session's own values
// for routingPinEnvNames members (see applyEndpoint), PLUS a
// pinnedFallbackRoutingEnv entry for every fallback name — the effective
// child value (spec.Env over the parent process) or "" when the session
// sets none. A repo-local harness setting naming any of these (for example
// env.ANTHROPIC_BASE_URL in the repo's settings file) is overridden by the
// pin on the layer the CLI resolves last, so the session bearer stays on
// the session's own route. Pinning the alternates even when the session
// sets none is what closes the provider-switch and proxy redirects: a
// gateway session whose spec carries only ANTHROPIC_BASE_URL still pins
// CLAUDE_CODE_USE_BEDROCK="" and HTTP_PROXY="", so a repo flipping the
// switch (or naming a proxy) loses to the pin. Credential values are never
// pinned (see routingPinEnvNames).
//
// The pin is present only when the session carries routing of its own (a
// non-empty routingPinEnvNames value in spec.Env): without a session
// destination to hold, a fallback-only pin would change host-login argv
// for no redirect it closes — those sessions pin nothing and stay
// byte-identical. The returned map is a fresh copy; spec is never mutated.
func pinnedRoutingEnv(spec agent.Spec) map[string]string {
	pin := make(map[string]string)
	hasOwnRouting := false
	for k, v := range spec.Env {
		if _, ok := routingPinEnvNames[k]; ok && v != "" {
			pin[k] = v
			hasOwnRouting = true
		}
	}
	if !hasOwnRouting {
		return nil
	}
	for _, name := range pinnedFallbackRoutingEnv {
		if _, already := pin[name]; already {
			continue
		}
		pin[name] = effectiveRoutingValue(name, spec.Env)
	}
	return pin
}

// effectiveRoutingValue reads the session process's effective value for a
// fallback routing name: the spec layer first, then the parent process.
// Using the effective value (rather than always blanking) keeps a host's
// real proxy working: a repo naming its own proxy still loses to the pin,
// but the pin states the host's own value rather than breaking it.
func effectiveRoutingValue(name string, specEnv map[string]string) string {
	if v, ok := specEnv[name]; ok && v != "" {
		return v
	}
	if v, ok := routingParentEnv(name); ok {
		return v
	}
	return ""
}

// routingParentEnv reads one name from the parent process environment.
// Split out so tests can isolate the spec layer from ambient state.
func routingParentEnv(name string) (string, bool) {
	return lookupRoutingParentEnv(name)
}

// lookupRoutingParentEnv is the os.Getenv seam behind routingParentEnv.
// Declared as a variable so tests can stub the ambient layer without
// mutating the real process environment.
var lookupRoutingParentEnv = func(name string) (string, bool) {
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

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
