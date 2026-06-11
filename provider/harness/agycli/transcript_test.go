package agycli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestParseTranscriptFile_Fixture parses a REAL transcript captured from agy
// v1.0.4 (testdata/transcript_sample.jsonl) and asserts the structured
// tool-use / tool-result mapping. Re-validate this fixture on every agy bump.
func TestParseTranscriptFile_Fixture(t *testing.T) {
	t.Parallel()
	events := parseTranscriptFile(filepath.Join("testdata", "transcript_sample.jsonl"))

	// USER_INPUT + CONVERSATION_HISTORY + final text PLANNER_RESPONSE are
	// skipped; two tool-invoking PLANNER_RESPONSEs + two tool-result steps
	// remain → 4 events in order.
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %#v", len(events), events)
	}

	tu0, ok := events[0].(agent.ToolUseEvent)
	if !ok || tu0.ToolName != "list_dir" {
		t.Fatalf("event[0] = %#v, want ToolUseEvent{list_dir}", events[0])
	}
	if _, has := tu0.Input["DirectoryPath"]; !has {
		t.Errorf("list_dir args missing DirectoryPath: %#v", tu0.Input)
	}

	tr1, ok := events[1].(agent.ToolResultEvent)
	if !ok || tr1.ToolName != "LIST_DIRECTORY" || tr1.IsError {
		t.Fatalf("event[1] = %#v, want ToolResultEvent{LIST_DIRECTORY,!err}", events[1])
	}
	if tr1.Content == "" {
		t.Errorf("LIST_DIRECTORY result content empty")
	}
	// The list_dir result must be id-paired to the list_dir use.
	if tu0.ToolUseID == "" || tr1.ToolUseID != tu0.ToolUseID {
		t.Errorf("tool use/result not id-paired: use=%q result=%q", tu0.ToolUseID, tr1.ToolUseID)
	}

	tu2, ok := events[2].(agent.ToolUseEvent)
	if !ok || tu2.ToolName != "view_file" {
		t.Fatalf("event[2] = %#v, want ToolUseEvent{view_file}", events[2])
	}

	tr3, ok := events[3].(agent.ToolResultEvent)
	if !ok || tr3.ToolName != "VIEW_FILE" {
		t.Fatalf("event[3] = %#v, want ToolResultEvent{VIEW_FILE}", events[3])
	}
}

func TestParseTranscriptFile_Missing(t *testing.T) {
	t.Parallel()
	if got := parseTranscriptFile(filepath.Join(t.TempDir(), "nope.jsonl")); got != nil {
		t.Errorf("missing transcript should return nil, got %#v", got)
	}
}

func TestParseTranscript_MultiCallPairing(t *testing.T) {
	t.Parallel()
	// One PLANNER_RESPONSE with TWO tool_calls, then two result steps in order.
	// FIFO-pairing must link result#1→call#0 and result#2→call#1.
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	lines := []string{
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"run_command","args":{"Command":"\"ls\""}},{"name":"view_file","args":{"AbsolutePath":"\"/x\""}}]}`,
		`{"step_index":2,"source":"MODEL","type":"RUN_COMMAND","status":"DONE","content":"out-A"}`,
		`{"step_index":3,"source":"MODEL","type":"VIEW_FILE","status":"DONE","content":"out-B"}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	events := parseTranscriptFile(path)
	// 2 ToolUse + 2 ToolResult.
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %#v", len(events), events)
	}
	use0 := events[0].(agent.ToolUseEvent)
	use1 := events[1].(agent.ToolUseEvent)
	res0 := events[2].(agent.ToolResultEvent)
	res1 := events[3].(agent.ToolResultEvent)
	if use0.ToolUseID == use1.ToolUseID {
		t.Errorf("the two tool-use ids should differ: %q == %q", use0.ToolUseID, use1.ToolUseID)
	}
	if res0.ToolUseID != use0.ToolUseID {
		t.Errorf("result#0 should pair to call#0: %q vs %q", res0.ToolUseID, use0.ToolUseID)
	}
	if res1.ToolUseID != use1.ToolUseID {
		t.Errorf("result#1 should pair to call#1: %q vs %q", res1.ToolUseID, use1.ToolUseID)
	}
}

func TestMapTranscriptLine_ErrorStatus(t *testing.T) {
	t.Parallel()
	tl := transcriptLine{Source: "MODEL", Type: "RUN_COMMAND", Status: "ERROR", Content: "boom"}
	evs := mapTranscriptLine(tl, []byte(`{}`))
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	tr, ok := evs[0].(agent.ToolResultEvent)
	if !ok || !tr.IsError {
		t.Errorf("ERROR status should yield IsError result: %#v", evs[0])
	}
}

