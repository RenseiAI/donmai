// Package translate holds the surface-protocol ⇄ IR codecs. Each protocol has
// ONE codec used on both sides of the hub: the inbound surface uses it to
// decode a client request and encode the client response; an outbound upstream
// speaking the same protocol uses it to encode the request and decode the
// response. N-to-N translation is therefore 2N codecs, never N².
//
// This file is the OpenAI Chat Completions codec (M1). It is a faithful
// mapping between the /v1/chat/completions wire shape and the canonical IR —
// so openai-chat-in → openai-compat-out is a near-identity round trip through
// the IR, which is exactly the property M1 proves before cross-protocol codecs
// land in M2.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/gateway/ir"
)

// ─── OpenAI Chat Completions wire types (the subset the gateway maps) ─────────

// ChatRequest is the /v1/chat/completions request body.
type ChatRequest struct {
	Model           string        `json:"model"`
	Messages        []ChatMessage `json:"messages"`
	Tools           []ChatTool    `json:"tools,omitempty"`
	ToolChoice      any           `json:"tool_choice,omitempty"`
	Temperature     *float64      `json:"temperature,omitempty"`
	TopP            *float64      `json:"top_p,omitempty"`
	MaxTokens       *int          `json:"max_tokens,omitempty"`
	Stop            []string      `json:"stop,omitempty"`
	Stream          bool          `json:"stream,omitempty"`
	StreamOptions   *StreamOpts   `json:"stream_options,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

// StreamOpts mirrors OpenAI's stream_options (include_usage requests a final
// usage-bearing chunk).
type StreamOpts struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatMessage is one message. Content is either a string or an array of typed
// parts; both are accepted on decode (RawContent) and a string is emitted.
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// ChatTool wraps a function tool declaration.
type ChatTool struct {
	Type     string       `json:"type"`
	Function ChatFunction `json:"function"`
}

// ChatFunction is the function schema inside a tool.
type ChatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// ChatToolCall is a model's tool call in a message / delta.
type ChatToolCall struct {
	Index    int              `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ChatToolCallFunc `json:"function"`
}

// ChatToolCallFunc is the name/arguments of a tool call.
type ChatToolCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ChatResponse is the non-streaming /v1/chat/completions response.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

