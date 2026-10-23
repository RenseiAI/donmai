// Package translate holds the surface-protocol ⇄ IR codecs. Each protocol has
// ONE codec used on both sides of the hub: the inbound surface uses it to
// decode a client request and encode the client response; an outbound upstream
// speaking the same protocol uses it to encode the request and decode the
// response. N-to-N translation is therefore 2N codecs, never N².
//
// This file is the OpenAI Responses codec (POST /v1/responses, with SSE). It
// maps the Responses wire shape onto the same canonical IR the Chat
// Completions codec uses, so responses-in → openai-compat-out is a faithful
// cross-protocol trip through the hub: input items become IR messages (with
// function_call_output items becoming tool-role results), and the IR reply is
// encoded back as a Responses object with message / function_call output
// items. A real Chat upstream serves Responses inbound through this IR; no
// native Responses upstream codec is needed for that backend.
//
// Fail-closed rules (never silently discard or fabricate):
//   - previous_response_id / item_reference: refused — the gateway keeps no
//     response-state store and invents no history authority.
//   - hosted tools (web_search, tool_search, file_search, …) and hosted
//     items (web_search_call, …): refused with ErrResponsesHostedTool — the
//     hub has no server-side tool to run and never drops them silently.
//     Client-executed kinds (function, custom, namespace) are served.
//   - unknown input item / content part types: refused — not silently skipped.
//   - parallel_tool_calls:false: refused — the gateway cannot enforce serial
//     tool use on a third-party upstream.
//   - background:true: refused — the gateway answers synchronously.
//   - logprobs / top_logprobs: refused — token probabilities do not survive
//     the hub.
//   - non-empty include (e.g. reasoning.encrypted_content): refused —
//     encrypted reasoning blobs are opaque to the hub.
//   - audio modalities: refused — the hub carries text, image references,
//     tool calls/results, and reasoning summaries.
//
// Ignored-but-documented (no observable semantic effect on one exchange):
// store (nothing is ever stored), metadata echo, max_tool_calls (a client loop
// bound over stateless requests), service_tier / safety_identifier /
// prompt_cache_key / user (routing/identity hints the loopback hub does not
// act on).
package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/gateway/ir"
)

// ErrResponsesHostedTool refuses a hosted (server-executed) tool kind that
// the gateway cannot run. The gateway only serves client-executed tools
// (function, custom, namespace); hosted kinds (web_search, tool_search and
// any other server-side tool) are never dropped silently.
var ErrResponsesHostedTool = errors.New("gateway/translate: responses hosted tool is not supported")

// ─── OpenAI Responses wire types (the subset the gateway maps) ───────────────

// ResponsesRequest is the POST /v1/responses request body.
type ResponsesRequest struct {
	Model              string              `json:"model"`
	Input              any                 `json:"input,omitempty"`
	Instructions       string              `json:"instructions,omitempty"`
	Tools              []ResponsesTool     `json:"tools,omitempty"`
	ToolChoice         any                 `json:"tool_choice,omitempty"`
	Reasoning          *ResponsesReasoning `json:"reasoning,omitempty"`
	Temperature        *float64            `json:"temperature,omitempty"`
	TopP               *float64            `json:"top_p,omitempty"`
	MaxOutputTokens    *int                `json:"max_output_tokens,omitempty"`
	Stream             bool                `json:"stream,omitempty"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls,omitempty"`
	Background         bool                `json:"background,omitempty"`
	PreviousResponseID string              `json:"previous_response_id,omitempty"`
	Include            []string            `json:"include,omitempty"`
	Logprobs           bool                `json:"logprobs,omitempty"`
	TopLogprobs        *int                `json:"top_logprobs,omitempty"`
	Modalities         []string            `json:"modalities,omitempty"`
}

// ResponsesTool is one tool declaration. The gateway serves the
// client-executed kinds: function, custom (freeform), and namespace (a named
// group of function/custom tools). Hosted (server-executed) kinds such as
// web_search and tool_search are refused fail-closed (see
// ErrResponsesHostedTool) — they are never dropped silently.
type ResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  map[string]any  `json:"parameters,omitempty"`
	Format      map[string]any  `json:"format,omitempty"`
	Tools       []ResponsesTool `json:"tools,omitempty"`
}

