package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/RenseiAI/donmai/gateway/ir"
)

// ErrResponsesUnsupported refuses semantics the selected codec cannot preserve.
var ErrResponsesUnsupported = errors.New("gateway/translate: unsupported responses semantics")

func strictJSON(body []byte, target any) error {
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

// Reject duplicate keys and bound nesting before decoding typed values.
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting limit")
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate JSON key")
			}
			seen[name] = true
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = d.Token()
	return err
}

type responseWireContent struct {
	Type        string                   `json:"type"`
	Text        string                   `json:"text,omitempty"`
	Annotations []responseWireAnnotation `json:"annotations,omitempty"`
	Logprobs    []any                    `json:"logprobs,omitempty"`
}

type responseWireSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responseWireItem struct {
	Type             string                       `json:"type"`
	ID               string                       `json:"id,omitempty"`
	Role             string                       `json:"role,omitempty"`
	Phase            string                       `json:"phase,omitempty"`
	Status           string                       `json:"status,omitempty"`
	Namespace        string                       `json:"namespace,omitempty"`
	Content          []responseWireContent        `json:"content,omitempty"`
	CallID           string                       `json:"call_id,omitempty"`
	Name             string                       `json:"name,omitempty"`
	Arguments        string                       `json:"arguments,omitempty"`
	Input            string                       `json:"input,omitempty"`
	Output           json.RawMessage              `json:"output,omitempty"`
	Summary          []responseWireSummary        `json:"summary,omitempty"`
	EncryptedContent string                       `json:"encrypted_content,omitempty"`
	Action           *responseWireWebSearchAction `json:"action,omitempty"`
}

func decodeResponseItem(body json.RawMessage) (ir.Item, error) {
	return decodeResponseItemMode(body, false)
}

func decodeResponseItemMode(body json.RawMessage, partial bool) (ir.Item, error) {
	body, err := expandMessageContent(body)
	if err != nil {
		return ir.Item{}, err
	}
	var w responseWireItem
	if err := strictJSON(body, &w); err != nil {
		return ir.Item{}, fmt.Errorf("%w: item shape", ErrResponsesUnsupported)
	}
	if err := validateResponseItemFields(body, w.Type); err != nil {
		return ir.Item{}, err
	}
	if w.Type != "web_search_call" && w.Status != "" && w.Status != "completed" && w.Status != "incomplete" && !(partial && w.Status == "in_progress") {
		return ir.Item{}, ErrResponsesUnsupported
	}
	item := ir.Item{
		Kind: w.Type, ID: w.ID, Role: ir.Role(w.Role), Phase: w.Phase, Status: w.Status, Namespace: w.Namespace,
		CallID: w.CallID, Name: w.Name, Arguments: w.Arguments, Input: w.Input,
		EncryptedContent: w.EncryptedContent,
	}
	switch w.Type {
	case "message":
		if w.Role != "user" && w.Role != "assistant" && w.Role != "system" && w.Role != "developer" {
			return ir.Item{}, ErrResponsesUnsupported
		}
		for _, c := range w.Content {
			if (c.Type != "input_text" && c.Type != "output_text") || len(c.Logprobs) != 0 ||
				(w.Role == "assistant" && c.Type != "output_text") || (w.Role != "assistant" && c.Type != "input_text") {
				return ir.Item{}, ErrResponsesUnsupported
			}
			annotations, err := decodeResponseAnnotations(c.Annotations)
			if err != nil || c.Type != "output_text" && len(annotations) > 0 {
				return ir.Item{}, ErrResponsesUnsupported
			}
			item.Parts = append(item.Parts, ir.Part{Kind: ir.PartText, Text: c.Text, Annotations: annotations})
		}
		if w.CallID != "" || w.Arguments != "" || w.Input != "" || len(w.Output) > 0 || len(w.Summary) > 0 || w.EncryptedContent != "" {
			return ir.Item{}, ErrResponsesUnsupported
		}
	case "function_call":
		if w.CallID == "" || w.Name == "" || (!partial && !json.Valid([]byte(w.Arguments))) || len(w.Content) > 0 || len(w.Output) > 0 {
			return ir.Item{}, ErrResponsesUnsupported
		}
	case "custom_tool_call":
		if w.CallID == "" || w.Name == "" || len(w.Content) > 0 || len(w.Output) > 0 {
			return ir.Item{}, ErrResponsesUnsupported
		}
	case "function_call_output", "custom_tool_call_output":
		if w.CallID == "" || len(w.Output) == 0 || bytes.Equal(bytes.TrimSpace(w.Output), []byte("null")) || len(w.Content) > 0 {
			return ir.Item{}, ErrResponsesUnsupported
		}
		if err := json.Unmarshal(w.Output, &item.Output); err != nil {
			return ir.Item{}, ErrResponsesUnsupported // structured output is not silently flattened
		}
	case "web_search_call":
		if w.Action != nil {
			action, err := decodeWebSearchAction(*w.Action)
			if err != nil {
				return ir.Item{}, err
			}
			item.WebSearchAction = &action
		}
	case "reasoning":
		for _, c := range w.Content {
			if c.Type != "reasoning_text" || len(c.Annotations) != 0 || len(c.Logprobs) != 0 {
				return ir.Item{}, ErrResponsesUnsupported
			}
			item.Parts = append(item.Parts, ir.Part{Kind: ir.PartThinking, Text: c.Text})
		}
		for _, s := range w.Summary {
			if s.Type != "summary_text" {
				return ir.Item{}, ErrResponsesUnsupported
			}
			item.Summary = append(item.Summary, s.Text)
		}
		if w.CallID != "" || len(w.Output) > 0 {
			return ir.Item{}, ErrResponsesUnsupported
		}
	default:
		return ir.Item{}, ErrResponsesUnsupported
	}
	return item, nil
}

// expandMessageContent rewrites the protocol's string shorthand for message
// content into its one equivalent text part (input_text, or output_text for an
// assistant message). Every other shape passes through untouched.
func expandMessageContent(body json.RawMessage) (json.RawMessage, error) {
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
		return nil, fmt.Errorf("%w: item shape", ErrResponsesUnsupported)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("%w: item shape", ErrResponsesUnsupported)
	}
	content := bytes.TrimSpace(fields["content"])
	if len(content) == 0 || content[0] != '"' || string(bytes.TrimSpace(fields["type"])) != `"message"` {
		return body, nil
	}
	var text, role string
	if err := json.Unmarshal(content, &text); err != nil {
		return nil, fmt.Errorf("%w: item shape", ErrResponsesUnsupported)
	}
	if raw, ok := fields["role"]; ok {
		if err := json.Unmarshal(raw, &role); err != nil {
			return nil, fmt.Errorf("%w: item shape", ErrResponsesUnsupported)
		}
	}
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	parts, err := json.Marshal([]responseWireContent{{Type: kind, Text: text}})
	if err != nil {
		return nil, err
	}
	fields["content"] = parts
	return json.Marshal(fields)
}

func encodeResponseItem(item ir.Item, partial ...bool) (responseWireItem, error) {
	isPartial := len(partial) > 0 && partial[0]
	w := responseWireItem{
		Type: item.Kind, ID: item.ID, Role: string(item.Role), Phase: item.Phase, Status: item.Status, Namespace: item.Namespace,
		CallID: item.CallID, Name: item.Name, Arguments: item.Arguments, Input: item.Input,
		EncryptedContent: item.EncryptedContent,
	}
	switch item.Kind {
	case "message":
		for _, p := range item.Parts {
			if p.Kind != ir.PartText {
				return w, ErrResponsesUnsupported
			}
			kind := "input_text"
			if item.Role == ir.RoleAssistant {
				kind = "output_text"
			}
			annotations, err := encodeResponseAnnotations(p.Annotations)
			if err != nil || kind != "output_text" && len(annotations) > 0 {
				return w, ErrResponsesUnsupported
			}
			w.Content = append(w.Content, responseWireContent{Type: kind, Text: p.Text, Annotations: annotations})
		}
	case "function_call":
		if item.CallID == "" || item.Name == "" || (!isPartial && !json.Valid([]byte(item.Arguments))) {
			return w, ErrResponsesUnsupported
		}
	case "custom_tool_call":
		if item.CallID == "" || item.Name == "" {
			return w, ErrResponsesUnsupported
		}
	case "function_call_output", "custom_tool_call_output":
		if item.CallID == "" {
			return w, ErrResponsesUnsupported
		}
		w.Output, _ = json.Marshal(item.Output)
	case "web_search_call":
		if item.WebSearchAction != nil {
			action, err := encodeWebSearchAction(*item.WebSearchAction)
			if err != nil {
				return w, err
			}
			w.Action = &action
		}
	case "reasoning":
		for _, part := range item.Parts {
			if part.Kind != ir.PartThinking {
				return w, ErrResponsesUnsupported
			}
			w.Content = append(w.Content, responseWireContent{Type: "reasoning_text", Text: part.Text})
		}
		for _, text := range item.Summary {
			w.Summary = append(w.Summary, responseWireSummary{Type: "summary_text", Text: text})
		}
	default:
		return w, ErrResponsesUnsupported
	}
	body, err := marshalResponseItem(w)
	if err != nil {
		return w, err
	}
	if _, err = decodeResponseItemMode(body, isPartial); err != nil {
		return w, err
	}
	if item.Kind != "message" && item.Kind != "reasoning" && len(item.Parts) != 0 || item.Kind != "reasoning" && len(item.Summary) != 0 || item.Kind != "web_search_call" && item.WebSearchAction != nil {
		return w, ErrResponsesUnsupported
	}
	return w, nil
}