// ChatChoice is one choice in a response.
type ChatChoice struct {
	Index        int          `json:"index"`
	Message      *ChatMessage `json:"message,omitempty"`
	Delta        *ChatDelta   `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

// ChatDelta is the streaming delta payload.
type ChatDelta struct {
	Role      string         `json:"role,omitempty"`
	Content   string         `json:"content,omitempty"`
	ToolCalls []ChatToolCall `json:"tool_calls,omitempty"`
}

// ChatUsage is the token accounting.
type ChatUsage struct {
	PromptTokens     int               `json:"prompt_tokens"`
	CompletionTokens int               `json:"completion_tokens"`
	TotalTokens      int               `json:"total_tokens"`
	CompletionDetail *CompletionDetail `json:"completion_tokens_details,omitempty"`
	PromptDetail     *PromptDetail     `json:"prompt_tokens_details,omitempty"`
}

// CompletionDetail carries reasoning-token accounting when the upstream reports it.
type CompletionDetail struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// PromptDetail carries cache-read accounting when the upstream reports it.
type PromptDetail struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// ─── request codec ───────────────────────────────────────────────────────────

// DecodeRequest maps a /v1/chat/completions request body onto the canonical IR.
func DecodeRequest(body []byte) (ir.Request, error) {
	var cr ChatRequest
	if err := json.Unmarshal(body, &cr); err != nil {
		return ir.Request{}, fmt.Errorf("gateway/translate: decode openai request: %w", err)
	}
	return chatRequestToIR(cr)
}

func chatRequestToIR(cr ChatRequest) (ir.Request, error) {
	req := ir.Request{
		Model:  cr.Model,
		Stream: cr.Stream,
		Sampling: ir.Sampling{
			Temperature: cr.Temperature,
			TopP:        cr.TopP,
			MaxTokens:   cr.MaxTokens,
			Stop:        cr.Stop,
		},
	}
	if cr.ReasoningEffort != "" {
		req.Thinking = ir.ThinkingSpec{Level: ir.LevelFromEffort(cr.ReasoningEffort), Emit: ir.EmitFull}
	}
	for _, t := range cr.Tools {
		req.Tools = append(req.Tools, ir.ToolDef{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	req.ToolChoice = toolChoiceToIR(cr.ToolChoice)

	for _, m := range cr.Messages {
		role := roleToIR(m.Role)
		if role == ir.RoleSystem {
			req.System = append(req.System, textParts(m.Content)...)
			continue
		}
		msg := ir.Message{Role: role}
		if role == ir.RoleTool {
			msg.Parts = append(msg.Parts, ir.Part{
				Kind:       ir.PartToolResult,
				Text:       contentString(m.Content),
				ToolCallID: m.ToolCallID,
			})
			req.Messages = append(req.Messages, msg)
			continue
		}
		msg.Parts = append(msg.Parts, textParts(m.Content)...)
		for _, tc := range m.ToolCalls {
			msg.Parts = append(msg.Parts, ir.Part{
				Kind: ir.PartToolCall,
				ToolCall: &ir.ToolCall{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		req.Messages = append(req.Messages, msg)
	}
	return req, nil
}

// EncodeRequest maps the canonical IR back onto a /v1/chat/completions request
// body for an OpenAI-compatible upstream.
func EncodeRequest(req ir.Request) ([]byte, error) {
	cr := ChatRequest{
		Model:  req.Model,
		Stream: req.Stream,
	}
	cr.Temperature = req.Sampling.Temperature
	cr.TopP = req.Sampling.TopP
	cr.MaxTokens = req.Sampling.MaxTokens
	cr.Stop = req.Sampling.Stop
	if req.Stream {
		cr.StreamOptions = &StreamOpts{IncludeUsage: true}
	}
	if effort := req.Thinking.EffortForOpenAI(); effort != "" {
		cr.ReasoningEffort = effort
	}
	for _, t := range req.Tools {
		cr.Tools = append(cr.Tools, ChatTool{
			Type:     "function",
			Function: ChatFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	cr.ToolChoice = toolChoiceFromIR(req.ToolChoice)

	if len(req.System) > 0 {
		cr.Messages = append(cr.Messages, ChatMessage{Role: "system", Content: joinText(req.System)})
	}
	for _, m := range req.Messages {
		cr.Messages = append(cr.Messages, messageFromIR(m)...)
	}
	return json.Marshal(cr)
}

// ─── response codec ──────────────────────────────────────────────────────────

// DecodeResponse maps a non-streaming /v1/chat/completions response onto the IR.
func DecodeResponse(body []byte) (ir.Response, error) {
	var cr ChatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return ir.Response{}, fmt.Errorf("gateway/translate: decode openai response: %w", err)
	}
	resp := ir.Response{Model: cr.Model, FinishReason: ir.FinishStop}
	if len(cr.Choices) > 0 {
		ch := cr.Choices[0]
		if ch.Message != nil {
			if s := contentString(ch.Message.Content); s != "" {
				resp.Content = append(resp.Content, ir.Part{Kind: ir.PartText, Text: s})
			}
			for _, tc := range ch.Message.ToolCalls {
				resp.Content = append(resp.Content, ir.Part{
					Kind:     ir.PartToolCall,
					ToolCall: &ir.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments},
				})
			}
		}
		if ch.FinishReason != nil {
			resp.FinishReason = finishToIR(*ch.FinishReason)
		}
	}
	if cr.Usage != nil {
		resp.Usage = usageToIR(cr.Usage)
	}
	return resp, nil
}

// EncodeResponse maps the IR back onto a non-streaming client response body.
// id and created stamp the envelope OpenAI clients expect.
func EncodeResponse(resp ir.Response, id string, created int64) ([]byte, error) {
	msg := &ChatMessage{Role: "assistant"}
	var text strings.Builder
	for _, p := range resp.Content {
		switch p.Kind {
		case ir.PartText:
			text.WriteString(p.Text)
		case ir.PartToolCall:
			if p.ToolCall != nil {
				msg.ToolCalls = append(msg.ToolCalls, ChatToolCall{
					ID:       p.ToolCall.ID,
					Type:     "function",
					Function: ChatToolCallFunc{Name: p.ToolCall.Name, Arguments: p.ToolCall.Arguments},
				})
			}
		}
	}
	if text.Len() > 0 {
		msg.Content = text.String()
	}
	finish := finishFromIR(resp.FinishReason)
	out := ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   resp.Model,
		Choices: []ChatChoice{{Index: 0, Message: msg, FinishReason: &finish}},
		Usage:   usageFromIR(resp.Usage),
	}
	return json.Marshal(out)
}

// ─── stream codec ────────────────────────────────────────────────────────────

// DecodeStreamChunk maps one SSE `data:` payload from an OpenAI-compatible
// upstream onto an ir.StreamDelta. done is true for the "[DONE]" sentinel.
func DecodeStreamChunk(data []byte) (delta ir.StreamDelta, done bool, err error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "[DONE]" {
		return ir.StreamDelta{}, true, nil
	}
	var cr ChatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return ir.StreamDelta{}, false, fmt.Errorf("gateway/translate: decode openai stream chunk: %w", err)
	}
	if len(cr.Choices) > 0 {
		ch := cr.Choices[0]
		if ch.Delta != nil {
			delta.TextDelta = ch.Delta.Content
			for _, tc := range ch.Delta.ToolCalls {
				delta.ToolCallDelta = &ir.ToolCallDelta{
					Index:     tc.Index,
					ID:        tc.ID,
					Name:      tc.Function.Name,
					ArgsDelta: tc.Function.Arguments,
				}
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			delta.Finish = finishToIR(*ch.FinishReason)
		}
	}
	if cr.Usage != nil {
		u := usageToIR(cr.Usage)
		delta.Usage = &u
	}
	return delta, false, nil
}

// EncodeStreamChunk maps an ir.StreamDelta onto one OpenAI-compatible
// chat.completion.chunk JSON object (the caller wraps it in `data: …\n\n`).
func EncodeStreamChunk(delta ir.StreamDelta, id, model string, created int64) ([]byte, error) {
	cd := &ChatDelta{}
	if delta.TextDelta != "" {
		cd.Content = delta.TextDelta
	}
	if delta.ToolCallDelta != nil {
		cd.ToolCalls = []ChatToolCall{{
			Index:    delta.ToolCallDelta.Index,
			ID:       delta.ToolCallDelta.ID,
			Type:     "function",
			Function: ChatToolCallFunc{Name: delta.ToolCallDelta.Name, Arguments: delta.ToolCallDelta.ArgsDelta},
		}}
	}
	choice := ChatChoice{Index: 0, Delta: cd}
	if delta.Finish != "" {
		f := finishFromIR(delta.Finish)
		choice.FinishReason = &f
	}
	chunk := ChatResponse{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChatChoice{choice},
	}
	if delta.Usage != nil {
		chunk.Usage = usageFromIR(*delta.Usage)
	}
	return json.Marshal(chunk)
}

// ─── small pure mappers ──────────────────────────────────────────────────────

func roleToIR(r string) ir.Role {
	switch r {
	case "system", "developer":
		return ir.RoleSystem
	case "assistant":
		return ir.RoleAssistant
	case "tool":
		return ir.RoleTool
	default:
		return ir.RoleUser
	}
}

func roleFromIR(r ir.Role) string {
	switch r {
	case ir.RoleSystem:
		return "system"
	case ir.RoleAssistant:
		return "assistant"
	case ir.RoleTool:
		return "tool"
	default:
		return "user"
	}
}

// contentString flattens a message Content (string or []part) to plain text.
func contentString(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			if pm, ok := part.(map[string]any); ok {
				if t, _ := pm["text"].(string); t != "" {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

// textParts turns a message Content into IR text parts (one part per string).
func textParts(content any) []ir.Part {
	s := contentString(content)
	if s == "" {
		return nil
	}
	return []ir.Part{{Kind: ir.PartText, Text: s}}
}

func joinText(parts []ir.Part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == ir.PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// messageFromIR maps one IR message onto one or more chat messages. A tool
// result becomes a role:"tool" message; assistant text + tool calls collapse
// into a single assistant message.
func messageFromIR(m ir.Message) []ChatMessage {
	if m.Role == ir.RoleTool {
		var out []ChatMessage
		for _, p := range m.Parts {
			if p.Kind == ir.PartToolResult {
				out = append(out, ChatMessage{Role: "tool", Content: p.Text, ToolCallID: p.ToolCallID})
			}
		}
		return out
	}
	cm := ChatMessage{Role: roleFromIR(m.Role)}
	var text strings.Builder
	for _, p := range m.Parts {
		switch p.Kind {
		case ir.PartText:
			text.WriteString(p.Text)
		case ir.PartToolCall:
			if p.ToolCall != nil {
				cm.ToolCalls = append(cm.ToolCalls, ChatToolCall{
					ID:       p.ToolCall.ID,
					Type:     "function",
					Function: ChatToolCallFunc{Name: p.ToolCall.Name, Arguments: p.ToolCall.Arguments},
				})
			}
		}
	}
	if text.Len() > 0 {
		cm.Content = text.String()
	}
	return []ChatMessage{cm}
}

func toolChoiceToIR(tc any) ir.ToolChoice {
	switch v := tc.(type) {
	case string:
		switch v {
		case "none":
			return ir.ToolChoiceNone
		case "required":
			return ir.ToolChoiceRequired
		case "auto":
			return ir.ToolChoiceAuto
		}
	case map[string]any:
		// {"type":"function",...} — a forced specific tool; canonicalize as required.
		return ir.ToolChoiceRequired
	}
	return ""
}

func toolChoiceFromIR(tc ir.ToolChoice) any {
	if tc == "" {
		return nil
	}
	return string(tc)
}

func finishToIR(r string) ir.FinishReason {
	switch r {
	case "stop", "end_turn":
		return ir.FinishStop
	case "tool_calls", "function_call":
		return ir.FinishToolCalls
	case "length", "max_tokens":
		return ir.FinishLength
	case "content_filter":
		return ir.FinishContentFilter
	default:
		return ir.FinishStop
	}
}

func finishFromIR(r ir.FinishReason) string {
	switch r {
	case ir.FinishToolCalls:
		return "tool_calls"
	case ir.FinishLength:
		return "length"
	case ir.FinishContentFilter:
		return "content_filter"
	case ir.FinishError:
		return "stop"
	default:
		return "stop"
	}
}

func usageToIR(u *ChatUsage) ir.Usage {
	out := ir.Usage{TokensIn: u.PromptTokens, TokensOut: u.CompletionTokens}
	if u.CompletionDetail != nil {
		out.ReasoningTokens = u.CompletionDetail.ReasoningTokens
	}
	if u.PromptDetail != nil {
		out.CacheReadTokens = u.PromptDetail.CachedTokens
	}
	return out
}

func usageFromIR(u ir.Usage) *ChatUsage {
	if u.TokensIn == 0 && u.TokensOut == 0 {
		return nil
	}
	cu := &ChatUsage{
		PromptTokens:     u.TokensIn,
		CompletionTokens: u.TokensOut,
		TotalTokens:      u.TokensIn + u.TokensOut,
	}
	if u.ReasoningTokens > 0 {
		cu.CompletionDetail = &CompletionDetail{ReasoningTokens: u.ReasoningTokens}
	}
	if u.CacheReadTokens > 0 {
		cu.PromptDetail = &PromptDetail{CachedTokens: u.CacheReadTokens}
	}
	return cu
}
