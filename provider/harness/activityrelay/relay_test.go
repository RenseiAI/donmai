package activityrelay

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelayScriptAppendsOneLineAndPrintsNothing(t *testing.T) {
	t.Parallel()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	payload := `{"session_id":"sess-1","hook_event_name":"SessionStart","source":"startup"}`
	cmd := exec.Command("/bin/sh", r.ScriptPath(), "session_start")
	cmd.Stdin = strings.NewReader(payload)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("relay script: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("relay script printed %q, want silence (any stdout may steer the session)", out)
	}

	raw, err := os.ReadFile(r.EventsPath())
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("events file has %d lines, want 1: %q", len(lines), raw)
	}
	var decoded struct {
		V       int             `json:"v"`
		Event   string          `json:"event"`
		TS      string          `json:"ts"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("relay line is not JSON: %v\n%s", err, lines[0])
	}
	if decoded.V != 1 || decoded.Event != "session_start" || decoded.TS == "" {
		t.Fatalf("relay envelope = %+v, want v=1 event=session_start with a timestamp", decoded)
	}
	var inner map[string]any
	if err := json.Unmarshal(decoded.Payload, &inner); err != nil {
		t.Fatalf("payload was not spliced verbatim: %v", err)
	}
	if inner["session_id"] != "sess-1" {
		t.Fatalf("payload lost the session id: %v", inner)
	}
}

func TestRelayScriptExitsZeroWithNoStdin(t *testing.T) {
	t.Parallel()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	cmd := exec.Command("/bin/sh", r.ScriptPath(), "stop")
	cmd.Stdin = strings.NewReader("")
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("relay script with empty stdin: %v (output %q)", err, out)
	}
	parsed, ok, err := ParseLine(mustRead(t, r.EventsPath()))
	if err != nil || !ok {
		t.Fatalf("ParseLine = %+v, %v, %v; want the empty-stdin fire parsed", parsed, ok, err)
	}
	if parsed.Event != "stop" {
		t.Fatalf("event = %q, want stop", parsed.Event)
	}
}

func TestRelayCommandBakesEventAndPath(t *testing.T) {
	t.Parallel()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	got := r.Command("SessionStart")
	if !strings.HasPrefix(got, "/bin/sh ") {
		t.Fatalf("command = %q, want /bin/sh invocation", got)
	}
	if !strings.HasSuffix(got, " session_start") {
		t.Fatalf("command = %q, want the canonical event on argv", got)
	}
	if !strings.Contains(got, r.ScriptPath()) {
		t.Fatalf("command = %q, want the relay script path baked in", got)
	}
}

func TestParseLineFixtures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		line      string
		wantEvent string
		wantOK    bool
	}{
		{
			name:      "session start with JSON payload",
			line:      `{"v":1,"event":"session_start","ts":"2026-10-12T00:00:00Z","payload":{"session_id":"s"}}`,
			wantEvent: "session_start",
			wantOK:    true,
		},
		{
			name:      "claude casing normalizes",
			line:      `{"v":1,"event":"UserPromptSubmit","ts":"t","payload":{"prompt":"hi"}}`,
			wantEvent: "user_prompt_submit",
			wantOK:    true,
		},
		{
			name:      "permission request",
			line:      `{"v":1,"event":"permission_request","ts":"t","payload":{"tool_name":"Bash"}}`,
			wantEvent: "permission_request",
			wantOK:    true,
		},
		{
			name:   "foreign event drops",
			line:   `{"v":1,"event":"PreToolUse","ts":"t","payload":{}}`,
			wantOK: false,
		},
		{
			name:   "wrong version drops",
			line:   `{"v":2,"event":"stop","ts":"t","payload":{}}`,
			wantOK: false,
		},
		{
			name:   "blank line drops",
			line:   "   \n",
			wantOK: false,
		},
		{
			name:   "non-JSON drops",
			line:   "hook fired at midnight",
			wantOK: false,
		},
		{
			name:   "empty drops",
			line:   "",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parsed, ok, err := ParseLine([]byte(tt.line))
			if err != nil {
				t.Fatalf("ParseLine: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ParseLine ok = %v, want %v (line %q)", ok, tt.wantOK, tt.line)
			}
			if ok && parsed.Event != tt.wantEvent {
				t.Fatalf("event = %q, want %q", parsed.Event, tt.wantEvent)
			}
		})
	}
}

func TestTailerSweepsAppendedLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	tailer := NewTailer(path)

	if got := tailer.Sweep(0, 0); len(got) != 0 {
		t.Fatalf("sweep of a missing file = %d events, want none", len(got))
	}

	appendLines(t, path,
		`{"v":1,"event":"session_start","ts":"t","payload":{}}`,
		`{"v":1,"event":"user_prompt_submit","ts":"t","payload":{}}`,
		"not a relay line",
		`{"v":1,"event":"stop","ts":"t","payload":{}}`,
	)
	events := tailer.Sweep(MaxLinesPerSweep, MaxEventsPerSweep)
	if len(events) != 3 {
		t.Fatalf("sweep = %d events, want 3 (foreign line dropped)", len(events))
	}

	// A second sweep with no new writes is quiet: offsets advanced past
	// consumed lines, including the dropped foreign one.
	if got := tailer.Sweep(MaxLinesPerSweep, MaxEventsPerSweep); len(got) != 0 {
		t.Fatalf("resweep = %d events, want none", len(got))
	}

	appendLines(t, path, `{"v":1,"event":"permission_request","ts":"t","payload":{}}`)
	events = tailer.Sweep(MaxLinesPerSweep, MaxEventsPerSweep)
	if len(events) != 1 {
		t.Fatalf("sweep after append = %d events, want 1", len(events))
	}
}

func TestTailerHoldsTornTrailingLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	tailer := NewTailer(path)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"v":1,"event":"stop","ts":"t","payload":{}}`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if got := tailer.Sweep(MaxLinesPerSweep, MaxEventsPerSweep); len(got) != 0 {
		t.Fatalf("sweep of a torn line = %d events, want none held", len(got))
	}
	appendLines(t, path, "")
	events := tailer.Sweep(MaxLinesPerSweep, MaxEventsPerSweep)
	if len(events) != 1 {
		t.Fatalf("sweep after the newline = %d events, want 1", len(events))
	}
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