func TestDecodeToolArgs_DoubleEncoded(t *testing.T) {
	t.Parallel()
	// agy double-encodes arg values as JSON strings.
	args := map[string]json.RawMessage{
		"AbsolutePath": json.RawMessage(`"\"/tmp/x\""`),
		"count":        json.RawMessage(`5`),
	}
	got := decodeToolArgs(args)
	if got["AbsolutePath"] != `"/tmp/x"` {
		t.Errorf("AbsolutePath decode = %#v, want quoted path string", got["AbsolutePath"])
	}
	if got["count"] != float64(5) {
		t.Errorf("count decode = %#v, want 5", got["count"])
	}
}

func TestDiscoverConvID(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	convID := "abc-123"
	writeTranscript(t, home, convID, "{}\n")

	// Empty "before" → conv is fresh → discovered.
	if got, ok := discoverConvID(home, "/some/cwd", map[string]struct{}{}); !ok || got != convID {
		t.Fatalf("discoverConvID = %q,%v want %q,true", got, ok, convID)
	}

	// If it already existed pre-spawn it is NOT fresh.
	before := map[string]struct{}{convID: {}}
	if _, ok := discoverConvID(home, "/some/cwd", before); ok {
		t.Errorf("pre-existing conv should not be discovered as fresh")
	}
}

func TestConvIDForCwd(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cacheDir := filepath.Join(home, "antigravity-cli", "cache")
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		t.Fatal(err)
	}
	cwd := "/work/tree/x"
	body, _ := json.Marshal(map[string]string{cwd: "conv-xyz"})
	if err := os.WriteFile(filepath.Join(cacheDir, "last_conversations.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := convIDForCwd(home, cwd); !ok || got != "conv-xyz" {
		t.Errorf("convIDForCwd = %q,%v want conv-xyz,true", got, ok)
	}
	if _, ok := convIDForCwd(home, "/not/mapped"); ok {
		t.Errorf("unmapped cwd should miss")
	}
}

func TestTailer_DiscoversAndParses(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	convID := "run-1"
	fixture, err := os.ReadFile(filepath.Join("testdata", "transcript_sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, home, convID, string(fixture))

	var evs []agent.Event
	var discovered []string
	tailer := &transcriptTailer{
		stateHome: home,
		cwd:       "/work/x",
		before:    map[string]struct{}{}, // empty snapshot → the conv is fresh
		emit:      func(ev agent.Event) { evs = append(evs, ev) },
		onConvID:  func(id string) { discovered = append(discovered, id) },
	}
	tailer.poll(true)

	if len(discovered) != 1 || discovered[0] != convID {
		t.Fatalf("onConvID calls = %v, want exactly [%s]", discovered, convID)
	}
	if len(evs) != 4 {
		t.Fatalf("end-to-end enrichment got %d events, want 4: %#v", len(evs), evs)
	}
	if _, ok := evs[0].(agent.ToolUseEvent); !ok {
		t.Errorf("first enriched event should be a ToolUseEvent, got %#v", evs[0])
	}

	// A second poll after discovery must not re-emit anything (offset
	// tracking) nor re-invoke onConvID.
	tailer.poll(true)
	if len(evs) != 4 || len(discovered) != 1 {
		t.Errorf("second poll re-emitted: %d events, %d discoveries", len(evs), len(discovered))
	}
}

func TestTailer_EmptyStateHome(t *testing.T) {
	t.Parallel()
	var evs []agent.Event
	tailer := &transcriptTailer{
		stateHome: "",
		emit:      func(ev agent.Event) { evs = append(evs, ev) },
	}
	tailer.poll(true)
	if len(evs) != 0 {
		t.Errorf("empty stateHome should emit nothing, got %#v", evs)
	}
}

func writeTranscript(t *testing.T, home, convID, body string) {
	t.Helper()
	dir := filepath.Join(home, "antigravity-cli", "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G703: test helper; dir/body are test-controlled temp paths
	if err := os.WriteFile(filepath.Join(dir, "transcript.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureWorkspaceTrusted(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	settingsDir := filepath.Join(home, "antigravity-cli")
	if err := os.MkdirAll(settingsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Seed with an existing field to confirm preservation.
	seed, _ := json.Marshal(map[string]any{"colorScheme": "x", "trustedWorkspaces": []string{"/already"}})
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), seed, 0o600); err != nil {
		t.Fatal(err)
	}

	cwd := "/work/tree/new"
	if changed := ensureWorkspaceTrusted(home, cwd); !changed {
		t.Fatal("first call should add the cwd and report changed")
	}
	// Idempotent second call.
	if changed := ensureWorkspaceTrusted(home, cwd); changed {
		t.Error("second call should be a no-op (already trusted)")
	}

	var settings map[string]any
	data, _ := os.ReadFile(filepath.Join(settingsDir, "settings.json"))
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["colorScheme"] != "x" {
		t.Errorf("unknown field not preserved: %#v", settings)
	}
	got := stringSlice(settings["trustedWorkspaces"])
	if !contains(got, "/already") || !contains(got, cwd) {
		t.Errorf("trustedWorkspaces = %v, want both /already and %s", got, cwd)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
