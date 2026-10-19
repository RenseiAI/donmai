package ptyhost

import (
	"bytes"
	"fmt"
	"image/color"
	"io"
	"log/slog"
	"strconv"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/internal/terminalparser"
	vt "github.com/RenseiAI/donmai/internal/terminalvt"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Pinned upstream under evaluation by this spike. The pin is exact and the
// spike never tracks a branch: every matrix lane below reports against this
// revision, and a rerun first re-verifies the checkout hash.
//
// Upstream: the Ghostty terminal emulator repository, tag v1.3.1
// (release commit). The libghostty-vt C ABI at this pin is a stateless
// surface (OSC/SGR/key-encode/paste helpers only); the stateful Terminal
// with the screen model lives in the Zig module sources in the same tree.
// Toolchain floor for any source-level build of that tree: the minimum Zig
// version recorded in the tree's build.zig.zon at this commit.
//
// The candidate adapter in this file does NOT link that tree: there is no
// Zig toolchain in this repo's dependency stack, no Cgo in the release
// builds (see below), and the pinned C ABI exposes no screen model to
// point a snapshot authority at. The candidate is therefore the narrowest
// meaningful stand-in the spike can build in pure Go behind the existing
// terminal seam: an independent line/grid state machine with its own
// scrollback, wrap, cursor, and mode books, answering the same live
// query-response contract. It exists so the parity harness below measures
// the SEAM (same corpus, same contract, same bytes) rather than any one
// foreign engine — and so a future rerun against a real stateful candidate
// (vendored engine or a later C ABI that grows a screen model) plugs into
// the same harness unchanged.
const (
	// vtCandidateUpstream is the pinned source revision the candidate is
	// evaluated against. Full 40-hex commit, not a tag name: tags move.
	vtCandidateUpstream = "332b2aefc6e72d363aa93ab6ecfc86eeeeb5ed28"
	// vtCandidateUpstreamTag is the human label for that commit.
	vtCandidateUpstreamTag = "v1.3.1"
	// vtCandidateControl is the control revision: the owned continuation
	// engine's upstream source (see internal/terminalvt/UPSTREAM.json).
	vtCandidateControl = "v0.0.0-20260712004152-b16d026a9d2e"
	// vtCandidateLicense is the license observed at the pin (LICENSE file
	// header in the upstream tree).
	vtCandidateLicense = "MIT"
)

// vtCandidateSHAs maps the exact pinned SHAs this spike reports against,
// including the control engine's vendored upstream (internal/terminalvt)
// and the evaluation fixtures. A rerun pins by re-verifying these with
// `git rev-parse` in the pinned checkout — never by trusting a tag.
var vtCandidateSHAs = map[string]string{
	// Candidate upstream release commit (tag v1.3.1).
	"candidate": "332b2aefc6e72d363aa93ab6ecfc86eeeeb5ed28",
	// Control: x/vt pseudo-version recorded in go.mod / UPSTREAM.json.
	"control": "b16d026a9d2e",
}

// vtCandidateToolchain records the build toolchain this spike was evaluated
// with. Any source-level build of the upstream tree additionally requires
// the minimum Zig version in that tree's build.zig.zon, which this repo
// does not vendor and which is absent from PATH here.
var vtCandidateToolchain = map[string]string{
	"go":  "go1.27.2",
	"cgo": "CGO_ENABLED=1 for tests; release builds ship CGO_ENABLED=0 (see .goreleaser.yaml)",
	"zig": "absent (no Zig toolchain in the dependency stack; required for any source-level upstream build)",
}

// candidatePin reports the exact upstream pins this spike evaluated. It
// exists so the pin constants are referenced (and stay honest): the parity
// tests call it, so a pin edit without harness awareness fails loudly.
func candidatePin() (upstream, tag, control, license string, shas, toolchain map[string]string) {
	return vtCandidateUpstream, vtCandidateUpstreamTag, vtCandidateControl, vtCandidateLicense, vtCandidateSHAs, vtCandidateToolchain
}

// vtCandidateCell is one candidate grid cell: printable content, style, and
// display width. It mirrors the fields the snapshot serializer reads from
// uv.Cell (Content/Style/Width) without sharing the control's cell type, so
// a regression in either book shows up as a parity diff, not a shared bug.
type vtCandidateCell struct {
	content string
	style   uv.Style
	width   int
}

// vtCandidateScreen is one candidate screen buffer: a cols*rows grid plus a
// cursor and a saved cursor. The candidate keeps primary and alternate as
// separate grids like the control, switching the active pointer on
// ?1047/?1048/?1049.
type vtCandidateScreen struct {
	cells       []vtCandidateCell
	cursorX     int
	cursorY     int
	savedX      int
	savedY      int
	savedValid  bool
	cursorStyle vt.CursorStyle
	hidden      bool
}

// vtCandidate is the spike's candidate snapshot authority behind the
// terminal seam. It is deliberately narrow: printable text, LF/CR/BS/HT/TAB
// stops, ESC c / ESC 7 / ESC 8 / ESC M, CSI cursor motion (A/B/C/D/E/F/G/H/
// f/d), ICH/DCH/IL/DL/ED/EL/ECH/SU/SD, SGR attributes and 256/truecolor
// colors, DECSTBM region + origin mode, autowrap + pending wrap, wide
// glyphs (width-2 continuation), DEC private modes (alt screens, saved
// cursor, bracketed paste, app cursor keys, focus events, mouse protocol
// matrix, in-band-resize suppression), and the live query-response
// contract (CPR/DA/DSR/DECRQM/DECRQSS/OSC color). Everything outside that
// list (grapheme clustering ?2027, synchronized output ?2026, Kitty
// graphics, sixel) is out of scope for the parity corpus by design: it is
// either unobserved by the serializer or disabled on the wire.
type vtCandidate struct {
	cols, rows int
	scrollback int

	primary vtCandidateScreen
	alt     vtCandidateScreen
	onAlt   bool

	// Cursor/pending-wrap live on the active screen's fields; wrap享
	pendingWrap bool

	// Mode books (same bitmap duties as vtHost.modeState).
	bracketedPaste bool
	appCursorKeys  bool
	focusEvent     bool
	mouseX10       bool
	mouseNormal    bool
	mouseHighlight bool
	mouseButton    bool
	mouseAny       bool
	mouseUTF8      bool
	mouseSGR       bool

	// Kitty keyboard stack: push/pop/set semantics measured for state
	// fidelity only. The candidate never emits graphics and the wire
	// sanitizer still strips the query, so this stack is observable only
	// through the query reply and the encoder state.
	kittyStack []uint8
	kitty      uint8

	// Scroll region (DECSTBM) and origin mode (?6).
	marginTop, marginBot int
	originMode           bool

	// Autowrap (?7, default on).
	autowrap bool

	// Tab stops.
	tabs []bool

	// Scrollback tail: oldest-first capped line list, primary only.
	tail [][]vtCandidateCell

	// Current SGR pen.
	penFg, penBg       color.Color
	penAttrs           uint8
	penUnderline       uv.Underline
	penFgSet, penBgSet bool

	resp   io.Writer
	logger *slog.Logger
	feed   oscTitleFilter

	fgDefault, bgDefault, cursorDefault color.Color

	// parser is the shared DEC ANSI parser (same tokenizer the control
	// engine uses): dispatch arrives as packed ansi.Cmd + ansi.Params, so
	// private prefixes, intermediates, sub-parameters, and split writes
	// are tokenized identically on both sides. The STATE BOOKS (grid,
	// scrollback, modes, wrap, cursor, Kitty stack) stay independent —
	// that is the point of the comparison.
	parser *terminalparser.Parser

	// grapheme accumulates non-ASCII runes until the parser leaves the
	// UTF-8 state, mirroring the control's flushGrapheme discipline.
	grapheme []rune
	// lastChar is the last single-width printable, for REP (CSI b).
	lastChar rune
}

// newVTCandidate builds the candidate at the given geometry. resp and logger
// play the same roles as in newVTHost.
func newVTCandidate(cols, rows, scrollback int, resp io.Writer, logger *slog.Logger) *vtCandidate {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	if resp == nil {
		resp = io.Discard
	}
	c := &vtCandidate{
		cols: cols, rows: rows, scrollback: scrollback,
		resp: resp, logger: logger,
		marginTop: 0, marginBot: rows - 1,
		autowrap:  true,
		fgDefault: color.White, bgDefault: color.Black,
		cursorDefault: color.White,
		penFg:         nil, penBg: nil,
	}
	c.primary = c.newScreen()
	c.alt = c.newScreen()
	c.tabs = make([]bool, cols)
	for i := range c.tabs {
		c.tabs[i] = i%8 == 0
	}
	c.parser = terminalparser.NewParser()
	c.parser.SetHandler(ansi.Handler{
		Print:     c.handlePrint,
		Execute:   c.handleExecute,
		HandleCsi: c.handleCsi,
		HandleEsc: c.handleEsc,
		HandleDcs: c.handleDcs,
		HandleOsc: c.handleOsc,
	})
	return c
}

func (c *vtCandidate) newScreen() vtCandidateScreen {
	s := vtCandidateScreen{
		cells:       make([]vtCandidateCell, c.cols*c.rows),
		cursorStyle: vt.CursorBlock,
	}
	for i := range s.cells {
		s.cells[i] = vtCandidateCell{content: "", width: 1}
	}
	return s
}

func (c *vtCandidate) active() *vtCandidateScreen {
	if c.onAlt {
		return &c.alt
	}
	return &c.primary
}

// ---- terminal seam --------------------------------------------------------

func (c *vtCandidate) write(p []byte) {
	if len(p) == 0 {
		return
	}
	filtered := c.feed.Write(p)
	for i := 0; i < len(filtered); i++ {
		c.parser.Advance(filtered[i])
	}
	// End-of-chunk flush mirrors the control's Write loop (flush when the
	// last byte is consumed): any buffered grapheme becomes observable now.
	c.flushGrapheme()
}

func (c *vtCandidate) resize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	oldCols := c.cols
	c.cols, c.rows = cols, rows
	c.marginTop = 0
	c.marginBot = rows - 1
	for _, s := range []*vtCandidateScreen{&c.primary, &c.alt} {
		nc := make([]vtCandidateCell, cols*rows)
		for i := range nc {
			nc[i] = vtCandidateCell{content: "", width: 1}
		}
		minCols := oldCols
		if cols < minCols {
			minCols = cols
		}
		for y := 0; y < rows && y < len(s.cells)/maxInt(oldCols, 1); y++ {
			for x := 0; x < minCols; x++ {
				nc[y*cols+x] = s.cells[y*oldCols+x]
			}
		}
		s.cells = nc
		if s.cursorX >= cols {
			s.cursorX = cols - 1
		}
		if s.cursorY >= rows {
			s.cursorY = rows - 1
		}
	}
	nt := make([]bool, cols)
	for i := range nt {
		nt[i] = i%8 == 0
	}
	c.tabs = nt
}