// ResponsesReasoning is the reasoning effort request.
type ResponsesReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ResponsesInputItem is one element of the input array. Type discriminates:
// "message" (user/system/developer/assistant text or images),
// "function_call_output" / "custom_tool_call_output" (a tool result),
// "function_call" / "custom_tool_call" (a replayed prior assistant tool call
// — mapped back to an assistant tool_call part), or "reasoning" (a summary
// the hub carries as thinking content).
type ResponsesInputItem struct {
	Type      string         `json:"type"`
	Role      string         `json:"role,omitempty"`
	Content   any            `json:"content,omitempty"`
	CallID    string         `json:"call_id,omitempty"`
	Output    any            `json:"output,omitempty"`
	Name      string         `json:"name,omitempty"`
	ID        string         `json:"id,omitempty"`
	CallID2   string         `json:"-"`
	Summary   []ResponseText `json:"summary,omitempty"`
	Namespace string         `json:"namespace,omitempty"`
	// Arguments carries a replayed function_call's argument string.
	Arguments string `json:"arguments,omitempty"`
	// Input carries a replayed custom_tool_call's freeform input string.
	Input string `json:"input,omitempty"`
}

// ResponseText is a {type:"summary_text", text} entry in a reasoning summary.
type ResponseText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ─── request codec ───────────────────────────────────────────────────────────

// DecodeResponsesRequest maps a POST /v1/responses request body onto the
// canonical IR. Fields the hub cannot serve truthfully are refused (see the
// package doc); fields with no per-exchange semantic effect are ignored.
func DecodeResponsesRequest(body []byte) (ir.Request, error) {
	var rr ResponsesRequest
	if err := json.Unmarshal(body, &rr); err != nil {
		return ir.Request{}, fmt.Errorf("gateway/translate: decode responses request: %w", err)
	}
	return responsesRequestToIR(rr)
}

func responsesRequestToIR(rr ResponsesRequest) (ir.Request, error) {
	if strings.TrimSpace(rr.PreviousResponseID) != "" {
		return ir.Request{}, fmt.Errorf("gateway/translate: responses previous_response_id is not supported (the gateway keeps no response history)")
	}
	if rr.Background {
		return ir.Request{}, fmt.Errorf("gateway/translate: responses background mode is not supported (the gateway answers synchronously)")
	}
	if rr.Logprobs || (rr.TopLogprobs != nil && *rr.TopLogprobs > 0) {
		return ir.Request{}, fmt.Errorf("gateway/translate: responses logprobs are not supported (token probabilities do not survive translation)")
	}
	if len(rr.Include) > 0 {
		return ir.Request{}, fmt.Errorf("gateway/translate: responses include %q is not supported", rr.Include)
	}
	if rr.ParallelToolCalls != nil && !*rr.ParallelToolCalls {
		return ir.Request{}, fmt.Errorf("gateway/translate: responses parallel_tool_calls=false is not supported (serial tool use cannot be enforced on the upstream)")
	}
	for _, m := range rr.Modalities {
		if strings.EqualFold(m, "audio") {
			return ir.Request{}, fmt.Errorf("gateway/translate: responses audio modality is not supported")
		}
	}
	for _, t := range rr.Tools {
		if err := checkResponsesToolKind(t.Type); err != nil {
			return ir.Request{}, err
		}
		if t.Type == "namespace" {
			if err := checkResponsesNamespaceTools(t); err != nil {
				return ir.Request{}, err
			}
		}
	}

	req := ir.Request{
		Model:  rr.Model,
		Stream: rr.Stream,
		Sampling: ir.Sampling{
			Temperature: rr.Temperature,
			TopP:        rr.TopP,
			MaxTokens:   rr.MaxOutputTokens,
		},
	}
	if rr.Reasoning != nil && rr.Reasoning.Effort != "" {
		level, err := ir.LevelFromEffort(rr.Reasoning.Effort)
		if err != nil {
			return ir.Request{}, fmt.Errorf("gateway/translate: decode responses request: %w", err)
		}
		req.Thinking = ir.ThinkingSpec{Level: level, Emit: ir.EmitFull}
	}
	for _, t := range rr.Tools {
		def, err := responsesToolToIR(t)
		if err != nil {
			return ir.Request{}, err
		}
		req.Tools = append(req.Tools, def)
	}
	req.ToolChoice = responsesToolChoiceToIR(rr.ToolChoice)

	if s := strings.TrimSpace(rr.Instructions); s != "" {
		req.System = append(req.System, ir.Part{Kind: ir.PartText, Text: s})
	}
	if err := appendResponsesInput(&req, rr.Input); err != nil {
		return ir.Request{}, err
	}
	return req, nil
}

