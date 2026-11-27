package codex

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestModerationMetadataRoutineIsDropped(t *testing.T) {
	t.Parallel()
	for _, params := range []string{
		`{"metadata":{"prompt":{},"generation":{},"tool_call":{}}}`,
		`{"metadata":{"prompt":{"omnimod":{"protection_type":"omnimod","outputs":[{"results":[{"category_flags":{"A1":false}}],"is_blocked":false}]}}}}`,
		`{"metadata":"not an object"}`,
	} {
		if got := dispatch(t, notificationHandle(), "turn/moderationMetadata", params); len(got) != 0 {
			t.Errorf("routine moderation %s mapped to %+v; want no event", params, got)
		}
	}
}

func TestModerationBlocksSortedAcrossStages(t *testing.T) {
	t.Parallel()
	got, blocked := moderationBlocks(json.RawMessage(`{"tool_call":{"b":{"is_blocked":true}},"prompt":{"a":{"outputs":[{"is_blocked":false},{"is_blocked":true}]},"c":{"is_blocked":false}}}`))
	want := []string{"prompt/a", "tool_call/b"}
	if !blocked || len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("moderationBlocks = %v, %v; want %v, true", got, blocked, want)
	}
}

// TestModerationBlockSurvivesUnexpectedShapes: the metadata is untyped in the
// app-server schema, so a scalar sibling must not hide a named block, and a
// block in a shape the walk cannot attribute still posts a generic notice.
func TestModerationBlockSurvivesUnexpectedShapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ params, want string }{
		{`{"metadata":{"version":1,"prompt":{"cyber_security":{"is_blocked":true}}}}`, "Codex moderation blocked: prompt/cyber_security"},
		{`{"metadata":{"prompt":[{"classifier":"x","is_blocked":true}]}}`, "Codex moderation blocked"},
	} {
		got := dispatch(t, notificationHandle(), "turn/moderationMetadata", tc.params)
		if len(got) != 1 {
			t.Fatalf("%s: events = %+v; want one notice", tc.params, got)
		}
		if ev, ok := got[0].(agent.SystemEvent); !ok || ev.Subtype != "codex_moderation" || ev.Message != tc.want {
			t.Fatalf("%s: event = %+v; want %q", tc.params, got[0], tc.want)
		}
	}
}

// completedItem validates an item/completed notification against the vendored
// app-server schema, so a fixture cannot drift from the wire shape, and maps it.
func completedItem(t *testing.T, params string) []agent.Event {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(params), &decoded); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	validateParams(t, compileParamSchema(t, "ItemCompletedNotification"), "item/completed fixture", decoded)
	return mapItem("item/completed", json.RawMessage(params), nil)
}

func singleToolResult(t *testing.T, got []agent.Event) agent.ToolResultEvent {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("mapped %d events; want 1: %+v", len(got), got)
	}
	res, ok := got[0].(agent.ToolResultEvent)
	if !ok {
		t.Fatalf("event = %#v; want ToolResultEvent", got[0])
	}
	return res
}

// TestFileChangeItemCarriesKindsAndDiff pins the fileChange rendering on the
// schema's PatchChangeKind objects: one "kind: path" line per file (with the
// rename target), then each update's unified diff under a ---/+++ header.
// Only an update whose diff is a hunk renders one: a pure rename's empty
// diff and an add's file content (even one that starts with "@@") do not.
func TestFileChangeItemCarriesKindsAndDiff(t *testing.T) {
	t.Parallel()
	params := `{"threadId":"t-1","turnId":"turn-1","completedAtMs":1,"item":{"type":"fileChange","id":"fc-1","status":"completed","changes":[` +
		`{"path":"a.go","kind":{"type":"update","move_path":null},"diff":"@@ -1 +1 @@\n-old\n+new\n"},` +
		`{"path":"b.go","kind":{"type":"update","move_path":"c.go"},"diff":"@@ -2 +2 @@\n-x\n+y"},` +
		`{"path":"r.go","kind":{"type":"update","move_path":"s.go"},"diff":""},` +
		`{"path":"d.go","kind":{"type":"add"},"diff":"@@ literal first line of a new file"},` +
		`{"path":"e.go","kind":{"type":"delete"},"diff":"package e\n"}]}}`
	res := singleToolResult(t, completedItem(t, params))
	want := "update: a.go\nupdate: b.go -> c.go\nupdate: r.go -> s.go\nadd: d.go\ndelete: e.go\n\n" +
		"--- a.go\n+++ a.go\n@@ -1 +1 @@\n-old\n+new\n" +
		"--- b.go\n+++ c.go\n@@ -2 +2 @@\n-x\n+y"
	if res.ToolName != "file_change" || res.ToolUseID != "fc-1" || res.Content != want {
		t.Fatalf("result = %+v\ncontent %q\nwant    %q", res, res.Content, want)
	}
}

// TestFileChangeItemLegacyStringKind keeps the older bare-string kind.
func TestFileChangeItemLegacyStringKind(t *testing.T) {
	t.Parallel()
	params := `{"item":{"type":"fileChange","id":"fc-2","status":"completed","changes":[{"path":"e.go","kind":"delete"}]}}`
	if res := singleToolResult(t, mapItem("item/completed", json.RawMessage(params), nil)); res.Content != "delete: e.go" {
		t.Fatalf("content = %q; want %q", res.Content, "delete: e.go")
	}
}

// TestToolResultContentDecodingFollowsTheWireShape pins where content parts
// are decoded: a shell command's aggregatedOutput is the command's own stdout
// and passes through verbatim even when it is a JSON array of typed objects,
// while an MCP result's content is a content-block array and decodes to text.
func TestToolResultContentDecodingFollowsTheWireShape(t *testing.T) {
	t.Parallel()
	const stdout = `[{"type":"modified","path":"main.go"}]`
	shell := `{"threadId":"t-1","turnId":"turn-1","completedAtMs":1,"item":{"type":"commandExecution","id":"exec-1","command":"cat changes.json","cwd":"/w","commandActions":[],"status":"completed","exitCode":0,"aggregatedOutput":` + strconv.Quote(stdout) + `}}`
	if res := singleToolResult(t, completedItem(t, shell)); res.Content != stdout {
		t.Fatalf("shell content = %q; want stdout verbatim %q", res.Content, stdout)
	}
	mcp := `{"threadId":"t-1","turnId":"turn-1","completedAtMs":1,"item":{"type":"mcpToolCall","id":"mcp-1","server":"docs","tool":"search","arguments":{"q":"x"},"status":"completed","result":{"content":[{"type":"text","text":"hit one"},{"type":"image","data":"AA==","mimeType":"image/png"}]}}}`
	if res := singleToolResult(t, completedItem(t, mcp)); res.Content != "hit one\n[image]" {
		t.Fatalf("mcp content = %q; want decoded text", res.Content)
	}
}
