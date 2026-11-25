package agent

import (
	"reflect"
	"testing"
)

func intp(n int) *int { return &n }

func TestV2EventsRoundTrip(t *testing.T) {
	t.Parallel()
	events := []Event{
		ReasoningEvent{ItemID: "r1", Summary: "Checking the schema"},
		PlanEvent{ItemID: "p1", Steps: []PlanStep{{Text: "read", Status: PlanStepCompleted}, {Text: "edit", Status: PlanStepInProgress}}},
		FileChangeEvent{ItemID: "fc1", Status: ItemCompleted, Files: []FileChange{{Path: "a.go", Change: FileUpdated, Additions: intp(3), Deletions: intp(1), Diff: "@@ -1 +1 @@"}}},
		InteractiveRequestEvent{RequestID: "q1", ItemID: "tu1", RequestKind: RequestApproval, Prompt: "Run rm?", Options: []string{"allow", "deny"}},
		TurnEvent{TurnID: "t1", Phase: TurnCompleted, Status: ItemCompleted},
		NoticeEvent{Severity: NoticeBlocked, Text: "moderation blocked: prompt/cyber_security", Code: "moderation_blocked"},
		ToolProgressEvent{ToolName: "Bash", ElapsedSeconds: 4, ItemID: "tu1"},
		AssistantTextEvent{Text: "done", Phase: AssistantFinal},
	}
	for _, want := range events {
		data, err := MarshalEvent(want)
		if err != nil {
			t.Fatalf("MarshalEvent(%T): %v", want, err)
		}
		got, err := UnmarshalEvent(data)
		if err != nil {
			t.Fatalf("UnmarshalEvent(%s): %v", data, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip %T:\n got %#v\nwant %#v", want, got, want)
		}
		if !isKnownEvent(want.Kind()) {
			t.Errorf("%s is not a known event kind", want.Kind())
		}
	}
}

func TestV2DecodeErrorIsWrapped(t *testing.T) {
	t.Parallel()
	if _, err := UnmarshalEvent([]byte(`{"kind":"plan","steps":"not-a-list"}`)); err == nil {
		t.Fatal("malformed plan decoded without error")
	}
	if _, err := UnmarshalEvent([]byte(`{"kind":"no_such_kind"}`)); err == nil {
		t.Fatal("unknown kind decoded without error")
	}
}

func TestV2Summaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ev   interface{ Summary() string }
		want string
	}{
		{PlanEvent{Steps: []PlanStep{{Text: "a", Status: PlanStepCompleted}, {Text: "b", Status: PlanStepInProgress}, {Text: "c", Status: PlanStepPending}}}, "Plan 1/3 · b"},
		{PlanEvent{Steps: []PlanStep{{Text: "a", Status: PlanStepCompleted}, {Text: "b", Status: PlanStepPending}}}, "Plan 1/2 · b"},
		{PlanEvent{Steps: []PlanStep{{Text: "a", Status: PlanStepCompleted}}}, "Plan 1/1"},
		{FileChangeEvent{Files: []FileChange{{Path: "a.go", Change: FileUpdated, Additions: intp(3), Deletions: intp(1)}}}, "update a.go (+3 −1)"},
		{FileChangeEvent{Files: []FileChange{{Path: "a.go", Change: FileMoved, MovePath: "b.go"}}}, "move a.go -> b.go"},
		{FileChangeEvent{Files: []FileChange{{Path: "a", Change: FileAdded, Additions: intp(2), Deletions: intp(0)}, {Path: "b", Change: FileDeleted, Additions: intp(0), Deletions: intp(5)}}}, "2 files changed (+2 −5)"},
		{FileChangeEvent{}, "no files changed"},
	}
	for _, tc := range cases {
		if got := tc.ev.Summary(); got != tc.want {
			t.Errorf("Summary() = %q, want %q", got, tc.want)
		}
	}
}
