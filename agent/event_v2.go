package agent

import (
	"encoding/json"
	"fmt"
)

// Event model v2 kinds (ADR-2026-11-20 harness interaction contract, D4).
// They are additive: each promotes a shape adapters used to smuggle through
// SystemEvent subtypes, so consumers stop re-parsing free-form messages.
const (
	EventReasoning          EventKind = "reasoning"
	EventPlan               EventKind = "plan"
	EventFileChange         EventKind = "file_change"
	EventInteractiveRequest EventKind = "interactive_request"
	EventTurn               EventKind = "turn"
	EventNotice             EventKind = "notice"
)

// ItemStatus is the lifecycle of one unit of work (a tool call, a file
// change, a plan). Consumers hold per-item state keyed by item id; progress
// and deltas update that state and never append.
type ItemStatus string

// Item statuses. Stable wire values.
const (
	ItemStarted     ItemStatus = "started"
	ItemRunning     ItemStatus = "running"
	ItemCompleted   ItemStatus = "completed"
	ItemFailed      ItemStatus = "failed"
	ItemInterrupted ItemStatus = "interrupted"
)

// AssistantPhase separates working commentary from the final answer on
// AssistantTextEvent. Empty means the adapter cannot tell.
type AssistantPhase string

// Assistant phases. Stable wire values.
const (
	AssistantCommentary AssistantPhase = "commentary"
	AssistantFinal      AssistantPhase = "final"
)

