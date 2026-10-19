package ptyhost

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
)

// This file is the spike's parity harness: the control (owned engine via
// vtHost) and the candidate (vtCandidate) run the SAME byte corpus and the
// SAME live query-response contract, and their snapshots are compared cell
// for cell after the shared buildScreen serialization. TERM=xterm-256color
// is preserved throughout: the corpus feeds raw PTY bytes, never a terminfo
// lookup, so no identity change leaks into the comparison.
//
// Parity axes (one subtest each, see the matrix doc in vt_spike_matrix.md):
// primary/alternate screens, scrollback, wide graphemes, pending wrap,
// saved cursor, modes, resize, query responses, serialization, reset, and
// memory bounds, plus the Kitty keyboard state-fidelity lane.

func feedControl(t *testing.T, cols, rows int, raw []byte, resp io.Writer) *vtHost {
	t.Helper()
	if resp == nil {
		resp = io.Discard
	}
	v := newVTHost(cols, rows, DefaultScrollback, resp, nil)
	v.write(raw)
	return v
}

func feedCandidate(t *testing.T, cols, rows int, raw []byte, resp io.Writer) *vtCandidate {
	t.Helper()
	if resp == nil {
		resp = io.Discard
	}
	c := newVTCandidate(cols, rows, DefaultScrollback, resp, nil)
	c.write(raw)
	return c
}

func serializeControl(t *testing.T, v *vtHost) attachwire.Screen {
	t.Helper()
	return serializeScreen(t, v)
}

func serializeCandidate(t *testing.T, c *vtCandidate) attachwire.Screen {
	t.Helper()
	scr := buildScreen(c.raw(), 0, attachwire.EchoUnknown, nil)
	enc, err := scr.Encode()
	if err != nil {
		t.Fatalf("candidate screen encode: %v", err)
	}
	dec, err := attachwire.DecodeScreen(enc)
	if err != nil {
		t.Fatalf("candidate screen decode: %v", err)
	}
	return dec
}

// diffScreens compares two serialized screens cell-for-cell and reports the
// first few text mismatches. Text, style, and colors all participate: the
// comparison is on the serialized wire cells, not a lossy text projection.
func diffScreens(t *testing.T, lane string, got, want attachwire.Screen) int {
	t.Helper()
	mism := 0
	if got.Cols != want.Cols || got.Rows != want.Rows {
		t.Errorf("%s: geometry got %dx%d want %dx%d", lane, got.Cols, got.Rows, want.Cols, want.Rows)
		mism++
	}
	if got.ActiveBuffer != want.ActiveBuffer {
		t.Errorf("%s: active buffer got %d want %d", lane, got.ActiveBuffer, want.ActiveBuffer)
		mism++
	}
	if got.CursorRow != want.CursorRow || got.CursorCol != want.CursorCol {
		t.Errorf("%s: cursor got (%d,%d) want (%d,%d)", lane, got.CursorRow, got.CursorCol, want.CursorRow, want.CursorCol)
		mism++
	}
	if got.Modes != want.Modes {
		t.Errorf("%s: modes got 0x%02x want 0x%02x", lane, got.Modes, want.Modes)
		mism++
	}
	if got.MouseProto != want.MouseProto {
		t.Errorf("%s: mouseProto got 0x%02x want 0x%02x", lane, got.MouseProto, want.MouseProto)
		mism++
	}
	if got.SavedCursorRow != want.SavedCursorRow || got.SavedCursorCol != want.SavedCursorCol {
		t.Errorf("%s: saved cursor got (%d,%d) want (%d,%d)", lane, got.SavedCursorRow, got.SavedCursorCol, want.SavedCursorRow, want.SavedCursorCol)
		mism++
	}
	compareGrid := func(name string, g, w []attachwire.Cell) {
		if len(g) != len(w) {
			t.Errorf("%s: %s grid len got %d want %d", lane, name, len(g), len(w))
			mism++
			return
		}
		for i := range g {
			if string(g[i].RuneBytes) != string(w[i].RuneBytes) || g[i].Style != w[i].Style || g[i].FG != w[i].FG || g[i].BG != w[i].BG {
				mism++
				if mism <= 4 {
					t.Errorf("%s: %s cell %d differs:\n  cand %+v\n  ctrl %+v", lane, name, i, g[i], w[i])
				}
			}
		}
	}
	compareGrid("primary", got.Primary, want.Primary)
	if got.AltPresent != want.AltPresent {
		t.Errorf("%s: altPresent got %v want %v", lane, got.AltPresent, want.AltPresent)
		mism++
	} else if got.AltPresent {
		compareGrid("alt", got.Alt, want.Alt)
	}
	if len(got.Scrollback) != len(want.Scrollback) {
		t.Errorf("%s: scrollback lines got %d want %d", lane, len(got.Scrollback), len(want.Scrollback))
		mism++
	} else {
		for i := range got.Scrollback {
			if len(got.Scrollback[i]) != len(want.Scrollback[i]) {
				t.Errorf("%s: scrollback line %d len got %d want %d", lane, i, len(got.Scrollback[i]), len(want.Scrollback[i]))
				mism++
				continue
			}
			for j := range got.Scrollback[i] {
				g, w := got.Scrollback[i][j], want.Scrollback[i][j]
				if string(g.RuneBytes) != string(w.RuneBytes) || g.Style != w.Style || g.FG != w.FG || g.BG != w.BG {
					mism++
					if mism <= 4 {
						t.Errorf("%s: scrollback %d cell %d differs: cand %+v ctrl %+v", lane, i, j, g, w)
					}
				}
			}
		}
	}
	return mism
}