type responsesWireTool struct {
	Type               string                             `json:"type"`
	Name               string                             `json:"name,omitempty"`
	Description        string                             `json:"description,omitempty"`
	Parameters         map[string]any                     `json:"parameters,omitempty"`
	Format             map[string]any                     `json:"format,omitempty"`
	Strict             *bool                              `json:"strict,omitempty"`
	DeferLoading       *bool                              `json:"defer_loading,omitempty"`
	Tools              []json.RawMessage                  `json:"tools,omitempty"`
	ExternalWebAccess  *bool                              `json:"external_web_access,omitempty"`
	IndexedWebAccess   *bool                              `json:"indexed_web_access,omitempty"`
	Filters            *responseWireWebSearchFilters      `json:"filters,omitempty"`
	UserLocation       *responseWireWebSearchUserLocation `json:"user_location,omitempty"`
	SearchContextSize  *string                            `json:"search_context_size,omitempty"`
	SearchContentTypes []string                           `json:"search_content_types,omitempty"`
}

type responsesWireRequest struct {
	Model             string            `json:"model"`
	Instructions      string            `json:"instructions,omitempty"`
	Input             json.RawMessage   `json:"input"`
	Tools             []json.RawMessage `json:"tools,omitempty"`
	ToolChoice        string            `json:"tool_choice"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Reasoning         map[string]string `json:"reasoning,omitempty"`
	Store             bool              `json:"store"`
	Stream            bool              `json:"stream"`
	Include           []string          `json:"include,omitempty"`
	ServiceTier       string            `json:"service_tier,omitempty"`
	PromptCacheKey    string            `json:"prompt_cache_key,omitempty"`
	Text              map[string]any    `json:"text,omitempty"`
	ClientMetadata    map[string]string `json:"client_metadata,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	MaxOutputTokens   *int              `json:"max_output_tokens,omitempty"`
}

// DecodeResponsesRequest admits the finite stateless text/function/custom/reasoning surface.
func DecodeResponsesRequest(body []byte) (ir.Request, error) {
	var w responsesWireRequest
	if err := strictJSON(body, &w); err != nil {
		return ir.Request{}, fmt.Errorf("%w: request shape", ErrResponsesUnsupported)
	}
	if strings.TrimSpace(w.Model) == "" || w.Input == nil || w.Store || (w.ToolChoice != "" && w.ToolChoice != "auto" && w.ToolChoice != "none" && w.ToolChoice != "required") {
		return ir.Request{}, ErrResponsesUnsupported
	}
	if w.MaxOutputTokens != nil && *w.MaxOutputTokens < 1 {
		return ir.Request{}, fmt.Errorf("%w: max_output_tokens", ErrResponsesUnsupported)
	}
	req := ir.Request{
		Model: w.Model, Instructions: w.Instructions, Stream: w.Stream,
		ToolChoice: ir.ToolChoice(w.ToolChoice),
		// Sampling controls are canonical IR: both codecs carry them.
		Sampling: ir.Sampling{Temperature: w.Temperature, TopP: w.TopP, MaxTokens: w.MaxOutputTokens},
		Responses: &ir.ResponsesControls{
			ParallelToolCalls: w.ParallelToolCalls, Store: w.Store,
			Include: w.Include, Reasoning: w.Reasoning, Text: w.Text,
			ServiceTier: w.ServiceTier, PromptCacheKey: w.PromptCacheKey, ClientMetadata: w.ClientMetadata,
		},
	}
	for _, include := range w.Include {
		if include != "reasoning.encrypted_content" {
			return ir.Request{}, ErrResponsesUnsupported
		}
	}
	for key := range w.Reasoning {
		if key != "effort" && key != "summary" && key != "context" {
			return ir.Request{}, ErrResponsesUnsupported
		}
	}
	if len(w.Text) > 0 {
		for key := range w.Text {
			if key != "verbosity" && key != "format" {
				return ir.Request{}, ErrResponsesUnsupported
			}
		}
	}
	if effort := w.Reasoning["effort"]; effort != "" {
		level, err := ir.LevelFromEffort(effort)
		if err != nil {
			return ir.Request{}, fmt.Errorf("%w: %w", ErrResponsesUnsupported, err)
		}
		req.Thinking = ir.ThinkingSpec{Level: level, Emit: ir.EmitFull}
	}
	items, err := decodeResponsesInput(w.Input)
	if err != nil {
		return ir.Request{}, err
	}
	req.Items = items
	for _, raw := range w.Tools {
		tool, err := decodeResponseTool(raw, false)
		if err != nil {
			return ir.Request{}, err
		}
		req.Tools = append(req.Tools, tool)
	}
	return req, nil
}

// decodeResponsesInput admits the two input shapes the protocol defines: a
// string, which is exactly one user text message, or an array of typed items.
func decodeResponsesInput(raw json.RawMessage) ([]ir.Item, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, ErrResponsesUnsupported
	}
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, ErrResponsesUnsupported
		}
		if text == "" {
			return nil, nil
		}
		return []ir.Item{{Kind: "message", Role: ir.RoleUser, Parts: []ir.Part{{Kind: ir.PartText, Text: text}}}}, nil
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(trimmed, &elems); err != nil {
			return nil, ErrResponsesUnsupported
		}
		var items []ir.Item
		for _, elem := range elems {
			item, err := decodeResponseItem(elem)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		return items, nil
	default:
		return nil, ErrResponsesUnsupported
	}
}

// EncodeResponsesRequest emits stateless Responses requests from canonical items.
func EncodeResponsesRequest(req ir.Request) ([]byte, error) {
	// Chat-shaped history and stop sequences have no Responses representation.
	if len(req.Messages) > 0 || len(req.System) > 0 || len(req.Sampling.Stop) > 0 {
		return nil, ErrResponsesUnsupported
	}
	controls := req.Responses
	if controls == nil {
		controls = &ir.ResponsesControls{}
	}
	if controls.Store {
		return nil, ErrResponsesUnsupported
	}
	w := responsesWireRequest{
		Model: req.Model, Instructions: req.Instructions, ToolChoice: string(req.ToolChoice),
		ParallelToolCalls: controls.ParallelToolCalls, Reasoning: controls.Reasoning, Store: false,
		Stream: req.Stream, Include: controls.Include, ServiceTier: controls.ServiceTier,
		PromptCacheKey: controls.PromptCacheKey, Text: controls.Text, ClientMetadata: controls.ClientMetadata,
		Temperature: req.Sampling.Temperature, TopP: req.Sampling.TopP, MaxOutputTokens: req.Sampling.MaxTokens,
	}
	if w.ToolChoice == "" {
		w.ToolChoice = "auto"
	}
	input := []json.RawMessage{}
	for _, item := range req.Items {
		wi, err := encodeResponseItem(item)
		if err != nil {
			return nil, err
		}
		body, err := marshalResponseItem(wi)
		if err != nil {
			return nil, err
		}
		input = append(input, body)
	}
	var err error
	if w.Input, err = json.Marshal(input); err != nil {
		return nil, err
	}
	for _, tool := range req.Tools {
		raw, err := encodeResponseTool(tool, false)
		if err != nil {
			return nil, err
		}
		w.Tools = append(w.Tools, raw)
	}
	body, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	if _, err = DecodeResponsesRequest(body); err != nil {
		return nil, err
	}
	return body, nil
}

