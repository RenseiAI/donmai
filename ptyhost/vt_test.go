package ptyhost

import (
	"bytes"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
)

// TestTmuxReference feeds recorded tmux sessions through the full VT →
// attachwire.Screen serialization path and asserts the serialized grid matches
// `tmux capture-pane -p -e` byte-for-byte (per-cell text), reusing the vt spike's
// comparison policy. Pass bar = 0 text mismatches. This is the recorded-fixture
// snapshot-correctness gate.
func TestTmuxReference(t *testing.T) {
	for _, name := range []string{"tmux_vim", "tmux_split"} {
		t.Run(name, func(t *testing.T) {
			raw, m := loadFixture(t, name)
			if m.Reference == nil || len(m.Reference.Panes) == 0 {
				t.Fatalf("%s: no tmux reference in sidecar", name)
			}
			off, ok := m.offset("attached_redraw")
			if !ok {
				t.Fatalf("%s: attached_redraw checkpoint missing", name)
			}
			v := feedVT(t, m.Cols, m.Rows, raw[:off])
			if !v.emu.IsAltScreen() {
				t.Fatalf("%s: expected tmux client on alt screen at redraw", name)
			}

			scr := serializeScreen(t, v)
			if scr.ActiveBuffer != attachwire.BufferAlt || !scr.AltPresent {
				t.Fatalf("%s: serialized screen should be alt-active with alt grid present", name)
			}
			grid := scr.Alt
			cols := int(scr.Cols) //nolint:gosec // G115: scr.Cols is the VT grid width from a small fixture terminal, always fits int

			total := 0
			for _, p := range m.Reference.Panes {
				capLines := strings.Split(stripSGR(p.CaptureE), "\n")
				mism := 0
				for j := 0; j < p.Height; j++ {
					got := gridRowText(grid, cols, p.Left, p.Top+j, p.Width)
					want := ""
					if j < len(capLines) {
						want = trimRightSpaces(capLines[j])
					}
					if got != want {
						mism++
						if mism <= 3 {
							t.Errorf("%s pane %s row %d text mismatch:\n  vt  |%s|\n  tmux|%s|", name, p.ID, j, got, want)
						}
					}
				}
				total += mism
				if mism == 0 {
					t.Logf("%s pane %s: %d rows, 0 mismatch", name, p.ID, p.Height)
				}
				if p.Active {
					wantX, wantY := p.Left+p.CursorX, p.Top+p.CursorY
					if int(scr.CursorCol) != wantX || int(scr.CursorRow) != wantY { //nolint:gosec // G115: cursor coords are clamped to the VT grid dims above, always fit int
						t.Errorf("%s active pane %s cursor = (%d,%d), want (%d,%d)",
							name, p.ID, scr.CursorCol, scr.CursorRow, wantX, wantY)
					}
				}
			}
			if total != 0 {
				t.Errorf("%s: %d total text mismatches (pass bar = 0)", name, total)
			}
		})
	}
}

// TestVimAltScreenRoundTrip feeds the deterministic vim fixture and asserts the
// serialized screen enters/exits the alt screen at the recorded checkpoints and
// round-trips escape-safe through attachwire.
func TestVimAltScreen(t *testing.T) {
	raw, m := loadFixture(t, "vim")

	openOff, ok := m.offset("vim_opened")
	if !ok {
		t.Fatal("vim_opened checkpoint missing")
	}
	v := feedVT(t, m.Cols, m.Rows, raw[:openOff])
	scr := serializeScreen(t, v)
	if scr.ActiveBuffer != attachwire.BufferAlt {
		t.Errorf("at vim_opened: expected alt-screen active, got buffer %d", scr.ActiveBuffer)
	}

	quitOff, ok := m.offset("shell_after")
	if !ok {
		t.Fatal("shell_after checkpoint missing")
	}
	v2 := feedVT(t, m.Cols, m.Rows, raw[:quitOff])
	scr2 := serializeScreen(t, v2)
	if scr2.ActiveBuffer != attachwire.BufferPrimary {
		t.Errorf("at shell_after: expected primary buffer after :q!, got buffer %d", scr2.ActiveBuffer)
	}
}

// TestQueryResponder proves the synchronous query responder (spike wrapper duty
// 2 & 3): a query fed into the VT produces the terminal-standard reply written to
// the response writer (the PTY master), and NO query reply is left in the grid.
func TestQueryResponder(t *testing.T) {
	tests := []struct {
		name string
		feed string
		want string // substring expected in the response
	}{
		{"CPR", "\x1b[6n", "\x1b[1;1R"},         // cursor at home → row 1 col 1
		{"DA1", "\x1b[c", "\x1b[?"},             // primary device attributes
		{"DA2", "\x1b[>c", "\x1b[>"},            // secondary device attributes
		{"DSR5", "\x1b[5n", "\x1b[0n"},          // operating status OK
		{"DECRQSS-SGR", "\x1bP$qm\x1b\\", "$r"}, // DECRQSS SGR request
		{"OSC11-bg-query", "\x1b]11;?\x07", "\x1b]11;rgb:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var resp bytes.Buffer
			v := newVTHost(80, 24, DefaultScrollback, &resp, nil)
			v.write([]byte(tc.feed))
			if !strings.Contains(resp.String(), tc.want) {
				t.Errorf("response %q does not contain %q", resp.String(), tc.want)
			}
		})
	}
}

// TestQueryResponderCursorMoved confirms the CPR reflects a non-home cursor.
func TestQueryResponderCursorMoved(t *testing.T) {
	var resp bytes.Buffer
	v := newVTHost(80, 24, DefaultScrollback, &resp, nil)
	// Move cursor to row 5, col 10 (CUP is 1-based) then query.
	v.write([]byte("\x1b[5;10H\x1b[6n"))
	if got := resp.String(); !strings.Contains(got, "\x1b[5;10R") {
		t.Errorf("CPR after CUP(5,10) = %q, want it to contain \\x1b[5;10R", got)
	}
}

// TestInBandResizeNoDeadlock proves that a child enabling DEC private mode ?2048
// (in-band resize) — whose emulator-internal reply would otherwise deadlock a
// feeder with no response-pipe reader — is suppressed and does not hang, while
// other modes batched with it are preserved.
func TestInBandResizeNoDeadlock(t *testing.T) {
	var resp bytes.Buffer
	v := newVTHost(80, 24, DefaultScrollback, &resp, nil)
	// ?2048 alone, then ?2048 batched with ?2004 (bracketed paste) and ?1 (DECCKM).
	v.write([]byte("\x1b[?2048h"))
	v.write([]byte("\x1b[?2004;2048;1h"))
	raw := v.raw()
	// Bracketed paste + app cursor keys must have been applied despite the ?2048
	// suppression; pending-wrap bit is set separately.
	if raw.modes&attachwire.ModeBracketedPaste == 0 {
		t.Error("bracketed paste (?2004) batched with ?2048 was lost")
	}
	if raw.modes&attachwire.ModeAppCursorKeys == 0 {
		t.Error("app cursor keys (?1) batched with ?2048 was lost")
	}
}