// ReasoningEvent is the model's reasoning, distinct from assistant text.
// Summary is the harness's own summary when it sends one; Text the full
// reasoning when it is exposed. Either may be empty, not both.
type ReasoningEvent struct {
	ItemID  string `json:"itemId,omitempty"`
	Text    string `json:"text,omitempty"`
	Summary string `json:"summary,omitempty"`
	Raw     any    `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (ReasoningEvent) Kind() EventKind { return EventReasoning }
func (ReasoningEvent) isAgentEvent()   {}

// PlanStepStatus is one plan step's progress. Stable wire values.
type PlanStepStatus string

// Plan step statuses.
const (
	PlanStepPending    PlanStepStatus = "pending"
	PlanStepInProgress PlanStepStatus = "in_progress"
	PlanStepCompleted  PlanStepStatus = "completed"
)

// PlanStep is one step of a plan or todo list.
type PlanStep struct {
	Text   string         `json:"text"`
	Status PlanStepStatus `json:"status"`
}

// PlanEvent carries the whole current plan. It replaces any earlier plan
// with the same ItemID; it is never a delta.
type PlanEvent struct {
	ItemID      string     `json:"itemId,omitempty"`
	Explanation string     `json:"explanation,omitempty"`
	Steps       []PlanStep `json:"steps"`
	Raw         any        `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (PlanEvent) Kind() EventKind { return EventPlan }
func (PlanEvent) isAgentEvent()   {}

// FileChangeKind is how one file changed. Stable wire values.
type FileChangeKind string

// File change kinds.
const (
	FileAdded   FileChangeKind = "add"
	FileUpdated FileChangeKind = "update"
	FileDeleted FileChangeKind = "delete"
	FileMoved   FileChangeKind = "move"
)

// FileChange is one file of a FileChangeEvent. Additions and Deletions
// count diff lines; nil means unknown, never zero.
type FileChange struct {
	Path      string         `json:"path"`
	Change    FileChangeKind `json:"change"`
	MovePath  string         `json:"movePath,omitempty"`
	Additions *int           `json:"additions,omitempty"`
	Deletions *int           `json:"deletions,omitempty"`
	Diff      string         `json:"diff,omitempty"`
}

// FileChangeEvent reports files a tool call changed. ItemID is the tool-use
// id of the call that made the change, so it pairs with that call's rows.
type FileChangeEvent struct {
	ItemID string       `json:"itemId,omitempty"`
	Status ItemStatus   `json:"status,omitempty"`
	Files  []FileChange `json:"files"`
	Raw    any          `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (FileChangeEvent) Kind() EventKind { return EventFileChange }
func (FileChangeEvent) isAgentEvent()   {}

// InteractiveRequestKind distinguishes an approval from a question.
type InteractiveRequestKind string

// Interactive request kinds. Stable wire values.
const (
	RequestApproval InteractiveRequestKind = "approval"
	RequestQuestion InteractiveRequestKind = "question"
)

// InteractiveRequestEvent is a harness asking for a decision before it
// continues. RequestID is what the respond verb answers; ItemID links it to
// the tool call it gates, when there is one.
type InteractiveRequestEvent struct {
	RequestID   string                 `json:"requestId"`
	ItemID      string                 `json:"itemId,omitempty"`
	RequestKind InteractiveRequestKind `json:"requestKind"`
	Prompt      string                 `json:"prompt"`
	Options     []string               `json:"options,omitempty"`
	Raw         any                    `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (InteractiveRequestEvent) Kind() EventKind { return EventInteractiveRequest }
func (InteractiveRequestEvent) isAgentEvent()   {}

// TurnPhase is a turn boundary. Stable wire values.
type TurnPhase string

// Turn phases.
const (
	TurnStarted   TurnPhase = "started"
	TurnCompleted TurnPhase = "completed"
)

// TurnEvent marks a turn boundary. TurnID is the harness's own id when it
// has one; Status is set on completion (completed, failed, interrupted).
type TurnEvent struct {
	TurnID string     `json:"turnId,omitempty"`
	Phase  TurnPhase  `json:"phase"`
	Status ItemStatus `json:"status,omitempty"`
	Raw    any        `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (TurnEvent) Kind() EventKind { return EventTurn }
func (TurnEvent) isAgentEvent()   {}

// NoticeSeverity grades a notice. Stable wire values.
type NoticeSeverity string

// Notice severities.
const (
	NoticeInfo    NoticeSeverity = "info"
	NoticeWarning NoticeSeverity = "warning"
	NoticeBlocked NoticeSeverity = "blocked"
)

// NoticeEvent is a one-line, reader-facing harness notice (a model reroute,
// a safety block, a compaction). Text is always human-readable, never raw
// payload JSON; Code is a stable machine identifier when the adapter has one.
type NoticeEvent struct {
	Severity NoticeSeverity `json:"severity"`
	Text     string         `json:"text"`
	Code     string         `json:"code,omitempty"`
	Raw      any            `json:"raw,omitempty"`
}

// Kind reports the EventKind discriminant.
func (NoticeEvent) Kind() EventKind { return EventNotice }
func (NoticeEvent) isAgentEvent()   {}

// isV2Kind reports an event model v2 kind.
func isV2Kind(kind EventKind) bool {
	switch kind {
	case EventReasoning, EventPlan, EventFileChange, EventInteractiveRequest, EventTurn, EventNotice, EventEffectiveSettings:
		return true
	}
	return false
}

// unmarshalV2Event decodes a v2 kind; ok is false for any other kind.
func unmarshalV2Event(kind EventKind, data []byte) (ev Event, ok bool, err error) {
	switch kind {
	case EventReasoning:
		return decodeV2[ReasoningEvent](data, "ReasoningEvent")
	case EventPlan:
		return decodeV2[PlanEvent](data, "PlanEvent")
	case EventFileChange:
		return decodeV2[FileChangeEvent](data, "FileChangeEvent")
	case EventInteractiveRequest:
		return decodeV2[InteractiveRequestEvent](data, "InteractiveRequestEvent")
	case EventTurn:
		return decodeV2[TurnEvent](data, "TurnEvent")
	case EventNotice:
		return decodeV2[NoticeEvent](data, "NoticeEvent")
	case EventEffectiveSettings:
		return decodeV2[EffectiveSettingsEvent](data, "EffectiveSettingsEvent")
	}
	return nil, false, nil
}

func decodeV2[T Event](data []byte, name string) (Event, bool, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, true, fmt.Errorf("agent: decode %s: %w", name, err)
	}
	return v, true, nil
}

// Summary renders the plan as one line: "Plan 2/5 · <step in progress>",
// falling back to the first pending step, or "Plan 5/5" when all are done.
func (e PlanEvent) Summary() string {
	done, current := 0, ""
	for _, s := range e.Steps {
		switch s.Status {
		case PlanStepCompleted:
			done++
		case PlanStepInProgress:
			if current == "" {
				current = s.Text
			}
		}
	}
	if current == "" {
		for _, s := range e.Steps {
			if s.Status != PlanStepCompleted {
				current = s.Text
				break
			}
		}
	}
	out := fmt.Sprintf("Plan %d/%d", done, len(e.Steps))
	if current != "" {
		out += " · " + current
	}
	return out
}

// Summary renders the change as one line: "update a.go (+3 −1)" for one
// file, "3 files changed (+10 −2)" for several. Unknown counts are omitted.
func (e FileChangeEvent) Summary() string {
	var adds, dels int
	known := len(e.Files) > 0
	for _, f := range e.Files {
		if f.Additions == nil || f.Deletions == nil {
			known = false
			continue
		}
		adds += *f.Additions
		dels += *f.Deletions
	}
	var out string
	switch len(e.Files) {
	case 0:
		return "no files changed"
	case 1:
		f := e.Files[0]
		out = string(f.Change) + " " + f.Path
		if f.MovePath != "" {
			out += " -> " + f.MovePath
		}
	default:
		out = fmt.Sprintf("%d files changed", len(e.Files))
	}
	if known {
		out += fmt.Sprintf(" (+%d −%d)", adds, dels)
	}
	return out
}
