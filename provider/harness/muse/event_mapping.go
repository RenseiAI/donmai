package muse

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// mapperState carries the running session totals across notifications
// so turn/completed can emit a ResultEvent with accumulated cost. Cost
// comes from the host's session/tokenUsage cumulative.cost.usd when the
// host prices the legs (absent on unpriced subscription hosts — then no
// cost is reported, never a fabricated zero); the turn/completed usage
// block overrides the token counters when present.
type mapperState struct {
	sessionID string
	model     string

	turnCount      int
	completedTurns int
	turnStarted    map[string]int64 // turnId → start wall ms (observed)

	// totalCostUSD is the last cumulative.cost.usd observed. The
	// host's cost member is the documented exception to
	// accumulate-only: it may move either way on price re-adoption.
	totalCostUSD   float64
	lastCostUSD    float64
	hasCost        bool
	costPartial    bool
	totalInputTok  int64
	totalOutputTok int64
	totalCachedTok int64
	cacheWriteTok  int64
	reasoningTok   int64

	// items tracks the last revision applied per itemId. Deltas and
	// updates at a stale revision are dropped; completion always
	// applies (the host guarantees item/completed is authoritative,
	// including for itemIds never opened on this connection).
	items     map[string]int
	textItems map[string]textItem

	// pendingToolUse records ToolUseEvents emitted for open toolCall /
	// userShell items so completion can pair the result.
	pendingToolUse map[string]pendingTool

	// gap is set when a view/gap notification marks the stream
	// inconsistent. While set, item events are held until the handle
	// recovers (resume re-anchor); turn/completed still applies.
	gap *ViewGapParams

	// seenTurns deduplicates turn/completed deliveries.
	seenTurns map[string]struct{}
}

type textItem struct {
	kind  ItemKind
	text  string
	field string
}

type pendingTool struct {
	name string
	id   string
}

func newMapperState(model string) *mapperState {
	return &mapperState{
		model:          model,
		textItems:      make(map[string]textItem),
		turnStarted:    make(map[string]int64),
		items:          make(map[string]int),
		pendingToolUse: make(map[string]pendingTool),
		seenTurns:      make(map[string]struct{}),
	}
}

