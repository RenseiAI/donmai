package codex

import (
	"fmt"

	"github.com/RenseiAI/donmai/agent"
)

// providerConfigCyberAccessProgram is the one agent.Spec.ProviderConfig key
// the codex provider consumes. Its value is forwarded verbatim as the
// app-server's turn/start `cyberAccessProgram` param (codex-cli 0.170.0
// experimental API: "Request an authorized cyber program for this turn.
// Omission preserves automatic behavior. This does not grant access.").
//
// The key is an explicit, per-session declaration that comes from the
// matched model profile. The provider never infers it, never defaults it, and
// never writes it into a config file: the request rides each turn of the
// session that declared it and nothing else. Authorization stays with the
// model provider — an account or model that is not approved fails the turn
// with the provider's own error, which surfaces like any other turn failure.
const providerConfigCyberAccessProgram = "cyberAccessProgram"

// cyberAccessPrograms is the closed value set the app-server accepts
// (CyberAccessProgram in the v2 protocol schema). Anything else is refused
// before the session starts instead of being rejected mid-spawn by the
// app-server's request validation.
var cyberAccessPrograms = map[string]bool{
	"standard":     true,
	"daybreakBlue": true,
	"daybreakRed":  true,
}

// resolveCyberAccessProgram returns the declared cyber access program, or ""
// when the session declares none. A declared value that is not a string or is
// outside the protocol's value set is an error: a declaration that cannot be
// honoured must fail the spawn rather than run the session without it.
func resolveCyberAccessProgram(spec agent.Spec) (string, error) {
	raw, ok := spec.ProviderConfig[providerConfigCyberAccessProgram]
	if !ok || raw == nil {
		return "", nil
	}
	program, isString := raw.(string)
	if !isString {
		return "", fmt.Errorf("codex: providerConfig.%s must be a string, got %T", providerConfigCyberAccessProgram, raw)
	}
	if !cyberAccessPrograms[program] {
		return "", fmt.Errorf("codex: providerConfig.%s %q is not one of standard, daybreakBlue, daybreakRed", providerConfigCyberAccessProgram, program)
	}
	return program, nil
}

// interactiveCyberAccessRefusal refuses an interactive launch that declares a
// cyber access program. The Codex TUI has no per-session flag for it (the
// program is a turn/start param of the headless app-server lane only), so an
// interactive session would silently run without what its profile declared.
func interactiveCyberAccessRefusal(spec agent.Spec) error {
	if _, declared := spec.ProviderConfig[providerConfigCyberAccessProgram]; !declared {
		return nil
	}
	return fmt.Errorf("codex: providerConfig.%s is honoured only by headless sessions (turn/start); the interactive Codex TUI cannot select it per session", providerConfigCyberAccessProgram)
}
