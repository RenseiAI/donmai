package activityrelay

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

func TestRelayScriptAppendsOneLineAndPrintsNothing(t *testing.T) {
	t.Parallel()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	// Hook stdin carries prompt text and tool input: it must never land in
	// the relay file. The moment is the telemetry, never the payload.
	secret := "prompt-secret-prompt-secret-12345"
	cmd := exec.Command("/bin/sh", r.ScriptPath(), "session_start") //nolint:gosec // test re-executes the relay script the test itself just rendered into its own temp dir
	cmd.Stdin = strings.NewReader(`{"session_id":"sess-1","prompt":"` + secret + `"}`)
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
	if strings.Contains(string(raw), secret) {
		t.Fatalf("relay file persisted hook stdin %q: %q", secret, raw)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("events file has %d lines, want 1: %q", len(lines), raw)
	}
	var decoded struct {
		V     int    `json:"v"`
		Event string `json:"event"`
		TS    string `json:"ts"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatalf("relay line is not JSON: %v\n%s", err, lines[0])
	}
	if decoded.V != 1 || decoded.Event != "session_start" || decoded.TS == "" {
		t.Fatalf("relay envelope = %+v, want v=1 event=session_start with a timestamp", decoded)
	}
	if strings.Contains(lines[0], "payload") || strings.Contains(lines[0], "sess-1") {
		t.Fatalf("relay line carries a payload, want moment only: %q", lines[0])
	}
}

func TestRelayScriptExitsZeroWithNoStdin(t *testing.T) {
	t.Parallel()
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	cmd := exec.Command("/bin/sh", r.ScriptPath(), "stop") //nolint:gosec // test re-executes the relay script the test itself just rendered into its own temp dir
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

// TestSweepRespectsEventCap pins A4: one sweep emits at most MaxEventsPerSweep,
// deferring the rest to later ticks rather than flooding the consumer.
func TestSweepRespectsEventCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var lines []string
	for range 30 {
		lines = append(lines, `{"v":1,"event":"stop","ts":"t"}`)
	}
	appendLines(t, path, lines...)
	tailer := NewTailer(path)
	first := tailer.Sweep(100, 16)
	if len(first) != 16 {
		t.Fatalf("first sweep = %d events, want 16 (event cap)", len(first))
	}
	second := tailer.Sweep(100, 16)
	if len(second) != 14 {
		t.Fatalf("second sweep = %d events, want the remaining 14", len(second))
	}
}

// TestSweepRespectsLineCap pins A5: one sweep consumes at most MaxLinesPerSweep
// lines, deferring the rest.
func TestSweepRespectsLineCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var lines []string
	for range 20 {
		lines = append(lines, `{"v":1,"event":"stop","ts":"t"}`)
	}
	appendLines(t, path, lines...)
	tailer := NewTailer(path)
	first := tailer.Sweep(10, 100)
	if len(first) != 10 {
		t.Fatalf("first sweep = %d events, want 10 (line cap)", len(first))
	}
	second := tailer.Sweep(10, 100)
	if len(second) != 10 {
		t.Fatalf("second sweep = %d events, want the remaining 10", len(second))
	}
}

// TestTailerResetsOffsetOnTruncate pins A6: when the file shrinks (rotation or
// truncation), the tailer restarts at zero instead of reading nothing forever.
func TestTailerResetsOffsetOnTruncate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	tailer := NewTailer(path)
	appendLines(t, path, `{"v":1,"event":"session_start","ts":"t"}`)
	if got := tailer.Sweep(MaxLinesPerSweep, MaxEventsPerSweep); len(got) != 1 {
		t.Fatalf("first sweep = %d events, want 1", len(got))
	}
	// Truncate to a shorter file carrying a different moment.
	if err := os.WriteFile(path, []byte(`{"v":1,"event":"stop","ts":"t"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := tailer.Sweep(MaxLinesPerSweep, MaxEventsPerSweep)
	if len(got) != 1 {
		t.Fatalf("post-truncate sweep = %d events, want 1 (offset reset)", len(got))
	}
	sys, ok := got[0].(agent.SystemEvent)
	if !ok || sys.Subtype != agent.SystemSubtypeHarnessLifecycle || sys.Message != agent.HarnessLifecycleStop {
		t.Fatalf("post-truncate event = %#v, want harness_lifecycle/stop", got[0])
	}
}

// TestSweepSkipsOversizeLine pins the M1 line cap: a line longer than
// maxRelayLineBytes is skipped (offset advances past it) without emitting,
// so a huge torn write never forces a huge allocation on every sweep.
func TestSweepSkipsOversizeLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	// Valid-shaped but oversize: without the cap it would parse and emit.
	big := `{"v":1,"event":"stop","ts":"` + strings.Repeat("x", 5000) + `"}`
	appendLines(t, path, big, `{"v":1,"event":"session_start","ts":"t"}`)
	tailer := NewTailer(path)
	got := tailer.Sweep(MaxLinesPerSweep, MaxEventsPerSweep)
	if len(got) != 1 {
		t.Fatalf("sweep = %d events, want 1 (oversize skipped, valid delivered)", len(got))
	}
	// The valid line must be the session_start, proving the oversize was
	// skipped rather than emitted.
	sys, ok := got[0].(agent.SystemEvent)
	if !ok || sys.Subtype != agent.SystemSubtypeHarnessLifecycle || sys.Message != agent.HarnessLifecycleSessionStart {
		t.Fatalf("event = %#v, want harness_lifecycle/session_start", got[0])
	}
	// A second sweep is quiet: the offset advanced past both lines.
	if again := tailer.Sweep(MaxLinesPerSweep, MaxEventsPerSweep); len(again) != 0 {
		t.Fatalf("resweep = %d events, want none", len(again))
	}
}