// ProjectResponsesToChat admits only the subset whose semantics the Chat
// codec can preserve. It never strips a Responses-only control or item.
// Parallel tool use is the Chat default, so only an explicit serial request
// is unrepresentable; a reasoning effort rides the canonical thinking ladder,
// while a requested reasoning summary has no Chat equivalent.
func ProjectResponsesToChat(req ir.Request) (ir.Request, error) {
	c := req.Responses
	if c != nil && (c.Store || c.ParallelToolCalls != nil && !*c.ParallelToolCalls || len(c.Include) != 0 ||
		len(c.Text) != 0 || c.ServiceTier != "" || c.PromptCacheKey != "" || len(c.ClientMetadata) != 0) {
		return ir.Request{}, ErrResponsesUnsupported
	}
	projected := req
	if c != nil {
		for key := range c.Reasoning {
			if key != "effort" {
				return ir.Request{}, ErrResponsesUnsupported
			}
		}
		if effort := c.Reasoning["effort"]; effort != "" {
			level, err := ir.LevelFromEffort(effort)
			if err != nil {
				return ir.Request{}, fmt.Errorf("%w: %w", ErrResponsesUnsupported, err)
			}
			projected.Thinking = ir.ThinkingSpec{Level: level, Emit: ir.EmitFull}
		}
	}
	projected.Responses = nil
	projected.Items = nil
	projected.Messages = nil
	if req.Instructions != "" {
		projected.System = []ir.Part{{Kind: ir.PartText, Text: req.Instructions}}
	}
	for _, item := range req.Items {
		switch item.Kind {
		case "message":
			if item.ID != "" || item.Phase != "" || item.Status != "" || item.Role == ir.RoleDeveloper {
				return ir.Request{}, ErrResponsesUnsupported
			}
			for _, part := range item.Parts {
				if part.Kind != ir.PartText || len(part.Annotations) > 0 {
					return ir.Request{}, ErrResponsesUnsupported
				}
			}
			projected.Messages = append(projected.Messages, ir.Message{Role: item.Role, Parts: item.Parts})
		case "function_call":
			if item.ID != "" || item.Namespace != "" || item.Status != "" {
				return ir.Request{}, ErrResponsesUnsupported
			}
			projected.Messages = append(projected.Messages, ir.Message{Role: ir.RoleAssistant, Parts: []ir.Part{{
				Kind: ir.PartToolCall, ToolCall: &ir.ToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments},
			}}})
		case "function_call_output":
			if item.Name != "" {
				return ir.Request{}, ErrResponsesUnsupported
			}
			if item.ID != "" || item.Namespace != "" || item.Status != "" {
				return ir.Request{}, ErrResponsesUnsupported
			}
			projected.Messages = append(projected.Messages, ir.Message{Role: ir.RoleTool, Parts: []ir.Part{{
				Kind: ir.PartToolResult, ToolCallID: item.CallID, Text: item.Output,
			}}})
		default:
			return ir.Request{}, ErrResponsesUnsupported
		}
	}
	for _, tool := range req.Tools {
		if (tool.Kind != "" && tool.Kind != "function") || tool.Strict != nil || tool.DeferLoading != nil || len(tool.Tools) > 0 || tool.WebSearch != nil || len(tool.Format) != 0 {
			return ir.Request{}, ErrResponsesUnsupported
		}
	}
	if strings.TrimSpace(projected.Model) == "" {
		return ir.Request{}, ErrResponsesUnsupported
	}
	return projected, nil
}

type responsesWireUsage struct {
	InputTokens  *int `json:"input_tokens"`
	OutputTokens *int `json:"output_tokens"`
	TotalTokens  *int `json:"total_tokens"`
	InputDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
	OutputDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
}

func usageFromResponses(w *responsesWireUsage) (*ir.Usage, error) {
	if w == nil || w.InputTokens == nil || w.OutputTokens == nil || w.TotalTokens == nil || *w.InputTokens < 0 || *w.OutputTokens < 0 || *w.TotalTokens < 0 {
		return nil, ErrResponsesUnsupported
	}
	if *w.TotalTokens != *w.InputTokens+*w.OutputTokens {
		return nil, ErrResponsesUnsupported
	}
	u := &ir.Usage{TokensIn: *w.InputTokens, TokensOut: *w.OutputTokens}
	if w.InputDetails != nil {
		u.CacheReadTokens = w.InputDetails.CachedTokens
	}
	if w.OutputDetails != nil {
		u.ReasoningTokens = w.OutputDetails.ReasoningTokens
	}
	if u.CacheReadTokens < 0 || u.CacheReadTokens > u.TokensIn || u.ReasoningTokens < 0 || u.ReasoningTokens > u.TokensOut {
		return nil, ErrResponsesUnsupported
	}
	return u, nil
}

func usageToResponses(u ir.Usage) responsesWireUsage {
	input, output, total := u.TokensIn, u.TokensOut, u.TokensIn+u.TokensOut
	return responsesWireUsage{
		InputTokens: &input, OutputTokens: &output, TotalTokens: &total,
		InputDetails: &struct {
			CachedTokens int `json:"cached_tokens"`
		}{u.CacheReadTokens},
		OutputDetails: &struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		}{u.ReasoningTokens},
	}
}