// appendResponsesInput maps the input (a plain string or an array of typed
// items) onto IR messages.
func appendResponsesInput(req *ir.Request, input any) error {
	if input == nil {
		return nil
	}
	if s, ok := input.(string); ok {
		if strings.TrimSpace(s) != "" {
			req.Messages = append(req.Messages, ir.Message{
				Role:  ir.RoleUser,
				Parts: []ir.Part{{Kind: ir.PartText, Text: s}},
			})
		}
		return nil
	}
	items, ok := input.([]any)
	if !ok {
		return fmt.Errorf("gateway/translate: responses input must be a string or an array of items")
	}
	for i, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("gateway/translate: responses input item %d must be an object", i)
		}
		item := ResponsesInputItem{
			Role:      stringVal(m, "role"),
			CallID:    stringVal(m, "call_id"),
			Name:      stringVal(m, "name"),
			ID:        stringVal(m, "id"),
			Arguments: stringVal(m, "arguments"),
			Namespace: stringVal(m, "namespace"),
			Input:     stringVal(m, "input"),
		}
		item.Type, _ = m["type"].(string)
		item.Content = m["content"]
		item.Output = m["output"]
		if sum, ok := m["summary"].([]any); ok {
			for _, sraw := range sum {
				if sm, ok := sraw.(map[string]any); ok {
					item.Summary = append(item.Summary, ResponseText{
						Type: stringVal(sm, "type"),
						Text: stringVal(sm, "text"),
					})
				}
			}
		}
		if err := appendResponsesItem(req, i, item); err != nil {
			return err
		}
	}
	return nil
}

// checkResponsesToolKind refuses hosted (server-executed) tool kinds
// fail-closed with a typed error. Client-executed kinds (function, custom,
// namespace) pass; hosted kinds (web_search, tool_search) and any other
// unknown kind are never dropped silently.
func checkResponsesToolKind(kind string) error {
	switch kind {
	case "function", "custom", "namespace":
		return nil
	case "web_search", "tool_search":
		return fmt.Errorf("%w: tool type %q requires server-side execution the gateway does not provide", ErrResponsesHostedTool, kind)
	default:
		if kind == "" {
			return fmt.Errorf("gateway/translate: responses tool is missing its type")
		}
		return fmt.Errorf("%w: tool type %q is not supported", ErrResponsesHostedTool, kind)
	}
}

// checkResponsesNamespaceTools validates a namespace tool's nested tools:
// only function and custom children are served, never a nested namespace and
// never a hosted kind.
func checkResponsesNamespaceTools(t ResponsesTool) error {
	if t.Name == "" {
		return fmt.Errorf("gateway/translate: responses namespace tool is missing its name")
	}
	if len(t.Tools) == 0 {
		return fmt.Errorf("gateway/translate: responses namespace tool %q carries no tools", t.Name)
	}
	for _, child := range t.Tools {
		switch child.Type {
		case "function", "custom":
		default:
			return fmt.Errorf("%w: namespace tool %q nests unsupported tool type %q", ErrResponsesHostedTool, t.Name, child.Type)
		}
		if child.Type == "namespace" || len(child.Tools) > 0 {
			return fmt.Errorf("gateway/translate: responses namespace tool %q must not nest namespaces", t.Name)
		}
	}
	return nil
}

