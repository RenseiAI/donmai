package ptyhost

import "unicode/utf8"

// composeTracker answers ONE question for the notice gate: does the child's
// line editor currently hold unsubmitted text?
//
// It answers it from the input byte stream alone, because that is the only
// thing the host may observe (§5: input is never re-sanitized and the host
// never asks the child about its state). The previous implementation asked a
// different, cheaper question — "was the LAST byte a line break?" — which is
// not the same question and gets the common case wrong: type `abc`, erase it
// with three backspaces, and the last byte is a backspace, so the gate latches
// shut on an EMPTY line and never opens again. Arrow keys, Escape, Tab,
// Ctrl-W, mouse reports and focus reports latch it the same way. A byte-level
// "last break byte" heuristic cannot express "the line is empty again", so it
// is replaced here by a miniature line-editor model.
//
// The model keeps the printable bytes it believes are pending and applies the
// edit keys that change that set:
//
//	CR / LF            submit          -> line cleared
//	Ctrl-C / Ctrl-U    discard line    -> line cleared
//	BS / DEL           erase one rune  -> last rune popped
//	Ctrl-W             erase one word  -> trailing spaces + one word popped
//	printable          insert          -> appended
//	other C0, Tab      unknown effect  -> ignored (never latches the gate)
//	ESC-introduced     cursor/report   -> consumed whole and ignored
//
// Escape sequences are consumed by a small incremental parser so a sequence
// SPLIT ACROSS WRITES is still consumed as a unit — arrow keys (CSI and SS3),
// Escape alone, mouse reports (`CSI <b;x;y M`), focus reports (`CSI I` / `CSI
// O`) and OSC strings therefore leave the gate exactly as they found it.
// Bracketed paste is tracked (`CSI 200~` / `CSI 201~`) so pasted CR/LF counts
// as pasted TEXT rather than as a submit.
//
// Known imprecision, all in the conservative direction (the gate stays shut,
// a notice is retried rather than spliced):
//
//   - Tab may trigger a completion that inserts text the host cannot see.
//   - `ESC` immediately followed by a printable byte is an Alt-chord, so that
//     byte is consumed rather than counted.
//   - Cursor movement is ignored, so Ctrl-W / BS act on the END of the line
//     regardless of where the cursor actually is.
//   - A line longer than composeLineCap stops accumulating and is simply
//     remembered as "non-empty".
//
// None of these can report an EMPTY line as pending forever, which was the
// defect: every one of them is bounded by a subsequent submit or discard.
type composeTracker struct {
	line []byte   // printable bytes believed pending in the child's line editor
	over bool     // line exceeded composeLineCap: treat as non-empty until cleared
	st   escState // incremental escape-sequence parser state
	csi  []byte   // CSI intermediate/parameter bytes, for paste-marker detection
	pste bool     // inside a bracketed-paste region (CSI 200~ … CSI 201~)
}

// composeLineCap bounds the remembered line. Beyond it the tracker stops
// accumulating and only remembers "non-empty" — enough for the gate, and it
// keeps a runaway paste from growing the session's memory.
const composeLineCap = 4096

// composeCSICap bounds the CSI parameter buffer so a malformed sequence
// cannot grow without limit. Only the first bytes matter (paste markers are
// three digits), so an overlong sequence is simply not recognized as a marker.
const composeCSICap = 16

type escState uint8

const (
	escNone   escState = iota // ordinary bytes
	escSeen                   // ESC received; the introducer decides what follows
	escCSI                    // CSI: consume until a final byte 0x40..0x7E
	escString                 // OSC/DCS/APC/PM: consume until BEL or ST
	escStrESC                 // inside a string, saw ESC (looking for the `\` of ST)
	escSS3                    // SS3: consume exactly one byte
)

// C0 control bytes the model understands.
const (
	byteETX = 0x03 // Ctrl-C — interrupt, discards the line
	byteBS  = 0x08 // Backspace
	byteTAB = 0x09
	byteLF  = 0x0a
	byteCR  = 0x0d
	byteNAK = 0x15 // Ctrl-U — kill line
	byteETB = 0x17 // Ctrl-W — kill word
	byteESC = 0x1b
	byteBEL = 0x07
	byteDEL = 0x7f // Backspace on most terminals
)

