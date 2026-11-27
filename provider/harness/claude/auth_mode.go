package claude

import (
	"maps"

	"github.com/RenseiAI/donmai/agent"
)

// hostLoginStrippedEnv are the env-var NAMES (never values) the Claude Code
// CLI would let override its own first-party host login: a model-provider
// key or token, and every knob that sends the request (and its bill)
// somewhere other than the host login's account. A host-session or local
// session must reach its child with none of them, from any layer.
//
// The alternate-host switches and base URLs mirror pinnedFallbackRoutingEnv;
// the request proxies listed there are left alone because they carry the
// host login's own traffic and select no account.
var hostLoginStrippedEnv = []string{
	"ANTHROPIC_API_KEY",
	EnvAuthToken,
	EnvBaseURL,
	EnvUseBedrock,
	EnvUseVertex,
	"CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS",
	"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
	"CLAUDE_CODE_USE_MANTLE",
	"CLAUDE_CODE_USE_GATEWAY",
	"ANTHROPIC_BEDROCK_BASE_URL",
	"ANTHROPIC_VERTEX_BASE_URL",
	"ANTHROPIC_FOUNDRY_BASE_URL",
	"ANTHROPIC_AWS_BASE_URL",
	"ANTHROPIC_GOOGLE_CLOUD_BASE_URL",
	"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
	"ANTHROPIC_UNIX_SOCKET",
}

// usesHostLogin reports whether the session's resolved auth mode means the
// CLI authenticates with the host's own login rather than a declared
// credential.
func usesHostLogin(mode agent.AuthMode) bool {
	return mode == agent.AuthHostSession || mode == agent.AuthLocal
}

// applyAuthMode keys the child's credential surface off the session's
// resolved auth mode (agent.Spec.AuthMode):
//
//	host-session, local   every hostLoginStrippedEnv name is removed from
//	                      Spec.Env, and the returned names are removed from
//	                      the composed child env as well, so neither an
//	                      inherited copy (a supervisor-declared injection
//	                      included) nor a spec copy reaches the CLI. The
//	                      CLI then uses the host's subscription login.
//	                      The routing pin is empty too (pinnedRoutingEnv),
//	                      so no --settings layer restates a stripped name.
//	byok, metered,        unchanged: the declared credential rides the
//	shared, ""            trusted layers, and the inherited-env blocklist
//	                      (runtime/env) keeps any undeclared ambient copy out.
//
// spec.Env is never mutated; a filtered copy is returned.
func applyAuthMode(spec agent.Spec) (agent.Spec, []string) {
	if !usesHostLogin(spec.AuthMode) {
		return spec, nil
	}
	if len(spec.Env) > 0 {
		env := maps.Clone(spec.Env)
		for _, name := range hostLoginStrippedEnv {
			delete(env, name)
		}
		spec.Env = env
	}
	return spec, append([]string(nil), hostLoginStrippedEnv...)
}
