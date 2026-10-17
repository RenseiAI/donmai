package codex

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// This file maps native rollout records to the public conversation events
// the interactive tailer streams. Every mapping mirrors the headless
// app-server lane (event_mapping.go) onto the same agent event types, so
// the interactive runner forwards them through the existing activity sink
// unchanged.
//
// Record sources and pairing: the rollout file carries each conversation
// record twice — once as a response_item (the model's own record, keyed by
// its payload id) and once as an event_msg item_completed/item_started
// envelope (the client's view, keyed by the item id). The two halves use
// DIFFERENT ids for the same record (a custom_tool_call's call_id never
// appears on its CommandExecution completion), so the tailer pairs them by
// CONTENT, not by id: the tool NAME plus the command/arguments. Either half
// arriving first stages the call; the completion delivers the paired
// ToolUseEvent + ToolResultEvent. A half without its partner still
// delivers from the completion alone, and a started-but-never-completed
// item emits nothing.
//
// What maps, per record kind:
//
//   - message role assistant with output_text parts → AssistantTextEvent,
//     exactly as item/completed agentMessage.
//   - message role user → an attribution-preserving SystemEvent marker
//     ("user_message"), never an input echo: provider user messages may be
//     injected or automated. Developer-role messages (environment context,
//     memory, skills, model-switch instructions) are harness framing and
//     never map.
//   - custom_tool_call (the exec tool) + its CommandExecution completion →
//     ToolUseEvent ("shell", command input) + ToolResultEvent, exactly as
//     item/completed commandExecution.
//   - function_call + function_call_output → ToolUseEvent/ToolResultEvent
//     under the collaboration namespace (collaboration__name), exactly as
//     the headless lane would surface a namespaced tool. Clock-namespace
//     sleep/wait calls (namespace "clock") are session pacing, not work,
//     and never map.
//   - McpToolCall completion → ToolUseEvent/ToolResultEvent under the
//     normalized mcp__server__tool name, exactly as item/completed
//     mcpToolCall.
//   - FileChange completion → ToolResultEvent ("file_change"), exactly as
//     item/completed fileChange. The change list is content, not a call,
//     so it delivers without a ToolUseEvent.
//   - AgentMessage completion with Text content → AssistantTextEvent. The
//     response_item message with the same text is the SAME record under a
//     different id: the dedupe set (keyed on the text identity) delivers
//     it once, whichever half arrives first.
//   - task_complete → a SystemEvent turn marker carrying the last agent
//     message; the terminal ResultEvent stays the PTY lifecycle's.
//   - token_count → LlmCallEvent per-turn usage, mirroring the
//     token_usage_record totals the exit read owns (last_token_usage is
//     that call's count). total_token_usage is the thread cumulative and
//     never maps per-turn.
//   - reasoning (encrypted content only), session_meta, turn_context,
//     world_state, task_started, thread_settings_applied, and unknown
//     shapes → nothing. Hidden reasoning stays hidden; lifecycle and
//     settings are not conversation.
//
// Redaction: tool input and output ride the events the same way the
// headless lane carries them (the ToolUseEvent Input map, the result
// content string); the activity poster applies its own cap downstream, so
// nothing is cut here. The one exception is collaboration send_message
// arguments, which carry an encrypted payload, not readable text: the
// ToolUseEvent records the tool name with no input rather than an opaque
// blob.

// mapLine maps one rollout line to zero or more conversation events,
// pairing tool halves by content across lines. Lines already in seen map
// to nothing so a re-read never double-emits.
func (t *interactiveConversationTailer) mapLine(line []byte) []agentEvent {
	var envelope rolloutConversationEnvelope
	if err := json.Unmarshal(line, &envelope); err != nil {
		return nil
	}
	raw := string(line)
	switch envelope.Type {
	case "response_item":
		return t.mapResponseItem(envelope.Payload, raw)
	case "event_msg":
		return t.mapEventMsg(envelope.Payload, raw)
	default:
		return nil
	}
}

// rolloutConversationEnvelope is the type discriminant of one rollout
// line. Payload stays raw: each branch decodes its own shape.
type rolloutConversationEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// rolloutMessageContent is one content part of a rollout message record.
type rolloutMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// rolloutMessagePayload is a response_item message record.
type rolloutMessagePayload struct {
	Type    string                  `json:"type"`
	ID      string                  `json:"id"`
	Role    string                  `json:"role"`
	Content []rolloutMessageContent `json:"content"`
	// Arguments mirrors function_call records decoded nearby; empty here.
	Authorship string `json:"-"`
}

