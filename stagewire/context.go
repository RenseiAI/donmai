package stagewire

import (
	"encoding/json"
	"fmt"
)

// StageContext is what the dispatched agent is told about where it runs:
// the stage, the tracker status it was dispatched from, the card and why it
// was chosen, and the work item's parent plus a bounded child summary.
type StageContext struct {
	// Stage carries key, name and role.
	Stage *Stage `json:"stage,omitempty"`
	// SourceStatus is the tracker status the work item was dispatched
	// from; null when unknown.
	SourceStatus *string `json:"sourceStatus"`
	// Card carries id and handle.
	Card   *Card          `json:"card,omitempty"`
	Reason DispatchReason `json:"reason,omitempty"`
	Parent *ContextParent `json:"parent,omitempty"`
	// Children is at most MaxChildren entries.
	Children []ContextChild `json:"children,omitempty"`
}

// ContextParent is the work item's parent.
type ContextParent struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
}

// ContextChild is one child of the work item.
type ContextChild struct {
	Identifier string `json:"identifier"`
	// Status is the child's tracker status; null when unknown.
	Status *string `json:"status"`
}

// UnmarshalJSON decodes a stage context and bounds it. Children past
// MaxChildren and children without a usable identifier are dropped; a
// parent without one is dropped.
func (c *StageContext) UnmarshalJSON(b []byte) error {
	type alias StageContext
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("stagewire: stage context: %w", err)
	}
	out := StageContext{
		Stage:        a.Stage,
		SourceStatus: boundTextPtr(a.SourceStatus),
		Card:         a.Card,
		Reason:       a.Reason,
	}
	if a.Parent != nil {
		if id := boundID(a.Parent.Identifier); id != "" {
			out.Parent = &ContextParent{Identifier: id, Title: boundText(a.Parent.Title)}
		}
	}
	if a.Children != nil {
		out.Children = make([]ContextChild, 0, min(len(a.Children), MaxChildren))
		for _, child := range a.Children {
			if len(out.Children) == MaxChildren {
				break
			}
			if id := boundID(child.Identifier); id != "" {
				out.Children = append(out.Children, ContextChild{Identifier: id, Status: boundTextPtr(child.Status)})
			}
		}
	}
	*c = out
	return nil
}

func (c StageContext) clone() StageContext {
	if c.Stage != nil {
		v := *c.Stage
		c.Stage = &v
	}
	if c.Card != nil {
		v := c.Card.clone()
		c.Card = &v
	}
	if c.Parent != nil {
		v := *c.Parent
		c.Parent = &v
	}
	c.SourceStatus = clonePtr(c.SourceStatus)
	if c.Children != nil {
		children := make([]ContextChild, len(c.Children))
		for i, child := range c.Children {
			children[i] = ContextChild{Identifier: child.Identifier, Status: clonePtr(child.Status)}
		}
		c.Children = children
	}
	return c
}

// boundTextPtr bounds an optional status. A value that fails the bound
// becomes unknown (nil), never an empty string that reads as a status.
func boundTextPtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := boundText(*s)
	if v == "" && *s != "" {
		return nil
	}
	return &v
}

func clonePtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}
