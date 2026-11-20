package codex

import (
	"encoding/json"
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
	got := moderationBlocks(json.RawMessage(`{"tool_call":{"b":{"is_blocked":true}},"prompt":{"a":{"outputs":[{"is_blocked":false},{"is_blocked":true}]},"c":{"is_blocked":false}}}`))
	want := []string{"prompt/a", "tool_call/b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("moderationBlocks = %v, want %v", got, want)
	}
}

func TestFileChangeItemCarriesKindsAndDiff(t *testing.T) {
	t.Parallel()
	params := `{"item":{"type":"fileChange","id":"fc-1","status":"completed","changes":[` +
		`{"path":"a.go","kind":{"type":"update","move_path":null},"diff":"@@ -1 +1 @@\n-old\n+new\n"},` +
		`{"path":"b.go","kind":{"type":"update","move_path":"c.go"},"diff":"@@ -2 +2 @@\n-x\n+y"},` +
		`{"path":"d.go","kind":{"type":"add"},"diff":"package d"},` +
		`{"path":"e.go","kind":"delete"}]}}`
	got := mapItem("item/completed", json.RawMessage(params), nil)
	if len(got) != 1 {
		t.Fatalf("fileChange mapped %d events; want 1", len(got))
	}
	res, ok := got[0].(agent.ToolResultEvent)
	if !ok {
		t.Fatalf("event = %#v; want ToolResultEvent", got[0])
	}
	want := "update: a.go\nupdate: b.go -> c.go\nadd: d.go\ndelete: e.go\n\n" +
		"--- a.go\n+++ a.go\n@@ -1 +1 @@\n-old\n+new\n" +
		"--- b.go\n+++ c.go\n@@ -2 +2 @@\n-x\n+y"
	if res.ToolName != "file_change" || res.ToolUseID != "fc-1" || res.Content != want {
		t.Fatalf("result = %+v\ncontent %q\nwant    %q", res, res.Content, want)
	}
}