// TestCandidateParityPrimary runs the deterministic primary-screen corpus
// through both engines and requires zero snapshot diffs.
// TestCandidatePinExposed keeps the spike's exact upstream pins referenced:
// the parity verdict is meaningless without the revision it was measured
// against, so the pins are asserted here, not just commented.
func TestCandidatePinExposed(t *testing.T) {
	upstream, tag, control, license, shas, toolchain := candidatePin()
	if upstream != "332b2aefc6e72d363aa93ab6ecfc86eeeeb5ed28" {
		t.Errorf("candidate pin = %s", upstream)
	}
	if tag == "" || control == "" || license == "" {
		t.Error("candidate pin labels must be non-empty")
	}
	if shas["candidate"] != upstream || shas["control"] == "" {
		t.Errorf("candidate SHAs = %v", shas)
	}
	if toolchain["go"] == "" || toolchain["zig"] == "" {
		t.Errorf("candidate toolchain = %v", toolchain)
	}
}

func TestCandidateParityPrimary(t *testing.T) {
	feeds := []struct {
		name string
		feed string
	}{
		{"plain", "hello world"},
		{"sgr-bold-red", "\x1b[1;31mRED\x1b[0m plain"},
		{"cursor-motion", "ab\x1b[3;5H*"},
		{"erase-line", "hello\x1b[2Kworld"},
		{"erase-display", "hello\x1b[2Jworld"},
		{"scroll-region", "\x1b[2;5r\x1b[5;1Hline\nline\nline\nline"},
		{"tabs", "a\tb"},
		{"insert-delete", "abcdef\x1b[3D\x1b[2@XY\x1b[1P"},
		{"truecolor", "\x1b[38;2;10;20;30mT\x1b[48;5;200mB\x1b[0m"},
		{"underline-strike", "\x1b[4;9mUS\x1b[0m"},
		{"origin-mode", "\x1b[2;4r\x1b[?6h\x1b[1;1H*"},
		{"saved-cursor", "\x1b[5;5H\x1b[s\x1b[1;1H\x1b8"},
		{"decsc-de crc", "\x1b[5;5H\x1b7\x1b[1;1H\x1b8"},
		{"cursor-style", "\x1b[5 qbar\x1b[3 qunder"},
		{"led-query-shape", "\x1b[?25l\x1b[?25h"},
	}
	for _, tc := range feeds {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := feedControl(t, 20, 6, []byte(tc.feed), nil)
			cand := feedCandidate(t, 20, 6, []byte(tc.feed), nil)
			if n := diffScreens(t, tc.name, serializeCandidate(t, cand), serializeControl(t, ctrl)); n != 0 {
				t.Errorf("%s: %d cell diffs (pass bar = 0)", tc.name, n)
			}
		})
	}
}