type responsesWireResponse struct {
	CreatedAt         *int64            `json:"created_at,omitempty"`
	Instructions      *string           `json:"instructions,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	ToolChoice        *string           `json:"tool_choice,omitempty"`
	Tools             []json.RawMessage `json:"tools,omitempty"`
	Reasoning         map[string]string `json:"reasoning,omitempty"`
	Text              map[string]any    `json:"text,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	ServiceTier       *string           `json:"service_tier,omitempty"`
	PromptCacheKey    *string           `json:"prompt_cache_key,omitempty"`
	Store             *bool             `json:"store,omitempty"`
	Background        *bool             `json:"background,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	MaxOutputTokens   *int              `json:"max_output_tokens,omitempty"`
	Truncation        *string           `json:"truncation,omitempty"`
	// Only null is representable for these state/error fields.
	PreviousResponseID json.RawMessage     `json:"previous_response_id,omitempty"`
	Error              json.RawMessage     `json:"error,omitempty"`
	IncompleteDetails  json.RawMessage     `json:"incomplete_details,omitempty"`
	User               json.RawMessage     `json:"user,omitempty"`
	ID                 string              `json:"id"`
	Object             string              `json:"object,omitempty"`
	Status             string              `json:"status,omitempty"`
	Model              string              `json:"model,omitempty"`
	Output             []json.RawMessage   `json:"output,omitempty"`
	Usage              *responsesWireUsage `json:"usage,omitempty"`
	EndTurn            *bool               `json:"end_turn,omitempty"`
}

// DecodeResponsesResponse requires a completed response with explicit usage.
func DecodeResponsesResponse(body []byte) (ir.Response, error) {
	var w responsesWireResponse
	if err := strictJSON(body, &w); err != nil {
		return ir.Response{}, ErrResponsesUnsupported
	}
	// Only a turn the backend ended is admitted: completed, or incomplete
	// with its reason. A failed or still-running response is never decoded.
	if w.ID == "" || w.Status != "completed" && w.Status != "incomplete" || validateResponseEnvelope(w) != nil {
		return ir.Response{}, ErrResponsesUnsupported
	}
	incomplete, err := responsesIncompleteFinish(w)
	if err != nil {
		return ir.Response{}, err
	}
	u, err := usageFromResponses(w.Usage)
	if err != nil {
		return ir.Response{}, err
	}
	out := ir.Response{ID: w.ID, Details: detailsFromResponses(w), Model: w.Model, FinishReason: ir.FinishStop, Usage: *u, UsageReported: true}
	for _, raw := range w.Output {
		item, err := decodeResponseItem(raw)
		if err != nil {
			return ir.Response{}, err
		}
		out.Items = append(out.Items, item)
		if item.Kind == "function_call" || item.Kind == "custom_tool_call" {
			out.FinishReason = ir.FinishToolCalls
		}
	}
	if incomplete != "" {
		out.FinishReason = incomplete
	}
	return out, nil
}

// responsesIncompleteFinish maps an incomplete envelope onto its canonical
// finish. A completed envelope yields ""; an incomplete one must name a
// reason the IR carries, and incomplete details on any other status refuse.
func responsesIncompleteFinish(w responsesWireResponse) (ir.FinishReason, error) {
	if w.Status != "incomplete" {
		if len(w.IncompleteDetails) > 0 && !nullJSON(w.IncompleteDetails) {
			return "", ErrResponsesUnsupported
		}
		return "", nil
	}
	var details struct {
		Reason string `json:"reason"`
	}
	if err := strictJSON(w.IncompleteDetails, &details); err != nil {
		return "", ErrResponsesUnsupported
	}
	switch details.Reason {
	case "max_output_tokens":
		return ir.FinishLength, nil
	case "content_filter":
		return ir.FinishContentFilter, nil
	default:
		return "", ErrResponsesUnsupported
	}
}

// EncodeResponsesResponse refuses response parts without a lossless Responses
// representation. A length or content-filter finish encodes as incomplete and
// an error finish as failed: a partial or failed reply is never labeled
// completed.
func EncodeResponsesResponse(resp ir.Response, id string) ([]byte, error) {
	if resp.ID != "" {
		id = resp.ID
	}
	if id == "" {
		return nil, ErrResponsesUnsupported
	}
	status, incompleteReason, err := responsesFinishStatus(resp.FinishReason)
	if err != nil {
		return nil, err
	}
	items := resp.Items
	if len(items) == 0 {
		item := ir.Item{Kind: "message", Role: ir.RoleAssistant, Parts: []ir.Part{}}
		for _, p := range resp.Content {
			switch p.Kind {
			case ir.PartText:
				item.Parts = append(item.Parts, p)
			case ir.PartToolCall:
				if p.ToolCall == nil {
					return nil, ErrResponsesUnsupported
				}
				items = append(items, ir.Item{Kind: "function_call", CallID: p.ToolCall.ID, Name: p.ToolCall.Name, Arguments: p.ToolCall.Arguments})
			default:
				return nil, ErrResponsesUnsupported
			}
		}
		if len(item.Parts) > 0 {
			items = append([]ir.Item{item}, items...)
		}
	}
	wire := responsesWireResponse{ID: id, Object: "response", Status: "completed", Model: resp.Model, Usage: func() *responsesWireUsage { u := usageToResponses(resp.Usage); return &u }(), Output: []json.RawMessage{}}
	for _, item := range items {
		w, err := encodeResponseItem(item)
		if err != nil {
			return nil, err
		}
		b, err := marshalResponseItem(w)
		if err != nil {
			return nil, err
		}
		wire.Output = append(wire.Output, b)
	}
	if err := applyResponseDetails(&wire, resp.Details); err != nil {
		return nil, err
	}
	if status != "completed" {
		// Each output item was validated above; the envelope decoder admits
		// only completed replies, so it cannot re-check this one.
		wire.Object = "response"
		wire.Status = status
		if incompleteReason != "" {
			if wire.IncompleteDetails, err = json.Marshal(map[string]string{"reason": incompleteReason}); err != nil {
				return nil, err
			}
		}
		return json.Marshal(wire)
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	if _, err = DecodeResponsesResponse(body); err != nil {
		return nil, err
	}
	return body, nil
}

// responsesFinishStatus maps a canonical finish onto the Responses status and
// incomplete reason. An unknown finish is refused rather than labeled.
func responsesFinishStatus(finish ir.FinishReason) (status, incompleteReason string, err error) {
	switch finish {
	case "", ir.FinishStop, ir.FinishToolCalls:
		return "completed", "", nil
	case ir.FinishLength:
		return "incomplete", "max_output_tokens", nil
	case ir.FinishContentFilter:
		return "incomplete", "content_filter", nil
	case ir.FinishError:
		return "failed", "", nil
	default:
		return "", "", ErrResponsesUnsupported
	}
}

type responsesWireEvent struct {
	Type            string                  `json:"type"`
	SequenceNumber  *int                    `json:"sequence_number,omitempty"`
	Arguments       string                  `json:"arguments,omitempty"`
	Input           string                  `json:"input,omitempty"`
	Response        *responsesWireResponse  `json:"response,omitempty"`
	Item            json.RawMessage         `json:"item,omitempty"`
	Part            *responseWireContent    `json:"part,omitempty"`
	Delta           string                  `json:"delta,omitempty"`
	Text            string                  `json:"text,omitempty"`
	ItemID          string                  `json:"item_id,omitempty"`
	CallID          string                  `json:"call_id,omitempty"`
	OutputIndex     int                     `json:"output_index,omitempty"`
	ContentIndex    int                     `json:"content_index,omitempty"`
	SummaryIndex    int                     `json:"summary_index,omitempty"`
	AnnotationIndex int                     `json:"annotation_index,omitempty"`
	Annotation      *responseWireAnnotation `json:"annotation,omitempty"`
}

// DecodeResponsesStreamEvent translates supported typed SSE events into the IR.
func DecodeResponsesStreamEvent(body []byte) (ir.ResponsesStreamEvent, error) {
	var w responsesWireEvent
	if err := strictJSON(body, &w); err != nil {
		return ir.ResponsesStreamEvent{}, ErrResponsesUnsupported
	}
	if err := validateResponseEventFields(body, w.Type); err != nil {
		return ir.ResponsesStreamEvent{}, err
	}
	ev := ir.ResponsesStreamEvent{Kind: w.Type, SequenceNumber: w.SequenceNumber, Delta: w.Delta, ItemID: w.ItemID, OutputIndex: w.OutputIndex, ContentIndex: w.ContentIndex, SummaryIndex: w.SummaryIndex, AnnotationIndex: w.AnnotationIndex}
	switch w.Type {
	case "response.created", "response.in_progress":
		if w.Response == nil || w.Response.ID == "" || validateResponseEnvelope(*w.Response) != nil {
			return ev, ErrResponsesUnsupported
		}
		ev.ResponseID = w.Response.ID
		ev.Response = &ir.Response{ID: w.Response.ID, Model: w.Response.Model, Details: detailsFromResponses(*w.Response)}
		for _, raw := range w.Response.Output {
			item, err := decodeResponseItemPartial(raw)
			if err != nil {
				return ev, err
			}
			ev.Response.Items = append(ev.Response.Items, item)
		}
	case "response.output_item.added", "response.output_item.done":
		if len(w.Item) == 0 {
			return ev, ErrResponsesUnsupported
		}
		var item ir.Item
		var err error
		if w.Type == "response.output_item.added" {
			item, err = decodeResponseItemPartial(w.Item)
		} else {
			item, err = decodeResponseItem(w.Item)
		}
		if err != nil {
			return ev, err
		}
		ev.Item = &item
	case "response.output_text.delta", "response.function_call_arguments.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.custom_tool_call_input.delta":
		if w.Delta == "" {
			return ev, ErrResponsesUnsupported
		}
	case "response.output_text.done", "response.function_call_arguments.done", "response.reasoning_summary_text.done", "response.reasoning_text.done", "response.custom_tool_call_input.done":
		ev.Delta = w.Text
		if w.Type == "response.function_call_arguments.done" {
			ev.Delta = w.Arguments
		}
		if w.Type == "response.custom_tool_call_input.done" {
			ev.Delta = w.Input
		}
	case "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		if w.Part == nil || (w.Part.Type != "input_text" && w.Part.Type != "output_text" && w.Part.Type != "summary_text" && w.Part.Type != "reasoning_text") || len(w.Part.Logprobs) != 0 {
			return ev, ErrResponsesUnsupported
		}
		annotations, err := decodeResponseAnnotations(w.Part.Annotations)
		if err != nil || w.Part.Type != "output_text" && len(annotations) > 0 {
			return ev, ErrResponsesUnsupported
		}
		ev.Part = &ir.Part{Kind: ir.PartText, Text: w.Part.Text, Annotations: annotations}
		if w.Part.Type == "summary_text" || w.Part.Type == "reasoning_text" {
			ev.Part.Kind = ir.PartThinking
		}
	case "response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed":
		if w.ItemID == "" {
			return ev, ErrResponsesUnsupported
		}
	case "response.output_text.annotation.added":
		if w.Annotation == nil || w.ItemID == "" || w.AnnotationIndex < 0 {
			return ev, ErrResponsesUnsupported
		}
		annotations, err := decodeResponseAnnotations([]responseWireAnnotation{*w.Annotation})
		if err != nil {
			return ev, err
		}
		ev.Annotation = &annotations[0]
	case "response.completed", "response.incomplete":
		if w.Response == nil || w.Response.ID == "" || validateResponseEnvelope(*w.Response) != nil {
			return ev, ErrResponsesUnsupported
		}
		ev.ResponseID = w.Response.ID
		ev.Response = &ir.Response{ID: w.Response.ID, Model: w.Response.Model, Details: detailsFromResponses(*w.Response)}
		usage, err := usageFromResponses(w.Response.Usage)
		if err != nil {
			return ev, err
		}
		// The terminal kind and the snapshot status must agree; an incomplete
		// terminal carries the reason the canonical finish records.
		if w.Type == "response.completed" && w.Response.Status != "" && w.Response.Status != "completed" ||
			w.Type == "response.incomplete" && w.Response.Status != "incomplete" {
			return ev, ErrResponsesUnsupported
		}
		if ev.Response.FinishReason, err = responsesIncompleteFinish(*w.Response); err != nil {
			return ev, err
		}
		for _, raw := range w.Response.Output {
			item, err := decodeResponseItem(raw)
			if err != nil {
				return ev, err
			}
			ev.Response.Items = append(ev.Response.Items, item)
		}
		ev.Response.Usage = *usage
		ev.Response.UsageReported = true
		ev.Usage = usage
		ev.EndTurn = w.Response.EndTurn
	case "response.failed", "response.error":
		return ev, ErrResponsesUnsupported
	default:
		return ev, ErrResponsesUnsupported
	}
	return ev, nil
}

func validateResponseEventFields(body []byte, kind string) error {
	keys := []string{"type", "sequence_number"}
	switch kind {
	case "response.created", "response.in_progress", "response.completed", "response.incomplete":
		keys = append(keys, "response")
	case "response.output_item.added", "response.output_item.done":
		keys = append(keys, "output_index", "item")
	case "response.output_text.delta", "response.reasoning_text.delta":
		keys = append(keys, "output_index", "item_id", "content_index", "delta")
	case "response.output_text.done", "response.reasoning_text.done":
		keys = append(keys, "output_index", "item_id", "content_index", "text")
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		keys = append(keys, "output_index", "item_id", "delta")
	case "response.function_call_arguments.done":
		keys = append(keys, "output_index", "item_id", "arguments")
	case "response.custom_tool_call_input.done":
		keys = append(keys, "output_index", "item_id", "input")
	case "response.reasoning_summary_text.delta":
		keys = append(keys, "output_index", "item_id", "summary_index", "delta")
	case "response.reasoning_summary_text.done":
		keys = append(keys, "output_index", "item_id", "summary_index", "text")
	case "response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed":
		keys = append(keys, "item_id", "output_index")
	case "response.output_text.annotation.added":
		keys = append(keys, "item_id", "output_index", "content_index", "annotation_index", "annotation")
	case "response.content_part.added", "response.content_part.done":
		keys = append(keys, "output_index", "item_id", "content_index", "part")
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		keys = append(keys, "output_index", "item_id", "summary_index", "part")
	default:
		return ErrResponsesUnsupported
	}
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return ErrResponsesUnsupported
	}
	for key := range fields {
		if !allowed[key] {
			return ErrResponsesUnsupported
		}
	}
	return nil
}

func decodeResponseItemPartial(body json.RawMessage) (ir.Item, error) {
	return decodeResponseItemMode(body, true)
}

func validateResponseItemFields(body []byte, kind string) error {
	common := map[string]bool{"type": true, "id": true, "status": true}
	switch kind {
	case "message":
		for _, k := range []string{"role", "phase", "content"} {
			common[k] = true
		}
	case "function_call":
		for _, k := range []string{"call_id", "name", "namespace", "arguments"} {
			common[k] = true
		}
	case "custom_tool_call":
		for _, k := range []string{"call_id", "name", "namespace", "input"} {
			common[k] = true
		}
	case "function_call_output", "custom_tool_call_output":
		for _, k := range []string{"call_id", "name", "namespace", "output"} {
			common[k] = true
		}
	case "web_search_call":
		common["action"] = true
	case "reasoning":
		for _, k := range []string{"summary", "content", "encrypted_content"} {
			common[k] = true
		}
	default:
		return ErrResponsesUnsupported
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return ErrResponsesUnsupported
	}
	for key := range fields {
		if !common[key] {
			return ErrResponsesUnsupported
		}
	}
	return nil
}

func marshalResponseItem(w responseWireItem) ([]byte, error) {
	body, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	switch w.Type {
	case "message":
		fields["content"] = w.Content
		if w.Content == nil {
			fields["content"] = []any{}
		}
	case "reasoning":
		fields["summary"] = w.Summary
		if w.Summary == nil {
			fields["summary"] = []any{}
		}
	case "function_call":
		fields["arguments"] = w.Arguments
	case "custom_tool_call":
		fields["input"] = w.Input
	}
	return json.Marshal(fields)
}

// EncodeResponsesStreamEvent renders an IR item lifecycle event as Responses JSON.
func EncodeResponsesStreamEvent(ev ir.ResponsesStreamEvent) ([]byte, error) {
	w := responsesWireEvent{Type: ev.Kind, SequenceNumber: ev.SequenceNumber, Delta: ev.Delta, ItemID: ev.ItemID, OutputIndex: ev.OutputIndex, ContentIndex: ev.ContentIndex, SummaryIndex: ev.SummaryIndex, AnnotationIndex: ev.AnnotationIndex}
	switch ev.Kind {
	case "response.created", "response.in_progress":
		if ev.ResponseID == "" {
			return nil, ErrResponsesUnsupported
		}
		w.Response = &responsesWireResponse{ID: ev.ResponseID}
		if ev.Response != nil {
			w.Response.Model = ev.Response.Model
			if err := applyResponseDetails(w.Response, ev.Response.Details); err != nil {
				return nil, err
			}
		}
	case "response.output_item.added", "response.output_item.done":
		if ev.Item == nil {
			return nil, ErrResponsesUnsupported
		}
		item, err := encodeResponseItem(*ev.Item, ev.Kind == "response.output_item.added")
		if err != nil {
			return nil, err
		}
		w.Item, _ = marshalResponseItem(item)
	case "response.output_text.delta", "response.function_call_arguments.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.custom_tool_call_input.delta":
		if ev.Delta == "" {
			return nil, ErrResponsesUnsupported
		}
	case "response.output_text.done", "response.function_call_arguments.done", "response.reasoning_summary_text.done", "response.reasoning_text.done", "response.custom_tool_call_input.done":
		w.Delta = ""
		w.Text = ev.Delta
		if ev.Kind == "response.function_call_arguments.done" {
			w.Text = ""
			w.Arguments = ev.Delta
		}
		if ev.Kind == "response.custom_tool_call_input.done" {
			w.Text = ""
			w.Input = ev.Delta
		}
	case "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		if ev.Part == nil || (ev.Part.Kind != ir.PartText && ev.Part.Kind != ir.PartThinking) {
			return nil, ErrResponsesUnsupported
		}
		kind := "output_text"
		if strings.Contains(ev.Kind, "reasoning_summary_part") {
			kind = "summary_text"
		} else if ev.Part.Kind == ir.PartThinking {
			kind = "reasoning_text"
		}
		annotations, err := encodeResponseAnnotations(ev.Part.Annotations)
		if err != nil || kind != "output_text" && len(annotations) > 0 {
			return nil, ErrResponsesUnsupported
		}
		w.Part = &responseWireContent{Type: kind, Text: ev.Part.Text, Annotations: annotations}
	case "response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed":
		if ev.ItemID == "" {
			return nil, ErrResponsesUnsupported
		}
	case "response.output_text.annotation.added":
		if ev.Annotation == nil || ev.ItemID == "" || ev.AnnotationIndex < 0 {
			return nil, ErrResponsesUnsupported
		}
		annotations, err := encodeResponseAnnotations([]ir.URLCitation{*ev.Annotation})
		if err != nil {
			return nil, err
		}
		w.Annotation = &annotations[0]
	case "response.completed", "response.incomplete":
		// The terminal kind is decided by the finish: a length or
		// content-filter finish is incomplete, never completed.
		if ev.ResponseID == "" || ev.Usage == nil {
			return nil, ErrResponsesUnsupported
		}
		var finish ir.FinishReason
		if ev.Response != nil {
			finish = ev.Response.FinishReason
		}
		status, reason, err := responsesFinishStatus(finish)
		if err != nil || "response."+status != ev.Kind {
			return nil, ErrResponsesUnsupported
		}
		u := usageToResponses(*ev.Usage)
		w.Response = &responsesWireResponse{ID: ev.ResponseID, Usage: &u, EndTurn: ev.EndTurn}
		if ev.Response != nil {
			w.Response.Model = ev.Response.Model
			if err := applyResponseDetails(w.Response, ev.Response.Details); err != nil {
				return nil, err
			}
			for _, item := range ev.Response.Items {
				wi, err := encodeResponseItem(item)
				if err != nil {
					return nil, err
				}
				b, err := marshalResponseItem(wi)
				if err != nil {
					return nil, err
				}
				w.Response.Output = append(w.Response.Output, b)
			}
		}
		w.Response.Status = status
		w.Response.Object = "response"
		if reason != "" {
			if w.Response.IncompleteDetails, err = json.Marshal(map[string]string{"reason": reason}); err != nil {
				return nil, err
			}
		}
	case "response.failed":
		// The gateway's own failure terminal: no upstream text crosses it.
		if ev.ResponseID == "" {
			return nil, ErrResponsesUnsupported
		}
		w.Response = &responsesWireResponse{
			ID: ev.ResponseID, Object: "response", Status: "failed",
			Error: json.RawMessage(`{"code":"gateway_stream_failed","message":"upstream stream failed"}`),
		}
	default:
		return nil, ErrResponsesUnsupported
	}
	body, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	// Required zero indexes must be emitted for item/content events.
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if ev.Item != nil || ev.ItemID != "" {
		fields["output_index"] = ev.OutputIndex
	}
	if ev.Part != nil || strings.Contains(ev.Kind, "output_text") || strings.Contains(ev.Kind, "reasoning_text") {
		fields["content_index"] = ev.ContentIndex
	}
	if ev.Annotation != nil {
		fields["annotation_index"] = ev.AnnotationIndex
	}
	if strings.Contains(ev.Kind, "reasoning_summary") {
		fields["summary_index"] = ev.SummaryIndex
		delete(fields, "content_index")
	}
	return json.Marshal(fields)
}

func validateResponseEnvelope(w responsesWireResponse) error {
	if w.Object != "" && w.Object != "response" || w.Store != nil && *w.Store || w.Background != nil && *w.Background {
		return ErrResponsesUnsupported
	}
	for _, v := range []json.RawMessage{w.PreviousResponseID, w.Error, w.User} {
		if len(v) > 0 && !bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return ErrResponsesUnsupported
		}
	}
	for _, raw := range w.Tools {
		if _, err := decodeResponseTool(raw, false); err != nil {
			return err
		}
	}
	return nil
}

func detailsFromResponses(w responsesWireResponse) *ir.ResponseDetails {
	d := &ir.ResponseDetails{Object: w.Object, Status: w.Status, CreatedAt: w.CreatedAt, Instructions: w.Instructions, ParallelToolCalls: w.ParallelToolCalls, ToolChoice: w.ToolChoice, Reasoning: w.Reasoning, Text: w.Text, Metadata: w.Metadata, ServiceTier: w.ServiceTier, PromptCacheKey: w.PromptCacheKey, Store: w.Store, Background: w.Background, Temperature: w.Temperature, TopP: w.TopP, MaxOutputTokens: w.MaxOutputTokens, Truncation: w.Truncation}
	for _, raw := range w.Tools {
		tool, err := decodeResponseTool(raw, false)
		if err == nil {
			d.Tools = append(d.Tools, tool)
		}
	}
	return d
}

func applyResponseDetails(w *responsesWireResponse, d *ir.ResponseDetails) error {
	if d == nil {
		return nil
	}
	w.Object = d.Object
	w.Status = d.Status
	w.CreatedAt = d.CreatedAt
	w.Instructions = d.Instructions
	w.ParallelToolCalls = d.ParallelToolCalls
	w.ToolChoice = d.ToolChoice
	w.Reasoning = d.Reasoning
	w.Text = d.Text
	w.Metadata = d.Metadata
	w.ServiceTier = d.ServiceTier
	w.PromptCacheKey = d.PromptCacheKey
	w.Store = d.Store
	w.Background = d.Background
	w.Temperature = d.Temperature
	w.TopP = d.TopP
	w.MaxOutputTokens = d.MaxOutputTokens
	w.Truncation = d.Truncation
	for _, tool := range d.Tools {
		raw, err := encodeResponseTool(tool, false)
		if err != nil {
			return err
		}
		w.Tools = append(w.Tools, raw)
	}
	return nil
}

// ValidateChatResponsesSubset classifies every field of an OpenAI-compatible
// Chat reply (or stream chunk) on an explicitly selected Responses exchange
// before the legacy Chat decoder reads it, because that decoder silently
// ignores whatever it does not declare. The classes are:
//
//   - carried: choices[0].index, finish_reason, message/delta role, content
//     and tool_calls, and usage prompt/completion/total tokens;
//   - envelope metadata, ignored: id, object, created, model,
//     system_fingerprint, service_tier, obfuscation (stream padding) and the
//     aggregator extensions provider (OpenRouter), x_groq (Groq) and
//     choices[].native_finish_reason (OpenRouter's raw provider finish);
//   - content-filter annotations, ignored while nothing was filtered:
//     prompt_filter_results and choices[].content_filter_results (Azure). A
//     filtered prompt is refused; filtered output must carry the
//     content_filter finish, which is served as incomplete;
//   - carried usage details: prompt_tokens_details.cached_tokens and
//     completion_tokens_details.reasoning_tokens, which must be non-negative
//     integers because they reach the cost row;
//   - accounting, ignored: every other usage member (counters, costs, timings
//     and flags) as long as it is numeric, boolean or null, or an object or
//     array of those;
//   - empty-only: logprobs, refusal, annotations, audio, function_call,
//     reasoning_content and every other field are admitted only when they
//     carry nothing (null, "", [], {} or an object of empty members). With
//     content they would be dropped, so they are refused.
//
// A finish other than stop, tool_calls, length or content_filter (or the
// end_turn and max_tokens aliases the Chat decoder maps faithfully) is
// refused rather than relabeled.
func ValidateChatResponsesSubset(body []byte, stream bool) error {
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(body)), 0); err != nil {
		return ErrResponsesUnsupported
	}
	if err := validateChatUsagePresence(body, stream); err != nil {
		return err
	}
	top, err := jsonObject(body)
	if err != nil {
		return err
	}
	for key, raw := range top {
		switch {
		case key == "choices", key == "usage", chatEnvelopeMetadata[key]:
		case key == "prompt_filter_results":
			if filterBlocked(raw) {
				return chatFieldRefused(key)
			}
		case !emptyJSON(raw):
			return chatFieldRefused(key)
		}
	}
	if err := validateChatUsageCounters(top["usage"]); err != nil {
		return err
	}
	var choices []json.RawMessage
	if raw, ok := top["choices"]; ok && !nullJSON(raw) {
		if err := json.Unmarshal(raw, &choices); err != nil {
			return chatFieldRefused("choices")
		}
	}
	if len(choices) > 1 || !stream && len(choices) != 1 {
		return fmt.Errorf("%w: chat reply carries %d choices", ErrResponsesUnsupported, len(choices))
	}
	for _, raw := range choices {
		if err := validateChatChoice(raw, stream); err != nil {
			return err
		}
	}
	// The carried fields must also have the types the legacy decoder reads.
	var w ChatResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return ErrResponsesUnsupported
	}
	return nil
}

// chatEnvelopeMetadata lists the documented reply envelope fields that carry
// no assistant output.
var chatEnvelopeMetadata = map[string]bool{
	"id": true, "object": true, "created": true, "model": true,
	"system_fingerprint": true, "service_tier": true, "obfuscation": true,
	"provider": true, "x_groq": true,
}

// chatFinishCarried lists the finish reasons the Chat decoder maps faithfully.
var chatFinishCarried = map[string]bool{
	"stop": true, "end_turn": true, "tool_calls": true,
	"length": true, "max_tokens": true, "content_filter": true,
}

func chatFieldRefused(field string) error {
	return fmt.Errorf("%w: chat reply field %q", ErrResponsesUnsupported, field)
}

func validateChatChoice(raw json.RawMessage, stream bool) error {
	fields, err := jsonObject(raw)
	if err != nil {
		return err
	}
	body := "message"
	if stream {
		body = "delta"
	}
	if _, ok := fields[body]; !ok && !stream {
		return chatFieldRefused("choices[].message")
	}
	var finish *string
	var filtered bool
	for key, v := range fields {
		switch key {
		case "index":
			var index int
			if err := json.Unmarshal(v, &index); err != nil || index != 0 {
				return chatFieldRefused("choices[].index")
			}
		case "finish_reason":
			if err := json.Unmarshal(v, &finish); err != nil || finish != nil && *finish != "" && !chatFinishCarried[*finish] {
				return chatFieldRefused("choices[].finish_reason")
			}
		case "native_finish_reason":
			var native *string
			if err := json.Unmarshal(v, &native); err != nil {
				return chatFieldRefused("choices[]." + key)
			}
		case "content_filter_results":
			filtered = filterBlocked(v)
		case body:
			if err := validateChatMessage(v, stream); err != nil {
				return err
			}
		default:
			if !emptyJSON(v) {
				return chatFieldRefused("choices[]." + key)
			}
		}
	}
	// Filtered output is served only as the content_filter finish it caused.
	if filtered && (finish == nil || *finish != "content_filter") {
		return chatFieldRefused("choices[].content_filter_results")
	}
	return nil
}

func validateChatMessage(raw json.RawMessage, delta bool) error {
	if delta && nullJSON(raw) {
		return nil
	}
	fields, err := jsonObject(raw)
	if err != nil {
		return err
	}
	var role *string
	for key, v := range fields {
		switch key {
		case "role":
			if err := json.Unmarshal(v, &role); err != nil {
				return chatFieldRefused("role")
			}
		case "content":
			var content *string
			if err := json.Unmarshal(v, &content); err != nil {
				return chatFieldRefused("content")
			}
		case "tool_calls":
			var calls []json.RawMessage
			if err := json.Unmarshal(v, &calls); err != nil || delta && len(calls) > 1 {
				return chatFieldRefused("tool_calls")
			}
			for _, call := range calls {
				if err := validateChatToolCall(call, delta); err != nil {
					return err
				}
			}
		default:
			if !emptyJSON(v) {
				return chatFieldRefused(key)
			}
		}
	}
	if delta && role != nil && *role != "" && *role != "assistant" || !delta && (role == nil || *role != "assistant") {
		return chatFieldRefused("role")
	}
	return nil
}

func validateChatToolCall(raw json.RawMessage, delta bool) error {
	fields, err := jsonObject(raw)
	if err != nil {
		return err
	}
	var id, kind, name, arguments string
	for key, v := range fields {
		var target *string
		switch key {
		case "index":
			var index int
			if err := json.Unmarshal(v, &index); err != nil || index < 0 {
				return chatFieldRefused("tool_calls[].index")
			}
			continue
		case "id":
			target = &id
		case "type":
			target = &kind
		case "function":
			function, err := jsonObject(v)
			if err != nil {
				return err
			}
			for fkey, fv := range function {
				switch fkey {
				case "name":
					target = &name
				case "arguments":
					target = &arguments
				default:
					if !emptyJSON(fv) {
						return chatFieldRefused("tool_calls[].function." + fkey)
					}
					continue
				}
				if err := json.Unmarshal(fv, target); err != nil {
					return chatFieldRefused("tool_calls[].function." + fkey)
				}
			}
			continue
		default:
			if !emptyJSON(v) {
				return chatFieldRefused("tool_calls[]." + key)
			}
			continue
		}
		if err := json.Unmarshal(v, target); err != nil {
			return chatFieldRefused("tool_calls[]." + key)
		}
	}
	if delta {
		if kind != "" && kind != "function" {
			return chatFieldRefused("tool_calls[].type")
		}
		return nil
	}
	if kind != "function" || id == "" || name == "" || !json.Valid([]byte(arguments)) {
		return chatFieldRefused("tool_calls[]")
	}
	return nil
}

func validateChatUsageCounters(raw json.RawMessage) error {
	if raw == nil || nullJSON(raw) {
		return nil
	}
	fields, err := jsonObject(raw)
	if err != nil {
		return err
	}
	for key, v := range fields {
		switch key {
		case "prompt_tokens", "completion_tokens", "total_tokens":
			// Presence and arithmetic are checked by validateChatUsagePresence.
		case "prompt_tokens_details", "completion_tokens_details":
			if nullJSON(v) {
				continue
			}
			details, err := jsonObject(v)
			if err != nil {
				return err
			}
			for detail, counter := range details {
				carried := key == "prompt_tokens_details" && detail == "cached_tokens" ||
					key == "completion_tokens_details" && detail == "reasoning_tokens"
				if carried && !nullJSON(counter) && !counterJSON(counter) || !carried && !accountingJSON(counter) {
					return chatFieldRefused("usage." + key + "." + detail)
				}
			}
		default:
			if !accountingJSON(v) {
				return chatFieldRefused("usage." + key)
			}
		}
	}
	return nil
}

func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, ErrResponsesUnsupported
	}
	return fields, nil
}

func nullJSON(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// emptyJSON reports whether raw carries nothing: null, "", [], {} or an object
// whose members all carry nothing.
func emptyJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	switch string(trimmed) {
	case "null", `""`, "[]", "{}":
		return true
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	fields, err := jsonObject(trimmed)
	if err != nil {
		return false
	}
	for _, v := range fields {
		if !emptyJSON(v) {
			return false
		}
	}
	return true
}

// accountingJSON reports whether raw is accounting data: a number, boolean
// or null, or an object or array of those. Text is never accounting.
func accountingJSON(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	var accounting func(any) bool
	accounting = func(v any) bool {
		switch t := v.(type) {
		case nil, bool, float64:
			return true
		case map[string]any:
			for _, child := range t {
				if !accounting(child) {
					return false
				}
			}
			return true
		case []any:
			for _, child := range t {
				if !accounting(child) {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	return accounting(v)
}

// filterBlocked reports whether a content-filter annotation records that
// anything was filtered; an unreadable annotation counts as filtered.
func filterBlocked(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return true
	}
	var blocked func(any) bool
	blocked = func(v any) bool {
		switch t := v.(type) {
		case map[string]any:
			for key, child := range t {
				if key == "filtered" && child == true || blocked(child) {
					return true
				}
			}
		case []any:
			for _, child := range t {
				if blocked(child) {
					return true
				}
			}
		}
		return false
	}
	return blocked(v)
}

func counterJSON(raw json.RawMessage) bool {
	var n int64
	return json.Unmarshal(raw, &n) == nil && n >= 0
}

// ChatTurnReport reads, leniently, what a Chat reply or stream chunk reports
// about its turn: whether a choice finished, and the usage when it is
// complete and consistent. It lets a reply the bridge refused still be
// metered for the turn the upstream billed; it never admits content.
func ChatTurnReport(body []byte) (finished bool, usage *ir.Usage) {
	var w struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *ChatUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return false, nil
	}
	for _, choice := range w.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			finished = true
		}
	}
	if w.Usage == nil || validateChatUsagePresence(body, false) != nil ||
		w.Usage.CompletionDetail != nil && w.Usage.CompletionDetail.ReasoningTokens < 0 ||
		w.Usage.PromptDetail != nil && w.Usage.PromptDetail.CachedTokens < 0 {
		return finished, nil
	}
	u := usageToIR(w.Usage)
	return finished, &u
}

// ResponsesReplyUsage reads, leniently, the usage a native reply reported
// when it is complete and consistent, or nil. A reply the codec refused is
// still metered for the turn the backend billed.
func ResponsesReplyUsage(body []byte) *ir.Usage {
	var w struct {
		Usage *responsesWireUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return nil
	}
	u, err := usageFromResponses(w.Usage)
	if err != nil {
		return nil
	}
	return u
}

// ResponsesTerminalUsage reads, leniently, the usage a native terminal event
// (completed, incomplete or failed) reported for responseID, or nil. A
// terminal for any other response is not this turn's report.
func ResponsesTerminalUsage(body []byte, responseID string) *ir.Usage {
	var w struct {
		Type     string `json:"type"`
		Response *struct {
			ID    string              `json:"id"`
			Usage *responsesWireUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &w); err != nil || w.Response == nil || responseID == "" || w.Response.ID != responseID {
		return nil
	}
	switch w.Type {
	case "response.completed", "response.incomplete", "response.failed":
	default:
		return nil
	}
	u, err := usageFromResponses(w.Response.Usage)
	if err != nil {
		return nil
	}
	return u
}

// Presence checks are local to the Responses bridge. The public legacy Chat
// types and decoding behavior retain their existing contract.
func validateChatUsagePresence(body []byte, stream bool) error {
	var wire struct {
		Usage *struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			TotalTokens      *int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return ErrResponsesUnsupported
	}
	if wire.Usage == nil {
		if stream {
			return nil
		}
		return ErrResponsesUnsupported
	}
	u := wire.Usage
	if u.PromptTokens == nil || u.CompletionTokens == nil || u.TotalTokens == nil || *u.PromptTokens < 0 || *u.CompletionTokens < 0 || *u.TotalTokens < 0 || *u.TotalTokens != *u.PromptTokens+*u.CompletionTokens {
		return ErrResponsesUnsupported
	}
	return nil
}

type responseWireAnnotation struct {
	Type       string `json:"type"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	StartIndex *int   `json:"start_index"`
	EndIndex   *int   `json:"end_index"`
}