// responsesToolToIR maps one client-executed tool declaration into the IR.
// Function tools behave as before; custom carries its grammar format and
// namespace carries its nested function/custom tools.
func responsesToolToIR(t ResponsesTool) (ir.ToolDef, error) {
	switch t.Type {
	case "function":
		if t.Name == "" {
			return ir.ToolDef{}, fmt.Errorf("gateway/translate: responses function tool is missing its name")
		}
		return ir.ToolDef{
			Kind:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		}, nil
	case "custom":
		if t.Name == "" {
			return ir.ToolDef{}, fmt.Errorf("gateway/translate: responses custom tool is missing its name")
		}
		if len(t.Format) == 0 {
			return ir.ToolDef{}, fmt.Errorf("gateway/translate: responses custom tool %q is missing its format", t.Name)
		}
		return ir.ToolDef{
			Kind:        "custom",
			Name:        t.Name,
			Description: t.Description,
			Format:      t.Format,
		}, nil
	case "namespace":
		if err := checkResponsesNamespaceTools(t); err != nil {
			return ir.ToolDef{}, err
		}
		def := ir.ToolDef{
			Kind:        "namespace",
			Name:        t.Name,
			Description: t.Description,
		}
		for _, child := range t.Tools {
			cdef, err := responsesToolToIR(child)
			if err != nil {
				return ir.ToolDef{}, err
			}
			def.Tools = append(def.Tools, cdef)
		}
		return def, nil
	default:
		return ir.ToolDef{}, checkResponsesToolKind(t.Type)
	}
}

// responsesToolFromIR maps one IR tool back onto its Responses wire shape.
// It is the inverse of responsesToolToIR: function, custom, and namespace
// round-trip; hosted kinds never appear in the IR so they can never be
// emitted here.
func responsesToolFromIR(def ir.ToolDef) ResponsesTool {
	kind := def.Kind
	if kind == "" {
		kind = "function"
	}
	out := ResponsesTool{
		Type:        kind,
		Name:        def.Name,
		Description: def.Description,
		Parameters:  def.Parameters,
		Format:      def.Format,
	}
	for _, child := range def.Tools {
		out.Tools = append(out.Tools, responsesToolFromIR(child))
	}
	return out
}

func stringVal(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func appendResponsesItem(req *ir.Request, i int, item ResponsesInputItem) error {
	switch item.Type {
	case "message":
		role := responsesRoleToIR(item.Role)
		parts, err := responsesContentParts(item.Content)
		if err != nil {
			return fmt.Errorf("gateway/translate: responses input item %d: %w", i, err)
		}
		if role == ir.RoleSystem {
			req.System = append(req.System, parts...)
			return nil
		}
		req.Messages = append(req.Messages, ir.Message{Role: role, Parts: parts})
		return nil
	case "function_call_output", "custom_tool_call_output":
		text, err := responsesOutputText(item.Output)
		if err != nil {
			return fmt.Errorf("gateway/translate: responses input item %d: %w", i, err)
		}
		req.Messages = append(req.Messages, ir.Message{
			Role:  ir.RoleTool,
			Parts: []ir.Part{{Kind: ir.PartToolResult, Text: text, ToolCallID: item.CallID}},
		})
		return nil
	case "function_call":
		// A replayed prior assistant tool call: back onto an assistant
		// tool_call part so the upstream sees the same call it issued.
		// The namespace rides the IR ToolCall so a namespaced follow-up
		// returns to the same namespace on encode.
		if item.CallID == "" || item.Name == "" {
			return fmt.Errorf("gateway/translate: responses input item %d is a function_call missing call_id or name", i)
		}
		req.Messages = append(req.Messages, ir.Message{
			Role: ir.RoleAssistant,
			Parts: []ir.Part{{
				Kind:     ir.PartToolCall,
				ToolCall: &ir.ToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Arguments, Namespace: item.Namespace, Kind: "function"},
			}},
		})
		return nil
	case "custom_tool_call":
		// A replayed prior freeform call: the freeform input rides
		// Arguments so the chat upstream sees the same bytes; the kind
		// marks it for custom_tool_call re-encode on the way back.
		if item.CallID == "" || item.Name == "" {
			return fmt.Errorf("gateway/translate: responses input item %d is a custom_tool_call missing call_id or name", i)
		}
		req.Messages = append(req.Messages, ir.Message{
			Role: ir.RoleAssistant,
			Parts: []ir.Part{{
				Kind:     ir.PartToolCall,
				ToolCall: &ir.ToolCall{ID: item.CallID, Name: item.Name, Arguments: item.Input, Namespace: item.Namespace, Kind: "custom"},
			}},
		})
		return nil
	case "reasoning":
		var b strings.Builder
		for _, s := range item.Summary {
			if s.Type != "" && s.Type != "summary_text" {
				return fmt.Errorf("gateway/translate: responses input item %d has unsupported reasoning summary type %q", i, s.Type)
			}
			b.WriteString(s.Text)
		}
		if b.Len() > 0 {
			req.Messages = append(req.Messages, ir.Message{
				Role:  ir.RoleAssistant,
				Parts: []ir.Part{{Kind: ir.PartThinking, Text: b.String()}},
			})
		}
		return nil
	case "item_reference":
		return fmt.Errorf("gateway/translate: responses input item %d is an item_reference, which requires response history the gateway does not keep", i)
	case "web_search_call", "tool_search_call", "file_search_call", "computer_call", "computer_use_call", "image_generation_call":
		return fmt.Errorf("%w: responses input item %d is a hosted %q item requiring server-side execution", ErrResponsesHostedTool, i, item.Type)
	default:
		return fmt.Errorf("gateway/translate: responses input item %d has unsupported type %q", i, item.Type)
	}
}