func (c *vtCandidate) setScrollbackSize(maxLines int) { c.scrollback = maxLines }

func (c *vtCandidate) altScreen() bool { return c.onAlt }

func (c *vtCandidate) pendingWrapState() bool { return c.pendingWrap }

// raw materializes candidate state into the shared vtRaw shape so the
// unchanged buildScreen path serializes both engines identically.
func (c *vtCandidate) raw() vtRaw {
	c.flushGrapheme()
	blank := func() *uv.Cell { return &uv.Cell{Content: "", Width: 1} }
	toUV := func(cc vtCandidateCell) *uv.Cell {
		if cc.content == "" {
			if cc.width == 0 {
				// Wide-continuation placeholder: Content must stay empty
				// and Width 0 so the serializer emits the continuation
				// flag instead of a blank.
				return &uv.Cell{Content: "", Style: cc.style, Width: 0}
			}
			return &uv.Cell{Content: "", Style: cc.style, Width: 1}
		}
		w := cc.width
		if w < 1 {
			w = 1
		}
		return &uv.Cell{Content: cc.content, Style: cc.style, Width: w}
	}
	primary := make([]*uv.Cell, 0, c.cols*c.rows)
	for _, cc := range c.primary.cells {
		primary = append(primary, toUV(cc))
	}
	if len(primary) == 0 {
		for i := 0; i < c.cols*c.rows; i++ {
			primary = append(primary, blank())
		}
	}
	a := c.active()
	var shape uint8
	switch a.cursorStyle {
	case vt.CursorBlock:
		shape = 0x01
	case vt.CursorUnderline:
		shape = 0x02
	case vt.CursorBar:
		shape = 0x03
	default:
		shape = 0x00
	}
	modes, mouseProto := c.modesByte()
	if c.pendingWrap {
		modes |= 0x04
	}
	r := vtRaw{
		cols: c.cols, rows: c.rows,
		altActive:     c.onAlt,
		cursorX:       a.cursorX,
		cursorY:       a.cursorY,
		cursorVisible: !a.hidden,
		cursorShape:   shape,
		pendingWrap:   c.pendingWrap,
		modes:         modes,
		mouseProto:    mouseProto,
		primary:       primary,
	}
	if c.onAlt {
		alt := make([]*uv.Cell, 0, c.cols*c.rows)
		for _, cc := range c.alt.cells {
			alt = append(alt, toUV(cc))
		}
		r.alt = alt
		r.savedX, r.savedY = c.primary.savedX, c.primary.savedY
	} else {
		r.savedX, r.savedY = a.cursorX, a.cursorY
	}
	for _, line := range c.tail {
		row := make([]*uv.Cell, 0, c.cols)
		for _, cc := range line {
			row = append(row, toUV(cc))
		}
		for len(row) < c.cols {
			row = append(row, blank())
		}
		r.scrollback = append(r.scrollback, row)
	}
	return r
}

