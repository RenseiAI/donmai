// Command vtfixture is the named, deterministic fixture TUI for the viewer-side
// attach test harness. Spawned in a PTY, it draws a KNOWN screen and responds to
// KNOWN keystrokes with KNOWN, byte-exact screen changes, so a smoke can assert
// exact screen state (cursor position, specific cell text, alt-screen enter/exit)
// and thereby DISTINGUISH a correctly-rendered redraw from garbled output.
//
// Fixture NAME: "vtfixture". Fixed geometry: 80 columns x 24 rows. No
// timestamps, no randomness — every drawn byte is deterministic.
//
// Screen-assertion contract (rows/cols are 0-indexed, matching
// attachwire.Screen; assert with the viewertest package helpers):
//
//	ON START (primary buffer):
//	  IsAltScreen(s)            == false
//	  RowText(s, 0)             == "FIXTURE-PRIMARY"
//	  CellText(s, 0, 0)         == "F"
//	  CellText(s, 2, 4)         == "R"      (the "READY" marker starts at col 4)
//	  RowText(s, 2)             == "    READY"   (READY at cols 4..8)
//	  CursorAt(s)               == (5, 10)
//
//	AFTER sending key 'a' (enter alt-screen, ESC[?1049h):
//	  IsAltScreen(s)            == true
//	  RowText(s, 0)             == "FIXTURE-ALT"
//	  CellText(s, 0, 0)         == "F"
//	  CellText(s, 1, 2)         == "A"      (the "ALPHA" marker starts at col 2)
//	  RowText(s, 1)             == "  ALPHA"      (ALPHA at cols 2..6)
//	  CursorAt(s)               == (7, 3)
//
//	AFTER sending key 'q' (exit alt-screen, ESC[?1049l):
//	  IsAltScreen(s)            == false
//	  RowText(s, 0)             == "FIXTURE-PRIMARY"   (primary buffer restored)
//	  CellText(s, 2, 4)         == "R"
//	  CursorAt(s)               == (5, 10)
//
// Keys other than 'a'/'q' are ignored. 'a' is a no-op while already in
// alt-screen; 'q' is a no-op while in primary. The process runs until its stdin
// closes or it is signalled (the host session's Stop path).
package main

import (
	"bufio"
	"fmt"
	"os"

	"golang.org/x/term"
)

// The fixture geometry and the byte-exact screen contract. These constants are
// the single source of truth the harness/smoke assertions are written against.
const (
	Name = "vtfixture"
	Cols = 80
	Rows = 24

	PrimaryBanner     = "FIXTURE-PRIMARY" // row 0, col 0
	PrimaryMarker     = "READY"           // row 2, col 4 (0-indexed)
	PrimaryMarkerRow0 = 2
	PrimaryMarkerCol0 = 4
	PrimaryCursorRow0 = 5
	PrimaryCursorCol0 = 10

	AltBanner     = "FIXTURE-ALT" // row 0, col 0
	AltMarker     = "ALPHA"       // row 1, col 2 (0-indexed)
	AltMarkerRow0 = 1
	AltMarkerCol0 = 2
	AltCursorRow0 = 7
	AltCursorCol0 = 3

	KeyEnterAlt = 'a'
	KeyExitAlt  = 'q'
)

func main() {
	// Raw mode: disable line-discipline echo so a driven keystroke is NOT echoed
	// onto the screen (which would pollute the deterministic cell contract), and
	// deliver input byte-at-a-time.
	fd := int(os.Stdin.Fd())
	if oldState, err := term.MakeRaw(fd); err == nil {
		defer func() { _ = term.Restore(fd, oldState) }()
	}

	out := bufio.NewWriter(os.Stdout)
	inAlt := false

	drawPrimary(out)
	flush(out)

	reader := bufio.NewReader(os.Stdin)
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return // stdin closed / EOF — normal teardown
		}
		switch b {
		case KeyEnterAlt:
			if !inAlt {
				enterAlt(out)
				inAlt = true
			}
		case KeyExitAlt:
			if inAlt {
				exitAlt(out)
				inAlt = false
			}
		}
		flush(out)
	}
}

func flush(out *bufio.Writer) { _ = out.Flush() }

// ws writes a literal string, discarding the never-until-Flush bufio error.
func ws(out *bufio.Writer, s string) { _, _ = out.WriteString(s) }

// moveTo positions the cursor at 0-indexed (row, col) via a 1-indexed CUP.
func moveTo(out *bufio.Writer, row0, col0 int) {
	_, _ = fmt.Fprintf(out, "\x1b[%d;%dH", row0+1, col0+1)
}

// drawPrimary paints the primary screen: clear, banner at (0,0), marker at
// (2,4), cursor parked at (5,10), cursor visible.
func drawPrimary(out *bufio.Writer) {
	ws(out, "\x1b[2J") // erase entire display
	moveTo(out, 0, 0)
	ws(out, PrimaryBanner)
	moveTo(out, PrimaryMarkerRow0, PrimaryMarkerCol0)
	ws(out, PrimaryMarker)
	moveTo(out, PrimaryCursorRow0, PrimaryCursorCol0)
	ws(out, "\x1b[?25h") // show cursor
}

// enterAlt switches to the alternate screen buffer and paints the alt screen:
// banner at (0,0), marker at (1,2), cursor parked at (7,3).
func enterAlt(out *bufio.Writer) {
	ws(out, "\x1b[?1049h") // enter alt-screen (saves cursor + primary)
	ws(out, "\x1b[2J")
	moveTo(out, 0, 0)
	ws(out, AltBanner)
	moveTo(out, AltMarkerRow0, AltMarkerCol0)
	ws(out, AltMarker)
	moveTo(out, AltCursorRow0, AltCursorCol0)
	ws(out, "\x1b[?25h")
}

// exitAlt returns to the primary buffer (restoring its preserved content) and
// reparks the cursor deterministically at (5,10) regardless of the emulator's
// saved-cursor restore behavior.
func exitAlt(out *bufio.Writer) {
	ws(out, "\x1b[?1049l") // exit alt-screen (restores primary buffer)
	moveTo(out, PrimaryCursorRow0, PrimaryCursorCol0)
	ws(out, "\x1b[?25h")
}