func responsesRoleToIR(r string) ir.Role {
	switch r {
	case "system", "developer":
		return ir.RoleSystem
	case "assistant":
		return ir.RoleAssistant
	default:
		return ir.RoleUser
	}
}

// responsesContentParts maps message content (a string or an array of typed
// parts) onto IR parts. input_text → text; input_image → image reference
// (re-encoded per surface downstream, never fetched).
func responsesContentParts(content any) ([]ir.Part, error) {
	if content == nil {
		return nil, nil
	}
	if s, ok := content.(string); ok {
		if s == "" {
			return nil, nil
		}
		return []ir.Part{{Kind: ir.PartText, Text: s}}, nil
	}
	arr, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("message content must be a string or an array of parts")
	}
	var out []ir.Part
	for _, raw := range arr {
		pm, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("message content part must be an object")
		}
		typ, _ := pm["type"].(string)
		switch typ {
		case "input_text", "output_text", "text":
			if t, _ := pm["text"].(string); t != "" {
				out = append(out, ir.Part{Kind: ir.PartText, Text: t})
			}
		case "input_image":
			url := ""
			if iu, ok := pm["image_url"].(map[string]any); ok {
				url, _ = iu["url"].(string)
			}
			if url == "" {
				url, _ = pm["image_url"].(string)
			}
			out = append(out, ir.Part{Kind: ir.PartImage, ImageURL: url})
		case "refusal":
			if t, _ := pm["refusal"].(string); t != "" {
				out = append(out, ir.Part{Kind: ir.PartText, Text: t})
			}
		default:
			return nil, fmt.Errorf("unsupported message content part type %q", typ)
		}
	}
	return out, nil
}

// responsesOutputText flattens a function_call_output output (a string or an
// array of input_text parts) to plain text.
func responsesOutputText(output any) (string, error) {
	if output == nil {
		return "", nil
	}
	if s, ok := output.(string); ok {
		return s, nil
	}
	arr, ok := output.([]any)
	if !ok {
		return "", fmt.Errorf("function_call_output output must be a string or an array of parts")
	}
	var b strings.Builder
	for _, raw := range arr {
		pm, ok := raw.(map[string]any)
		if !ok {
			return "", fmt.Errorf("function_call_output output part must be an object")
		}
		if typ, _ := pm["type"].(string); typ != "" && typ != "input_text" && typ != "text" {
			return "", fmt.Errorf("unsupported function_call_output output part type %q", typ)
		}
		if t, _ := pm["text"].(string); t != "" {
			b.WriteString(t)
		}
	}
	return b.String(), nil
}

func responsesToolChoiceToIR(tc any) ir.ToolChoice {
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
		// {"type":"function",...} — a forced specific tool; canonicalize as
		// required, mirroring the chat codec.
		if typ, _ := v["type"].(string); typ == "function" {
			return ir.ToolChoiceRequired
		}
	}
	return ""
}

// ─── response codec ──────────────────────────────────────────────────────────

// ResponsesObject is the non-streaming POST /v1/responses response body (and
// the snapshot carried by response.completed / response.failed).
type ResponsesObject struct {
	ID                string                `json:"id"`
	Object            string                `json:"object"`
	CreatedAt         int64                 `json:"created_at"`
	Model             string                `json:"model"`
	Output            []ResponsesOutputItem `json:"output"`
	Status            string                `json:"status"`
	ParallelToolCalls bool                  `json:"parallel_tool_calls"`
	ToolChoice        string                `json:"tool_choice"`
	Usage             *ResponsesUsage       `json:"usage,omitempty"`
	IncompleteDetails *ResponsesIncomplete  `json:"incomplete_details,omitempty"`
}