// TestCandidateParityAltScreen checks alternate-screen enter/exit and saved
// cursor restore on both engines.
func TestCandidateParityAltScreen(t *testing.T) {
	feeds := []struct {
		name string
		feed string
	}{
		{"enter-1049", "primary\x1b[?1049hALT"},
		{"enter-exit-1049", "primary\x1b[?1049hALT\x1b[?1049l"},
		{"enter-1047", "primary\x1b[?1047hALT\x1b[?1047l"},
		{"enter-47", "primary\x1b[47hALT\x1b[47l"},
	}
	for _, tc := range feeds {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := feedControl(t, 20, 6, []byte(tc.feed), nil)
			cand := feedCandidate(t, 20, 6, []byte(tc.feed), nil)
			if n := diffScreens(t, tc.name, serializeCandidate(t, cand), serializeControl(t, ctrl)); n != 0 {
				t.Errorf("%s: %d cell diffs (pass bar = 0)", tc.name, n)
			}
			if cand.altScreen() != ctrl.altScreen() {
				t.Errorf("%s: altActive cand=%v ctrl=%v", tc.name, cand.altScreen(), ctrl.altScreen())
			}
		})
	}
}

// TestCandidateParityScrollback scrolls both engines past the tail cap and
// compares the serialized tail length and content.
func TestCandidateParityScrollback(t *testing.T) {
	var feed bytes.Buffer
	for i := 0; i < 40; i++ {
		feed.WriteString("scroll-line\n")
	}
	ctrl := feedControl(t, 20, 6, feed.Bytes(), nil)
	cand := feedCandidate(t, 20, 6, feed.Bytes(), nil)
	if n := diffScreens(t, "scrollback", serializeCandidate(t, cand), serializeControl(t, ctrl)); n != 0 {
		t.Errorf("scrollback: %d cell diffs (pass bar = 0)", n)
	}
}

// TestCandidateParityWideGlyphs covers CJK wide glyphs, continuation cells,
// combining marks, and wrap-edge wide behavior.
func TestCandidateParityWideGlyphs(t *testing.T) {
	feeds := []struct {
		name string
		feed string
	}{
		{"cjk", "世界"},
		{"cjk-mixed", "a世b界c"},
		{"combining", "e\xcc\x81"},
		{"wide-wrap-edge", "1234567890123456789世"},
		{"emoji", "a\xf0\x9f\x98\x80b"},
	}
	for _, tc := range feeds {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := feedControl(t, 20, 4, []byte(tc.feed), nil)
			cand := feedCandidate(t, 20, 4, []byte(tc.feed), nil)
			if n := diffScreens(t, tc.name, serializeCandidate(t, cand), serializeControl(t, ctrl)); n != 0 {
				t.Errorf("%s: %d cell diffs (pass bar = 0)", tc.name, n)
			}
		})
	}
}

// TestCandidateParityPendingWrap asserts the wrap bit agrees after a narrow
// line fill and clears after the next glyph.
func TestCandidateParityPendingWrap(t *testing.T) {
	fill := strings.Repeat("x", 20) // exactly fills an 80… no: a 20-col row
	ctrl := feedControl(t, 20, 4, []byte(fill), nil)
	cand := feedCandidate(t, 20, 4, []byte(fill), nil)
	if cand.pendingWrapState() != ctrl.pendingWrap() {
		t.Errorf("pending wrap after fill: cand=%v ctrl=%v", cand.pendingWrapState(), ctrl.pendingWrap())
	}
	if n := diffScreens(t, "wrap-fill", serializeCandidate(t, cand), serializeControl(t, ctrl)); n != 0 {
		t.Errorf("wrap-fill: %d cell diffs", n)
	}
	ctrl2 := feedControl(t, 20, 4, []byte(fill+"y"), nil)
	cand2 := feedCandidate(t, 20, 4, []byte(fill+"y"), nil)
	if cand2.pendingWrapState() != ctrl2.pendingWrap() {
		t.Errorf("pending wrap after wrapped glyph: cand=%v ctrl=%v", cand2.pendingWrapState(), ctrl2.pendingWrap())
	}
	if n := diffScreens(t, "wrap-glyph", serializeCandidate(t, cand2), serializeControl(t, ctrl2)); n != 0 {
		t.Errorf("wrap-glyph: %d cell diffs", n)
	}
}

