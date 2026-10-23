package translate

import (
	"errors"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/gateway/ir"
)

// Fixtures below are recorded from the pinned Codex CLI request shapes
// (rust-v0.157.1 tools/src/tool_spec.rs): function with strict:false,
// custom (freeform) with grammar format, namespace grouping both, web_search
// with live-access knobs, and tool_search with execution/parameters. The
// response-item shapes (function_call with namespace, custom_tool_call with
// input, and their outputs) come from codex-rs/protocol/src/models.rs at the
// same pin.

// TestDecodeResponsesRequest_CodexNamespaceCustomTools pins that the
// client-executed kinds map into the canonical IR: function as before,
// custom with its grammar format, and namespace with its nested tools.
func TestDecodeResponsesRequest_CodexNamespaceCustomTools(t *testing.T) {
	body := `{
		"model": "m",
		"input": "hi",
		"tools": [
			{"type":"function","name":"lookup_order","description":"Look up an order","strict":false,"parameters":{"type":"object","properties":{"order_id":{"type":"string"}},"required":["order_id"],"additionalProperties":false}},
			{"type":"custom","name":"apply_patch","description":"Apply a patch","format":{"type":"grammar","syntax":"lark","definition":"start: \"patch\""}},
			{"type":"namespace","name":"mcp__demo__","description":"Demo tools","tools":[
				{"type":"function","name":"lookup_order","description":"Look up an order","strict":false,"parameters":{"type":"object","properties":{"order_id":{"type":"string"}}}},
				{"type":"custom","name":"apply_patch","description":"Apply a patch","format":{"type":"grammar","syntax":"lark","definition":"start: \"patch\""}}
			]}
		]
	}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode codex tools: %v", err)
	}
	if len(req.Tools) != 3 {
		t.Fatalf("tools = %+v, want 3 (function, custom, namespace)", req.Tools)
	}
	fn := req.Tools[0]
	if fn.Kind != "function" || fn.Name != "lookup_order" {
		t.Errorf("function tool = %+v, want kind=function name=lookup_order", fn)
	}
	custom := req.Tools[1]
	if custom.Kind != "custom" || custom.Name != "apply_patch" {
		t.Errorf("custom tool = %+v, want kind=custom name=apply_patch", custom)
	}
	if custom.Format["syntax"] != "lark" {
		t.Errorf("custom format = %v, want grammar/lark", custom.Format)
	}
	ns := req.Tools[2]
	if ns.Kind != "namespace" || ns.Name != "mcp__demo__" {
		t.Fatalf("namespace tool = %+v, want kind=namespace", ns)
	}
	if len(ns.Tools) != 2 || ns.Tools[0].Kind != "function" || ns.Tools[1].Kind != "custom" {
		t.Fatalf("namespace children = %+v, want function+custom", ns.Tools)
	}

	// And back: the IR tools re-encode to the same wire kinds.
	for i, def := range req.Tools {
		wire := responsesToolFromIR(def)
		if wire.Type != def.Kind {
			t.Errorf("tool %d round-trip type = %q, want %q", i, wire.Type, def.Kind)
		}
		if wire.Name != def.Name {
			t.Errorf("tool %d round-trip name = %q, want %q", i, wire.Name, def.Name)
		}
	}
	nsWire := responsesToolFromIR(ns)
	if len(nsWire.Tools) != 2 || nsWire.Tools[0].Type != "function" || nsWire.Tools[1].Type != "custom" {
		t.Errorf("namespace round-trip children = %+v", nsWire.Tools)
	}
}

// TestDecodeResponsesRequest_CodexFollowUps pins that namespaced and custom
// calls (including their outputs) map into the IR and back, so a multi-turn
// Codex session returns to the same namespace/kind.
func TestDecodeResponsesRequest_CodexFollowUps(t *testing.T) {
	body := `{
		"model": "m",
		"input": [
			{"type":"function_call","call_id":"call-1","name":"lookup_order","namespace":"mcp__demo__","arguments":"{\"order_id\":\"1\"}"},
			{"type":"function_call_output","call_id":"call-1","output":"ok"},
			{"type":"custom_tool_call","call_id":"call-2","name":"apply_patch","namespace":"mcp__demo__","input":"patch text"},
			{"type":"custom_tool_call_output","call_id":"call-2","output":"applied"}
		],
		"tools": [
			{"type":"function","name":"lookup_order","parameters":{"type":"object"}},
			{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: \"patch\""}}
		]
	}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode follow-ups: %v", err)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %+v, want 4 (call, output, custom call, custom output)", req.Messages)
	}
	fnCall := req.Messages[0].Parts[0]
	if fnCall.Kind != ir.PartToolCall || fnCall.ToolCall.Name != "lookup_order" || fnCall.ToolCall.Namespace != "mcp__demo__" || fnCall.ToolCall.Kind != "function" {
		t.Errorf("function call = %+v, want namespaced function", fnCall.ToolCall)
	}
	customCall := req.Messages[2].Parts[0]
	if customCall.Kind != ir.PartToolCall || customCall.ToolCall.Name != "apply_patch" || customCall.ToolCall.Namespace != "mcp__demo__" || customCall.ToolCall.Kind != "custom" {
		t.Errorf("custom call = %+v, want namespaced custom", customCall.ToolCall)
	}
	if customCall.ToolCall.Arguments != "patch text" {
		t.Errorf("custom input = %q, want the freeform text", customCall.ToolCall.Arguments)
	}
	if req.Messages[1].Parts[0].Kind != ir.PartToolResult || req.Messages[3].Parts[0].Kind != ir.PartToolResult {
		t.Errorf("outputs = %+v, want tool results", req.Messages)
	}

	// And back: the IR calls re-encode to their own wire kinds with namespace.
	resp := ir.Response{
		Model:        "m",
		FinishReason: ir.FinishToolCalls,
		Content: []ir.Part{
			{Kind: ir.PartToolCall, ToolCall: &ir.ToolCall{ID: "call-1", Name: "lookup_order", Arguments: "{}", Namespace: "mcp__demo__", Kind: "function"}},
			{Kind: ir.PartToolCall, ToolCall: &ir.ToolCall{ID: "call-2", Name: "apply_patch", Arguments: "patch text", Namespace: "mcp__demo__", Kind: "custom"}},
		},
	}
	obj := EncodeResponsesResponse(resp, ResponsesIDs{Response: "r"})
	if len(obj.Output) != 2 {
		t.Fatalf("output = %+v, want 2 items", obj.Output)
	}
	if obj.Output[0].Type != "function_call" || obj.Output[0].Namespace != "mcp__demo__" {
		t.Errorf("first item = %+v, want namespaced function_call", obj.Output[0])
	}
	if obj.Output[1].Type != "custom_tool_call" || obj.Output[1].Namespace != "mcp__demo__" || obj.Output[1].Input != "patch text" {
		t.Errorf("second item = %+v, want namespaced custom_tool_call with input", obj.Output[1])
	}
	if obj.Output[1].Arguments != "" {
		t.Errorf("custom item must carry input, not arguments: %+v", obj.Output[1])
	}
}

