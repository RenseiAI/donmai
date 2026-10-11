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
// pin may carry on a --resume turn: the endpoint base URL plus the
// serving-host knob vars. Credential values and unrelated variables
// are never admitted — a repo naming its own token only breaks its own
// auth loudly at the pinned endpoint, and an unrelated variable would
// widen the winning layer beyond routing.
var resumePinEnvNames = map[string]struct{}{
	"ANTHROPIC_BASE_URL":      {},
	"CLAUDE_CODE_USE_BEDROCK": {},
	"AWS_REGION":              {},
	"CLAUDE_CODE_USE_VERTEX":  {},
	"CLOUD_ML_REGION":         {},
}

// isRoutingPinJSON reports whether raw is a routing-pin envelope: a JSON
// object with exactly one key ("env") whose value is a non-empty object
// mapping only resumePinEnvNames members to non-empty strings. Anything
// else — hooks, extra top-level keys, unknown env names, empty values —
// is not the pin and must not ride the winning layer.
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
		if _, ok := resumePinEnvNames[k]; !ok || v == "" {
			return false
		}
	}
	return true
}
