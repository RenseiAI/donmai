package stagewire

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Bounds applied while decoding. They sit above what a well-behaved control
// plane sends, so a valid emit always survives, while a runaway value is
// dropped instead of reaching logs, prompts or tracker calls.
const (
	// MaxIDBytes bounds identifiers: ids, keys, handles, tracker state ids
	// and work-item identifiers.
	MaxIDBytes = 256
	// MaxTextBytes bounds display text: stage names, titles and tracker
	// status names.
	MaxTextBytes = 1024
	// MaxChildren bounds the child summaries a stage context carries.
	MaxChildren = 50
	// MaxRetries caps retry.retries.
	MaxRetries = 10
	// MaxBackoffSeconds caps retry.backoffSeconds.
	MaxBackoffSeconds = 3600
)

// Role is the closed set of seven stage roles the runner branches on.
// Stable wire strings; never repurpose one.
type Role string

// The seven stage roles.
const (
	// RoleHold is not scheduled.
	RoleHold Role = "hold"
	// RoleIntake is ready to be picked up.
	RoleIntake Role = "intake"
	// RoleInflight is being built.
	RoleInflight Role = "inflight"
	// RoleReview is being verified.
	RoleReview Role = "review"
	// RoleRework failed verification and routes back with findings.
	RoleRework Role = "rework"
	// RoleDone is completed.
	RoleDone Role = "done"
	// RoleClosed is abandoned.
	RoleClosed Role = "closed"
)

// Roles lists every role in declaration order.
var Roles = []Role{RoleHold, RoleIntake, RoleInflight, RoleReview, RoleRework, RoleDone, RoleClosed}

// ParseRole validates a raw role string. An empty or unknown value reports
// false: roles are structure, and an unknown structure is never guessed at.
func ParseRole(s string) (Role, bool) {
	switch r := Role(s); r {
	case RoleHold, RoleIntake, RoleInflight, RoleReview, RoleRework, RoleDone, RoleClosed:
		return r, true
	default:
		return "", false
	}
}

// UnmarshalJSON decodes a role, dropping any value outside the seven.
func (r *Role) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("stagewire: role: %w", err)
	}
	*r, _ = ParseRole(s)
	return nil
}

// Contract is what a run owes before it counts as delivered. Each flag is
// explicit card data; an all-false contract owes nothing.
type Contract struct {
	// PR: the run owes commits on a pushed branch and a pull request.
	PR bool `json:"pr"`
	// Verdict: the run owes a review verdict.
	Verdict bool `json:"verdict"`
	// WorkResult: the run owes a structured work result.
	WorkResult bool `json:"workResult"`
}

// DefaultContract is the role's default completion contract, used when the
// dispatched card carries none. done and closed owe nothing, as does an
// unknown role.
func (r Role) DefaultContract() Contract {
	switch r {
	case RoleInflight, RoleRework:
		return Contract{PR: true, WorkResult: true}
	case RoleReview:
		return Contract{Verdict: true, WorkResult: true}
	case RoleIntake, RoleHold:
		return Contract{WorkResult: true}
	default:
		return Contract{}
	}
}

// Stage identifies the stage a work item is in.
type Stage struct {
	ID   string `json:"id,omitempty"`
	Key  string `json:"key,omitempty"`
	Name string `json:"name,omitempty"`
	// Role is the role of the stage the work was dispatched from.
	Role Role `json:"role,omitempty"`
	// RunRole is the role of the stage the run executes in. It differs
	// from Role when the stage's start transition moves the work item on
	// dispatch (a run dispatched from an intake stage whose start target
	// is an inflight stage runs as inflight).
	RunRole Role `json:"runRole,omitempty"`
}

// UnmarshalJSON decodes a stage and bounds its strings.
func (s *Stage) UnmarshalJSON(b []byte) error {
	type alias Stage
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("stagewire: stage: %w", err)
	}
	*s = Stage{
		ID:      boundID(a.ID),
		Key:     boundID(a.Key),
		Name:    boundText(a.Name),
		Role:    a.Role,
		RunRole: a.RunRole,
	}
	return nil
}

// EffectiveRunRole is the role the run executes under: RunRole, else Role.
// It reports false when neither is a known role.
func (s *Stage) EffectiveRunRole() (Role, bool) {
	if s == nil {
		return "", false
	}
	if r, ok := ParseRole(string(s.RunRole)); ok {
		return r, true
	}
	return ParseRole(string(s.Role))
}

// Card identifies the dispatched agent card and what its run owes.
type Card struct {
	ID     string `json:"id,omitempty"`
	Handle string `json:"handle,omitempty"`
	// Contract is the card's completion contract. Nil means the card
	// declares none and the stage role's default applies.
	Contract *Contract `json:"contract,omitempty"`
}