// TestDecodeResponsesRequest_RefusesHostedTools pins fail-closed refusal of
// the hosted kinds with a typed error: web_search and tool_search are never
// dropped silently.
func TestDecodeResponsesRequest_RefusesHostedTools(t *testing.T) {
	cases := map[string]string{
		"web_search":        `{"model":"m","input":"x","tools":[{"type":"web_search","external_web_access":true}]}`,
		"tool_search":       `{"model":"m","input":"x","tools":[{"type":"tool_search","execution":"sync","description":"Search app tools","parameters":{"type":"object"}}]}`,
		"web_search_item":   `{"model":"m","input":[{"type":"web_search_call","id":"ws-1","status":"completed"}]}`,
		"tool_search_item":  `{"model":"m","input":[{"type":"tool_search_call","id":"ts-1","status":"completed"}]}`,
		"file_search_item":  `{"model":"m","input":[{"type":"file_search_call","id":"fs-1","status":"completed"}]}`,
		"computer_item":     `{"model":"m","input":[{"type":"computer_call","id":"c-1","status":"completed"}]}`,
		"computer_use_item": `{"model":"m","input":[{"type":"computer_use_call","id":"cu-1","status":"completed"}]}`,
		"image_gen_item":    `{"model":"m","input":[{"type":"image_generation_call","id":"ig-1","status":"completed"}]}`,
		"nested_hosted":     `{"model":"m","input":"x","tools":[{"type":"namespace","name":"n","tools":[{"type":"function","name":"f","parameters":{"type":"object"}},{"type":"web_search"}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeResponsesRequest([]byte(body))
			if err == nil {
				t.Fatalf("DecodeResponsesRequest(%s) = nil error, want a loud hosted refusal", name)
			}
			if !errors.Is(err, ErrResponsesHostedTool) {
				t.Fatalf("DecodeResponsesRequest(%s) error = %v, want ErrResponsesHostedTool (%v)", name, err, ErrResponsesHostedTool)
			}
		})
	}
}

// TestEncodeRequest_RefusesNamespacedCustom pins that the chat upstream
// cannot serve namespace/custom: it refuses fail-closed rather than
// flattening them onto function tools the model never saw.
func TestEncodeRequest_RefusesNamespacedCustom(t *testing.T) {
	for name, req := range map[string]ir.Request{
		"custom_tool":     {Model: "m", Tools: []ir.ToolDef{{Kind: "custom", Name: "c", Format: map[string]any{"type": "grammar"}}}},
		"namespace_tool":  {Model: "m", Tools: []ir.ToolDef{{Kind: "namespace", Name: "n", Tools: []ir.ToolDef{{Kind: "function", Name: "f"}}}}},
		"custom_call":     {Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant, Parts: []ir.Part{{Kind: ir.PartToolCall, ToolCall: &ir.ToolCall{ID: "1", Name: "c", Arguments: "x", Kind: "custom"}}}}}},
		"namespaced_call": {Model: "m", Messages: []ir.Message{{Role: ir.RoleAssistant, Parts: []ir.Part{{Kind: ir.PartToolCall, ToolCall: &ir.ToolCall{ID: "1", Name: "f", Arguments: "{}", Namespace: "n"}}}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := EncodeRequest(req); err == nil {
				t.Fatalf("EncodeRequest(%s) = nil error, want a loud refusal", name)
			} else if !errors.Is(err, ErrResponsesHostedTool) {
				t.Fatalf("EncodeRequest(%s) error = %v, want ErrResponsesHostedTool", name, err)
			}
		})
	}
}

// TestDecodeResponsesRequest_RefusesUnnamedNamespace pins that a namespace
// tool without its grouping name is refused: follow-ups route by that name,
// so an empty grouping key must never enter the IR silently.
func TestDecodeResponsesRequest_RefusesUnnamedNamespace(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[{"type":"namespace","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}]}`
	_, err := DecodeResponsesRequest([]byte(body))
	if err == nil {
		t.Fatalf("DecodeResponsesRequest(unnamed namespace) = nil error, want refusal")
	}
}

// TestDecodeResponsesRequest_RefusesNestedNamespaceTools pins fail-closed
// handling of namespace children that themselves carry tools: a function
// child with a Tools array would otherwise have its nested tools silently
// dropped on the way into the IR.
func TestDecodeResponsesRequest_RefusesNestedNamespaceTools(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"n","tools":[{"type":"function","name":"f","parameters":{"type":"object"},"tools":[{"type":"function","name":"g"}]}]}]}`
	_, err := DecodeResponsesRequest([]byte(body))
	if err == nil {
		t.Fatalf("DecodeResponsesRequest(nested tools) = nil error, want refusal")
	}
}

// TestDecodeResponsesRequest_RefusesTypelessTool pins that a tool
// declaration with a missing/empty type is refused instead of entering the
// IR as an empty phantom ToolDef.
func TestDecodeResponsesRequest_RefusesTypelessTool(t *testing.T) {
	for name, body := range map[string]string{
		"missing_type": `{"model":"m","input":"hi","tools":[{"name":"x"}]}`,
		"empty_type":   `{"model":"m","input":"hi","tools":[{"type":"","name":"x"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeResponsesRequest([]byte(body))
			if err == nil {
				t.Fatalf("DecodeResponsesRequest(%s) = nil error, want refusal", name)
			}
		})
	}
}

// TestDecodeResponsesRequest_RefusesFormatlessCustom pins that a custom
// tool without its grammar format is refused: the model never saw a tool
// contract, so it must never enter the IR silently.
func TestDecodeResponsesRequest_RefusesFormatlessCustom(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[{"type":"custom","name":"c"}]}`
	_, err := DecodeResponsesRequest([]byte(body))
	if err == nil {
		t.Fatalf("DecodeResponsesRequest(format-less custom) = nil error, want refusal")
	}
}

// TestDecodeResponsesRequest_FunctionToolsUnchanged pins that function tools
// behave as before: strict/defer_loading (which Codex sends) are accepted and
// the IR carries the function as before.
func TestDecodeResponsesRequest_FunctionToolsUnchanged(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[{"type":"function","name":"get_weather","description":"d","strict":false,"parameters":{"type":"object"}}]}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode function: %v", err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Kind != "function" || req.Tools[0].Name != "get_weather" {
		t.Errorf("tools = %+v, want the function", req.Tools)
	}
	if !strings.Contains(req.Tools[0].Name, "get_weather") {
		t.Errorf("name = %q", req.Tools[0].Name)
	}
}