type responseWireWebSearchAction struct {
	Type    string   `json:"type"`
	Query   *string  `json:"query,omitempty"`
	Queries []string `json:"queries,omitempty"`
	URL     *string  `json:"url,omitempty"`
	Pattern *string  `json:"pattern,omitempty"`
}

type responseWireWebSearchFilters struct {
	AllowedDomains []string `json:"allowed_domains,omitempty"`
}

type responseWireWebSearchUserLocation struct {
	Type     string  `json:"type"`
	Country  *string `json:"country,omitempty"`
	Region   *string `json:"region,omitempty"`
	City     *string `json:"city,omitempty"`
	Timezone *string `json:"timezone,omitempty"`
}

func decodeResponseAnnotations(w []responseWireAnnotation) ([]ir.URLCitation, error) {
	var annotations []ir.URLCitation
	for _, a := range w {
		if a.Type != "url_citation" || a.URL == "" || a.StartIndex == nil || a.EndIndex == nil || *a.StartIndex < 0 || *a.EndIndex < *a.StartIndex {
			return nil, ErrResponsesUnsupported
		}
		annotations = append(annotations, ir.URLCitation{URL: a.URL, Title: a.Title, StartIndex: *a.StartIndex, EndIndex: *a.EndIndex})
	}
	return annotations, nil
}

