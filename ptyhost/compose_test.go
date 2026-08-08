package ptyhost

import "testing"

// TestComposeTracker_Pending is the unit-level half of the composition gate.
// The PTY-level half is TestSession_TryWriteNoticeRespectsPendingCompose; this
// one is exhaustive and fast.
//
// The regression it exists for: the previous "was the last byte a line break?"
// heuristic answered TRUE for every one of the erase / navigation / report
// cases below, so the gate latched permanently shut on a line that was in fact
// empty and no notice was ever delivered again.
func TestComposeTracker_Pending(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		// Baseline.
		{name: "nothing typed", input: "", want: false},
		{name: "half-typed line", input: "abcdef", want: true},
		{name: "submitted with CR", input: "abcdef\r", want: false},
		{name: "submitted with LF", input: "abcdef\n", want: false},
		{name: "interrupted with ctrl-c", input: "abcdef\x03", want: false},
		{name: "killed line with ctrl-u", input: "abcdef\x15", want: false},
		{name: "typed again after submitting", input: "abc\rdef", want: true},
		{name: "whitespace is text", input: "  ", want: true},

		// Erase keys — the previous heuristic latched on every one of these.
		{name: "backspace to empty (DEL)", input: "abc\x7f\x7f\x7f", want: false},
		{name: "backspace to empty (BS)", input: "abc\x08\x08\x08", want: false},
		{name: "backspace partway", input: "abc\x7f", want: true},
		{name: "backspace past empty", input: "a\x7f\x7f\x7f", want: false},
		{name: "backspace erases a whole rune", input: "あ\x7f", want: false},
		{name: "backspace erases only one rune", input: "ああ\x7f", want: true},
		{name: "ctrl-w erases the only word", input: "hello\x17", want: false},
		{name: "ctrl-w erases trailing spaces and the word", input: "hello   \x17", want: false},
		{name: "ctrl-w leaves an earlier word", input: "hello world\x17", want: true},

		// Navigation / escape sequences — no text change, must not latch.
		{name: "bare escape", input: "\x1b", want: false},
		{name: "escape then escape", input: "\x1b\x1b", want: false},
		{name: "CSI arrow right", input: "\x1b[C", want: false},
		{name: "SS3 arrow right (application cursor keys)", input: "\x1bOC", want: false},
		{name: "home and end", input: "\x1b[H\x1b[F", want: false},
		{name: "tab", input: "\t", want: false},
		{name: "other C0 control (ctrl-a)", input: "\x01", want: false},
		{name: "SGR mouse press report", input: "\x1b[<35;10;5M", want: false},
		{name: "SGR mouse release report", input: "\x1b[<35;10;5m", want: false},
		{name: "focus-in report", input: "\x1b[I", want: false},
		{name: "focus-out report", input: "\x1b[O", want: false},
		{name: "OSC string terminated by BEL", input: "\x1b]0;title\x07", want: false},
		{name: "OSC string terminated by ST", input: "\x1b]0;title\x1b\\", want: false},
		{name: "arrow key while composing still composing", input: "abc\x1b[D", want: true},
		{name: "arrow key after submitting stays clear", input: "abc\r\x1b[D", want: false},

		// The exact reviewer repro: type, erase, walk away at an empty prompt.
		{name: "type, erase, then navigate away", input: "abc\x7f\x7f\x7f\x1b[C\x1b[D\t", want: false},

		// Bracketed paste: content is TEXT, not keys.
		{name: "open paste with content", input: "\x1b[200~hi", want: true},
		{name: "CR inside a paste does not submit", input: "\x1b[200~hi\r", want: true},
		{name: "paste closed then submitted", input: "\x1b[200~hi\x1b[201~\r", want: false},
		{name: "empty paste region", input: "\x1b[200~\x1b[201~", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c composeTracker
			c.feed([]byte(tc.input))
			if got := c.pending(); got != tc.want {
				t.Fatalf("pending() after %q = %v; want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestComposeTracker_SequencesSplitAcrossWrites pins the incremental parser:
// a relay delivers keystrokes in whatever chunks the socket hands it, so an
// escape sequence routinely arrives split. Each fragment must be consumed as
// part of the sequence rather than as text.
func TestComposeTracker_SequencesSplitAcrossWrites(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   bool
	}{
		{name: "arrow split after ESC", chunks: []string{"\x1b", "[C"}, want: false},
		{name: "arrow split before final", chunks: []string{"\x1b[", "C"}, want: false},
		{name: "mouse report split three ways", chunks: []string{"\x1b[<", "35;10;", "5M"}, want: false},
		{name: "paste markers split", chunks: []string{"\x1b[20", "0~", "hi", "\x1b[2", "01~"}, want: true},
		{name: "paste opened and closed across writes", chunks: []string{"\x1b[200~", "hi\x1b[201~", "\r"}, want: false},
		{name: "text after a split arrow still counts", chunks: []string{"\x1b[", "C", "x"}, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c composeTracker
			for _, chunk := range tc.chunks {
				c.feed([]byte(chunk))
			}
			if got := c.pending(); got != tc.want {
				t.Fatalf("pending() after %q = %v; want %v", tc.chunks, got, tc.want)
			}
		})
	}
}

// TestComposeTracker_OverlongLineStaysBoundedAndClears keeps a runaway line
// from growing the session's memory while preserving the property that
// matters: a submit still clears the gate.
func TestComposeTracker_OverlongLineStaysBoundedAndClears(t *testing.T) {
	var c composeTracker
	blob := make([]byte, composeLineCap*3)
	for i := range blob {
		blob[i] = 'x'
	}
	c.feed(blob)

	if !c.pending() {
		t.Fatal("an overlong composed line must report pending")
	}
	if len(c.line) > composeLineCap {
		t.Fatalf("tracked line grew to %d bytes; cap is %d", len(c.line), composeLineCap)
	}
	c.feed([]byte("\r"))
	if c.pending() {
		t.Fatal("submit must clear the gate even after the line exceeded the cap")
	}
}
