package muse

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// mapperState carries the running session totals across notifications
// so turn/completed can emit a ResultEvent with accumulated cost. Cost
// comes from the host: session/tokenUsage arrives per model step with
// cumulative.cost.usd, and turn/completed.usage overrides when present
// (the spec's usage rule). The host prices legs server-side; this
// harness keeps no local pricing table.
type mapperState struct {
	sessionID string
	model     string
	// promptTokens/outputTokens are counted-once session totals folded
	// from session/tokenUsage cumulative blocks.
	promptTokens int64
	outputTokens int64
	// costUSD is the last cumulative.cost.usd observed; partial marks
	// at least one unpriced leg.
	costUSD     float64
	costPartial bool
	hasCost     bool
	turnCount   int
	// completedTurns counts turn/completed terminals observed; the
	// terminal ResultEvent carries it as ObservedTurns.
	completedTurns       int
	seenCompletedTurnIDs map[string]struct{}
	turnStarted          time.Time
	// items tracks open transcript items by itemId for revision-aware
	// application (replace-iff-higher; deltas never bump revision).
	items map[string]*Item
	// viewCursor is the last cursor observed. Gap recovery re-anchors
	// from it.
	viewCursor string
	// lastUsageSource names the event the terminal cost was folded
	// from ("turn/completed" override vs "session/tokenUsage"), for
	// diagnostics.
	lastUsageSource string
}

// mapNotification translates one inbound JSON-RPC notification into
// zero or more agent.Events. Events route by params.sessionId — the
// caller filters to this Handle's session before invoking — and each
// carries a viewCursor the state folds for gap recovery.
func mapNotification(method string, params json.RawMessage, state *mapperState, raw any) []agent.Event {
	switch method {
	// ─── Session lifecycle ──────────────────────────────────────
	case "session/started":
		var p SessionStartedParams
		_ = json.Unmarshal(params, &p)
		if p.Session.SessionID != "" {
			state.sessionID = p.Session.SessionID
			if p.Session.ModelID != "" {
				state.model = p.Session.ModelID
			}
			return []agent.Event{agent.InitEvent{SessionID: p.Session.SessionID, Raw: raw}}
		}
		return nil

	case "session/statusChanged":
		var p SessionStatusChangedParams
		_ = json.Unmarshal(params, &p)
		foldCursor(state, p.ViewCursor)
		msg := "status: " + p.Status
		if len(p.Attention) > 0 {
			msg += " attention=" + strings.Join(p.Attention, ",")
		}
		return []agent.Event{agent.SystemEvent{Subtype: "session_status_changed", Message: msg, Raw: raw}}

	case "session/contextUsage":
		return mapContextUsage(params, state, raw)

	case "session/tokenUsage":
		return mapTokenUsage(params, state, raw)

	case "session/todoListChanged":
		var p SessionTodoListChangedParams
		_ = json.Unmarshal(params, &p)
		foldCursor(state, &p.ViewCursor)
		lines := make([]string, 0, len(p.Items))
		for _, item := range p.Items {
			row := item.Status
			if row == "" {
				row = "todo"
			}
			row += ": " + item.Text
			lines = append(lines, row)
		}
		return []agent.Event{agent.SystemEvent{Subtype: "todo_list_changed", Message: strings.Join(lines, "\n"), Raw: raw}}

	// ─── Turn lifecycle ─────────────────────────────────────────
	case "turn/started":
		var p TurnStartedParams
		_ = json.Unmarshal(params, &p)
		foldCursor(state, &p.ViewCursor)
		state.turnCount++
		state.turnStarted = time.Now()
		return []agent.Event{agent.SystemEvent{
			Subtype: "turn_started",
			Message: fmt.Sprintf("Turn %d started", state.turnCount),
			Raw:     raw,
		}}

	case "turn/foregroundCompleted":
		var p TurnForegroundCompletedParams
		_ = json.Unmarshal(params, &p)
		foldCursor(state, &p.ViewCursor)
		msg := "foreground work done"
		if len(p.BlockingAgents) > 0 {
			msg += "; waiting on " + strings.Join(p.BlockingAgents, ",")
		}
		return []agent.Event{agent.SystemEvent{Subtype: "turn_foreground_completed", Message: msg, Raw: raw}}

	case "turn/completed":
		return mapTurnCompleted(params, state, raw)

	// ─── Item lifecycle (revision-aware) ────────────────────────
	case "item/started":
		var p ItemStartedParams
		_ = json.Unmarshal(params, &p)
		foldCursor(state, &p.ViewCursor)
		return mapItemStarted(p.Item, state, raw)

	case "item/updated":
		var p ItemUpdatedParams
		_ = json.Unmarshal(params, &p)
		foldCursor(state, &p.ViewCursor)
		return mapItemUpdated(p.Item, state, raw)

	case "item/delta":
		var p ItemDeltaParams
		_ = json.Unmarshal(params, &p)
		foldCursor(state, &p.ViewCursor)
		return mapItemDelta(p, state, raw)

	case "item/completed":
		var p ItemCompletedParams
		_ = json.Unmarshal(params, &p)
		foldCursor(state, &p.ViewCursor)
		return mapItemCompleted(p.Item, state, raw)

	// ─── Gap: fatal to the live view ────────────────────────────
	//
	// A view/gap names a hole in push delivery. The live transcript
	// can no longer be trusted: item revisions may have been missed,
	// so replace-iff-higher would apply a stale base. The Handle
	// treats this as fatal (see handle.go recoverFromGap) and the
	// mapper surfaces it as a bearing event so the gap is visible
	// even when recovery succeeds.
	case "view/gap":
		var p ViewGapParams
		_ = json.Unmarshal(params, &p)
		return []agent.Event{agent.SystemEvent{
			Subtype: "view_gap",
			Message: fmt.Sprintf("view gap: events between %q and %q were dropped; recovering", p.After, p.Next),
			Raw:     raw,
		}}

	// ─── Approvals ──────────────────────────────────────────────
	case "approval/requested":
		// Informational fold of the durable Requested record. The
		// decision path runs through handleServerRequest
		// (approval/request) or the approval bridge answering the
		// notification directly; this event keeps the request
		// visible on the activity stream.
		var p ApprovalRequestParams
		_ = json.Unmarshal(params, &p)
		foldCursor(state, stringPtr(p.ViewCursor))
		return []agent.Event{agent.SystemEvent{
			Subtype: "approval_requested",
			Message: describeApproval(p),
			Raw:     raw,
		}}

	case "approval/resolved":
		var p ApprovalResolvedParams
		_ = json.Unmarshal(params, &p)
		foldCursor(state, stringPtr(p.ViewCursor))
		return []agent.Event{agent.SystemEvent{
			Subtype: "approval_resolved",
			Message: fmt.Sprintf("approval %s: %s", p.ApprovalID, p.Decision),
			Raw:     raw,
		}}

	case "approval/updated":
		return []agent.Event{agent.SystemEvent{Subtype: "approval_updated", Message: "approval updated", Raw: raw}}

	case "session/approvalModeChanged", "session/modelChanged", "session/reasoningEffortChanged",
		"session/nameChanged", "session/goalChanged", "session/branchChanged":
		return []agent.Event{agent.SystemEvent{Subtype: subtypeOf(method), Message: method + " changed", Raw: raw}}

	default:
		return []agent.Event{agent.SystemEvent{
			Subtype: "unknown",
			Message: "Unhandled muse notification: " + method,
			Raw:     raw,
		}}
	}
}

