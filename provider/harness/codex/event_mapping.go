package codex

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// mapperState carries the running totals across notifications so
// turn/completed can emit a ResultEvent with accumulated cost. Mirrors
// AppServerEventMapperState from
// ../donmai-libraries/packages/core/src/providers/codex-app-server-provider.ts.
type mapperState struct {
	sessionID            string
	model                string
	totalInputTokens     int64
	totalOutputTokens    int64
	totalCachedInputTok  int64
	totalCostUSD         float64
	turnCount            int
	completedTurns       int
	seenCompletedTurnIDs map[string]struct{}
	turnStarted          time.Time
	// rateLimits is the last account rate-limit snapshot this handle
	// saw, merged field by field across partial
	// `account/rateLimits/updated` notifications. A partial
	// notification omits the plan, so classifying its windows against
	// the merged snapshot keeps a monthly allowance monthly instead
	// of falling back to the session default — the same merged-plan
	// rule the probe-side subscription applies, so stream and probe
	// rows share window ids.
	rateLimits    *RateLimitSnapshot
	ignoredCounts map[string]uint64
	usageTotal    *codexTokenUsage
	usageThisTurn bool
	reasoning     map[string]*reasoningBuffer
}

// mapNotification translates one inbound JSON-RPC notification into
// zero or more agent.Events. Mirrors mapAppServerNotification +
// mapAppServerItemEvent from the legacy TS but pulled into one switch
// for clarity.
//
// Returning a slice (vs. a single Event) preserves the legacy
// item/started + item/completed emit-two-events shape for tool calls.
func mapNotification(method string, params json.RawMessage, state *mapperState, raw any) []agent.Event {
	if _, ignored := ignoredNotifications[method]; ignored {
		countIgnored(state, method)
		return nil
	}
	switch method {
	// ─── Thread lifecycle ─────────────────────────────────────────
	case "thread/started":
		var p struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Thread.ID != "" {
			state.sessionID = p.Thread.ID
			return []agent.Event{agent.InitEvent{SessionID: p.Thread.ID, Raw: raw}}
		}
		return nil

	case "thread/closed":
		return []agent.Event{agent.SystemEvent{Subtype: subtypeFromMethod(method), Raw: raw}}

	case "mcpServer/startupStatus/updated":
		var p struct {
			Name          string `json:"name"`
			Status        string `json:"status"`
			FailureReason string `json:"failureReason"`
		}
		_ = json.Unmarshal(params, &p)
		switch p.Status {
		case "starting", "ready":
			return nil
		case "failed", "cancelled":
			name := p.Name
			if name == "" {
				name = "unknown"
			}
			message := fmt.Sprintf("Codex MCP server %q startup %s", name, p.Status)
			if p.FailureReason != "" {
				message += ": " + p.FailureReason
			}
			return []agent.Event{agent.SystemEvent{
				Subtype: "mcp_server_startup_" + p.Status,
				Message: message,
				Raw:     raw,
			}}
		default:
			return []agent.Event{agent.SystemEvent{
				Subtype: "mcp_server_startup_status",
				Message: fmt.Sprintf("Codex MCP server %q reported startup status %q", p.Name, p.Status),
				Raw:     raw,
			}}
		}

	// ─── Turn lifecycle ───────────────────────────────────────────
	case "turn/started":
		state.usageThisTurn = false
		state.turnCount++
		state.turnStarted = time.Now()
		return []agent.Event{agent.SystemEvent{
			Subtype: "turn_started",
			Message: fmt.Sprintf("Turn %d started", state.turnCount),
			Raw:     raw,
		}}

	case "turn/completed":
		events := flushReasoning(state, raw)
		return append(events, mapTurnCompleted(params, state, raw)...)

	// The account rate-limit notification carries the same snapshot
	// shape as the probe's read response, so it maps through the one
	// shared mapper onto the probe-stable window ids. The snapshot
	// merges field by field onto the last one this handle saw before
	// mapping, so a partial notification that omits the plan still
	// classifies against it. A model-specific notification maps to
	// nothing and stays silent.
	case "account/rateLimits/updated":
		return mapRateLimitsUpdated(params, state, raw)

	// ─── Item lifecycle ───────────────────────────────────────────
	case "item/started", "item/completed":
		var item struct {
			Item struct {
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"item"`
		}
		_ = json.Unmarshal(params, &item)
		if _, ignored := ignoredItems[item.Item.Type]; ignored {
			countIgnored(state, "item/"+item.Item.Type)
			return nil
		}
		events := mapItem(method, params, raw, state)
		if method == "item/completed" && item.Item.Type == "reasoning" {
			buffered := finishReasoning(state, item.Item.ID, raw)
			if len(events) == 0 {
				return buffered
			}
		}
		return events

	// ─── Streaming deltas ─────────────────────────────────────────
	//
	// Partial-message frames (one token per notification) carry no
	// content for the orchestrator: they map to the content-free
	// stream-progress liveness signal, not to AssistantTextEvent — each
	// streamed token as its own AssistantTextEvent turned into a separate
	// "thought" activity on the platform topology view (one-token-per-line
	// spam). The full text still arrives on the matching `item/completed`
	// notification (see mapItem: agentMessage → the complete
	// AssistantTextEvent; reasoning → the complete summary SystemEvent),
	// so the progress signal loses no content and keeps the WORK_RESULT
	// marker scan from splitting across tokens. The runner's stall
	// detector disarms on the signal: deltas arriving means the model
	// request is alive, so a healthy long generation never reads as a
	// stall, while a hung request (no deltas) still trips it.
	//
	// Live, incremental rendering (the interview token-delta stream) has
	// its own dedicated transport in runtime/tokendelta — it does not and
	// must not ride the activity-event stream.
	case "item/agentMessage/delta":
		return []agent.Event{agent.StreamProgressEvent()}

	case "item/commandExecution/outputDelta":
		var p struct {
			Delta  string `json:"delta"`
			Output string `json:"output"`
		}
		_ = json.Unmarshal(params, &p)
		text := p.Delta
		if text == "" {
			text = p.Output
		}
		return []agent.Event{agent.SystemEvent{
			Subtype: "command_progress",
			Message: stripANSI(text),
			Raw:     raw,
		}}

	// ─── Diff / plan ──────────────────────────────────────────────
	case "turn/diff/updated":
		var p struct {
			Diff string `json:"diff"`
		}
		_ = json.Unmarshal(params, &p)
		return []agent.Event{agent.SystemEvent{Subtype: "diff_updated", Message: p.Diff, Raw: raw}}

	case "turn/plan/updated":
		var p struct {
			Plan json.RawMessage `json:"plan"`
		}
		_ = json.Unmarshal(params, &p)
		return []agent.Event{agent.SystemEvent{Subtype: "plan_updated", Message: string(p.Plan), Raw: raw}}

	case "thread/tokenUsage/updated":
		return mapTokenUsageUpdated(params, state, raw)
	case "model/rerouted", "turn/moderationMetadata", "model/safetyBuffering/updated":
		return mapWarning(method, params, state, raw)
	case "thread/compacted":
		return []agent.Event{agent.ContextUsageEvent{Harness: "codex", Source: "app-server", NoticeKind: agent.ContextUsageCompaction, Raw: raw}}
	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded":
		bufferReasoning(method, params, state)
		return []agent.Event{agent.StreamProgressEvent()}
	case "item/fileChange/outputDelta", "item/fileChange/patchUpdated", "item/plan/delta":
		var p struct {
			Delta   string          `json:"delta"`
			Changes json.RawMessage `json:"changes"`
		}
		if json.Unmarshal(params, &p) != nil {
			return nil
		}
		text := stripANSI(p.Delta)
		if method == "item/fileChange/patchUpdated" {
			text = string(p.Changes)
		}
		if text == "" {
			return nil
		}
		return []agent.Event{agent.SystemEvent{Subtype: "codex_progress", Message: text, Raw: raw}}
	default:
		countIgnored(state, method)
		slog.Debug("Unrecognized Codex notification", "method", method)
		return nil
	}
}

func subtypeFromMethod(m string) string {
	// Ascii-clean replace using strings.ReplaceAll. Codex method
	// names are ASCII identifiers separated by '/', so a byte-wise
	// rewrite is correct without ranging over runes.
	return strings.ReplaceAll(m, "/", "_")
}

// mapTurnCompleted handles the turn/completed notification, the only
// path that emits a terminal ResultEvent in autonomous mode.
func mapTurnCompleted(params json.RawMessage, state *mapperState, raw any) []agent.Event {
	var p struct {
		Turn struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Usage  struct {
				InputTokens       int64 `json:"input_tokens"`
				OutputTokens      int64 `json:"output_tokens"`
				CachedInputTokens int64 `json:"cached_input_tokens"`
				InputTokensCamel  int64 `json:"inputTokens"`
				OutputTokensCamel int64 `json:"outputTokens"`
				CachedInputCamel  int64 `json:"cachedInputTokens"`
			} `json:"usage"`
			Error struct {
				Message        string `json:"message"`
				CodexErrorInfo string `json:"codexErrorInfo"`
				HTTPStatusCode int    `json:"httpStatusCode"`
			} `json:"error"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(params, &p)
	if p.Turn.ID != "" {
		if _, duplicate := state.seenCompletedTurnIDs[p.Turn.ID]; duplicate {
			return nil
		}
		if state.seenCompletedTurnIDs == nil {
			state.seenCompletedTurnIDs = make(map[string]struct{})
		}
		state.seenCompletedTurnIDs[p.Turn.ID] = struct{}{}
	}
	state.completedTurns++

	// Codex has shipped both snake_case and camelCase usage shapes.
	// extractUsageTokens normalizes; we accept either form here.
	in := p.Turn.Usage.InputTokens
	if in == 0 {
		in = p.Turn.Usage.InputTokensCamel
	}
	out := p.Turn.Usage.OutputTokens
	if out == 0 {
		out = p.Turn.Usage.OutputTokensCamel
	}
	cached := p.Turn.Usage.CachedInputTokens
	if cached == 0 {
		cached = p.Turn.Usage.CachedInputCamel
	}
	// The provider reports input_tokens with cached reads inside it, but the
	// token rule (CostData) counts cached input once, at the cached rate:
	// InputTokens excludes cache reads. Report the fresh remainder here so
	// per-call and accumulated input match. Floored: a provider that
	// over-reports cached beyond input yields zero fresh, never negative.
	fresh := in - cached
	if fresh < 0 {
		fresh = 0
	}
	if state.usageThisTurn {
		// The separate usage notifications have already contributed spend.
		fresh, out, cached = 0, 0, 0
	} else {
		state.totalInputTokens += fresh
		state.totalOutputTokens += out
		state.totalCachedInputTok += cached
		state.totalCostUSD += calculateCostUSD(fresh, cached, out, state.model)
	}

	cost := &agent.CostData{
		InputTokens:       state.totalInputTokens,
		OutputTokens:      state.totalOutputTokens,
		CachedInputTokens: state.totalCachedInputTok,
		TotalCostUsd:      state.totalCostUSD,
		NumTurns:          state.turnCount,
	}
	finishReason := p.Turn.Status
	if finishReason == "" || finishReason == "completed" {
		finishReason = "stop"
	}
	llm := agent.LlmCallEvent{
		System:            "openai",
		Model:             state.model,
		InputTokens:       fresh,
		OutputTokens:      out,
		CachedInputTokens: cached,
		FinishReason:      finishReason,
		UsageSource:       agent.LlmUsageProvider,
		TurnCompleted:     true,
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

	switch p.Turn.Status {
	case "", "completed":
		return []agent.Event{llm, agent.ResultEvent{Success: true, Cost: cost, ObservedTurns: &completedTurns, Raw: raw}}
	case "failed":
		errMsg := p.Turn.Error.Message
		if errMsg == "" {
			errMsg = "Turn failed"
		}
		subtype := p.Turn.Error.CodexErrorInfo
		if subtype == "" {
			subtype = "turn_failed"
		}
		return []agent.Event{llm, agent.ResultEvent{
			Success:       false,
			Errors:        []string{errMsg},
			ErrorSubtype:  subtype,
			Cost:          cost,
			ObservedTurns: &completedTurns,
			Raw:           raw,
		}}
	case "interrupted":
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
			Message: "Turn completed with status: " + p.Turn.Status,
			Raw:     raw,
		}}
	}
}

// mapRateLimitsUpdated maps one `account/rateLimits/updated`
// notification onto a sparse quota-window update through the shared
// probe mapper. The notification merges field by field onto the last
// snapshot this handle saw, then maps; a window the notification
// omits is unchanged, and a notification that names no main-allowance
// window maps to nothing. The update rides a UsageEvent so the runner
// can forward it to the daemon's quota cache, where it merges by
// window id onto the probe snapshot.
func mapRateLimitsUpdated(params json.RawMessage, state *mapperState, raw any) []agent.Event {
	if state == nil {
		return nil
	}
	var decoded rateLimitsUpdatedParams
	if err := json.Unmarshal(params, &decoded); err != nil || decoded.RateLimits == nil {
		return nil
	}
	state.rateLimits = MergeRateLimits(state.rateLimits, decoded.RateLimits)
	named := *decoded.RateLimits
	if named.PlanType == nil && state.rateLimits != nil {
		named.PlanType = state.rateLimits.PlanType
	}
	update := RateLimitsToUpdate(&named, agent.ISOTime(time.Now()))
	if update == nil {
		return nil
	}
	return []agent.Event{agent.UsageEvent{Usage: update, Raw: raw}}
}

// mapItem handles item/started and item/completed notifications.
func mapItem(method string, params json.RawMessage, raw any, states ...*mapperState) []agent.Event {
	var p struct {
		Item struct {
			ID       string          `json:"id"`
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Summary  json.RawMessage `json:"summary"`
			Content  json.RawMessage `json:"content"`
			Command  string          `json:"command"`
			Status   string          `json:"status"`
			ExitCode *int            `json:"exitCode"`
			Server   string          `json:"server"`
			Tool     string          `json:"tool"`
			Args     json.RawMessage `json:"arguments"`
			Result   struct {
				Content json.RawMessage `json:"content"`
			} `json:"result"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Changes []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"changes"`
		} `json:"item"`
	}
	_ = json.Unmarshal(params, &p)

	if p.Item.Type == "" {
		return nil
	}

	isStarted := method == "item/started"
	isCompleted := method == "item/completed"

	switch p.Item.Type {
	case "userMessage":
		// The caller already owns this input. Codex echoes it as a thread item;
		// forwarding it would duplicate the prompt in the activity stream.
		return nil

	case "agentMessage":
		if isCompleted && p.Item.Text != "" {
			return []agent.Event{agent.AssistantTextEvent{Text: p.Item.Text, Raw: raw}}
		}
		return nil

	case "reasoning":
		// Only the COMPLETED reasoning item carries the final summary —
		// mirror the agentMessage gate. item/started must not emit: the
		// activity poster now forwards reasoning SystemEvents as "thought"
		// activities, so a started-time emission would double-post (or post
		// a partial summary) for every reasoning item.
		if isCompleted {
			if msg := reasoningText(p.Item.Summary); msg != "" || reasoningText(p.Item.Content) != "" {
				if msg == "" {
					msg = reasoningText(p.Item.Content)
				}
				return []agent.Event{agent.SystemEvent{Subtype: "reasoning", Message: msg, Raw: raw}}
			}
		}
		return nil

	case "commandExecution":
		if isStarted {
			return []agent.Event{agent.ToolUseEvent{
				ToolName:  "shell",
				ToolUseID: p.Item.ID,
				Input:     map[string]any{"command": p.Item.Command},
				Raw:       raw,
			}}
		}
		if isCompleted {
			isError := p.Item.Status == "failed" || (p.Item.ExitCode != nil && *p.Item.ExitCode != 0)
			return []agent.Event{agent.ToolResultEvent{
				ToolName:  "shell",
				ToolUseID: p.Item.ID,
				Content:   stripANSI(p.Item.Text),
				IsError:   isError,
				Raw:       raw,
			}}
		}
		return nil

	case "fileChange":
		if isCompleted {
			content := ""
			for i, ch := range p.Item.Changes {
				if i > 0 {
					content += "\n"
				}
				content += ch.Kind + ": " + ch.Path
			}
			return []agent.Event{agent.ToolResultEvent{
				ToolName:  "file_change",
				ToolUseID: p.Item.ID,
				Content:   content,
				IsError:   p.Item.Status == "failed",
				Raw:       raw,
			}}
		}
		return nil

	case "mcpToolCall":
		toolName := normalizeMcpToolName(p.Item.Server, p.Item.Tool)
		if isStarted {
			input := map[string]any{}
			if len(p.Item.Args) > 0 {
				_ = json.Unmarshal(p.Item.Args, &input)
			}
			return []agent.Event{agent.ToolUseEvent{
				ToolName:     toolName,
				ToolUseID:    p.Item.ID,
				Input:        input,
				ToolCategory: classifyTool(toolName),
				Raw:          raw,
			}}
		}
		if isCompleted {
			isError := p.Item.Status == "failed" || p.Item.Error.Message != ""
			content := p.Item.Error.Message
			if content == "" && len(p.Item.Result.Content) > 0 {
				content = string(p.Item.Result.Content)
			}
			return []agent.Event{agent.ToolResultEvent{
				ToolName:  toolName,
				ToolUseID: p.Item.ID,
				Content:   content,
				IsError:   isError,
				Raw:       raw,
			}}
		}
		return nil

	case "plan":
		return []agent.Event{agent.SystemEvent{Subtype: "plan", Message: p.Item.Text, Raw: raw}}

	case "webSearch":
		return []agent.Event{agent.SystemEvent{Subtype: "web_search", Message: "Web search: " + p.Item.Text, Raw: raw}}

	case "contextCompaction":
		return []agent.Event{agent.SystemEvent{Subtype: "context_compaction", Message: "Context history compacted", Raw: raw}}

	default:
		if len(states) > 0 {
			countIgnored(states[0], "item/"+p.Item.Type)
		}
		slog.Debug("Unrecognized Codex item", "type", p.Item.Type)
		return nil
	}
}

// normalizeMcpToolName matches the format used by the Claude provider
// for in-process MCP tools: `mcp__{server}__{tool}`. Mirrors
// normalizeMcpToolName in the legacy TS.
func normalizeMcpToolName(server, tool string) string {
	if server != "" && tool != "" {
		return "mcp__" + server + "__" + tool
	}
	if server == "" {
		server = "unknown"
	}
	if tool == "" {
		tool = "unknown"
	}
	return "mcp:" + server + "/" + tool
}

// classifyTool tags the runner-facing ToolCategory for a normalized
// tool name. The runner uses this to decide whether to count a call
// against the code-intelligence quota etc.
//
// The full taxonomy is owned by F.2.6 runner; we approximate here with
// prefix matching so the data is at least non-empty.
func classifyTool(name string) string {
	switch {
	case name == "shell" || name == "Bash":
		return "shell"
	case name == "file_change" || name == "Edit" || name == "Write":
		return "filesystem"
	case startsWith(name, "mcp__af-linear") || startsWith(name, "mcp__af_linear"):
		return "linear"
	case startsWith(name, "mcp__af-code") || startsWith(name, "mcp__af_code"):
		return "code-intel"
	default:
		return ""
	}
}

func startsWith(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return s[:len(prefix)] == prefix
}

// stripANSI removes ANSI escape sequences from raw shell output. The
// pattern is the regex used by the legacy TS stripAnsi.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07|\x1b[()][AB012]|\x1b\[[\d;]*m`)

func stripANSI(text string) string {
	if text == "" {
		return ""
	}
	return ansiPattern.ReplaceAllString(text, "")
}

// codexPricing is the per-1M-token USD pricing table mirrored from
// CODEX_PRICING in the legacy TS. Update when codex pricing changes;
// the table is exposed here as data so a simple constant table swap is
// the only change required.
var codexPricing = map[string]struct {
	input       float64
	cachedInput float64
	output      float64
}{
	"gpt-5-codex":   {input: 2.00, cachedInput: 0.50, output: 8.00},
	"gpt-5.2-codex": {input: 1.00, cachedInput: 0.25, output: 4.00},
	"gpt-5.3-codex": {input: 0.50, cachedInput: 0.125, output: 2.00},
}

var defaultCodexPricing = codexPricing["gpt-5-codex"]

// calculateCostUSD mirrors calculateCostUsd from the legacy TS. It
// applies the fresh-input/cachedInput/output split and falls back to the
// default pricing for unknown models. The first argument is fresh input
// (reported input minus cached reads); the caller does the subtraction so
// per-call tokens, accumulated totals and dollars all share one meaning.
func calculateCostUSD(freshInputTokens, cachedInputTokens, outputTokens int64, model string) float64 {
	pricing := defaultCodexPricing
	if model != "" {
		if p, ok := codexPricing[model]; ok {
			pricing = p
		}
	}
	if freshInputTokens < 0 {
		freshInputTokens = 0
	}
	return (float64(freshInputTokens)/1_000_000)*pricing.input +
		(float64(cachedInputTokens)/1_000_000)*pricing.cachedInput +
		(float64(outputTokens)/1_000_000)*pricing.output
}
