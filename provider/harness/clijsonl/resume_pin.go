package clijsonl

// resumePinEnvNames and isRoutingPinJSON are the driver's syntactic guard
// for harness-supplied --resume argv fragments (see resumeArgv in
// handle.go).
//
// The driver cannot import a harness package (the extraction exists to
// keep harness dependencies one-directional), so the allowlist below
// names the pin's routing-destination keys directly. It deliberately
// mirrors the claude harness's routing pin set — the only pin any
// current harness renders — and the resume pin test in the claude
// package (production Inject, attacker repo file) is the behavioural
// proof that the two stay in step: if this list ever drops a name the
// harness pins, that test's resolved URL flips to the attacker value.

import "encoding/json"

// resumePinEnvNames are the routing-destination env names a --settings
// pin may carry on a --resume turn: the session's own routing names plus
// the fallback routing names (see pinnedFallbackRoutingEnv in the claude
// harness) whose "" blanks hold the ambient default on the winning layer.
// Credential values and unrelated variables are never admitted — a repo
// naming its own token only breaks its own auth loudly at the pinned
// endpoint, and an unrelated variable would widen the winning layer beyond
// routing.
var resumePinEnvNames = map[string]struct{}{
	"ANTHROPIC_BASE_URL":                     {},
	"CLAUDE_CODE_USE_BEDROCK":                {},
	"AWS_REGION":                             {},
	"CLAUDE_CODE_USE_VERTEX":                 {},
	"CLOUD_ML_REGION":                        {},
	"CLAUDE_CODE_USE_FOUNDRY":                {},
	"ANTHROPIC_BEDROCK_BASE_URL":             {},
	"ANTHROPIC_VERTEX_BASE_URL":              {},
	"ANTHROPIC_FOUNDRY_BASE_URL":             {},
	"CLAUDE_CODE_USE_ANTHROPIC_AWS":          {},
	"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD": {},
	"CLAUDE_CODE_USE_MANTLE":                 {},
	"CLAUDE_CODE_USE_GATEWAY":                {},
	"ANTHROPIC_AWS_BASE_URL":                 {},
	"ANTHROPIC_GOOGLE_CLOUD_BASE_URL":        {},
	"ANTHROPIC_BEDROCK_MANTLE_BASE_URL":      {},
	"ANTHROPIC_UNIX_SOCKET":                  {},
	"HTTP_PROXY":                             {},
	"HTTPS_PROXY":                            {},
	"ALL_PROXY":                              {},
	"http_proxy":                             {},
	"https_proxy":                            {},
	"all_proxy":                              {},
}

// isRoutingPinJSON reports whether raw is a routing-pin envelope: a JSON
// object with exactly one key ("env") whose value is a non-empty object
// mapping only resumePinEnvNames members to strings. Fallback routing names
// (proxies, alternate serving-host switches and base URLs) may be blank:
// a blank states the ambient default on the winning layer. The session's
// own routing names must be non-empty — the driver cannot tell a session
// value from a caller smuggling an empty override. Anything else — hooks,
// extra top-level keys, unknown env names — is not the pin and must not
// ride the winning layer.
func isRoutingPinJSON(raw string) bool {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return false
	}
	if len(envelope) != 1 {
		return false
	}
	envRaw, ok := envelope["env"]
	if !ok {
		return false
	}
	var env map[string]string
	if err := json.Unmarshal(envRaw, &env); err != nil {
		return false
	}
	if len(env) == 0 {
		return false
	}
	for k, v := range env {
		if _, ok := resumePinEnvNames[k]; !ok {
			return false
		}
		if v == "" && !isBlankableResumePinName(k) {
			return false
		}
	}
	return true
}

// isBlankableResumePinName reports whether a resume pin member may carry an
// empty value: the fallback routing names, whose blank states the ambient
// default on the winning layer. The session's own routing names must still
// be non-empty.
func isBlankableResumePinName(name string) bool {
	switch name {
	case "CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY",
		"ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_VERTEX_BASE_URL",
		"ANTHROPIC_FOUNDRY_BASE_URL",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
		"CLAUDE_CODE_USE_MANTLE",
		"CLAUDE_CODE_USE_GATEWAY",
		"ANTHROPIC_AWS_BASE_URL",
		"ANTHROPIC_GOOGLE_CLOUD_BASE_URL",
		"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
		"ANTHROPIC_UNIX_SOCKET",
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"ALL_PROXY",
		"http_proxy",
		"https_proxy",
		"all_proxy":
		return true
	}
	return false
}