// UnmarshalJSON decodes a card and bounds its strings.
func (c *Card) UnmarshalJSON(b []byte) error {
	type alias Card
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("stagewire: card: %w", err)
	}
	*c = Card{ID: boundID(a.ID), Handle: boundID(a.Handle), Contract: a.Contract}
	return nil
}

// Retry is the stage's mechanical retry policy: how many times a provider,
// spawn or launch error is retried and how long to wait between attempts.
type Retry struct {
	Retries        int `json:"retries"`
	BackoffSeconds int `json:"backoffSeconds"`
}

// UnmarshalJSON decodes a retry policy and clamps it to
// [0, MaxRetries] x [0, MaxBackoffSeconds].
func (r *Retry) UnmarshalJSON(b []byte) error {
	type alias Retry
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("stagewire: retry: %w", err)
	}
	*r = Retry{Retries: clamp(a.Retries, MaxRetries), BackoffSeconds: clamp(a.BackoffSeconds, MaxBackoffSeconds)}
	return nil
}

// DispatchReason is why the dispatched card runs. Values outside the closed
// set are dropped on decode.
type DispatchReason string

// The dispatch reasons.
const (
	DispatchReasonMention   DispatchReason = "mention"
	DispatchReasonCondition DispatchReason = "condition"
	DispatchReasonDefault   DispatchReason = "default"
	DispatchReasonWorkflow  DispatchReason = "workflow"
)

// UnmarshalJSON decodes a dispatch reason, dropping unknown values.
func (d *DispatchReason) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("stagewire: dispatch reason: %w", err)
	}
	switch v := DispatchReason(s); v {
	case DispatchReasonMention, DispatchReasonCondition, DispatchReasonDefault, DispatchReasonWorkflow:
		*d = v
	default:
		*d = ""
	}
	return nil
}

// BatchKind is the discriminant of a batch work item. It replaces carrying
// the batch kind in the legacy workType. Oversized or malformed values are
// dropped on decode.
type BatchKind string

// UnmarshalJSON decodes a batch kind and bounds it.
func (k *BatchKind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("stagewire: batch kind: %w", err)
	}
	*k = BatchKind(boundID(s))
	return nil
}

// Fields is the set of optional 1.0 fields a session work item carries.
// It is embedded (untagged) in every struct that carries a session work
// item across a hop, so the fields sit at the top level of the JSON object
// and each hop forwards them with one assignment.
//
// Fields deliberately has no JSON methods of its own: an embedded type's
// MarshalJSON or UnmarshalJSON would be promoted to, and take over, the
// whole enclosing work item. Each field type validates itself instead.
type Fields struct {
	Stage          *Stage         `json:"stage,omitempty"`
	Card           *Card          `json:"card,omitempty"`
	Transitions    *Transitions   `json:"transitions,omitempty"`
	Retry          *Retry         `json:"retry,omitempty"`
	StageContext   *StageContext  `json:"stageContext,omitempty"`
	DispatchReason DispatchReason `json:"dispatchReason,omitempty"`
}

// HasStageOrCard reports whether the work item carries a stage or a card.
// When it does, the runner never branches on the legacy work-type name.
func (f Fields) HasStageOrCard() bool {
	return f.Stage != nil || f.Card != nil
}

// StageRunRole is the role the run executes under (see
// Stage.EffectiveRunRole); false when the work item carries no stage or no
// known role.
func (f Fields) StageRunRole() (Role, bool) {
	return f.Stage.EffectiveRunRole()
}

// CloneFields returns a deep copy, so a projection of a work item can never
// alias the work item it was projected from.
func (f Fields) CloneFields() Fields {
	out := Fields{DispatchReason: f.DispatchReason}
	if f.Stage != nil {
		v := *f.Stage
		out.Stage = &v
	}
	if f.Card != nil {
		v := f.Card.clone()
		out.Card = &v
	}
	if f.Transitions != nil {
		v := f.Transitions.clone()
		out.Transitions = &v
	}
	if f.Retry != nil {
		v := *f.Retry
		out.Retry = &v
	}
	if f.StageContext != nil {
		v := f.StageContext.clone()
		out.StageContext = &v
	}
	return out
}

func (c Card) clone() Card {
	if c.Contract != nil {
		v := *c.Contract
		c.Contract = &v
	}
	return c
}

// boundID keeps an identifier only when it is valid UTF-8, free of control
// characters, and at most MaxIDBytes long; otherwise it is dropped.
func boundID(s string) string { return bounded(s, MaxIDBytes) }

// boundText is boundID for display text, at MaxTextBytes.
func boundText(s string) string { return bounded(s, MaxTextBytes) }

func bounded(s string, limit int) string {
	if len(s) > limit || !utf8.ValidString(s) {
		return ""
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return s
}

func clamp(v, upper int) int {
	switch {
	case v < 0:
		return 0
	case v > upper:
		return upper
	default:
		return v
	}
}