func encodeResponseAnnotations(annotations []ir.URLCitation) ([]responseWireAnnotation, error) {
	var wire []responseWireAnnotation
	for _, a := range annotations {
		if a.URL == "" || a.StartIndex < 0 || a.EndIndex < a.StartIndex {
			return nil, ErrResponsesUnsupported
		}
		start, end := a.StartIndex, a.EndIndex
		wire = append(wire, responseWireAnnotation{Type: "url_citation", URL: a.URL, Title: a.Title, StartIndex: &start, EndIndex: &end})
	}
	return wire, nil
}

func decodeWebSearchAction(w responseWireWebSearchAction) (ir.WebSearchAction, error) {
	action := ir.WebSearchAction{Kind: w.Type, Query: w.Query, Queries: w.Queries, URL: w.URL, Pattern: w.Pattern}
	switch w.Type {
	case "search":
		if w.URL != nil || w.Pattern != nil {
			return action, ErrResponsesUnsupported
		}
	case "open_page":
		if w.Query != nil || w.Queries != nil || w.Pattern != nil {
			return action, ErrResponsesUnsupported
		}
	case "find_in_page":
		if w.Query != nil || w.Queries != nil {
			return action, ErrResponsesUnsupported
		}
	default:
		return action, ErrResponsesUnsupported
	}
	return action, nil
}