// mapNotification translates one inbound JSON-RPC notification into
// zero or more agent.Events. Server-requests (approval/request,
// userInput/request) never reach here — the handle answers those
// directly.
func mapNotification(method string, params json.RawMessage, state *mapperState, raw any) []agent.Event {
	switch method {
	// ─── Session lifecycle ──────────────────────────────────
	case "session/started":
		var p SessionStartedParams
		_ = json.Unmarshal(params, &p)
		if p.Session.SessionID != "" {
			state.sessionID = p.Session.SessionID
			return []agent.Event{agent.InitEvent{SessionID: p.Session.SessionID, Raw: raw}}
		}
		return nil

	case "session/statusChanged":
		var p SessionStatusChangedParams
		_ = json.Unmarshal(params, &p)
		msg := fmt.Sprintf("Session %s: %s", p.SessionID, p.Status)
		return []agent.Event{agent.SystemEvent{Subtype: "session_status_changed", Message: msg, Raw: raw}}

	case "session/closed":
		return []agent.Event{agent.SystemEvent{Subtype: "session_closed", Raw: raw}}

	// ─── Turn lifecycle ─────────────────────────────────────
	case "turn/started":
		var p TurnStartedParams
		_ = json.Unmarshal(params, &p)
		state.turnCount++
		return []agent.Event{agent.SystemEvent{
			Subtype: "turn_started",
			Message: fmt.Sprintf("Turn %d started", state.turnCount),
			Raw:     raw,
		}}

	case "turn/foregroundCompleted":
		var p TurnForegroundCompletedParams
		_ = json.Unmarshal(params, &p)
		return []agent.Event{agent.SystemEvent{
			Subtype: "turn_foreground_completed",
			Message: fmt.Sprintf("Turn foreground completed (held by %d background checks)", len(p.BlockingAgents)),
			Raw:     raw,
		}}

	case "turn/completed":
		return mapTurnCompleted(params, state, raw)

	case "turn/unqueued", "turn/retracted", "turn/retryScheduled":
		return []agent.Event{agent.SystemEvent{Subtype: methodToSubtype(method), Message: "Turn update: " + method, Raw: raw}}

	// ─── Item lifecycle (revision-aware) ────────────────────
	case "item/started", "item/updated", "item/completed":
		return mapItemEvent(method, params, state, raw)

	case "item/delta":
		return mapItemDelta(params, state, raw)

	// ─── Usage ──────────────────────────────────────────────
	case "session/tokenUsage":
		return mapTokenUsage(params, state, raw)

	case "session/contextUsage":
		var p SessionContextUsageParams
		_ = json.Unmarshal(params, &p)
		used := p.UsedTokens
		window := int64(0)
		if p.WindowTokens != nil {
			window = *p.WindowTokens
		}
		notice := &agent.ContextUsageEvent{
			Harness:    string(agent.HarnessMuse),
			Source:     agent.ContextUsageSourceMuseTokenUsage,
			NoticeKind: agent.ContextUsageThreshold,
			Raw:        raw,
		}
		notice.InputTokens = &used
		if window > 0 {
			notice.ContextWindowSize = &window
			pct := float64(used) / float64(window) * 100
			notice.UsedPercent = &pct
		}
		return []agent.Event{*notice}

	case "usage/changed":
		return []agent.Event{agent.SystemEvent{Subtype: "subscription_usage_changed", Raw: raw}}

	case "session/todoListChanged":
		var p SessionTodoListChangedParams
		_ = json.Unmarshal(params, &p)
		lines := make([]string, 0, len(p.Items))
		for _, it := range p.Items {
			lines = append(lines, it.Status+": "+it.Text)
		}
		return []agent.Event{agent.SystemEvent{
			Subtype: "todo_list_changed",
			Message: strings.Join(lines, "\n"),
			Raw:     raw,
		}}

	// ─── Approvals (view fold) ──────────────────────────────
	case "approval/requested":
		var p ApprovalRequestParams
		_ = json.Unmarshal(params, &p)
		return []agent.Event{agent.SystemEvent{
			Subtype: "approval_requested",
			Message: fmt.Sprintf("Approval requested: %s (%s)", p.ToolName, p.ApprovalID),
			Raw:     raw,
		}}

	case "approval/resolved":
		var p ApprovalResolvedParams
		_ = json.Unmarshal(params, &p)
		return []agent.Event{agent.SystemEvent{
			Subtype: "approval_resolved",
			Message: fmt.Sprintf("Approval %s: %s", p.ApprovalID, p.Decision),
			Raw:     raw,
		}}

	case "approval/updated":
		return []agent.Event{agent.SystemEvent{Subtype: "approval_updated", Raw: raw}}

	case "session/approvalModeChanged":
		// Observed live: the host announces approval-mode transitions
		// (e.g. after session/start applies the requested mode).
		// Recorded, never terminal.
		return []agent.Event{agent.SystemEvent{Subtype: "approval_mode_changed", Message: "Muse approval mode changed", Raw: raw}}

	// ─── Gap: fatal, the handle must recover ────────────────
	case "view/gap":
		var p ViewGapParams
		_ = json.Unmarshal(params, &p)
		state.gap = &p
		// SessionContinues: the handle re-anchors via session/resume
		// and the session ends on its own terminal, so the gap is on
		// the record but never the run's failure.
		return []agent.Event{agent.ErrorEvent{
			Message:          fmt.Sprintf("muse view gap: events (%s, %s] were dropped; session must recover", p.After, p.Next),
			Code:             "view_gap",
			SessionContinues: true,
		}}

	default:
		return []agent.Event{agent.SystemEvent{
			Subtype: "unknown",
			Message: "Unhandled muse notification: " + method,
			Raw:     raw,
		}}
	}
}

func methodToSubtype(m string) string {
	return strings.ReplaceAll(m, "/", "_")
}