func foldCursor(state *mapperState, cursor *string) {
	if state == nil || cursor == nil || *cursor == "" {
		return
	}
	state.viewCursor = *cursor
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// subtypeOf maps an MSP method name to the agent event subtype
// ("session/approvalModeChanged" → "session_approvalModeChanged").
func subtypeOf(method string) string {
	out := make([]byte, 0, len(method))
	for i := 0; i < len(method); i++ {
		if c := method[i]; c == '/' {
			out = append(out, '_')
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}

// mapTurnCompleted handles turn/completed, the only path that emits a
// terminal ResultEvent.
func mapTurnCompleted(params json.RawMessage, state *mapperState, raw any) []agent.Event {
	var p TurnCompletedParams
	_ = json.Unmarshal(params, &p)
	foldCursor(state, &p.ViewCursor)
	if p.TurnID != "" {
		if _, duplicate := state.seenCompletedTurnIDs[p.TurnID]; duplicate {
			return nil
		}
		if state.seenCompletedTurnIDs == nil {
			state.seenCompletedTurnIDs = make(map[string]struct{})
		}
		state.seenCompletedTurnIDs[p.TurnID] = struct{}{}
	}
	state.completedTurns++

	// turn/completed.usage overrides the folded per-step usage when
	// present (the spec's usage rule): it is the turn aggregate.
	if p.Usage != nil {
		state.promptTokens += p.Usage.InputTokens
		state.outputTokens += p.Usage.OutputTokens
		state.lastUsageSource = "turn/completed"
	}

	cost := &agent.CostData{
		InputTokens:  state.promptTokens,
		OutputTokens: state.outputTokens,
		TotalCostUsd: state.costUSD,
		NumTurns:     state.turnCount,
	}
	finishReason := "stop"
	llm := agent.LlmCallEvent{
		System:        "muse",
		Model:         state.model,
		InputTokens:   inputTokensOf(p.Usage),
		OutputTokens:  outputTokensOf(p.Usage),
		FinishReason:  finishReason,
		UsageSource:   agent.LlmUsageProvider,
		TurnCompleted: true,
	}
	ended := time.Now()
	started := state.turnStarted
	if started.IsZero() {
		started = ended
	}
	llm.StartTimeUnixNano = fmt.Sprintf("%d", started.UnixNano())
	llm.EndTimeUnixNano = fmt.Sprintf("%d", ended.UnixNano())
	state.turnStarted = time.Time{}
	completedTurns := state.completedTurns

	switch p.Terminal {
	case "", TurnTerminalCompleted:
		return []agent.Event{llm, agent.ResultEvent{Success: true, Cost: cost, ObservedTurns: &completedTurns, Raw: raw}}
	case TurnTerminalFailed:
		errMsg := "Turn failed"
		if p.Error != nil && p.Error.Message != "" {
			errMsg = p.Error.Message
		} else if p.Reason != "" {
			errMsg = p.Reason
		}
		subtype := "turn_failed"
		if p.Error != nil && p.Error.Kind != "" {
			subtype = p.Error.Kind
		}
		return []agent.Event{llm, agent.ResultEvent{
			Success:       false,
			Errors:        []string{errMsg},
			ErrorSubtype:  subtype,
			Cost:          cost,
			ObservedTurns: &completedTurns,
			Raw:           raw,
		}}
	case TurnTerminalCancelled:
		return []agent.Event{llm, agent.ResultEvent{
			Success:       false,
			Errors:        []string{"Turn was interrupted"},
			ErrorSubtype:  "interrupted",
			Cost:          cost,
			ObservedTurns: &completedTurns,
			Raw:           raw,
		}}
	default:
		return []agent.Event{llm, agent.SystemEvent{
			Subtype: "turn_completed",
			Message: "Turn completed with terminal: " + string(p.Terminal),
			Raw:     raw,
		}}
	}
}

func inputTokensOf(u *TokenUsage) int64 {
	if u == nil {
		return 0
	}
	return u.InputTokens
}

func outputTokensOf(u *TokenUsage) int64 {
	if u == nil {
		return 0
	}
	return u.OutputTokens
}

// mapTokenUsage folds one session/tokenUsage step into the running
// totals and surfaces a UsageEvent carrying the host's own cumulative
// cost. The cumulative block is accumulate-only; cost is the
// documented exception (it tracks price re-adoption).
func mapTokenUsage(params json.RawMessage, state *mapperState, raw any) []agent.Event {
	var p SessionTokenUsageParams
	_ = json.Unmarshal(params, &p)
	foldCursor(state, &p.ViewCursor)
	if p.ModelID != nil && *p.ModelID != "" {
		state.model = *p.ModelID
	}
	state.promptTokens = p.Cumulative.PromptTokens
	state.outputTokens = p.Cumulative.OutputTokens
	if p.Cumulative.Cost != nil {
		state.costUSD = p.Cumulative.Cost.USD
		state.costPartial = p.Cumulative.Cost.Partial
		state.hasCost = true
		state.lastUsageSource = "session/tokenUsage"
	}
	events := []agent.Event{agent.LlmCallEvent{
		System:            "muse",
		Model:             state.model,
		InputTokens:       p.Usage.InputTokens,
		OutputTokens:      p.Usage.OutputTokens,
		CachedInputTokens: p.Usage.CachedTokens,
		CacheWriteTokens:  p.Usage.CacheWriteTokens,
		ReasoningTokens:   p.Usage.ReasoningTokens,
		UsageSource:       agent.LlmUsageProvider,
	}}
	if state.hasCost {
		cost := state.costUSD
		events = append(events, agent.SystemEvent{
			Subtype: "token_usage",
			Message: fmt.Sprintf("session cost usd=%.4f partial=%v", cost, state.costPartial),
			Raw:     raw,
		})
	}
	return events
}

// mapContextUsage translates session/contextUsage into the typed
// harness notice. Window size rides a pointer (nil when the host
// basis has no limit); usedTokens is the honest occupancy floor.
func mapContextUsage(params json.RawMessage, state *mapperState, _ any) []agent.Event {
	var p SessionContextUsageParams
	_ = json.Unmarshal(params, &p)
	foldCursor(state, &p.ViewCursor)
	notice := agent.ContextUsageEvent{
		Harness:    "muse",
		Source:     agent.ContextUsageSourceMuseTokenUsage,
		NoticeKind: agent.ContextUsageThreshold,
	}
	used := p.UsedTokens
	notice.InputTokens = &used
	if p.WindowTokens != nil {
		window := *p.WindowTokens
		notice.ContextWindowSize = &window
		if window > 0 {
			percent := float64(used) / float64(window) * 100
			notice.UsedPercent = &percent
		}
	}
	return []agent.Event{notice}
}

// ─── Revision-aware item application ────────────────────────────────
//
// Items are versioned: replace iff the incoming revision is higher.
// Deltas append to the tracked open item's field but never bump its
// revision. Stale revisions (<= tracked) are dropped — they are
// replays from before a gap fill or out-of-order redelivery.

// mapItemStarted records the open item and emits its first rendering.
func mapItemStarted(item Item, state *mapperState, raw any) []agent.Event {
	if item.ItemID == "" {
		return nil
	}
	if state.items == nil {
		state.items = make(map[string]*Item)
	}
	if tracked, ok := state.items[item.ItemID]; ok && tracked.Revision >= item.Revision {
		return nil
	}
	cp := item
	state.items[item.ItemID] = &cp
	return renderItem(&item, false, raw)
}

// mapItemUpdated replaces the tracked item iff the revision is higher
// and re-renders the full object.
func mapItemUpdated(item Item, state *mapperState, raw any) []agent.Event {
	if item.ItemID == "" {
		return nil
	}
	if state.items == nil {
		state.items = make(map[string]*Item)
	}
	if tracked, ok := state.items[item.ItemID]; ok && tracked.Revision >= item.Revision {
		return nil
	}
	cp := item
	state.items[item.ItemID] = &cp
	return renderItem(&item, false, raw)
}

// mapItemDelta appends a streaming delta to the tracked open item's
// field. Deltas for unknown items are dropped: the authoritative open
// arrives on item/started (or item/completed after a gap fill), and
// buffering unbounded deltas for never-opened items would pin memory
// on a hostile stream.
func mapItemDelta(p ItemDeltaParams, state *mapperState, raw any) []agent.Event {
	if p.ItemID == "" || p.Delta == "" {
		return nil
	}
	tracked, ok := state.items[p.ItemID]
	if !ok || tracked == nil {
		return nil
	}
	field := p.Field
	if field == "" {
		field = "text"
	}
	// Only the text-family fields stream; other field paths are
	// carried by the next item/updated re-emission.
	switch field {
	case "text", "summary", "output":
		tracked.Text += p.Delta
	default:
		return nil
	}
	_ = raw
	return nil
}

// mapItemCompleted applies the authoritative terminal object (accepting
// it even for an itemId never seen started) and emits its terminal
// rendering.
func mapItemCompleted(item Item, state *mapperState, raw any) []agent.Event {
	if item.ItemID == "" {
		return nil
	}
	if state.items == nil {
		state.items = make(map[string]*Item)
	}
	if tracked, ok := state.items[item.ItemID]; ok && tracked.Revision > item.Revision {
		return nil
	}
	cp := item
	state.items[item.ItemID] = &cp
	return renderItem(&item, true, raw)
}

// renderItem translates one item snapshot into agent.Events. The
// completed flag selects the terminal rendering (tool results,
// assistant text); open items render starts (tool uses) and skip
// intermediate states deltas already cover.
func renderItem(item *Item, completed bool, raw any) []agent.Event {
	if item == nil {
		return nil
	}
	switch item.Kind {
	case ItemKindUserMessage, ItemKindReminderChild:
		// The caller owns the input; reminders hold no turn content.
		return nil

	case ItemKindAgentMessage:
		if completed && item.Text != "" {
			return []agent.Event{agent.AssistantTextEvent{Text: item.Text, Raw: raw}}
		}
		return nil

	case ItemKindReasoning:
		if completed {
			if msg := item.Summary; msg != "" || item.Content != "" {
				if msg == "" {
					msg = item.Content
				}
				return []agent.Event{agent.SystemEvent{Subtype: "reasoning", Message: msg, Raw: raw}}
			}
		}
		return nil

	case ItemKindToolCall:
		name, category := toolNameAndCategory(item.Tool)
		if !completed {
			input := map[string]any{}
			if item.Args != "" {
				_ = json.Unmarshal([]byte(item.Args), &input)
			}
			return []agent.Event{agent.ToolUseEvent{
				ToolName:     name,
				ToolUseID:    item.ItemID,
				Input:        input,
				ToolCategory: category,
				Raw:          raw,
			}}
		}
		content := item.Text
		if content == "" {
			content = item.Content
		}
		isError := item.Status == "failed" || item.Status == "rejected" || item.Status == "timedOut"
		if item.ExitCode != nil && *item.ExitCode != 0 && item.Kind == ItemKindToolCall {
			isError = true
		}
		return []agent.Event{agent.ToolResultEvent{
			ToolName:  name,
			ToolUseID: item.ItemID,
			Content:   content,
			IsError:   isError,
			Raw:       raw,
		}}

	case ItemKindUserShell:
		if !completed {
			return []agent.Event{agent.ToolUseEvent{
				ToolName:  "shell",
				ToolUseID: item.ItemID,
				Input:     map[string]any{"command": item.CommandText},
				Raw:       raw,
			}}
		}
		isError := item.Status == "failed" || (item.ExitCode != nil && *item.ExitCode != 0)
		content := item.Text
		if content == "" {
			content = item.Content
		}
		return []agent.Event{agent.ToolResultEvent{
			ToolName:  "shell",
			ToolUseID: item.ItemID,
			Content:   content,
			IsError:   isError,
			Raw:       raw,
		}}

	case ItemKindCompaction:
		return []agent.Event{agent.SystemEvent{Subtype: "context_compaction", Message: compactionMessage(item), Raw: raw}}

	case ItemKindSubagent:
		phase := agent.SubagentStarted
		if completed {
			phase = agent.SubagentCompleted
			if item.Status == "failed" {
				phase = agent.SubagentFailed
			}
		}
		return []agent.Event{agent.SubagentEvent{
			ToolName:       "subagent",
			ToolUseID:      item.ItemID,
			Phase:          phase,
			ChildSessionID: item.ChildSessionID,
			Raw:            raw,
		}}

	case ItemKindWorkflow:
		msg := item.Text
		if msg == "" {
			msg = item.Summary
		}
		if msg == "" {
			msg = item.FallbackText
		}
		return []agent.Event{agent.SystemEvent{Subtype: "workflow", Message: msg, Raw: raw}}

	default:
		// Open-enum evolution: render unknown kinds generically, never
		// drop them silently.
		msg := item.Text
		if msg == "" {
			msg = item.FallbackText
		}
		if msg == "" {
			msg = item.Summary
		}
		return []agent.Event{agent.SystemEvent{
			Subtype: "unknown_item",
			Message: "Unhandled muse item kind: " + string(item.Kind) + ": " + msg,
			Raw:     raw,
		}}
	}
}

// toolNameAndCategory maps the MSP tool name to the runner-facing tool
// name + category. File edits are toolCalls named write|edit|patch;
// they surface under the filesystem category so the runner's
// code-intelligence accounting sees them.
func toolNameAndCategory(tool string) (string, string) {
	name := strings.TrimSpace(tool)
	if name == "" {
		name = "unknown"
	}
	lower := strings.ToLower(name)
	switch {
	case lower == "write" || lower == "edit" || lower == "patch":
		return name, "filesystem"
	case strings.Contains(lower, "shell") || strings.Contains(lower, "bash") || strings.Contains(lower, "exec"):
		return name, "shell"
	case strings.HasPrefix(lower, "mcp__"):
		return name, ""
	default:
		return name, ""
	}
}

func compactionMessage(item *Item) string {
	if item.Summary != "" {
		return "Context history compacted: " + item.Summary
	}
	return "Context history compacted"
}

func describeApproval(p ApprovalRequestParams) string {
	var b strings.Builder
	b.WriteString("approval requested")
	if p.ToolName != "" {
		b.WriteString(": " + p.ToolName)
	}
	subject := p.Subject
	switch {
	case subject.Command != "":
		b.WriteString(" " + subject.Command)
	case subject.Path != "":
		b.WriteString(" " + subject.Path)
	case subject.Target != "":
		b.WriteString(" " + subject.Target)
	}
	if p.ApprovalID != "" {
		b.WriteString(" (approval " + p.ApprovalID + ")")
	}
	return b.String()
}