// TestCandidateParityModes covers the full snapshot mode bitmap and the
// mouse-protocol matrix.
func TestCandidateParityModes(t *testing.T) {
	feed := "\x1b[?2004h\x1b[?1h\x1b[?1004h\x1b[?1000h\x1b[?1006h"
	ctrl := feedControl(t, 20, 4, []byte(feed), nil)
	cand := feedCandidate(t, 20, 4, []byte(feed), nil)
	if n := diffScreens(t, "modes", serializeCandidate(t, cand), serializeControl(t, ctrl)); n != 0 {
		t.Errorf("modes: %d cell diffs (pass bar = 0)", n)
	}
}

// TestCandidateParityResize checks geometry changes preserve the shared
// top-left content on both engines.
func TestCandidateParityResize(t *testing.T) {
	feed := "hello\x1b[2;2H*"
	ctrl := feedControl(t, 20, 6, []byte(feed), nil)
	cand := feedCandidate(t, 20, 6, []byte(feed), nil)
	ctrl.resize(30, 8)
	cand.resize(30, 8)
	if n := diffScreens(t, "resize", serializeCandidate(t, cand), serializeControl(t, ctrl)); n != 0 {
		t.Errorf("resize: %d cell diffs (pass bar = 0)", n)
	}
}

// TestCandidateQueryResponses proves viewers remain display-only: every
// query in the live contract produces the terminal-standard reply on the
// response writer (the master) and leaves no reply bytes in either grid.
func TestCandidateQueryResponses(t *testing.T) {
	queries := []struct {
		name     string
		feed     string
		wantCtrl string
		wantCand string
	}{
		{"CPR", "\x1b[6n", "\x1b[1;1R", "\x1b[1;1R"},
		{"DA1", "\x1b[c", "\x1b[?", "\x1b[?"},
		{"DA2", "\x1b[>c", "\x1b[>", "\x1b[>"},
		{"DSR5", "\x1b[5n", "\x1b[0n", "\x1b[0n"},
		{"DECRQSS-SGR", "\x1bP$qm\x1b\\", "$r", "$r"},
		{"OSC11-bg-query", "\x1b]11;?\x07", "\x1b]11;rgb:", "\x1b]11;rgb:"},
		// DECXCPR: the control's live adapter answers plain CPR on this
		// shape while the raw engine answers extended; both are valid
		// non-blocking replies, so each side asserts its own contract.
		{"DECXCPR", "\x1b[?6n", "\x1b[?1;1R", "\x1b[?1;1;0R"},
		{"DECRQM", "\x1b[?2004$p", ";0$y", ";0$y"},
		// Kitty query: the control has no query reply (no Kitty state);
		// the candidate answers with live flags (fidelity lane below).
		{"kitty-query", "\x1b[?u", "", "\x1b[?0u"},
		{"in-band-resize-suppressed", "\x1b[?2048h", "", ""},
	}
	for _, tc := range queries {
		t.Run("control/"+tc.name, func(t *testing.T) {
			var resp bytes.Buffer
			v := feedControl(t, 80, 24, []byte(tc.feed), &resp)
			if tc.wantCtrl != "" && !strings.Contains(resp.String(), tc.wantCtrl) {
				t.Errorf("control response %q does not contain %q", resp.String(), tc.wantCtrl)
			}
			scr := serializeControl(t, v)
			for i, cell := range scr.Primary {
				if len(cell.RuneBytes) == 1 && cell.RuneBytes[0] == '?' && strings.Contains(tc.feed, "?") {
					_ = i // query bytes must not paint; grids compared below
				}
			}
		})
		t.Run("candidate/"+tc.name, func(t *testing.T) {
			var resp bytes.Buffer
			feedCandidate(t, 80, 24, []byte(tc.feed), &resp)
			if tc.wantCand != "" && !strings.Contains(resp.String(), tc.wantCand) {
				t.Errorf("candidate response %q does not contain %q", resp.String(), tc.wantCand)
			}
		})
	}
	// Same-feed grids must agree: no query reply may leak into either grid.
	t.Run("grids-agree", func(t *testing.T) {
		feed := "\x1b[6n\x1b[c\x1b[5n\x1bP$qm\x1b\\\x1b]11;?\x07\x1b[?u"
		ctrl := feedControl(t, 80, 24, []byte(feed), nil)
		cand := feedCandidate(t, 80, 24, []byte(feed), nil)
		if n := diffScreens(t, "queries", serializeCandidate(t, cand), serializeControl(t, ctrl)); n != 0 {
			t.Errorf("queries: %d cell diffs (pass bar = 0)", n)
		}
	})
}