// ResponsesOutputItem is one output entry: a message, a function_call, a
// custom_tool_call, or a reasoning summary.
type ResponsesOutputItem struct {
	Type      string                   `json:"type"`
	ID        string                   `json:"id,omitempty"`
	Status    string                   `json:"status,omitempty"`
	Role      string                   `json:"role,omitempty"`
	Content   []ResponsesOutputContent `json:"content,omitempty"`
	CallID    string                   `json:"call_id,omitempty"`
	Name      string                   `json:"name,omitempty"`
	Arguments string                   `json:"arguments,omitempty"`
	Input     string                   `json:"input,omitempty"`
	Namespace string                   `json:"namespace,omitempty"`
	Summary   []ResponsesSummaryPart   `json:"summary,omitempty"`
}

// ResponsesOutputContent is one content part of a message output item.
type ResponsesOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ResponsesSummaryPart is one summary_text entry of a reasoning output item.
type ResponsesSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ResponsesUsage is the token accounting.
type ResponsesUsage struct {
	InputTokens         int                     `json:"input_tokens"`
	OutputTokens        int                     `json:"output_tokens"`
	TotalTokens         int                     `json:"total_tokens"`
	InputTokensDetails  *ResponsesInputDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *ResponsesOutputDetails `json:"output_tokens_details,omitempty"`
}

// ResponsesInputDetails carries cache-read accounting.
type ResponsesInputDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// ResponsesOutputDetails carries reasoning-token accounting.
type ResponsesOutputDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// ResponsesIncomplete explains an incomplete status.
type ResponsesIncomplete struct {
	Reason string `json:"reason"`
}

// EncodeResponsesResponse maps the IR back onto a non-streaming Responses
// object. respID/item IDs are supplied by the caller (the surface mints them
// with its ID seam so fixtures stay deterministic).
func EncodeResponsesResponse(resp ir.Response, ids ResponsesIDs) ResponsesObject {
	out := ResponsesObject{
		ID:                ids.Response,
		Object:            "response",
		CreatedAt:         ids.CreatedAt,
		Model:             resp.Model,
		Status:            responsesStatus(resp.FinishReason),
		ParallelToolCalls: true,
		ToolChoice:        "auto",
		Usage:             responsesUsageFromIR(resp.Usage),
	}
	if reason := responsesIncompleteReason(resp.FinishReason); reason != "" {
		out.IncompleteDetails = &ResponsesIncomplete{Reason: reason}
	}
	out.Output = responsesOutputItems(resp, ids)
	return out
}

// ResponsesIDs carries the caller-minted identifiers for one encoded response.
type ResponsesIDs struct {
	Response  string
	CreatedAt int64
}

func responsesStatus(f ir.FinishReason) string {
	switch f {
	case ir.FinishLength, ir.FinishContentFilter:
		return "incomplete"
	case ir.FinishError:
		return "failed"
	default:
		return "completed"
	}
}

func responsesIncompleteReason(f ir.FinishReason) string {
	switch f {
	case ir.FinishLength:
		return "max_output_tokens"
	case ir.FinishContentFilter:
		return "content_filter"
	default:
		return ""
	}
}