func encodeWebSearchAction(a ir.WebSearchAction) (responseWireWebSearchAction, error) {
	w := responseWireWebSearchAction{Type: a.Kind, Query: a.Query, Queries: a.Queries, URL: a.URL, Pattern: a.Pattern}
	_, err := decodeWebSearchAction(w)
	return w, err
}

func decodeResponseTool(body json.RawMessage, nested bool) (ir.ToolDef, error) {
	var w responsesWireTool
	if err := strictJSON(body, &w); err != nil {
		return ir.ToolDef{}, ErrResponsesUnsupported
	}
	allowed := map[string]bool{"type": true}
	switch w.Type {
	case "function":
		for _, key := range []string{"name", "description", "parameters", "strict", "defer_loading"} {
			allowed[key] = true
		}
	case "custom":
		for _, key := range []string{"name", "description", "format", "defer_loading"} {
			allowed[key] = true
		}
	case "namespace":
		if nested {
			return ir.ToolDef{}, ErrResponsesUnsupported
		}
		for _, key := range []string{"name", "description", "tools"} {
			allowed[key] = true
		}
	case "web_search":
		if nested {
			return ir.ToolDef{}, ErrResponsesUnsupported
		}
		for _, key := range []string{"external_web_access", "indexed_web_access", "filters", "user_location", "search_context_size", "search_content_types"} {
			allowed[key] = true
		}
	default:
		return ir.ToolDef{}, ErrResponsesUnsupported
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return ir.ToolDef{}, ErrResponsesUnsupported
	}
	for key := range fields {
		if !allowed[key] {
			return ir.ToolDef{}, ErrResponsesUnsupported
		}
	}
	tool := ir.ToolDef{Kind: w.Type, Name: w.Name, Description: w.Description, Parameters: w.Parameters, Format: w.Format, Strict: w.Strict, DeferLoading: w.DeferLoading}
	switch w.Type {
	case "function":
		if w.Name == "" {
			return ir.ToolDef{}, ErrResponsesUnsupported
		}
	case "custom":
		if w.Name == "" || len(w.Format) == 0 {
			return ir.ToolDef{}, ErrResponsesUnsupported
		}
	case "namespace":
		if w.Name == "" || w.Tools == nil {
			return ir.ToolDef{}, ErrResponsesUnsupported
		}
		for _, raw := range w.Tools {
			child, err := decodeResponseTool(raw, true)
			if err != nil {
				return ir.ToolDef{}, err
			}
			tool.Tools = append(tool.Tools, child)
		}
	case "web_search":
		config := &ir.WebSearchConfig{ExternalWebAccess: w.ExternalWebAccess, IndexedWebAccess: w.IndexedWebAccess, SearchContextSize: w.SearchContextSize, SearchContentTypes: w.SearchContentTypes}
		if w.SearchContextSize != nil && *w.SearchContextSize != "low" && *w.SearchContextSize != "medium" && *w.SearchContextSize != "high" {
			return ir.ToolDef{}, ErrResponsesUnsupported
		}
		for _, kind := range w.SearchContentTypes {
			if kind != "text" && kind != "image" {
				return ir.ToolDef{}, ErrResponsesUnsupported
			}
		}
		if w.Filters != nil {
			config.Filters = &ir.WebSearchFilters{AllowedDomains: w.Filters.AllowedDomains}
		}
		if w.UserLocation != nil {
			if w.UserLocation.Type != "approximate" {
				return ir.ToolDef{}, ErrResponsesUnsupported
			}
			config.UserLocation = &ir.WebSearchUserLocation{Type: w.UserLocation.Type, Country: w.UserLocation.Country, Region: w.UserLocation.Region, City: w.UserLocation.City, Timezone: w.UserLocation.Timezone}
		}
		tool.WebSearch = config
	}
	return tool, nil
}

