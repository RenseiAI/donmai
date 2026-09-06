package stubagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// EnvToolPolicy carries the tool-permission policy the HARNESS received for
// this session — the `allowedTools`/`disallowedTools` lists that reached the
// stub provider on the native tool-policy channel — so the child can record
// them in its own transcript.
//
// It is diagnostic, not configuration: nothing the child does depends on the
// value, because the child has no tools to permit or deny in the first place.
// That is exactly why it is worth writing down. The stub's interactive profile
// declares the native tool-policy channel satisfied BY CONSTRUCTION
// (agent.ToolDeliveryNoToolSurface), and a claim that a policy was honoured is
// only worth as much as the evidence that the policy actually arrived. This
// variable, and the transcript line the child prints from it, are that
// evidence: a smoke asserting a seat received its deny-list reads the
// session's own output rather than trusting a manifest constant.
const EnvToolPolicy = "DONMAI_STUB_TOOL_POLICY"

// ToolPolicyNoticePrefix precedes the one line the child prints when
// EnvToolPolicy is set. Exported so a test or a smoke matches the literal this
// package emits instead of re-typing it.
const ToolPolicyNoticePrefix = "stub agent: tool policy "

// ToolPolicy is the received tool-permission surface, as the parent composed
// it. Both fields are omitted when empty so an all-empty policy encodes as
// `{}` rather than as two null lists.
type ToolPolicy struct {
	AllowedTools    []string `json:"allowedTools,omitempty"`
	DisallowedTools []string `json:"disallowedTools,omitempty"`
}

// Empty reports whether the policy carries no entries at all. The parent uses
// it to decide whether to set EnvToolPolicy: a session that received no policy
// must print no line, so the PRESENCE of the transcript line is itself the
// evidence that a policy arrived.
func (p ToolPolicy) Empty() bool {
	return len(p.AllowedTools) == 0 && len(p.DisallowedTools) == 0
}

// EncodeToolPolicy renders the policy as the JSON value EnvToolPolicy carries.
func EncodeToolPolicy(policy ToolPolicy) (string, error) {
	encoded, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("stub tool policy: encode: %w", err)
	}
	return string(encoded), nil
}

// LoadToolPolicy reads EnvToolPolicy through getenv (os.Getenv in production,
// a map in tests). An unset or blank variable yields the zero policy and a nil
// error — the ordinary case for a session that was handed no policy.
//
// A value that is set but malformed is an ERROR rather than a silent zero, and
// unknown fields are refused, for the same reason Parse refuses them for a
// scenario: this variable exists to make a claim auditable, and a child that
// quietly discarded the record while still printing "honoured by construction"
// would be asserting exactly the thing it had just lost.
func LoadToolPolicy(getenv func(string) string) (ToolPolicy, error) {
	raw := strings.TrimSpace(getenv(EnvToolPolicy))
	if raw == "" {
		return ToolPolicy{}, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var policy ToolPolicy
	if err := decoder.Decode(&policy); err != nil {
		return ToolPolicy{}, fmt.Errorf("stub tool policy: decode %s: %w", EnvToolPolicy, err)
	}
	// Trailing content is refused for the same reason Parse refuses it for a
	// scenario: a value holding a policy followed by anything else would
	// otherwise be read as the policy alone, silently.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ToolPolicy{}, fmt.Errorf("stub tool policy: %s carries trailing content after the policy object", EnvToolPolicy)
	}
	return policy, nil
}

// Notice is the single line the child writes to its transcript, ahead of the
// scenario's own output.
//
// It names both lists and states WHY no application step follows: the stub's
// interactive child is a scripted fake agent with no tool registry, no MCP
// client and no shell, so every restriction it is handed is already in force.
// The wording is deliberately a statement about the child rather than about
// the policy — "honoured by construction" is only true because there is
// nothing here that could run a tool.
func (p ToolPolicy) Notice() string {
	return fmt.Sprintf(
		"%sreceived allowed=[%s] disallowed=[%s]; honoured by construction (this agent registers no tools)",
		ToolPolicyNoticePrefix,
		strings.Join(p.AllowedTools, ","),
		strings.Join(p.DisallowedTools, ","),
	)
}