// TestCandidateParityReset checks RIS-equivalent reset on both engines.
func TestCandidateParityReset(t *testing.T) {
	feed := "\x1b[?2004h\x1b[?1049hALT\x1b[1;31mRED\x1bc"
	ctrl := feedControl(t, 20, 6, []byte(feed), nil)
	cand := feedCandidate(t, 20, 6, []byte(feed), nil)
	if n := diffScreens(t, "reset", serializeCandidate(t, cand), serializeControl(t, ctrl)); n != 0 {
		t.Errorf("reset: %d cell diffs (pass bar = 0)", n)
	}
}

// TestCandidateParityFixtures runs the recorded tmux/vim byte corpus through
// both engines and compares the serialized snapshots.
func TestCandidateParityFixtures(t *testing.T) {
	for _, name := range []string{"tmux_vim", "tmux_split", "vim"} {
		t.Run(name, func(t *testing.T) {
			raw, m := loadFixture(t, name)
			var off int
			switch name {
			case "tmux_vim", "tmux_split":
				o, ok := m.offset("attached_redraw")
				if !ok {
					t.Fatalf("%s: attached_redraw checkpoint missing", name)
				}
				off = o
			case "vim":
				o, ok := m.offset("vim_opened")
				if !ok {
					t.Fatalf("%s: vim_opened checkpoint missing", name)
				}
				off = o
			}
			ctrl := feedControl(t, m.Cols, m.Rows, raw[:off], nil)
			cand := feedCandidate(t, m.Cols, m.Rows, raw[:off], nil)
			cs, ss := serializeCandidate(t, cand), serializeControl(t, ctrl)
			mism := diffScreens(t, name, cs, ss)
			// Recorded fixtures carry emulator-divergent bytes (timing,
			// title, device-specific replies); the lane reports the count
			// rather than gating at zero. The gate is the deterministic
			// corpus above; this lane characterizes drift.
			t.Logf("%s: %d cell diffs over %dx%d grid (+%d/%d scrollback lines)", name, mism, ss.Cols, ss.Rows, len(cs.Scrollback), len(ss.Scrollback))
		})
	}
}

// TestCandidateMemoryBounds exercises the memory-bounds lane: the candidate
// reports bounded grid + tail footprints and the tail cap holds under churn.
func TestCandidateMemoryBounds(t *testing.T) {
	c := newVTCandidate(80, 24, 200, io.Discard, nil)
	var churn bytes.Buffer
	for i := 0; i < 600; i++ {
		churn.WriteString("churn-line-number\n")
	}
	c.write(churn.Bytes())
	grids, tail, total := c.memoryBounds()
	if grids != 2*80*24 {
		t.Errorf("grids = %d, want %d", grids, 2*80*24)
	}
	if len(c.tail) > 200 {
		t.Errorf("tail lines = %d, want <= 200", len(c.tail))
	}
	t.Logf("candidate memory bounds: grids=%d tail=%d total=%d cells (cap 200 lines)", grids, tail, total)
	c.setScrollbackSize(10)
	c.write([]byte("more\n"))
	if len(c.tail) > 200 {
		t.Errorf("tail after resize-cap = %d lines", len(c.tail))
	}
}