func encodeResponseTool(tool ir.ToolDef, nested bool) (json.RawMessage, error) {
	kind := tool.Kind
	if kind == "" {
		kind = "function"
	}
	w := responsesWireTool{Type: kind, Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters, Format: tool.Format, Strict: tool.Strict, DeferLoading: tool.DeferLoading}
	if kind != "namespace" && len(tool.Tools) > 0 || kind != "web_search" && tool.WebSearch != nil {
		return nil, ErrResponsesUnsupported
	}
	if kind == "namespace" {
		w.Tools = []json.RawMessage{}
		for _, child := range tool.Tools {
			raw, err := encodeResponseTool(child, true)
			if err != nil {
				return nil, err
			}
			w.Tools = append(w.Tools, raw)
		}
	}
	if tool.WebSearch != nil {
		c := tool.WebSearch
		w.ExternalWebAccess = c.ExternalWebAccess
		w.IndexedWebAccess = c.IndexedWebAccess
		w.SearchContextSize = c.SearchContextSize
		w.SearchContentTypes = c.SearchContentTypes
		if c.Filters != nil {
			w.Filters = &responseWireWebSearchFilters{AllowedDomains: c.Filters.AllowedDomains}
		}
		if c.UserLocation != nil {
			w.UserLocation = &responseWireWebSearchUserLocation{Type: c.UserLocation.Type, Country: c.UserLocation.Country, Region: c.UserLocation.Region, City: c.UserLocation.City, Timezone: c.UserLocation.Timezone}
		}
	}
	body, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	if kind == "namespace" {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(w.Tools)
		if err != nil {
			return nil, err
		}
		fields["tools"] = raw
		body, err = json.Marshal(fields)
		if err != nil {
			return nil, err
		}
	}
	if _, err := decodeResponseTool(body, nested); err != nil {
		return nil, err
	}
	return body, nil
}
