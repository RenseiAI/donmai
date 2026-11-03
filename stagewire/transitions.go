package stagewire

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Outcome names one of the four run outcomes a stage declares a target for,
// plus the start transition applied when the run begins.
type Outcome string

// The outcomes. Stable wire strings; never repurpose one.
const (
	OutcomeStart   Outcome = "start"
	OutcomeSuccess Outcome = "success"
	OutcomeFailure Outcome = "failure"
	OutcomeBlocked Outcome = "blocked"
	OutcomeClose   Outcome = "close"
)

// stayLiteral is the wire spelling of "no move".
const stayLiteral = "stay"

// Transition is where a work item goes on one outcome: a stage key and the
// tracker's native state id to write, or no move at all. The zero value is
// "stay". On the wire it is either the string "stay" or
// {"stageKey": ..., "writeStatus": ...}.
type Transition struct {
	StageKey string `json:"stageKey"`
	// WriteStatus is the tracker's native state id to set, as resolved by
	// the control plane. It is data: the runner holds no status names.
	WriteStatus string `json:"writeStatus"`
}

// Stay reports whether the transition moves nothing.
func (t Transition) Stay() bool { return t.WriteStatus == "" }

// JSONValue is the transition's wire value as a plain JSON document: the
// string "stay", or the two-member object.
func (t Transition) JSONValue() any {
	if t.Stay() {
		return stayLiteral
	}
	return map[string]any{"stageKey": t.StageKey, "writeStatus": t.WriteStatus}
}

// MarshalJSON writes "stay" or the target object.
func (t Transition) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(t.JSONValue())
	if err != nil {
		return nil, fmt.Errorf("stagewire: transition: %w", err)
	}
	return b, nil
}

// UnmarshalJSON reads "stay" or a target object. A target missing either
// member, or carrying an unbounded one, decodes as stay: a transition the
// runner cannot apply exactly is never applied approximately. Any other
// string also decodes as stay.
func (t *Transition) UnmarshalJSON(b []byte) error {
	*t = Transition{}
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return fmt.Errorf("stagewire: transition: %w", err)
		}
		return nil
	}
	type alias Transition
	var a alias
	if err := json.Unmarshal(trimmed, &a); err != nil {
		return fmt.Errorf("stagewire: transition: %w", err)
	}
	key, status := boundID(a.StageKey), boundID(a.WriteStatus)
	if key == "" || status == "" {
		return nil
	}
	*t = Transition{StageKey: key, WriteStatus: status}
	return nil
}

// Transitions are the resolved targets of the stage a dispatch came from.
// An absent outcome means stay. Start is present only when the stage
// declares one.
type Transitions struct {
	Start   *Transition `json:"start,omitempty"`
	Success *Transition `json:"success,omitempty"`
	Failure *Transition `json:"failure,omitempty"`
	Blocked *Transition `json:"blocked,omitempty"`
	Close   *Transition `json:"close,omitempty"`
}

// For returns the transition for outcome; the zero (stay) transition when
// the outcome is absent or unknown. A nil receiver is all-stay.
func (t *Transitions) For(outcome Outcome) Transition {
	if t == nil {
		return Transition{}
	}
	var target *Transition
	switch outcome {
	case OutcomeStart:
		target = t.Start
	case OutcomeSuccess:
		target = t.Success
	case OutcomeFailure:
		target = t.Failure
	case OutcomeBlocked:
		target = t.Blocked
	case OutcomeClose:
		target = t.Close
	}
	if target == nil {
		return Transition{}
	}
	return *target
}

func (t Transitions) clone() Transitions {
	for _, p := range []**Transition{&t.Start, &t.Success, &t.Failure, &t.Blocked, &t.Close} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	return t
}