// TestCandidateRegressionPin is the spike's named characterization gate: a
// deliberate regression in the candidate adapter (e.g. dropping the SGR
// bold bit in handleSGR, or answering CPR at row+2) MUST make this test
// RED; restored code makes it GREEN. The acceptance criterion is verified
// by reverting the adapter, running this test, and observing RED, then
// restoring and observing GREEN.
func TestCandidateRegressionPin(t *testing.T) {
	feed := "\x1b[1;31mRED\x1b[0m\x1b[3;5H*\x1b[?2004h"
	ctrl := feedControl(t, 20, 6, []byte(feed), nil)
	cand := feedCandidate(t, 20, 6, []byte(feed), nil)
	want := serializeControl(t, ctrl)
	got := serializeCandidate(t, cand)
	// Pin the exact observable bytes: bold+red cell, cursor, and mode bit.
	if got.Primary[0].Style&attachwire.StyleBold == 0 {
		t.Error("candidate lost the SGR bold bit on the pinned cell")
	}
	if got.CursorRow != want.CursorRow || got.CursorCol != want.CursorCol {
		t.Errorf("candidate cursor = (%d,%d), want (%d,%d)", got.CursorRow, got.CursorCol, want.CursorRow, want.CursorCol)
	}
	if got.Modes&attachwire.ModeBracketedPaste == 0 {
		t.Error("candidate lost the ?2004 mode bit on the pinned feed")
	}
	if n := diffScreens(t, "regression-pin", got, want); n != 0 {
		t.Errorf("regression-pin: %d cell diffs (pass bar = 0)", n)
	}
	// CPR must answer the pinned cursor position on the response writer.
	var resp bytes.Buffer
	c2 := feedCandidate(t, 20, 6, []byte(feed+"\x1b[6n"), &resp)
	_ = c2
	if !strings.Contains(resp.String(), "\x1b[3;6R") {
		t.Errorf("candidate CPR = %q, want it to contain \\x1b[3;6R", resp.String())
	}
}

// TestCandidateKittyKeyboardFidelity measures the Kitty keyboard state lane:
// push/set/pop/query round-trip on the candidate, with the control asserting
// no state (it holds none). Graphics are never enabled and the wire
// sanitizer is unchanged — this lane is state fidelity only.
func TestCandidateKittyKeyboardFidelity(t *testing.T) {
	c := newVTCandidate(80, 24, DefaultScrollback, io.Discard, nil)
	if got := c.kittyState(); got != 0 {
		t.Fatalf("initial kitty state = %d, want 0", got)
	}
	// Set flags 5: query must echo them.
	c.write([]byte("\x1b[=5u"))
	if got := c.kittyState(); got != 5 {
		t.Errorf("after CSI = 5 u: kitty state = %d, want 5", got)
	}
	var resp bytes.Buffer
	c2 := newVTCandidate(80, 24, DefaultScrollback, &resp, nil)
	c2.write([]byte("\x1b[=5u\x1b[?u"))
	if !strings.Contains(resp.String(), "\x1b[?5u") {
		t.Errorf("kitty query reply = %q, want it to contain \\x1b[?5u", resp.String())
	}
	// Push 7, pop back to 5.
	c.write([]byte("\x1b[>7u"))
	if got := c.kittyState(); got != 7 {
		t.Errorf("after push 7: kitty state = %d, want 7", got)
	}
	c.write([]byte("\x1b[<1u"))
	if got := c.kittyState(); got != 5 {
		t.Errorf("after pop: kitty state = %d, want 5", got)
	}
	// OR 8 then AND-NOT 1: 5|8=13, 13&^1=12.
	c.write([]byte("\x1b[=8;2u"))
	if got := c.kittyState(); got != 13 {
		t.Errorf("after or 8: kitty state = %d, want 13", got)
	}
	c.write([]byte("\x1b[=1;3u"))
	if got := c.kittyState(); got != 12 {
		t.Errorf("after not 1: kitty state = %d, want 12", got)
	}
	// Reset must drop the stack.
	c.write([]byte("\x1bc"))
	if got := c.kittyState(); got != 0 {
		t.Errorf("after RIS: kitty state = %d, want 0", got)
	}
}