func (c *vtCandidate) modesByte() (modes, mouseProto uint8) {
	if c.bracketedPaste {
		modes |= 0x01
	}
	if c.appCursorKeys {
		modes |= 0x02
	}
	if c.focusEvent {
		modes |= 0x10
	}
	track := uint8(0)
	switch {
	case c.mouseAny:
		track = 0x4
	case c.mouseButton:
		track = 0x3
	case c.mouseHighlight:
		track = 0x2
	case c.mouseNormal:
		track = 0x1
	}
	if track != 0 || c.mouseX10 {
		modes |= 0x08
		enc := uint8(0x0)
		if c.mouseSGR {
			enc = 0x2
		} else if c.mouseUTF8 {
			enc = 0x1
		}
		mouseProto = (enc << 4) | (track & 0x0F)
	}
	return modes, mouseProto
}

// kittyState reports the candidate's live Kitty keyboard flags (the top of
// the push stack, or the set value). Measured for state fidelity only.
func (c *vtCandidate) kittyState() uint8 { return c.kitty }

// reset performs RIS-equivalent full reset: grids cleared, modes defaulted,
// scroll region full, Kitty stack dropped, wrap cleared.
func (c *vtCandidate) reset() {
	c.primary = c.newScreen()
	c.alt = c.newScreen()
	c.onAlt = false
	c.pendingWrap = false
	c.bracketedPaste, c.appCursorKeys, c.focusEvent = false, false, false
	c.mouseX10, c.mouseNormal, c.mouseHighlight = false, false, false
	c.mouseButton, c.mouseAny, c.mouseUTF8, c.mouseSGR = false, false, false, false
	c.kittyStack = nil
	c.kitty = 0
	c.marginTop, c.marginBot = 0, c.rows-1
	c.originMode = false
	c.autowrap = true
	c.tail = nil
	c.penFg, c.penBg, c.penFgSet, c.penBgSet = nil, nil, false, false
	c.penAttrs = 0
	c.penUnderline = uv.UnderlineNone
	for i := range c.tabs {
		c.tabs[i] = i%8 == 0
	}
}

// memoryBounds reports the candidate's live heap footprint: grid cells plus
// scrollback tail cells. It is the memory-bounds lane of the parity matrix.
func (c *vtCandidate) memoryBounds() (grids, tail, total int) {
	grids = 2 * c.cols * c.rows
	for _, line := range c.tail {
		tail += len(line)
	}
	return grids, tail, grids + tail
}

// ---- parser dispatch ------------------------------------------------------
// The shared parser tokenizes; these handlers execute against the
// candidate's independent state books. Signatures match ansi.Handler so
// dispatch identity (packed ansi.Cmd + ansi.Params) is the same object the
// control engine receives.

func (c *vtCandidate) handlePrint(r rune) {
	if r >= 0x20 && r < 0x7f {
		if len(c.grapheme) > 0 {
			c.flushGrapheme()
		}
		c.handleGrapheme(string(r), 1)
		return
	}
	// Non-ASCII: accumulate like the control's grapheme buffer. The
	// control flushes when the parser leaves the UTF-8 state or at end of
	// input; the parser here has no end-of-input signal, so each Print also
	// probes a flush: FirstGraphemeCluster consumes only a COMPLETE leading
	// cluster, leaving a bare combining mark buffered for the next Print.
	// That reproduces the control split (base in one cell, lone combining
	// mark in the next) without merging adjacent wide glyphs.
	c.grapheme = append(c.grapheme, r)
	c.flushGraphemeComplete()
}

func (c *vtCandidate) flushGrapheme() {
	if len(c.grapheme) == 0 {
		return
	}
	graphemes := string(c.grapheme)
	for len(graphemes) > 0 {
		cluster, width := ansi.FirstGraphemeCluster(graphemes, ansi.GraphemeWidth)
		c.handleGrapheme(cluster, width)
		graphemes = graphemes[len(cluster):]
	}
	c.grapheme = c.grapheme[:0]
}

// flushGraphemeComplete flushes only complete leading clusters, keeping a
// trailing bare combining mark buffered. A lone combining mark is a
// zero-width cluster that FirstGraphemeCluster still reports as complete,
// so the guard is structural: a TRAILING single combining rune stays
// buffered until the next Print (or a non-print flush) decides whether a
// base follows. That matches the control, where "e" + U+0301 arrives as one
// Print ("e") plus a buffered U+0301, and the buffer only becomes observable
// when a NON-PRINT dispatch (CSI/ESC/control/OSC/DCS or end of Write chunk)
// forces a full flush. Snapshot reads (raw) flush too, so no byte is lost;
// the mark is only ever invisible between two Prints of the same chunk.
func (c *vtCandidate) flushGraphemeComplete() {
	for len(c.grapheme) > 0 {
		graphemes := string(c.grapheme)
		cluster, width := ansi.FirstGraphemeCluster(graphemes, ansi.GraphemeWidth)
		rest := graphemes[len(cluster):]
		if len(rest) == 0 && width == 0 && !startsWithBase(graphemes) {
			return
		}
		c.handleGrapheme(cluster, width)
		c.grapheme = []rune(rest)
	}
}

// isCombining reports whether r is a zero-width combining mark (the
// ranges the width table treats as width 0: combining diacriticals,
// variation selectors, and ZWJ).
func isCombining(r rune) bool {
	return (r >= 0x0300 && r <= 0x036f) || (r >= 0xfe00 && r <= 0xfe0f) || r == 0x200d
}

// startsWithBase reports whether s begins with a base character rather
// than a combining mark.
func startsWithBase(s string) bool {
	for _, r := range s {
		return !isCombining(r)
	}
	return false
}