func responsesUsageFromIR(u ir.Usage) *ResponsesUsage {
	if u.TokensIn == 0 && u.TokensOut == 0 {
		return nil
	}
	out := &ResponsesUsage{
		InputTokens:  u.TokensIn,
		OutputTokens: u.TokensOut,
		TotalTokens:  u.TokensIn + u.TokensOut,
	}
	if u.CacheReadTokens > 0 {
		out.InputTokensDetails = &ResponsesInputDetails{CachedTokens: u.CacheReadTokens}
	}
	if u.ReasoningTokens > 0 {
		out.OutputTokensDetails = &ResponsesOutputDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

// responsesOutputItems renders IR content as Responses output items: reasoning
// summaries first, then the assistant message, then one function_call or
// custom_tool_call item per tool call — the order native Responses servers
// use. Custom calls carry their freeform input in input (not arguments) and
// namespaced calls carry their namespace so follow-ups return to the same
// namespace.
func responsesOutputItems(resp ir.Response, ids ResponsesIDs) []ResponsesOutputItem {
	var thinking, text strings.Builder
	type call struct {
		id, name, args, namespace, kind string
	}
	var calls []call
	for _, p := range resp.Content {
		switch p.Kind {
		case ir.PartText:
			text.WriteString(p.Text)
		case ir.PartThinking:
			thinking.WriteString(p.Text)
		case ir.PartToolCall:
			if p.ToolCall != nil {
				calls = append(calls, call{id: p.ToolCall.ID, name: p.ToolCall.Name, args: p.ToolCall.Arguments, namespace: p.ToolCall.Namespace, kind: p.ToolCall.Kind})
			}
		}
	}
	var out []ResponsesOutputItem
	if thinking.Len() > 0 {
		out = append(out, ResponsesOutputItem{
			Type:    "reasoning",
			ID:      ids.Response + "-rs-0",
			Status:  "completed",
			Summary: []ResponsesSummaryPart{{Type: "summary_text", Text: thinking.String()}},
		})
	}
	if text.Len() > 0 {
		out = append(out, ResponsesOutputItem{
			Type:    "message",
			ID:      ids.Response + "-msg-0",
			Status:  "completed",
			Role:    "assistant",
			Content: []ResponsesOutputContent{{Type: "output_text", Text: text.String()}},
		})
	}
	for i, c := range calls {
		id := c.id
		if id == "" {
			id = fmt.Sprintf("%s-fc-%d", ids.Response, i)
		}
		if c.kind == "custom" {
			out = append(out, ResponsesOutputItem{
				Type:      "custom_tool_call",
				ID:        id,
				CallID:    id,
				Status:    "completed",
				Name:      c.name,
				Input:     c.args,
				Namespace: c.namespace,
			})
			continue
		}
		out = append(out, ResponsesOutputItem{
			Type:      "function_call",
			ID:        id,
			CallID:    id,
			Status:    "completed",
			Name:      c.name,
			Arguments: c.args,
			Namespace: c.namespace,
		})
	}
	if out == nil {
		out = []ResponsesOutputItem{}
	}
	return out
}

// ─── stream codec ───

// ResponsesStreamEvent is one SSE `data:` payload on the Responses stream.
// Type names the event; the remaining fields carry the event's payload — only
// the fields relevant to Type are populated. The surface streaming loop mints
// stable item ids (one per output item) and announces each item exactly once
// with output_item.added before its first delta, so fragments for one tool
// call share one item id.
type ResponsesStreamEvent struct {
	Type         string               `json:"-"`
	Response     *ResponsesObject     `json:"response,omitempty"`
	Item         *ResponsesOutputItem `json:"item,omitempty"`
	OutputIndex  int                  `json:"output_index,omitempty"`
	ContentIndex int                  `json:"content_index,omitempty"`
	ItemID       string               `json:"item_id,omitempty"`
	Delta        string               `json:"delta,omitempty"`
	Text         string               `json:"text,omitempty"`
	Arguments    string               `json:"arguments,omitempty"`
}

// MarshalJSON renders the event in its wire shape: {"type":…,…payload}.
// Only the payload keys the event type defines are emitted.
func (e ResponsesStreamEvent) MarshalJSON() ([]byte, error) {
	m := map[string]any{"type": e.Type}
	switch e.Type {
	case "response.created", "response.in_progress":
		m["response"] = e.Response
	case "response.output_item.added", "response.output_item.done":
		m["output_index"] = e.OutputIndex
		m["item"] = e.Item
	case "response.output_text.delta", "response.function_call_arguments.delta":
		m["item_id"] = e.ItemID
		m["output_index"] = e.OutputIndex
		m["content_index"] = e.ContentIndex
		m["delta"] = e.Delta
	case "response.output_text.done":
		m["item_id"] = e.ItemID
		m["output_index"] = e.OutputIndex
		m["content_index"] = e.ContentIndex
		m["text"] = e.Text
	case "response.function_call_arguments.done":
		m["item_id"] = e.ItemID
		m["output_index"] = e.OutputIndex
		m["content_index"] = e.ContentIndex
		m["arguments"] = e.Arguments
	case "response.completed", "response.incomplete", "response.failed":
		m["response"] = e.Response
	}
	return json.Marshal(m)
}

// ResponsesCreated renders the opening response.created event. The snapshot
// is a stub (id/object/model only): the full output snapshot rides the
// terminal response.completed event.
func ResponsesCreated(ids ResponsesIDs, model string) ResponsesStreamEvent {
	return ResponsesStreamEvent{
		Type: "response.created",
		Response: &ResponsesObject{
			ID:     ids.Response,
			Object: "response",
			Model:  model,
			Output: []ResponsesOutputItem{},
			Status: "in_progress",
		},
	}
}

// ResponsesMessageAdded announces the assistant message output item before
// its first text delta.
func ResponsesMessageAdded(outputIndex int, itemID string) ResponsesStreamEvent {
	return ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: outputIndex,
		Item: &ResponsesOutputItem{
			Type:   "message",
			ID:     itemID,
			Status: "in_progress",
			Role:   "assistant",
		},
	}
}

// ResponsesFunctionCallAdded announces one function_call output item before
// its first arguments delta.
func ResponsesFunctionCallAdded(outputIndex int, itemID, name string) ResponsesStreamEvent {
	return ResponsesStreamEvent{
		Type:        "response.output_item.added",
		OutputIndex: outputIndex,
		Item: &ResponsesOutputItem{
			Type:      "function_call",
			ID:        itemID,
			CallID:    itemID,
			Status:    "in_progress",
			Name:      name,
			Arguments: "",
		},
	}
}

// ResponsesTextDelta renders one assistant-text fragment.
func ResponsesTextDelta(outputIndex int, itemID, delta string) ResponsesStreamEvent {
	return ResponsesStreamEvent{
		Type:        "response.output_text.delta",
		ItemID:      itemID,
		OutputIndex: outputIndex,
		Delta:       delta,
	}
}

// ResponsesArgsDelta renders one function-call arguments fragment.
func ResponsesArgsDelta(outputIndex int, itemID, delta string) ResponsesStreamEvent {
	return ResponsesStreamEvent{
		Type:        "response.function_call_arguments.delta",
		ItemID:      itemID,
		OutputIndex: outputIndex,
		Delta:       delta,
	}
}

// ResponsesTextDone renders the terminal done event for the message item
// with the assembled text.
func ResponsesTextDone(outputIndex int, itemID, text string) ResponsesStreamEvent {
	return ResponsesStreamEvent{
		Type:        "response.output_text.done",
		ItemID:      itemID,
		OutputIndex: outputIndex,
		Text:        text,
	}
}

// ResponsesFunctionCallDone renders the terminal done event for one
// function_call item with the assembled arguments.
func ResponsesFunctionCallDone(outputIndex int, itemID, args string) ResponsesStreamEvent {
	return ResponsesStreamEvent{
		Type:        "response.function_call_arguments.done",
		ItemID:      itemID,
		OutputIndex: outputIndex,
		Arguments:   args,
	}
}

// ResponsesTerminal renders the final response.completed / response.incomplete
// / response.failed snapshot event for resp.
func ResponsesTerminal(resp ResponsesObject) ResponsesStreamEvent {
	terminal := "response.completed"
	switch resp.Status {
	case "incomplete":
		terminal = "response.incomplete"
	case "failed":
		terminal = "response.failed"
	}
	return ResponsesStreamEvent{Type: terminal, Response: &resp}
}

// EncodeResponsesOrderedResponse builds a Responses snapshot from output
// items in a caller-determined order (the streaming path's announcement
// order), with the caller's usage and finish. Status, incomplete details, and
// the parallel_tool_calls / tool_choice echoes match EncodeResponsesResponse.
func EncodeResponsesOrderedResponse(model string, usage ir.Usage, finish ir.FinishReason, ids ResponsesIDs, output []ResponsesOutputItem) ResponsesObject {
	out := ResponsesObject{
		ID:                ids.Response,
		Object:            "response",
		CreatedAt:         ids.CreatedAt,
		Model:             model,
		Output:            output,
		Status:            responsesStatus(finish),
		ParallelToolCalls: true,
		ToolChoice:        "auto",
		Usage:             responsesUsageFromIR(usage),
	}
	if reason := responsesIncompleteReason(finish); reason != "" {
		out.IncompleteDetails = &ResponsesIncomplete{Reason: reason}
	}
	if out.Output == nil {
		out.Output = []ResponsesOutputItem{}
	}
	return out
}