// mapTurnCompleted handles turn/completed, the only path that emits a
// terminal ResultEvent. terminal completed → success; cancelled →
// interrupted failure; failed → failure with the host's error text.
// turn/completed.usage overrides the accumulated counters when present.
func mapTurnCompleted(params json.RawMessage, state *mapperState, raw any) []agent.Event {
	var p TurnCompletedParams
	_ = json.Unmarshal(params, &p)
	if p.TurnID != "" {
		if _, dup := state.seenTurns[p.TurnID]; dup {
			return nil
		}
		if state.seenTurns == nil {
			state.seenTurns = make(map[string]struct{})
		}
		state.seenTurns[p.TurnID] = struct{}{}
	}
	state.completedTurns++
	completedTurns := state.completedTurns
	delete(state.turnStarted, p.TurnID)

	in, out, cached, cacheWrite, reasoning := state.totalInputTok, state.totalOutputTok, state.totalCachedTok, state.cacheWriteTok, state.reasoningTok
	if p.Usage != nil {
		in = p.Usage.InputTokens
		out = p.Usage.OutputTokens
		cached = p.Usage.CachedTokens
		if p.Usage.CacheReadTokens != nil {
			cached = *p.Usage.CacheReadTokens
		}
		if p.Usage.CacheWriteTokens != nil {
			cacheWrite = *p.Usage.CacheWriteTokens
		}
		reasoning = p.Usage.ReasoningTokens
	}
	cost := &agent.CostData{
		InputTokens:       in,
		OutputTokens:      out,
		CachedInputTokens: cached,
		CacheWriteTokens:  cacheWrite,
		ReasoningTokens:   reasoning,
		NumTurns:          state.turnCount,
	}
	var observedCost *float64
	if state.hasCost {
		c := state.totalCostUSD
		observedCost = &c
		cost.TotalCostUsd = state.totalCostUSD
	}
	llm := agent.LlmCallEvent{
		System:            "muse",
		Model:             state.model,
		InputTokens:       in,
		OutputTokens:      out,
		CachedInputTokens: cached,
		FinishReason:      string(p.Terminal),
		UsageSource:       agent.LlmUsageAggregate,
		TurnCompleted:     true,
		CacheWriteTokens:  cacheWrite,
		ReasoningTokens:   reasoning,
	}

	switch p.Terminal {
	case TurnTerminalCompleted:
		return []agent.Event{llm, agent.ResultEvent{
			Success:         true,
			Cost:            cost,
			ObservedCostUsd: observedCost,
			ObservedTurns:   &completedTurns,
			Raw:             raw,
		}}
	case TurnTerminalCancelled:
		errs := []string{"Turn was interrupted"}
		if p.Reason != nil && *p.Reason != "" {
			errs = []string{*p.Reason}
		}
		return []agent.Event{llm, agent.ResultEvent{
			Success:         false,
			Errors:          errs,
			ErrorSubtype:    "interrupted",
			Cost:            cost,
			ObservedCostUsd: observedCost,
			ObservedTurns:   &completedTurns,
			Raw:             raw,
		}}
	case TurnTerminalFailed:
		msg := "Turn failed"
		kind := "turn_failed"
		if p.Error != nil {
			if p.Error.Message != "" {
				msg = p.Error.Message
			}
			if p.Error.Kind != "" {
				kind = p.Error.Kind
			}
		} else if p.Reason != nil && *p.Reason != "" {
			msg = *p.Reason
		}
		return []agent.Event{llm, agent.ResultEvent{
			Success:         false,
			Errors:          []string{msg},
			ErrorSubtype:    kind,
			Cost:            cost,
			ObservedCostUsd: observedCost,
			ObservedTurns:   &completedTurns,
			Raw:             raw,
		}}
	default:
		return []agent.Event{llm, agent.SystemEvent{
			Subtype: "turn_completed",
			Message: "Turn completed with terminal: " + string(p.Terminal),
			Raw:     raw,
		}}
	}
}