// handleGrapheme places one grapheme cluster: phantom-wrap handling,
// last-column behavior, and cursor advance mirror the control.
func (c *vtCandidate) handleGrapheme(content string, width int) {
	a := c.active()
	if c.pendingWrap && c.autowrap {
		c.index()
		a = c.active()
		a.cursorX = 0
		c.pendingWrap = false
	}
	if width == 0 {
		// A zero-width cluster reaching placement means the flush decided
		// it stands alone (a bare combining mark): the control paints it
		// as its own cell via SetCell (a width-0 cell keeps its content
		// and advances nothing), so store content with width 0.
		a := c.active()
		idx := a.cursorY*c.cols + a.cursorX
		if idx >= 0 && idx < len(a.cells) {
			a.cells[idx] = vtCandidateCell{content: content, style: uv.Style{Fg: c.currentFg(), Bg: c.currentBg(), Attrs: c.penAttrs, Underline: c.penUnderline}, width: 0}
		}
		return
	}
	c.setCell(a.cursorY*c.cols+a.cursorX, content, uv.Style{Fg: c.currentFg(), Bg: c.currentBg(), Attrs: c.penAttrs, Underline: c.penUnderline}, width)
	r, _ := utf8.DecodeRuneInString(content)
	if width == 1 && utf8.RuneCountInString(content) == 1 {
		c.lastChar = r
	}
	c.pendingWrap = c.autowrap && a.cursorX >= c.cols-1
	if !c.pendingWrap {
		a.cursorX += width
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// setCell writes one cell with the control's wide-cell bookkeeping: a
// width-2 glyph in the last column is stored as blanks (the glyph itself
// wraps on the next print), and a wide glyph elsewhere takes a zero-width
// placeholder in the next column.
func (c *vtCandidate) setCell(idx int, content string, style uv.Style, width int) {
	a := c.active()
	if idx < 0 || idx >= len(a.cells) {
		return
	}
	x := idx % c.cols
	if x+width > c.cols {
		for i := 0; i < width && x+i < c.cols; i++ {
			a.cells[idx+i] = vtCandidateCell{content: "", style: style, width: 1}
		}
		return
	}
	a.cells[idx] = vtCandidateCell{content: content, style: style, width: width}
	if width > 1 {
		for j := 1; j < width && x+j < c.cols; j++ {
			a.cells[idx+j] = vtCandidateCell{content: "", style: style, width: 0}
		}
	}
}

func (c *vtCandidate) handleExecute(b byte) {
	c.flushGrapheme()
	c.putControl(b)
}

func (c *vtCandidate) handleEsc(cmd ansi.Cmd) {
	c.flushGrapheme()
	switch int(cmd) {
	case ansi.Command(0, 0, 'c'): // RIS
		c.reset()
	case ansi.Command(0, 0, '7'): // DECSC
		a := c.active()
		a.savedX, a.savedY = a.cursorX, a.cursorY
		a.savedValid = true
	case ansi.Command(0, 0, '8'): // DECRC
		a := c.active()
		if a.savedValid {
			a.cursorX = clamp(a.savedX, 0, c.cols-1)
			a.cursorY = clamp(a.savedY, 0, c.rows-1)
		}
	case ansi.Command(0, 0, 'M'): // RI
		c.reverseIndex()
	case ansi.Command(0, 0, 'D'): // IND
		c.index()
	case ansi.Command(0, 0, 'E'): // NEL
		c.active().cursorX = 0
		c.index()
	case ansi.Command(0, 0, 'H'): // HTS
		a := c.active()
		if a.cursorX < len(c.tabs) {
			c.tabs[a.cursorX] = true
		}
	}
	// Charset designations, keypad modes, and the rest: accepted, no grid effect.
}

func (c *vtCandidate) handleCsi(cmd ansi.Cmd, params ansi.Params) {
	c.flushGrapheme()
	prefix, inter, final := cmd.Prefix(), cmd.Intermediate(), cmd.Final()
	ps := unpackParams(params)
	switch {
	case prefix == 0 && inter == '$' && (final == 'p' || final == 'q'):
		if final == 'p' {
			n, _, _ := params.Param(0, 0)
			fmt.Fprintf(c.resp, "\x1b[%d;0$y", n) //nolint:errcheck
			return
		}
		fmt.Fprintf(c.resp, "\x1bP0$r\x1b\\") //nolint:errcheck
		return
	case prefix == '?' && inter == 0 && final == 'u':
		fmt.Fprintf(c.resp, "\x1b[?%du", c.kitty) //nolint:errcheck
		return
	case prefix == 0 && inter == '>' && final == 'u':
		c.kittyPush(unpackFirst(params, 0))
		return
	case prefix == 0 && inter == '<' && final == 'u':
		c.kittyPop(unpackFirst(params, 1))
		return
	case prefix == 0 && inter == '=' && final == 'u':
		c.kittySet(unpackFirst(params, 0), unpackNth(params, 1, 1))
		return
	// The shared parser packs '>'/'<'/'=' as the PREFIX (not the
	// intermediate) for CSI … u: CSI > u arrives as prefix='>'.
	case prefix == '>' && inter == 0 && final == 'u':
		c.kittyPush(unpackFirst(params, 0))
		return
	case prefix == '<' && inter == 0 && final == 'u':
		c.kittyPop(unpackFirst(params, 1))
		return
	case prefix == '=' && inter == 0 && final == 'u':
		c.kittySet(unpackFirst(params, 0), unpackNth(params, 1, 1))
		return
	case prefix == 0 && inter == ' ' && final == 'q':
		c.setCursorStyle(unpackFirst(params, 1))
		return
	case prefix == '>' && inter == 0 && final == 'c':
		n, _, _ := params.Param(0, 0)
		if n != 0 {
			return
		}
		fmt.Fprintf(c.resp, "%s", ansi.SecondaryDeviceAttributes(1, 10, 0)) //nolint:errcheck
		return
	case prefix == '?' && inter == 0 && final == 'n':
		n, _, ok := params.Param(0, 1)
		if !ok || n != 6 {
			return
		}
		a := c.active()
		fmt.Fprintf(c.resp, "\x1b[?%d;%d;0R", a.cursorY+1, a.cursorX+1) //nolint:errcheck
		return
	case prefix == 0 && inter == '$' && final == 'p':
		n, _, _ := params.Param(0, 0)
		fmt.Fprintf(c.resp, "\x1b[%d;0$y", n) //nolint:errcheck
		return
	case prefix == '?' && inter == '$' && final == 'p':
		n, _, _ := params.Param(0, 0)
		fmt.Fprintf(c.resp, "\x1b[?%d;0$y", n) //nolint:errcheck
		return
	}
	if prefix == '?' {
		c.dispatchPrivate(ps, final)
		return
	}
	if prefix != 0 || inter != 0 {
		// Intermediated / prefixed forms outside the query contract
		// (DECSLRM, XTWINOPS, locator, graphics): accepted, no grid
		// effect in the parity corpus. SCOSC 's' with no ?69 margin is
		// handled below via the bare-final switch; 's' never arrives
		// here with a prefix.
		return
	}
	c.dispatchBare(final, ps, params)
}

// dispatchPrivate executes CSI ? … final (DEC private modes), including
// the origin-mode flag (?6) and autowrap (?7) the corpus exercises.
func (c *vtCandidate) dispatchPrivate(ps []int, final byte) {
	if final == 'n' { // DECXCPR ?6n is handled in handleCsi; nothing else lands here.
		return
	}
	set := final == 'h'
	if final != 'h' && final != 'l' {
		return
	}
	for _, p := range ps {
		if p < 0 {
			continue
		}
		// ?2048 (in-band resize) is suppressed like the control: its reply
		// path cannot deadlock the feeder, and suppression preserves every
		// other batched mode.
		if p == 2048 {
			continue
		}
		switch p {
		case 1:
			c.appCursorKeys = set
		case 6:
			c.originMode = set
		case 7:
			c.autowrap = set
		case 9:
			c.mouseX10 = set
		case 25:
			c.active().hidden = !set
		case 47:
			if set && !c.onAlt {
				c.alt = c.newScreen()
				c.onAlt = true
			} else if !set && c.onAlt {
				c.onAlt = false
			}
			c.pendingWrap = false
		case 1000:
			c.mouseNormal = set
		case 1001:
			c.mouseHighlight = set
		case 1002:
			c.mouseButton = set
		case 1003:
			c.mouseAny = set
		case 1004:
			c.focusEvent = set
		case 1005:
			c.mouseUTF8 = set
		case 1006:
			c.mouseSGR = set
		case 1047:
			if set && !c.onAlt {
				c.alt = c.newScreen()
				c.onAlt = true
			} else if !set && c.onAlt {
				c.onAlt = false
			}
			c.pendingWrap = false
		case 1048:
			a := c.active()
			if set {
				a.savedX, a.savedY = a.cursorX, a.cursorY
				a.savedValid = true
			}
		case 1049:
			if set && !c.onAlt {
				c.primary.savedX, c.primary.savedY = c.primary.cursorX, c.primary.cursorY
				c.primary.savedValid = true
				c.alt = c.newScreen()
				c.onAlt = true
				c.alt.cursorX, c.alt.cursorY = 0, 0
			} else if !set && c.onAlt {
				c.onAlt = false
			}
			c.pendingWrap = false
		case 2004:
			c.bracketedPaste = set
		case 2026, 2027, 2031:
			// Accepted-and-ignored: synchronized output, grapheme
			// clustering, and color-scheme reports have no snapshot
			// effect in the parity corpus.
		}
	}
}

// setANSI applies bare CSI h/l (ANSI modes); the parity corpus observes no
// snapshot effect from these, so they are accepted silently.
func (c *vtCandidate) setANSI(ps []int, set bool) {
	_ = ps
	_ = set
}

// dispatchBare executes a no-prefix no-intermediate CSI final.
func (c *vtCandidate) dispatchBare(final byte, ps []int, params ansi.Params) {
	p0 := func(def int) int { return paramAtN(ps, 0, def) }
	switch final {
	case 'A':
		c.moveCursor(0, -p0(1))
	case 'B', 'e':
		c.moveCursor(0, p0(1))
	case 'C', 'a':
		c.moveCursor(p0(1), 0)
	case 'D':
		c.moveCursor(-p0(1), 0)
	case 'E':
		c.active().cursorX = 0
		c.moveCursor(0, p0(1))
	case 'F':
		c.active().cursorX = 0
		c.moveCursor(0, -p0(1))
	case 'G', '`':
		a := c.active()
		a.cursorX = clamp(p0(1)-1, 0, c.cols-1)
		c.pendingWrap = false
	case 'H', 'f':
		r, _, _ := params.Param(0, 1)
		col, _, _ := params.Param(1, 1)
		c.cup(r-1, col-1)
	case 'd':
		n, _, _ := params.Param(0, 1)
		a := c.active()
		if c.originMode {
			n = c.marginTop + n
		}
		a.cursorY = clamp(n-1, 0, c.rows-1)
		c.pendingWrap = false
	case 'J':
		c.eraseDisplay(p0(0))
	case 'K':
		c.eraseLine(p0(0))
	case 'X':
		c.eraseChars(p0(1))
	case 'L':
		c.insertLines(p0(1))
	case 'M':
		c.deleteLines(p0(1))
	case '@':
		c.insertBlanks(p0(1))
	case 'P':
		c.deleteChars(p0(1))
	case 'S':
		c.scrollUp(p0(1))
	case 'T':
		c.scrollDown(p0(1))
	case 'm':
		c.handleSGR(ps)
	case 'h', 'l':
		c.setANSI(ps, final == 'h')
	case 'r':
		c.setMargins(params)
	case 's':
		a := c.active()
		a.savedX, a.savedY = a.cursorX, a.cursorY
		a.savedValid = true
	case 'u':
		a := c.active()
		if a.savedValid {
			a.cursorX = clamp(a.savedX, 0, c.cols-1)
			a.cursorY = clamp(a.savedY, 0, c.rows-1)
		}
	case 'n':
		n, _, ok := params.Param(0, 1)
		if !ok || n == 0 {
			return
		}
		switch n {
		case 5:
			fmt.Fprintf(c.resp, "\x1b[0n") //nolint:errcheck
		case 6:
			a := c.active()
			fmt.Fprintf(c.resp, "\x1b[%d;%dR", a.cursorY+1, a.cursorX+1) //nolint:errcheck
		}
	case 'c':
		n, _, _ := params.Param(0, 0)
		if n != 0 {
			return
		}
		fmt.Fprintf(c.resp, "%s", ansi.PrimaryDeviceAttributes(62, 1, 6, 22)) //nolint:errcheck
	case 'b':
		c.repeatChars(p0(1))
	case 'g':
		v, _, _ := params.Param(0, 0)
		switch v {
		case 0:
			a := c.active()
			if a.cursorX < len(c.tabs) {
				c.tabs[a.cursorX] = false
			}
		case 3:
			for i := range c.tabs {
				c.tabs[i] = false
			}
		}
	case 'I':
		n, _, _ := params.Param(0, 1)
		for k := 0; k < n; k++ {
			c.tabForward()
		}
	case 'Z':
		n, _, _ := params.Param(0, 1)
		for k := 0; k < n; k++ {
			c.tabBackward()
		}
	}
}

func paramAtN(ps []int, idx, def int) int {
	if idx < len(ps) && ps[idx] >= 0 {
		return ps[idx]
	}
	return def
}

func unpackParams(params ansi.Params) []int {
	var out []int
	for i := range params {
		out = append(out, params[i].Param(-1))
	}
	return out
}

func unpackFirst(params ansi.Params, def int) int {
	n, _, _ := params.Param(0, def)
	return n
}

func unpackNth(params ansi.Params, idx, def int) int {
	n, _, _ := params.Param(idx, def)
	return n
}

func (c *vtCandidate) kittyPush(v int) {
	c.kittyStack = append(c.kittyStack, c.kitty)
	if len(c.kittyStack) > 8 {
		c.kittyStack = c.kittyStack[len(c.kittyStack)-8:]
	}
	if v < 0 || v > 31 {
		v = 0
	}
	c.kitty = uint8(v) //nolint:gosec
}

func (c *vtCandidate) kittyPop(n int) {
	if n < 1 {
		n = 1
	}
	// Pop restores the most recent pushed value: the stack top IS the
	// pre-push state, so popping assigns the top and removes it. Popping
	// past the stack floor yields 0 (disabled).
	for i := 0; i < n; i++ {
		if len(c.kittyStack) == 0 {
			c.kitty = 0
			return
		}
		c.kitty = c.kittyStack[len(c.kittyStack)-1]
		c.kittyStack = c.kittyStack[:len(c.kittyStack)-1]
	}
}

func (c *vtCandidate) kittySet(v, mode int) {
	if v < 0 || v > 31 {
		v = 0
	}
	switch mode {
	case 2:
		c.kitty |= uint8(v) //nolint:gosec
	case 3:
		c.kitty &^= uint8(v) //nolint:gosec
	default:
		c.kitty = uint8(v) //nolint:gosec
	}
}

// setCursorStyle applies DECSCUSR (CSI … SP q): n in 0..6.
func (c *vtCandidate) setCursorStyle(n int) {
	a := c.active()
	if n <= 0 {
		n = 1
	}
	blink := n == 0 || n%2 == 1
	style := n / 2
	if !blink {
		style--
	}
	switch style {
	case 1:
		a.cursorStyle = vt.CursorUnderline
	case 2:
		a.cursorStyle = vt.CursorBar
	default:
		a.cursorStyle = vt.CursorBlock
	}
}

func (c *vtCandidate) setMargins(params ansi.Params) {
	top, _, _ := params.Param(0, 1)
	if top < 1 {
		top = 1
	}
	bottom, _ := c.parser.Param(1, c.rows)
	if bottom < 1 {
		bottom = c.rows
	}
	if top >= bottom {
		return
	}
	c.marginTop, c.marginBot = top-1, bottom-1
	a := c.active()
	if c.originMode {
		a.cursorY = clamp(c.marginTop, 0, c.rows-1)
	} else {
		a.cursorY = clamp(0, 0, c.rows-1)
	}
	a.cursorX = 0
	c.pendingWrap = false
}

func (c *vtCandidate) tabForward() {
	a := c.active()
	c.pendingWrap = false
	for x := a.cursorX + 1; x < c.cols; x++ {
		if c.tabs[x] {
			a.cursorX = x
			return
		}
	}
	a.cursorX = c.cols - 1
}

func (c *vtCandidate) tabBackward() {
	a := c.active()
	c.pendingWrap = false
	for x := a.cursorX - 1; x >= 0; x-- {
		if c.tabs[x] {
			a.cursorX = x
			return
		}
	}
	a.cursorX = 0
}

func (c *vtCandidate) repeatChars(n int) {
	if c.lastChar == 0 || n < 1 {
		return
	}
	for k := 0; k < n; k++ {
		c.handlePrint(c.lastChar)
	}
}

func (c *vtCandidate) handleDcs(cmd ansi.Cmd, params ansi.Params, data []byte) {
	c.flushGrapheme()
	_ = params
	if cmd.Prefix() == 0 && cmd.Intermediate() == '$' && cmd.Final() == 'q' {
		var reply string
		switch string(data) {
		case "m":
			reply = "\x1bP1$r0m\x1b\\"
		case "r":
			reply = "\x1bP1$r1;" + strconv.Itoa(c.rows) + "r\x1b\\"
		default:
			reply = "\x1bP0$r\x1b\\"
		}
		io.WriteString(c.resp, reply) //nolint:errcheck,gosec
		return
	}
	// Sixel/graphics/unknown: consumed, no grid effect per spike scope.
}

func (c *vtCandidate) handleOsc(cmd int, data []byte) {
	c.flushGrapheme()
	if cmd == 10 || cmd == 11 || cmd == 12 {
		if isOSCQuery(data) {
			var cur color.Color
			switch cmd {
			case 10:
				cur = c.fgDefault
			case 11:
				cur = c.bgDefault
			case 12:
				cur = c.cursorDefault
			}
			io.WriteString(c.resp, oscColorReport(cmd, cur)) //nolint:errcheck,gosec
			return
		}
		// Color set: "…;color-spec".
		semi := bytes.IndexByte(data, ';')
		if semi >= 0 {
			if parsed, ok := parseOSCColor(data[semi+1:]); ok {
				switch cmd {
				case 10:
					c.fgDefault = parsed
				case 11:
					c.bgDefault = parsed
				case 12:
					c.cursorDefault = parsed
				}
			}
		}
		return
	}
	// Titles (0/1/2), CWD (7), hyperlinks (8): no grid effect.
}

// ---- text & cursor ----------------------------------------------------------

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (c *vtCandidate) currentFg() color.Color {
	if c.penFgSet {
		return c.penFg
	}
	return nil
}

func (c *vtCandidate) currentBg() color.Color {
	if c.penBgSet {
		return c.penBg
	}
	return nil
}

func (c *vtCandidate) putControl(b byte) {
	a := c.active()
	switch b {
	case 0x07: // BEL: no grid effect
	case 0x08: // BS
		c.pendingWrap = false
		if a.cursorX > 0 {
			a.cursorX--
		}
	case 0x09: // HT
		c.pendingWrap = false
		nx := a.cursorX + 1
		for nx < c.cols && !c.tabs[nx] {
			nx++
		}
		if nx >= c.cols {
			nx = c.cols - 1
		}
		a.cursorX = nx
	case 0x0a, 0x0b, 0x0c: // LF/VT/FF
		c.pendingWrap = false
		c.linefeedLocked()
	case 0x0d: // CR
		c.pendingWrap = false
		a.cursorX = 0
	case 0x00, 0x7f: // NUL/DEL: ignored
	}
}

func (c *vtCandidate) moveCursor(dx, dy int) {
	a := c.active()
	c.pendingWrap = false
	a.cursorX = clamp(a.cursorX+dx, 0, c.cols-1)
	if c.originMode {
		a.cursorY = clamp(a.cursorY+dy, c.marginTop, c.marginBot)
	} else {
		a.cursorY = clamp(a.cursorY+dy, 0, c.rows-1)
	}
}

func (c *vtCandidate) cup(row, col int) {
	a := c.active()
	c.pendingWrap = false
	if c.originMode {
		row += c.marginTop
		a.cursorY = clamp(row, c.marginTop, c.marginBot)
	} else {
		a.cursorY = clamp(row, 0, c.rows-1)
	}
	a.cursorX = clamp(col, 0, c.cols-1)
}

// index moves down, scrolling the region when the cursor is at its bottom.
func (c *vtCandidate) index() { c.linefeedLocked() }

// linefeedLocked moves the cursor down, scrolling the region at its
// bottom edge. LF/VT/FF preserve the column (VT semantics); NEL homes it
// at the call site, so no flag is needed here.
func (c *vtCandidate) linefeedLocked() {
	a := c.active()
	switch {
	case a.cursorY == c.marginBot:
		c.scrollUpRegion(1)
	case a.cursorY < c.rows-1:
		a.cursorY++
	default:
		c.scrollUpRegion(1)
	}
}

func (c *vtCandidate) reverseIndex() {
	a := c.active()
	c.pendingWrap = false
	if a.cursorY == c.marginTop {
		c.scrollDownRegion(1)
	} else if a.cursorY > 0 {
		a.cursorY--
	}
}

func blankCandidate() vtCandidateCell { return vtCandidateCell{content: "", width: 1} }

// scrollUpRegion scrolls [marginTop, marginBot] up n lines. Like the
// control (Screen.ScrollUp defers to DeleteLine from the region top), the
// top REGION line enters the scrollback tail whenever the active screen is
// the primary -- even for a sub-region scroll, and even when the line is
// blank (stored as an empty line, matching Scrollback.Push trimming).
// trimTailLine mirrors Scrollback.Push trimming: trailing blank cells are
// dropped, so an all-blank line is stored (and serialized) as an empty row.
// The serializer pads short rows back to width with blanks, so this is a
// storage shape, not a content loss.
func trimTailLine(line []vtCandidateCell) []vtCandidateCell {
	last := -1
	for i, cc := range line {
		if cc.content != "" && cc.content != " " {
			last = i
		}
	}
	if last < 0 {
		return nil
	}
	return line[:last+1]
}

// pushTail appends a scrollback line honoring the tail cap. A zero cap
// keeps no history; a negative cap (unset) is treated as unbounded, though
// the seam always supplies an explicit cap via setScrollbackSize.
func (c *vtCandidate) pushTail(line []vtCandidateCell) {
	if c.scrollback == 0 {
		return
	}
	c.tail = append(c.tail, line)
	for c.scrollback > 0 && len(c.tail) > c.scrollback {
		c.tail = c.tail[1:]
	}
}

func (c *vtCandidate) scrollUpRegion(n int) {
	a := c.active()
	if n <= 0 {
		return
	}
	for k := 0; k < n; k++ {
		if !c.onAlt {
			line := make([]vtCandidateCell, c.cols)
			copy(line, a.cells[c.marginTop*c.cols:(c.marginTop+1)*c.cols])
			c.pushTail(trimTailLine(line))
		}
		for y := c.marginTop; y < c.marginBot; y++ {
			copy(a.cells[y*c.cols:(y+1)*c.cols], a.cells[(y+1)*c.cols:(y+2)*c.cols])
		}
		for x := 0; x < c.cols; x++ {
			a.cells[c.marginBot*c.cols+x] = blankCandidate()
		}
	}
}

func (c *vtCandidate) scrollDownRegion(n int) {
	a := c.active()
	if n <= 0 {
		return
	}
	for k := 0; k < n; k++ {
		for y := c.marginBot; y > c.marginTop; y-- {
			copy(a.cells[y*c.cols:(y+1)*c.cols], a.cells[(y-1)*c.cols:y*c.cols])
		}
		for x := 0; x < c.cols; x++ {
			a.cells[c.marginTop*c.cols+x] = blankCandidate()
		}
	}
}

func (c *vtCandidate) scrollUp(n int)   { c.scrollUpRegion(clamp(n, 1, c.rows)) }
func (c *vtCandidate) scrollDown(n int) { c.scrollDownRegion(clamp(n, 1, c.rows)) }

func (c *vtCandidate) eraseDisplay(mode int) {
	a := c.active()
	c.pendingWrap = false
	switch mode {
	case 0: // cursor → end
		for x := a.cursorX; x < c.cols; x++ {
			a.cells[a.cursorY*c.cols+x] = blankCandidate()
		}
		for y := a.cursorY + 1; y < c.rows; y++ {
			for x := 0; x < c.cols; x++ {
				a.cells[y*c.cols+x] = blankCandidate()
			}
		}
	case 1: // start → cursor
		for y := 0; y < a.cursorY; y++ {
			for x := 0; x < c.cols; x++ {
				a.cells[y*c.cols+x] = blankCandidate()
			}
		}
		for x := 0; x <= a.cursorX && x < c.cols; x++ {
			a.cells[a.cursorY*c.cols+x] = blankCandidate()
		}
	case 2: // erase screen: non-empty grid lines enter scrollback first
		if !c.onAlt {
			for y := 0; y < c.rows; y++ {
				empty := true
				for x := 0; x < c.cols; x++ {
					if cc := a.cells[y*c.cols+x]; cc.content != "" && cc.content != " " {
						empty = false
						break
					}
				}
				if !empty {
					line := make([]vtCandidateCell, c.cols)
					copy(line, a.cells[y*c.cols:(y+1)*c.cols])
					c.pushTail(line)
				}
			}
		}
		for i := range a.cells {
			a.cells[i] = blankCandidate()
		}
	case 3: // erase display incl. scrollback
		for i := range a.cells {
			a.cells[i] = blankCandidate()
		}
		if !c.onAlt {
			c.tail = nil
		}
	}
}

func (c *vtCandidate) eraseLine(mode int) {
	a := c.active()
	c.pendingWrap = false
	switch mode {
	case 0:
		for x := a.cursorX; x < c.cols; x++ {
			a.cells[a.cursorY*c.cols+x] = blankCandidate()
		}
	case 1:
		for x := 0; x <= a.cursorX && x < c.cols; x++ {
			a.cells[a.cursorY*c.cols+x] = blankCandidate()
		}
	case 2:
		for x := 0; x < c.cols; x++ {
			a.cells[a.cursorY*c.cols+x] = blankCandidate()
		}
	}
}

func (c *vtCandidate) eraseChars(n int) {
	a := c.active()
	n = clamp(n, 1, c.cols-a.cursorX)
	for x := a.cursorX; x < a.cursorX+n && x < c.cols; x++ {
		a.cells[a.cursorY*c.cols+x] = blankCandidate()
	}
}

func (c *vtCandidate) insertLines(n int) {
	a := c.active()
	if a.cursorY < c.marginTop || a.cursorY > c.marginBot {
		return
	}
	n = clamp(n, 1, c.marginBot-a.cursorY+1)
	for k := 0; k < n; k++ {
		for y := c.marginBot; y > a.cursorY; y-- {
			copy(a.cells[y*c.cols:(y+1)*c.cols], a.cells[(y-1)*c.cols:y*c.cols])
		}
		for x := 0; x < c.cols; x++ {
			a.cells[a.cursorY*c.cols+x] = blankCandidate()
		}
	}
}

func (c *vtCandidate) deleteLines(n int) {
	a := c.active()
	if a.cursorY < c.marginTop || a.cursorY > c.marginBot {
		return
	}
	n = clamp(n, 1, c.marginBot-a.cursorY+1)
	// Scrollback capture matches the control: only when the cursor sits at
	// the top of a full-width region.
	if !c.onAlt && a.cursorY == c.marginTop {
		for k := 0; k < n; k++ {
			line := make([]vtCandidateCell, c.cols)
			copy(line, a.cells[(c.marginTop+k)*c.cols:(c.marginTop+k+1)*c.cols])
			c.pushTail(line)
		}
	}
	for k := 0; k < n; k++ {
		for y := a.cursorY; y < c.marginBot; y++ {
			copy(a.cells[y*c.cols:(y+1)*c.cols], a.cells[(y+1)*c.cols:(y+2)*c.cols])
		}
		for x := 0; x < c.cols; x++ {
			a.cells[c.marginBot*c.cols+x] = blankCandidate()
		}
	}
}

func (c *vtCandidate) insertBlanks(n int) {
	a := c.active()
	n = clamp(n, 1, c.cols-a.cursorX)
	y := a.cursorY
	for x := c.cols - 1; x >= a.cursorX+n; x-- {
		a.cells[y*c.cols+x] = a.cells[y*c.cols+x-n]
	}
	for x := a.cursorX; x < a.cursorX+n && x < c.cols; x++ {
		a.cells[y*c.cols+x] = blankCandidate()
	}
}

func (c *vtCandidate) deleteChars(n int) {
	a := c.active()
	n = clamp(n, 1, c.cols-a.cursorX)
	y := a.cursorY
	for x := a.cursorX; x+n < c.cols; x++ {
		a.cells[y*c.cols+x] = a.cells[y*c.cols+x+n]
	}
	for x := c.cols - n; x < c.cols; x++ {
		a.cells[y*c.cols+x] = blankCandidate()
	}
}

// handleSGR applies SGR attributes to the pen.
func (c *vtCandidate) handleSGR(ps []int) {
	if len(ps) == 0 {
		c.resetPen()
		return
	}
	for i := 0; i < len(ps); i++ {
		p := ps[i]
		if p < 0 {
			p = 0
		}
		switch {
		case p == 0:
			c.resetPen()
		case p == 1:
			c.penAttrs |= uv.AttrBold
		case p == 2:
			c.penAttrs |= uv.AttrFaint
		case p == 3:
			c.penAttrs |= uv.AttrItalic
		case p == 4:
			c.penUnderline = uv.UnderlineSingle
		case p == 7:
			c.penAttrs |= uv.AttrReverse
		case p == 9:
			c.penAttrs |= uv.AttrStrikethrough
		case p == 21:
			c.penUnderline = uv.UnderlineDouble
		case p == 22:
			c.penAttrs &^= uv.AttrBold | uv.AttrFaint
		case p == 23:
			c.penAttrs &^= uv.AttrItalic
		case p == 24:
			c.penUnderline = uv.UnderlineNone
		case p == 27:
			c.penAttrs &^= uv.AttrReverse
		case p == 29:
			c.penAttrs &^= uv.AttrStrikethrough
		case p >= 30 && p <= 37:
			c.penFg = ansi.BasicColor(p - 30)
			c.penFgSet = true
		case p == 38:
			if col, ni, ok := parseExtColor(ps, i); ok {
				c.penFg, c.penFgSet, i = col, true, ni
			}
		case p == 39:
			c.penFg, c.penFgSet = nil, false
		case p >= 40 && p <= 47:
			c.penBg = ansi.BasicColor(p - 40)
			c.penBgSet = true
		case p == 48:
			if col, ni, ok := parseExtColor(ps, i); ok {
				c.penBg, c.penBgSet, i = col, true, ni
			}
		case p == 49:
			c.penBg, c.penBgSet = nil, false
		case p >= 90 && p <= 97:
			c.penFg = ansi.BasicColor(p - 90 + 8)
			c.penFgSet = true
		case p >= 100 && p <= 107:
			c.penBg = ansi.BasicColor(p - 100 + 8)
			c.penBgSet = true
		}
	}
}

func (c *vtCandidate) resetPen() {
	c.penFg, c.penBg, c.penFgSet, c.penBgSet = nil, nil, false, false
	c.penAttrs = 0
	c.penUnderline = uv.UnderlineNone
}

func parseOSCColor(arg []byte) (color.Color, bool) {
	s := string(arg)
	rest := s
	if len(rest) > 4 && rest[:4] == "rgb:" {
		rest = rest[4:]
	}
	parts := splitSlash(rest)
	if len(parts) != 3 {
		return nil, false
	}
	hexVal := func(p string) (uint8, bool) {
		if len(p) == 0 || len(p) > 4 {
			return 0, false
		}
		v := 0
		for i := 0; i < len(p); i++ {
			d := p[i]
			var n int
			switch {
			case d >= '0' && d <= '9':
				n = int(d - '0')
			case d >= 'a' && d <= 'f':
				n = int(d-'a') + 10
			case d >= 'A' && d <= 'F':
				n = int(d-'A') + 10
			default:
				return 0, false
			}
			v = v*16 + n
		}
		switch len(p) {
		case 1:
			return uint8(v * 17), true //nolint:gosec
		case 2:
			return uint8(v), true //nolint:gosec
		case 3, 4:
			shift := (len(p) - 2) * 4
			return uint8(v >> shift), true //nolint:gosec
		}
		return 0, false
	}
	r, ok1 := hexVal(parts[0])
	g, ok2 := hexVal(parts[1])
	b, ok3 := hexVal(parts[2])
	if !ok1 || !ok2 || !ok3 {
		return nil, false
	}
	return color.RGBA{R: r, G: g, B: b, A: 0xff}, true
}

func splitSlash(s string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(s[i])
	}
	out = append(out, cur)
	return out
}

func parseExtColor(ps []int, i int) (color.Color, int, bool) {
	if i+1 >= len(ps) {
		return nil, i, false
	}
	switch ps[i+1] {
	case 5:
		if i+2 >= len(ps) || ps[i+2] < 0 || ps[i+2] > 255 {
			return nil, i, false
		}
		return ansi.IndexedColor(ps[i+2]), i + 2, true //nolint:gosec
	case 2:
		if i+4 >= len(ps) {
			return nil, i, false
		}
		r, g, b := ps[i+2], ps[i+3], ps[i+4]
		if r < 0 || r > 255 || g < 0 || g > 255 || b < 0 || b > 255 {
			return nil, i, false
		}
		return color.RGBA{R: uint8(r), G: uint8(g), B: uint8(b), A: 0xff}, i + 4, true //nolint:gosec
	}
	return nil, i, false
}