// pending reports whether the child's line editor is believed to hold
// unsubmitted text. The notice gate refuses a write while this is true.
func (c *composeTracker) pending() bool {
	return c.over || c.pste || len(c.line) > 0
}

// pasteOpen reports whether the tracked line editor is currently inside a
// bracketed-paste region (CSI 200~ seen, CSI 201~ not yet seen).
//
// A SYSTEM-authority write that must land as ordinary keystrokes — never as
// pasted text — checks this before writing, so it can close a region a
// dropped 201~ frame left dangling instead of having its own bytes swallowed
// as paste content (see ptyhost/systeminput.go).
func (c *composeTracker) pasteOpen() bool {
	return c.pste
}

// feed applies every byte the PTY actually accepted.
func (c *composeTracker) feed(p []byte) {
	for _, b := range p {
		c.feedByte(b)
	}
}

func (c *composeTracker) feedByte(b byte) {
	switch c.st {
	case escSeen:
		c.introducer(b)
		return
	case escCSI:
		if len(c.csi) < composeCSICap {
			c.csi = append(c.csi, b)
		}
		if b >= 0x40 && b <= 0x7e { // final byte
			c.finishCSI(b)
		}
		return
	case escString:
		switch b {
		case byteBEL:
			c.st = escNone
		case byteESC:
			c.st = escStrESC
		}
		return
	case escStrESC:
		// ST is ESC `\`; anything else is still string payload.
		if b == '\\' {
			c.st = escNone
		} else {
			c.st = escString
		}
		return
	case escSS3:
		c.st = escNone
		return
	}

	if b == byteESC {
		c.st = escSeen
		return
	}
	if c.pste {
		// Inside a bracketed paste every byte is TEXT, including CR/LF: the
		// child receives it as pasted content, not as a submit key.
		c.insert(b)
		return
	}
	switch b {
	case byteCR, byteLF, byteETX, byteNAK:
		c.clear()
	case byteBS, byteDEL:
		c.eraseRune()
	case byteETB:
		c.eraseWord()
	case byteTAB:
		// Completion: the inserted text is invisible to the host. Leaving the
		// line unchanged is the conservative reading — a Tab on an empty line
		// must not latch the gate shut.
	default:
		if b < 0x20 {
			return // other C0 controls (cursor moves, Ctrl-L, …): no text change
		}
		c.insert(b)
	}
}

// introducer handles the byte immediately after ESC.
func (c *composeTracker) introducer(b byte) {
	switch b {
	case '[':
		c.st, c.csi = escCSI, c.csi[:0]
	case ']', 'P', '^', '_':
		c.st = escString
	case 'O':
		c.st = escSS3
	case byteESC:
		// ESC ESC: stay armed for the next introducer.
		c.st = escSeen
	default:
		// Alt-chord or a two-byte escape: consumed, no text change.
		c.st = escNone
	}
}

// finishCSI closes a CSI sequence and picks out the bracketed-paste markers.
func (c *composeTracker) finishCSI(final byte) {
	c.st = escNone
	if final != '~' || len(c.csi) < 4 {
		return
	}
	switch string(c.csi[:len(c.csi)-1]) {
	case "200":
		c.pste = true
	case "201":
		c.pste = false
	}
}

func (c *composeTracker) clear() {
	c.line, c.over, c.pste = c.line[:0], false, false
}

func (c *composeTracker) insert(b byte) {
	if len(c.line) >= composeLineCap {
		c.over = true
		return
	}
	c.line = append(c.line, b)
}

// eraseRune pops one whole UTF-8 rune, so erasing a multi-byte character takes
// one Backspace here exactly as it does in the child.
func (c *composeTracker) eraseRune() {
	if c.over {
		// The true length is unknown past the cap; stay conservative.
		return
	}
	if len(c.line) == 0 {
		return
	}
	_, size := utf8.DecodeLastRune(c.line)
	if size <= 0 {
		size = 1
	}
	c.line = c.line[:len(c.line)-size]
}

// eraseWord pops trailing spaces then one word, matching the readline Ctrl-W
// most line editors implement.
func (c *composeTracker) eraseWord() {
	if c.over {
		return
	}
	i := len(c.line)
	for i > 0 && c.line[i-1] == ' ' {
		i--
	}
	for i > 0 && c.line[i-1] != ' ' {
		i--
	}
	c.line = c.line[:i]
}
