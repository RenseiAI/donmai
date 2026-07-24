package pi

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestLFFramingOnly pins design §1's framing invariant: events are split on
// U+000A ONLY. A JSON string value that legitimately contains U+2028 / U+2029
// (Unicode line separators, which readline-style splitters wrongly break on)
// must arrive as ONE event, not two. This test fails loudly if a future
// refactor swaps bufio.ScanLines for a splitter that also breaks on those
// runes.
func TestLFFramingOnly(t *testing.T) {
	t.Parallel()
	// One JSON line whose "text" value literally contains U+2028 and U+2029
	// (Unicode line separators). readline-style splitters wrongly break on
	// these; bufio.ScanLines must not, so this must decode as ONE event.
	const wantText = "a\u2028b\u2029c"
	line := "{\"type\":\"message_end\",\"text\":\"" + wantText + "\"}\n"
	c := newRPCClient(io.Discard, strings.NewReader(line))

	var got []rawEvent
	for ev := range c.Events() {
		got = append(got, ev)
	}
	if len(got) != 1 {
		t.Fatalf("LF framing: got %d events, want exactly 1 (U+2028/U+2029 must not split the line)", len(got))
	}
	if txt, _ := got[0].Fields["text"].(string); txt != wantText {
		t.Errorf("text field = %q, want the line separators preserved intact", txt)
	}
}

// TestWriteCommand_Framing verifies commands are LF-terminated and that an
// embedded raw newline in a marshaled command is refused (framing safety).
func TestWriteCommand_Framing(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	c := newRPCClient(&buf, strings.NewReader(""))
	if err := c.WriteCommand(map[string]any{"command": "prompt", "text": "hello\nworld"}); err != nil {
		t.Fatalf("WriteCommand: %v", err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("command not LF-terminated: %q", out)
	}
	// The embedded newline in the value must be JSON-escaped, so the framed
	// line contains exactly one trailing "\n".
	if strings.Count(out, "\n") != 1 {
		t.Errorf("command line has %d newlines, want exactly 1 (value newline must be escaped): %q", strings.Count(out, "\n"), out)
	}
	if !strings.Contains(out, `\n`) {
		t.Errorf("expected the value's newline JSON-escaped as \\n, got %q", out)
	}
}

// TestReadLoop_DecodesTypes confirms the event type discriminant is decoded.
func TestReadLoop_DecodesTypes(t *testing.T) {
	t.Parallel()
	stream := strings.Join([]string{
		`{"type":"agent_start","sessionId":"ses_1"}`,
		`not json at all`,
		`{"type":"agent_end","success":true}`,
	}, "\n") + "\n"
	c := newRPCClient(io.Discard, strings.NewReader(stream))
	var types []string
	for ev := range c.Events() {
		types = append(types, ev.Type)
	}
	// The malformed middle line becomes a typeless event (observability, not
	// fatal) so all three lines are surfaced.
	if len(types) != 3 || types[0] != "agent_start" || types[2] != "agent_end" {
		t.Errorf("decoded types = %v, want [agent_start, <empty>, agent_end]", types)
	}
}
