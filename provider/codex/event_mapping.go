package codex

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// mapperState carries the running totals across notifications so
// turn/completed can emit a ResultEvent with accumulated cost. Mirrors
// AppServerEventMapperState from
// ../agentfactory/packages/core/src/providers/codex-app-server-provider.ts.
type mapperState struct {
	sessionID           string
	model               string
	totalInputTokens    int64
	totalOutputTokens   int64
	totalCachedInputTok int64
	turnCount           int
}

// mapNotification translates one inbound JSON-RPC notification into
// zero or more agent.Events. Mirrors mapAppServerNotification +
// mapAppServerItemEvent from the legacy TS but pulled into one switch
// for clarity.
//
// Returning a slice (vs. a single Event) preserves the legacy
// item/started + item/completed emit-two-events shape for tool calls.
func mapNotification(method string, params json.RawMessage, state *mapperState, raw any) []agent.Event {
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

	case "thread/closed", "thread/status/changed":
		return []agent.Event{agent.SystemEvent{Subtype: subtypeFromMethod(method), Raw: raw}}

	// ─── Turn lifecycle ───────────────────────────────────────────
	case "turn/started":
		state.turnCount++
		return []agent.Event{agent.SystemEvent{
			Subtype: "turn_started",
			Message: fmt.Sprintf("Turn %d started", state.turnCount),
			Raw:     raw,
		}}

	case "turn/completed":
		return mapTurnCompleted(params, state, raw)

	// ─── Item lifecycle ───────────────────────────────────────────
	case "item/started", "item/completed":
		return mapItem(method, params, raw)

	// ─── Streaming deltas ─────────────────────────────────────────
	case "item/agentMessage/delta":
		var p struct {
			Delta string `json:"delta"`
			Text  string `json:"text"`
		}
		_ = json.Unmarshal(params, &p)
		text := p.Delta
		if text == "" {
			text = p.Text
		}
		if text == "" {
			return nil
		}
		return []agent.Event{agent.AssistantTextEvent{Text: text, Raw: raw}}

	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		var p struct {
			Text  string `json:"text"`
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(params, &p)
		text := p.Text
		if text == "" {
			text = p.Delta
		}
		if text == "" {
			return nil
		}
		return []agent.Event{agent.SystemEvent{Subtype: "reasoning", Message: text, Raw: raw}}

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

	default:
		return []agent.Event{agent.SystemEvent{
			Subtype: "unknown",
			Message: "Unhandled codex notification: " + method,
			Raw:     raw,
		}}
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
	state.totalInputTokens += in
	state.totalOutputTokens += out
	state.totalCachedInputTok += cached

	cost := &agent.CostData{
		InputTokens:       state.totalInputTokens,
		OutputTokens:      state.totalOutputTokens,
		CachedInputTokens: state.totalCachedInputTok,
		TotalCostUsd:      calculateCostUSD(state.totalInputTokens, state.totalCachedInputTok, state.totalOutputTokens, state.model),
		NumTurns:          state.turnCount,
	}

	switch p.Turn.Status {
	case "", "completed":
		return []agent.Event{agent.ResultEvent{Success: true, Cost: cost, Raw: raw}}
	case "failed":
		errMsg := p.Turn.Error.Message
		if errMsg == "" {
			errMsg = "Turn failed"
		}
		subtype := p.Turn.Error.CodexErrorInfo
		if subtype == "" {
			subtype = "turn_failed"
		}
		return []agent.Event{agent.ResultEvent{
			Success:      false,
			Errors:       []string{errMsg},
			ErrorSubtype: subtype,
			Cost:         cost,
			Raw:          raw,
		}}
	case "interrupted":
		return []agent.Event{agent.ResultEvent{
			Success:      false,
			Errors:       []string{"Turn was interrupted"},
			ErrorSubtype: "interrupted",
			Cost:         cost,
			Raw:          raw,
		}}
	default:
		return []agent.Event{agent.SystemEvent{
			Subtype: "turn_completed",
			Message: "Turn completed with status: " + p.Turn.Status,
			Raw:     raw,
		}}
	}
}

// mapItem handles item/started and item/completed notifications.
func mapItem(method string, params json.RawMessage, raw any) []agent.Event {
	var p struct {
		Item struct {
			ID       string          `json:"id"`
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Summary  string          `json:"summary"`
			Content  string          `json:"content"`
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
	case "agentMessage":
		if isCompleted && p.Item.Text != "" {
			return []agent.Event{agent.AssistantTextEvent{Text: p.Item.Text, Raw: raw}}
		}
		return nil

	case "reasoning":
		if msg := p.Item.Summary; msg != "" || p.Item.Content != "" {
			if msg == "" {
				msg = p.Item.Content
			}
			return []agent.Event{agent.SystemEvent{Subtype: "reasoning", Message: msg, Raw: raw}}
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
		return []agent.Event{agent.SystemEvent{
			Subtype: "unknown_item",
			Message: "Unhandled codex item type: " + p.Item.Type,
			Raw:     raw,
		}}
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
// applies the input/cachedInput/output split and falls back to the
// default pricing for unknown models.
func calculateCostUSD(inputTokens, cachedInputTokens, outputTokens int64, model string) float64 {
	pricing := defaultCodexPricing
	if model != "" {
		if p, ok := codexPricing[model]; ok {
			pricing = p
		}
	}
	freshInput := inputTokens - cachedInputTokens
	if freshInput < 0 {
		freshInput = 0
	}
	return (float64(freshInput)/1_000_000)*pricing.input +
		(float64(cachedInputTokens)/1_000_000)*pricing.cachedInput +
		(float64(outputTokens)/1_000_000)*pricing.output
}