// rolloutCustomCallPayload is a response_item custom_tool_call record
// (the exec tool) or its custom_tool_call_output completion.
type rolloutCustomCallPayload struct {
	Type   string                  `json:"type"`
	ID     string                  `json:"id"`
	Status string                  `json:"status"`
	CallID string                  `json:"call_id"`
	Name   string                  `json:"name"`
	Input  string                  `json:"input"`
	Output []rolloutMessageContent `json:"output"`
}

// rolloutFunctionPayload is a response_item function_call record or its
// function_call_output completion.
type rolloutFunctionPayload struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Arguments string `json:"arguments"`
	CallID    string `json:"call_id"`
	Output    string `json:"output"`
}

// rolloutEventItem is the item inside an event_msg item_started or
// item_completed envelope.
type rolloutEventItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Status           string          `json:"status"`
	Command          []string        `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	Stdout           string          `json:"stdout"`
	Stderr           string          `json:"stderr"`
	ExitCode         *int            `json:"exitCode"`
	Exit             *int            `json:"exit_code"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Changes          map[string]struct {
		Kind string `json:"type"`
	} `json:"changes"`
	Result struct {
		Content json.RawMessage `json:"content"`
	} `json:"result"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	ItemContent []rolloutEventItemContent `json:"content"`
}

// rolloutEventItemContent is one content part of an event_msg item.
type rolloutEventItemContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// rolloutEventPayload is an event_msg envelope: item_started,
// item_completed, task_complete, or token_count.
type rolloutEventPayload struct {
	Type        string            `json:"type"`
	ThreadID    string            `json:"thread_id"`
	TurnID      string            `json:"turn_id"`
	LastMessage string            `json:"last_agent_message"`
	Info        *rolloutTokenInfo `json:"info"`
	Item        *rolloutEventItem `json:"item"`
}

// rolloutTokenInfo is the token_count usage block: the thread cumulative
// plus the last call's own count.
type rolloutTokenInfo struct {
	Total *rolloutTokenUsage `json:"total_token_usage"`
	Last  *rolloutTokenUsage `json:"last_token_usage"`
}

// mapResponseItem maps one response_item record.
func (t *interactiveConversationTailer) mapResponseItem(payload json.RawMessage, raw string) []agentEvent {
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &kind); err != nil {
		return nil
	}
	switch kind.Type {
	case "message":
		return t.mapRolloutMessage(payload, raw)
	case "custom_tool_call":
		return t.mapCustomCall(payload, raw)
	case "custom_tool_call_output":
		// The exec output half carries the call id but no command to
		// pair with; the CommandExecution completion delivers the same
		// call with its output, so this half maps to nothing on its own.
		return nil
	case "function_call":
		return t.mapFunctionCall(payload, raw)
	case "function_call_output":
		return t.mapFunctionCallOutput(payload, raw)
	default:
		// reasoning (encrypted only), agent_message (a transport
		// envelope, not conversation — the event_msg AgentMessage
		// completion carries the text), and unknown shapes map to
		// nothing.
		return nil
	}
}

// mapRolloutMessage maps one response_item message record: assistant
// output_text parts become AssistantTextEvent (exactly as the headless
// agentMessage mapping, sharing the text identity with the event_msg
// AgentMessage completion of the same record); user roles become
// attribution-preserving markers; developer roles are harness framing and
// never map.
func (t *interactiveConversationTailer) mapRolloutMessage(payload json.RawMessage, raw string) []agentEvent {
	var msg rolloutMessagePayload
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil
	}
	switch msg.Role {
	case "assistant":
		var out []agentEvent
		for _, part := range msg.Content {
			if part.Type != "output_text" || part.Text == "" {
				continue
			}
			// The event_msg AgentMessage completion carries this same
			// text under a different id: the shared text identity
			// delivers it once, whichever half arrives first.
			key := "assistant-text:" + part.Text
			if _, dup := t.seen[key]; dup {
				continue
			}
			t.seen[key] = struct{}{}
			out = append(out, agent.AssistantTextEvent{Text: part.Text, Raw: raw})
		}
		return out
	case "user":
		var texts []string
		for _, part := range msg.Content {
			if part.Type != "input_text" || part.Text == "" {
				continue
			}
			texts = append(texts, part.Text)
		}
		return t.mapUserTexts(msg.ID, texts, raw)
	default:
		// developer and every other role are harness framing
		// (environment context, memory, skills, model-switch
		// instructions), not conversation.
		return nil
	}
}

// mapUserTexts surfaces provider user text as an attribution-preserving
// SystemEvent marker, never as an input echo. Empty and harness-framed
// texts map to nothing.
func (t *interactiveConversationTailer) mapUserTexts(id string, texts []string, raw string) []agentEvent {
	var kept []string
	for _, text := range texts {
		trimmed := strings.TrimSpace(text)
		if trimmed == "" || isRolloutHarnessFraming(trimmed) {
			continue
		}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		return nil
	}
	key := "user-message:" + id + "\x00" + strings.Join(kept, "\x00")
	if _, dup := t.seen[key]; dup {
		return nil
	}
	t.seen[key] = struct{}{}
	return []agentEvent{agent.SystemEvent{Subtype: "user_message", Message: strings.Join(kept, "\n"), Raw: raw}}
}

// isRolloutHarnessFraming reports whether user text is harness framing
// rather than conversation: environment context, agent instructions,
// memory/skill payloads, and model-switch directives.
func isRolloutHarnessFraming(text string) bool {
	for _, prefix := range []string{
		"<environment_context>",
		"<model_switch>",
		"## Memory",
		"# AGENTS.md",
		"<skills_instructions>",
		"<collaboration_mode>",
	} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// mapCustomCall stages the exec half of a shell call: the ToolUseEvent
// fields are held until the CommandExecution completion arrives. Starts
// stage by call id, so two identical commands stage independently; the
// completion pairs through the per-command queue (see
// pendingExecByCommand), oldest staged start first. A duplicate start for
// the same call id stages once.
func (t *interactiveConversationTailer) mapCustomCall(payload json.RawMessage, raw string) []agentEvent {
	var call rolloutCustomCallPayload
	if err := json.Unmarshal(payload, &call); err != nil || call.CallID == "" {
		return nil
	}
	command := rolloutExecCommand(call.Input)
	key := "shell-call:" + call.CallID
	if _, dup := t.pending[key]; dup {
		return nil
	}
	t.pending[key] = pendingConversationCall{
		toolName:  "shell",
		toolUseID: call.CallID,
		input:     map[string]any{"command": command},
		raw:       raw,
		stagedAt:  t.sweeps,
	}
	t.pendingExecByCommand[command] = append(t.pendingExecByCommand[command], key)
	return nil
}

// mapFunctionCall stages a namespaced tool call until its output
// arrives. Clock-namespace pacing calls (sleep/wait) are session pacing,
// not work, and never map. Collaboration send_message arguments carry an
// encrypted payload, not readable text: the staged input stays empty
// rather than carrying an opaque blob.
func (t *interactiveConversationTailer) mapFunctionCall(payload json.RawMessage, raw string) []agentEvent {
	var call rolloutFunctionPayload
	if err := json.Unmarshal(payload, &call); err != nil || call.CallID == "" {
		return nil
	}
	if call.Namespace == "clock" {
		return nil
	}
	name := call.Name
	if call.Namespace != "" {
		name = call.Namespace + "__" + call.Name
	}
	if name == "" {
		return nil
	}
	key := "function:" + call.CallID
	if _, dup := t.pending[key]; dup {
		return nil
	}
	var input map[string]any
	if call.Namespace != "collaboration" || call.Name != "send_message" {
		input = decodeRolloutArguments(call.Arguments)
	}
	t.pending[key] = pendingConversationCall{
		toolName:  name,
		toolUseID: call.CallID,
		input:     input,
		raw:       raw,
		stagedAt:  t.sweeps,
	}
	return nil
}

// mapFunctionCallOutput delivers a namespaced tool result: the staged
// ToolUseEvent first (stable identity through the call id), then the
// ToolResultEvent. An output without its staged start still delivers the
// result alone under the completion's own identity.
func (t *interactiveConversationTailer) mapFunctionCallOutput(payload json.RawMessage, raw string) []agentEvent {
	var out rolloutFunctionPayload
	if err := json.Unmarshal(payload, &out); err != nil || out.CallID == "" {
		return nil
	}
	key := "function:" + out.CallID
	staged, ok := t.pending[key]
	if ok {
		delete(t.pending, key)
	}
	useID := out.CallID
	name := ""
	if ok {
		name = staged.toolName
	}
	resultKey := "function-result:" + out.CallID + "\x00" + out.Output
	if _, dup := t.seen[resultKey]; dup {
		return nil
	}
	t.seen[resultKey] = struct{}{}
	var events []agentEvent
	if ok {
		events = append(events, agent.ToolUseEvent{
			ToolName:  staged.toolName,
			ToolUseID: staged.toolUseID,
			Input:     staged.input,
			Raw:       staged.raw,
		})
	}
	events = append(events, agent.ToolResultEvent{
		ToolName:  name,
		ToolUseID: useID,
		Content:   out.Output,
		Raw:       raw,
	})
	return events
}

// mapEventMsg maps one event_msg envelope.
func (t *interactiveConversationTailer) mapEventMsg(payload json.RawMessage, raw string) []agentEvent {
	var msg rolloutEventPayload
	if err := json.Unmarshal(payload, &msg); err != nil {
		return nil
	}
	switch msg.Type {
	case "item_completed":
		return t.mapItemCompleted(msg.Item, raw)
	case "task_complete":
		return t.mapTaskComplete(msg, raw)
	case "token_count":
		// The envelope turn id scopes the usage record to its own turn;
		// without it two turns with identical counters are one record.
		return t.mapTokenCount(msg.TurnID, msg.Info, raw)
	default:
		// item_started (a started-but-never-completed item emits
		// nothing), task_started, thread_settings_applied, and unknown
		// envelopes map to nothing.
		return nil
	}
}

// mapItemCompleted maps one completed item: assistant text, user text,
// shell completions (paired with the staged exec start), MCP tool
// completions, file changes, and subagent/obscure shapes per the headless
// item mapping.
func (t *interactiveConversationTailer) mapItemCompleted(item *rolloutEventItem, raw string) []agentEvent {
	if item == nil || item.ID == "" {
		return nil
	}
	switch item.Type {
	case "AgentMessage":
		return t.mapAgentMessageItem(item, raw)
	case "UserMessage":
		return t.mapUserMessageItem(item, raw)
	case "CommandExecution":
		return t.mapCommandExecutionItem(item, raw)
	case "McpToolCall":
		return t.mapMcpToolCallItem(item, raw)
	case "FileChange":
		return t.mapFileChangeItem(item, raw)
	case "Reasoning", "Extension", "ImageView", "SubAgentActivity",
		"CollabAgentToolCall", "ContextCompaction":
		// Hidden reasoning, session pacing, image payloads, delegation
		// lifecycle (the headless lane emits no subagent events for
		// this harness), and compaction markers are not conversation.
		return nil
	default:
		return nil
	}
}

// mapAgentMessageItem maps an AgentMessage completion to AssistantTextEvent.
// The response_item message with the same text is the SAME record under a
// different id: the text identity dedupes them, whichever half arrives
// first. Both halves claim the shared key before emitting, so the second
// half maps to nothing.
func (t *interactiveConversationTailer) mapAgentMessageItem(item *rolloutEventItem, raw string) []agentEvent {
	var out []agentEvent
	for _, part := range item.ItemContent {
		if part.Type != "Text" && part.Type != "text" {
			continue
		}
		if part.Text == "" {
			continue
		}
		key := "assistant-text:" + part.Text
		if _, dup := t.seen[key]; dup {
			continue
		}
		t.seen[key] = struct{}{}
		out = append(out, agent.AssistantTextEvent{Text: part.Text, Raw: raw})
	}
	return out
}

// mapUserMessageItem maps a UserMessage completion to the
// attribution-preserving marker, mirroring mapUserTexts.
func (t *interactiveConversationTailer) mapUserMessageItem(item *rolloutEventItem, raw string) []agentEvent {
	var texts []string
	for _, part := range item.ItemContent {
		if part.Type != "text" || part.Text == "" {
			continue
		}
		texts = append(texts, strings.TrimSpace(part.Text))
	}
	return t.mapUserTexts(item.ID, texts, raw)
}

// mapCommandExecutionItem maps a shell completion: the staged exec start
// pairs by command, delivering ToolUseEvent + ToolResultEvent exactly as
// the headless commandExecution mapping. A completion without its staged
// start still delivers both halves reconstructed from the completion
// record.
func (t *interactiveConversationTailer) mapCommandExecutionItem(item *rolloutEventItem, raw string) []agentEvent {
	if _, dup := t.seen["exec-result:"+item.ID]; dup {
		return nil
	}
	t.seen["exec-result:"+item.ID] = struct{}{}
	command := strings.Join(item.Command, " ")
	pairCommand := rolloutExecCommandFromArgv(item.Command)
	var staged pendingConversationCall
	ok := false
	if queue := t.pendingExecByCommand[pairCommand]; len(queue) > 0 {
		key := queue[0]
		if s, present := t.pending[key]; present {
			staged, ok = s, true
			delete(t.pending, key)
		}
		if len(queue) == 1 {
			delete(t.pendingExecByCommand, pairCommand)
		} else {
			t.pendingExecByCommand[pairCommand] = queue[1:]
		}
	}
	var events []agentEvent
	if ok {
		events = append(events, agent.ToolUseEvent{
			ToolName:  staged.toolName,
			ToolUseID: staged.toolUseID,
			Input:     staged.input,
			Raw:       staged.raw,
		})
	} else if command != "" {
		events = append(events, agent.ToolUseEvent{
			ToolName:  "shell",
			ToolUseID: item.ID,
			Input:     map[string]any{"command": command},
			Raw:       raw,
		})
	}
	content := rolloutCommandOutput(item)
	isError := item.Status == "failed" || rolloutExitFailed(item)
	useID := item.ID
	if ok {
		useID = staged.toolUseID
	}
	events = append(events, agent.ToolResultEvent{
		ToolName:  "shell",
		ToolUseID: useID,
		Content:   content,
		IsError:   isError,
		Raw:       raw,
	})
	return events
}

// mapMcpToolCallItem maps an MCP tool completion to ToolUseEvent +
// ToolResultEvent under the normalized mcp__server__tool name, exactly as
// the headless mcpToolCall mapping. Only completions deliver: the rollout
// carries no separate start half for MCP calls.
func (t *interactiveConversationTailer) mapMcpToolCallItem(item *rolloutEventItem, raw string) []agentEvent {
	if _, dup := t.seen["mcp-result:"+item.ID]; dup {
		return nil
	}
	t.seen["mcp-result:"+item.ID] = struct{}{}
	name := normalizeMcpToolName(item.Server, item.Tool)
	input := map[string]any{}
	if len(item.Arguments) > 0 {
		_ = json.Unmarshal(item.Arguments, &input)
	}
	content := item.Error.Message
	if content == "" && len(item.Result.Content) > 0 {
		content = string(item.Result.Content)
	}
	return []agentEvent{
		agent.ToolUseEvent{
			ToolName:     name,
			ToolUseID:    item.ID,
			Input:        input,
			ToolCategory: classifyTool(name),
			Raw:          raw,
		},
		agent.ToolResultEvent{
			ToolName:  name,
			ToolUseID: item.ID,
			Content:   content,
			IsError:   item.Status == "failed" || item.Error.Message != "",
			Raw:       raw,
		},
	}
}

// mapFileChangeItem maps a file-change completion to a ToolResultEvent,
// exactly as the headless fileChange mapping. The change list is content,
// not a call, so it delivers without a ToolUseEvent.
func (t *interactiveConversationTailer) mapFileChangeItem(item *rolloutEventItem, raw string) []agentEvent {
	if _, dup := t.seen["file-change:"+item.ID]; dup {
		return nil
	}
	t.seen["file-change:"+item.ID] = struct{}{}
	var b strings.Builder
	first := true
	for _, path := range sortedChangePaths(item.Changes) {
		if !first {
			b.WriteString("\n")
		}
		first = false
		b.WriteString(item.Changes[path].Kind + ": " + path)
	}
	return []agentEvent{agent.ToolResultEvent{
		ToolName:  "file_change",
		ToolUseID: item.ID,
		Content:   b.String(),
		IsError:   item.Status == "failed",
		Raw:       raw,
	}}
}

// mapTaskComplete surfaces turn completion as a SystemEvent turn marker
// carrying the last agent message. The terminal ResultEvent stays the PTY
// lifecycle's, never the tailer's: a turn ending is not a session ending.
func (t *interactiveConversationTailer) mapTaskComplete(msg rolloutEventPayload, raw string) []agentEvent {
	key := "turn-complete:" + msg.TurnID
	if msg.TurnID == "" || msg.LastMessage == "" {
		return nil
	}
	if _, dup := t.seen[key]; dup {
		return nil
	}
	t.seen[key] = struct{}{}
	return []agentEvent{agent.SystemEvent{Subtype: "turn_completed", Message: msg.LastMessage, Raw: raw}}
}

// mapTokenCount maps a token_count record's last-call usage to an
// LlmCallEvent, mirroring the turn/completed per-turn shape: fresh input
// excludes both cache buckets, floored at zero. The thread cumulative
// (total_token_usage) never maps per-turn. A nil or empty last-call usage
// maps to nothing.
//
// The usage identity is the turn id when the envelope carries one, else
// the raw line: identical counters on different turns are different
// records, while a re-read of the same line collapses. Counters alone
// are probability, not identity.
func (t *interactiveConversationTailer) mapTokenCount(turnID string, info *rolloutTokenInfo, raw string) []agentEvent {
	if info == nil || info.Last == nil {
		return nil
	}
	last := info.Last
	fresh := last.InputTokens - last.CachedInputTokens - last.CacheWriteInputTokens
	if fresh < 0 {
		fresh = 0
	}
	if last.InputTokens == 0 && last.OutputTokens == 0 && last.CachedInputTokens == 0 && last.CacheWriteInputTokens == 0 {
		return nil
	}
	// The usage identity spans both the last-call and cumulative counts:
	// a re-read repeats them exactly and collapses. The turn id makes
	// identical counters on different turns two records; a record with
	// no turn id falls back to the raw line, which only its own re-read
	// repeats.
	var identity string
	switch {
	case turnID != "":
		identity = "turn-usage-id:" + turnID
	case raw != "":
		identity = "turn-usage-line:" + raw
	default:
		// No turn id and no raw line (unreachable through the tail,
		// which always passes the line): counters alone, with a nil
		// cumulative reading as zero rather than panicking.
		var total rolloutTokenUsage
		if info.Total != nil {
			total = *info.Total
		}
		identity = fmt.Sprintf("turn-usage-counters:%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d",
			last.InputTokens, last.CachedInputTokens, last.CacheWriteInputTokens, last.OutputTokens, last.ReasoningOutputTokens,
			total.InputTokens, total.CachedInputTokens, total.CacheWriteInputTokens, total.OutputTokens, total.ReasoningOutputTokens)
	}
	if _, dup := t.seen[identity]; dup {
		return nil
	}
	t.seen[identity] = struct{}{}
	return []agentEvent{agent.LlmCallEvent{
		InputTokens:       fresh,
		OutputTokens:      last.OutputTokens,
		CachedInputTokens: last.CachedInputTokens,
		CacheWriteTokens:  last.CacheWriteInputTokens,
		ReasoningTokens:   last.ReasoningOutputTokens,
		UsageSource:       agent.LlmUsageProvider,
		TurnCompleted:     true,
	}}
}

// rolloutExecCommand extracts the shell command from a custom_tool_call
// input: the cmd argument of the exec invocation.
func rolloutExecCommand(input string) string {
	start := strings.Index(input, `cmd:"`)
	if start < 0 {
		return strings.TrimSpace(input)
	}
	rest := input[start+len(`cmd:"`):]
	var b strings.Builder
	escaped := false
	for _, r := range rest {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// rolloutExecCommandFromArgv renders the staged pairing key from the
// completion's argv: the shell payload after the runner flag.
func rolloutExecCommandFromArgv(argv []string) string {
	for i, arg := range argv {
		if arg == "-lc" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return strings.Join(argv, " ")
}

// rolloutCommandOutput renders a CommandExecution completion's output:
// the aggregated output when present, else stdout plus stderr.
func rolloutCommandOutput(item *rolloutEventItem) string {
	if item.AggregatedOutput != "" {
		return stripANSI(item.AggregatedOutput)
	}
	out := item.Stdout
	if item.Stderr != "" {
		if out != "" {
			out += "\n"
		}
		out += item.Stderr
	}
	return stripANSI(out)
}

// rolloutExitFailed reports whether a CommandExecution completion failed
// by exit code. Either spelling (exitCode/exit_code) counts; a missing
// code defers to the status field the caller already checked.
func rolloutExitFailed(item *rolloutEventItem) bool {
	if item.ExitCode != nil {
		return *item.ExitCode != 0
	}
	if item.Exit != nil {
		return *item.Exit != 0
	}
	return false
}

// sortedChangePaths returns the changed paths in stable order.
func sortedChangePaths(changes map[string]struct {
	Kind string `json:"type"`
},
) []string {
	paths := make([]string, 0, len(changes))
	for path := range changes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// decodeRolloutArguments unmarshals function_call arguments as
// map[string]any. Any decode failure returns nil so the runner still sees
// the tool call.
func decodeRolloutArguments(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}