// mapTokenUsage emits step usage while retaining cumulative terminal totals.
// Cost may be re-priced downward; only positive changes are new spend.
func mapTokenUsage(params json.RawMessage, state *mapperState, _ any) []agent.Event {
	var p struct {
		SessionTokenUsageParams
		Usage *TokenUsage `json:"usage"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil
	}
	usage := TokenUsage{
		InputTokens:  max(p.Cumulative.PromptTokens-state.totalInputTok, 0),
		OutputTokens: max(p.Cumulative.OutputTokens-state.totalOutputTok, 0),
	}
	cached, cacheWrite := state.totalCachedTok, state.cacheWriteTok
	if p.Cumulative.CacheReadTokens != nil {
		cached = *p.Cumulative.CacheReadTokens
	}
	if p.Cumulative.CacheWriteTokens != nil {
		cacheWrite = *p.Cumulative.CacheWriteTokens
	}
	readDelta, writeDelta := max(cached-state.totalCachedTok, 0), max(cacheWrite-state.cacheWriteTok, 0)
	usage.CachedTokens = readDelta
	usage.CacheWriteTokens = &writeDelta
	if p.Usage != nil {
		usage = *p.Usage
	}
	read := usage.CachedTokens
	if usage.CacheReadTokens != nil {
		read = *usage.CacheReadTokens
	}
	write := int64(0)
	if usage.CacheWriteTokens != nil {
		write = *usage.CacheWriteTokens
	}
	state.totalInputTok = p.Cumulative.PromptTokens
	state.totalOutputTok = p.Cumulative.OutputTokens
	if p.Cumulative.CacheReadTokens == nil {
		cached = state.totalCachedTok + read
	}
	if p.Cumulative.CacheWriteTokens == nil {
		cacheWrite = state.cacheWriteTok + write
	}
	state.totalCachedTok = cached
	state.cacheWriteTok = cacheWrite
	state.reasoningTok += usage.ReasoningTokens
	var observedCost *float64
	if p.Cumulative.Cost != nil {
		delta := max(p.Cumulative.Cost.USD-state.lastCostUSD, 0)
		observedCost = &delta
		state.lastCostUSD = p.Cumulative.Cost.USD
		state.totalCostUSD = p.Cumulative.Cost.USD
		state.hasCost = true
		state.costPartial = p.Cumulative.Cost.Partial
	}
	if p.ModelID != nil && *p.ModelID != "" {
		state.model = *p.ModelID
	}
	finishReason := ""
	if p.FinishReason != nil {
		finishReason = *p.FinishReason
	}
	return []agent.Event{agent.LlmCallEvent{
		System: "muse", Model: state.model,
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CachedInputTokens: read, CacheWriteTokens: write,
		ReasoningTokens: usage.ReasoningTokens, FinishReason: finishReason,
		UsageSource: agent.LlmUsageProvider, ObservedCostUsd: observedCost,
	}}
}

// mapItemEvent handles item/started|updated|completed with the
// replace-iff-higher revision rule. While a view gap is open, item
// events are dropped (the stream is inconsistent until the handle
// recovers); item/completed always applies since it is authoritative.
func mapItemEvent(method string, params json.RawMessage, state *mapperState, raw any) []agent.Event {
	var envelope struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil || len(envelope.Item) == 0 {
		return nil
	}
	var item Item
	if err := json.Unmarshal(envelope.Item, &item); err != nil || item.ItemID == "" {
		return nil
	}
	item.Raw = envelope.Item

	isCompleted := method == "item/completed"
	if state.gap != nil && !isCompleted {
		return nil
	}
	if last, ok := state.items[item.ItemID]; ok && item.Revision <= last && !isCompleted {
		return nil // stale revision: drop
	}
	state.items[item.ItemID] = item.Revision

	switch item.Kind {
	case ItemKindUserMessage, ItemKindReminderChild:
		// The caller owns this input; forwarding would duplicate the
		// prompt in the activity stream.
		return nil
	case ItemKindAgentMessage, ItemKindReasoning:
		text := itemText(&item)
		if item.Kind == ItemKindReasoning {
			text = reasoningText(&item)
		}
		previous := state.textItems[item.ItemID]
		// Snapshots are authoritative; forward only the unseen suffix when
		// they extend the streaming text. A replacement is emitted in full.
		unseen := strings.TrimPrefix(text, previous.text)
		switch {
		case isCompleted:
			delete(state.textItems, item.ItemID)
		case len(text) <= 1<<20 && (previous.kind != "" || len(state.textItems) < 1024):
			field := "text"
			if item.Kind == ItemKindReasoning {
				var parts []string
				if json.Unmarshal(item.Summary, &parts) == nil && len(parts) > 0 {
					field = fmt.Sprintf("summary.%d", len(parts)-1)
				}
			}
			state.textItems[item.ItemID] = textItem{kind: item.Kind, text: text, field: field}
		default:
			delete(state.textItems, item.ItemID)
		}
		return textEvents(item.Kind, unseen, raw)
	case ItemKindToolCall:
		return mapToolCall(item, method, isCompleted, state, raw)
	case ItemKindUserShell:
		return mapUserShell(item, method, isCompleted, state, raw)
	case ItemKindCompaction:
		if isCompleted {
			msg := "Context history compacted"
			if item.Outcome != nil && *item.Outcome != "" {
				msg += ": " + *item.Outcome
			}
			return []agent.Event{agent.SystemEvent{Subtype: "context_compaction", Message: msg, Raw: raw}}
		}
		return nil
	case ItemKindSubagent:
		return mapSubagent(item, method, isCompleted, state, raw)
	case ItemKindWorkflow:
		msg := "Workflow update"
		switch {
		case item.FinalSummary != nil && *item.FinalSummary != "":
			msg = *item.FinalSummary
		case item.Message != nil && *item.Message != "":
			msg = *item.Message
		case item.FallbackText != nil && *item.FallbackText != "":
			msg = *item.FallbackText
		}
		return []agent.Event{agent.SystemEvent{Subtype: "workflow", Message: msg, Raw: raw}}
	default:
		// Open enum: render unknown kinds generically, never drop.
		msg := "Unhandled muse item kind: " + string(item.Kind)
		if item.FallbackText != nil && *item.FallbackText != "" {
			msg += ": " + *item.FallbackText
		}
		return []agent.Event{agent.SystemEvent{Subtype: "unknown_item", Message: msg, Raw: raw}}
	}
}

// mapItemDelta forwards UTF-8 text appends for known open text items.
// Unknown items/fields stay silent: a delta carries no item kind. Completion
// still supplies the authoritative snapshot if the stream was saturated.
func mapItemDelta(params json.RawMessage, state *mapperState, raw any) []agent.Event {
	var p ItemDeltaParams
	if json.Unmarshal(params, &p) != nil || p.ItemID == "" || state.gap != nil {
		return nil
	}
	item, ok := state.textItems[p.ItemID]
	if !ok {
		return nil
	}
	field := strVal(p.Field)
	if field != "" && field != "text" && !(item.kind == ItemKindReasoning && strings.HasPrefix(field, "summary.")) {
		return nil
	}
	delta := p.Delta
	if item.kind == ItemKindReasoning && strings.HasPrefix(field, "summary.") && item.field != field && item.text != "" {
		delta = "\n" + delta
	}
	// Bound retained text while preserving liveness for oversized streams.
	if len(item.text)+len(delta) > 1<<20 {
		return []agent.Event{agent.StreamProgressEvent()}
	}
	item.field = field
	item.text += delta
	state.textItems[p.ItemID] = item
	return textEvents(item.kind, delta, raw)
}

func textEvents(kind ItemKind, text string, raw any) []agent.Event {
	if text == "" {
		return nil
	}
	if kind == ItemKindReasoning {
		return []agent.Event{agent.SystemEvent{Subtype: "reasoning", Message: text, Raw: raw}}
	}
	return []agent.Event{agent.AssistantTextEvent{Text: text, Raw: raw}}
}

func itemText(item *Item) string {
	if item.Text != nil {
		return *item.Text
	}
	return ""
}

func reasoningText(item *Item) string {
	// Current hosts export summary as an array of strings. Older hosts
	// used text-bearing parts; retain that decoding for existing sessions.
	var summary []string
	if json.Unmarshal(item.Summary, &summary) == nil && len(summary) > 0 {
		return strings.Join(summary, "\n")
	}
	if len(item.Summary) > 0 {
		var parts []struct {
			Text *string `json:"text"`
		}
		if json.Unmarshal(item.Summary, &parts) == nil {
			var texts []string
			for _, p := range parts {
				if p.Text != nil && *p.Text != "" {
					texts = append(texts, *p.Text)
				}
			}
			if len(texts) > 0 {
				return strings.Join(texts, "\n")
			}
		}
		// Fall back to the raw summary JSON when it is not the
		// expected shape.
		var single struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(item.Summary, &single) == nil && single.Text != "" {
			return single.Text
		}
	}
	if item.Text != nil {
		return *item.Text
	}
	return ""
}

// mapToolCall maps toolCall items. File edits (write|edit|patch) ride
// the filesystem category; started emits ToolUse, completed emits the
// paired ToolResult.
func mapToolCall(item Item, method string, isCompleted bool, state *mapperState, raw any) []agent.Event {
	tool := ""
	if item.Tool != nil {
		tool = *item.Tool
	}
	if tool == "" {
		tool = "unknown"
	}
	isStarted := method == "item/started"
	if isStarted {
		input := map[string]any{}
		if item.Args != nil && *item.Args != "" {
			_ = json.Unmarshal([]byte(*item.Args), &input)
		}
		state.pendingToolUse[item.ItemID] = pendingTool{name: tool, id: item.ItemID}
		return []agent.Event{agent.ToolUseEvent{
			ToolName:     tool,
			ToolUseID:    item.ItemID,
			Input:        input,
			ToolCategory: classifyTool(tool),
			Raw:          raw,
		}}
	}
	if isCompleted {
		name := tool
		if pending, ok := state.pendingToolUse[item.ItemID]; ok {
			name = pending.name
			delete(state.pendingToolUse, item.ItemID)
		}
		isError := item.Status == ItemStatusFailed || item.Status == ItemStatusRejected ||
			item.Status == ItemStatusCancelled || item.Status == ItemStatusTimedOut
		content := ""
		switch {
		case item.FailureReason != nil:
			content = *item.FailureReason
		case item.VisibleOutput != nil:
			content = *item.VisibleOutput
		case item.FallbackText != nil:
			content = *item.FallbackText
		}
		return []agent.Event{agent.ToolResultEvent{
			ToolName:  name,
			ToolUseID: item.ItemID,
			Content:   content,
			IsError:   isError,
			Raw:       raw,
		}}
	}
	// item/updated on an open tool call: surface the status line as
	// progress without emitting a duplicate ToolUse.
	return nil
}

// mapUserShell maps userShell items (submitted commands + their results).
func mapUserShell(item Item, method string, isCompleted bool, state *mapperState, raw any) []agent.Event {
	isStarted := method == "item/started"
	if isStarted {
		cmd := ""
		if item.CommandText != nil {
			cmd = *item.CommandText
		}
		state.pendingToolUse[item.ItemID] = pendingTool{name: "shell", id: item.ItemID}
		return []agent.Event{agent.ToolUseEvent{
			ToolName:  "shell",
			ToolUseID: item.ItemID,
			Input:     map[string]any{"command": cmd},
			Raw:       raw,
		}}
	}
	if isCompleted {
		name := "shell"
		if pending, ok := state.pendingToolUse[item.ItemID]; ok {
			name = pending.name
			delete(state.pendingToolUse, item.ItemID)
		}
		isError := item.Status == ItemStatusFailed || item.Status == ItemStatusRejected ||
			item.Status == ItemStatusCancelled || item.Status == ItemStatusTimedOut ||
			(item.ExitCode != nil && *item.ExitCode != 0)
		content := ""
		if item.FailureReason != nil {
			content = *item.FailureReason
		} else if item.VisibleOutput != nil {
			content = *item.VisibleOutput
		}
		return []agent.Event{agent.ToolResultEvent{
			ToolName:  name,
			ToolUseID: item.ItemID,
			Content:   content,
			IsError:   isError,
			Raw:       raw,
		}}
	}
	return nil
}

// mapSubagent maps subagent items to SubagentEvent lifecycles plus the
// delegating tool events.
func mapSubagent(item Item, method string, isCompleted bool, state *mapperState, raw any) []agent.Event {
	tool := "subagent"
	if item.Tool != nil && *item.Tool != "" {
		tool = *item.Tool
	}
	child := ""
	if item.ChildSessionID != nil {
		child = *item.ChildSessionID
	} else if item.SubagentID != nil {
		child = *item.SubagentID
	}
	isStarted := method == "item/started"
	if isStarted {
		state.pendingToolUse[item.ItemID] = pendingTool{name: tool, id: item.ItemID}
		return []agent.Event{
			agent.ToolUseEvent{ToolName: tool, ToolUseID: item.ItemID, Input: map[string]any{"objective": strVal(item.Objective)}, Raw: raw},
			agent.SubagentEvent{ToolName: tool, ToolUseID: item.ItemID, Phase: agent.SubagentStarted, ChildSessionID: child, Raw: raw},
		}
	}
	if isCompleted {
		name := tool
		if pending, ok := state.pendingToolUse[item.ItemID]; ok {
			name = pending.name
			delete(state.pendingToolUse, item.ItemID)
		}
		failed := item.Status == ItemStatusFailed || item.Status == ItemStatusRejected ||
			item.Status == ItemStatusCancelled || item.Status == ItemStatusTimedOut
		content := strVal(item.FinalSummary)
		if content == "" {
			content = strVal(item.FallbackText)
		}
		if content == "" {
			content = strVal(item.FailureReason)
		}
		phase := agent.SubagentCompleted
		if failed {
			phase = agent.SubagentFailed
		}
		return []agent.Event{
			agent.ToolResultEvent{ToolName: name, ToolUseID: item.ItemID, Content: content, IsError: failed, Raw: raw},
			agent.SubagentEvent{ToolName: name, ToolUseID: item.ItemID, Phase: phase, ChildSessionID: child, Raw: raw},
		}
	}
	return nil
}

// classifyTool tags the runner-facing ToolCategory for a tool name.
func classifyTool(name string) string {
	lower := strings.ToLower(name)
	switch {
	case lower == "shell" || lower == "bash":
		return "shell"
	case lower == "write" || lower == "edit" || lower == "patch":
		return "filesystem"
	case strings.HasPrefix(lower, "mcp__"):
		return "mcp"
	default:
		return ""
	}
}

func strVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
