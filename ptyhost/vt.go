package ptyhost

import (
	"image/color"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"unsafe"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	vt "github.com/charmbracelet/x/vt"
)

// terminal is the small internal interface the Session feeds and snapshots.
// It exists so the concrete VT (github.com/charmbracelet/x/vt) is swappable
// behind this seam without touching Session logic.
type terminal interface {
	// write feeds raw PTY output bytes into the emulator. Query responses the
	// emulator would emit (CPR/DA/DSR/DECRQM/DECRQSS/OSC-color) are synthesized
	// synchronously and written to the response writer (the PTY master); they
	// never reach this method's caller.
	write(p []byte)
	// resize applies new geometry to the emulator grid.
	resize(cols, rows int)
	// setScrollbackSize bounds the scrollback tail (§12.1).
	setScrollbackSize(maxLines int)
	// raw materializes the current screen state for snapshot serialization.
	raw() vtRaw
}

// vtHost wraps *vt.Emulator to provide the snapshot-authority surface the host
// needs (spike verdict wrapper duties 1-7). It is NOT internally locked: the
// Session guarantees single-feeder discipline (§12) by only ever calling write /
// resize / raw under its own mutex (duty 6).
type vtHost struct {
	emu    *vt.Emulator
	resp   io.Writer // PTY master: query answers flow here synchronously (duty 2)
	logger *slog.Logger

	// modes tracked via SetCallbacks EnableMode/DisableMode (duty 1).
	m modeState

	// pendingFeed carries synthetic sequences to re-feed after the current
	// write returns (used to preserve non-?2048 modes when a ?2048 in-band-resize
	// enable is suppressed — see decPrivateSet). Draining it after write avoids
	// ANSI parser re-entrancy.
	pendingFeed []byte

	// Cached unsafe pointers into unexported emulator/screen state (duty 4). The
	// Emulator pointer and its embedded scrs array are stable for the emulator's
	// life, so these are computed once. They are the two documented upstream
	// accessor gaps:
	//   - atPhantom  -> upstream PendingWrap() bool   (deferred-wrap fidelity)
	//   - scrs[0].saved -> upstream SavedCursor() Cursor (?1049 restore point)
	atPhantom   *bool
	savedCursor *vt.Cursor
	primaryScr  *vt.Screen // &scrs[0]
	altScr      *vt.Screen // &scrs[1]
}

// modeState is the raw mode tracking accumulated from EnableMode/DisableMode.
type modeState struct {
	bracketedPaste bool // ?2004
	appCursorKeys  bool // ?1  DECCKM
	focusEvent     bool // ?1004
	mouseX10       bool // ?9
	mouseNormal    bool // ?1000
	mouseHighlight bool // ?1001
	mouseButton    bool // ?1002
	mouseAny       bool // ?1003
	mouseUTF8      bool // ?1005
	mouseSGR       bool // ?1006
}

// newVTHost builds the wrapper at the given geometry. resp is where synchronous
// query answers are written (the PTY master); pass io.Discard in VT-only tests.
func newVTHost(cols, rows, scrollback int, resp io.Writer, logger *slog.Logger) *vtHost {
	if resp == nil {
		resp = io.Discard
	}
	e := vt.NewEmulator(cols, rows)
	v := &vtHost{emu: e, resp: resp, logger: logger}

	// Duty 1: modes bitmap via callbacks.
	e.SetCallbacks(vt.Callbacks{
		EnableMode:  func(m ansi.Mode) { v.setMode(m, true) },
		DisableMode: func(m ansi.Mode) { v.setMode(m, false) },
	})

	// Duty 7: bound scrollback.
	e.SetScrollbackSize(scrollback)

	// Duty 4: cache the two unexported accessors and both screen pointers.
	v.bindReflection()

	// Duties 2 & 3: synchronous query responders write to the master and return
	// true so the emulator's default handlers (which write to its internal
	// synchronous response pipe, deadlocking a feed with no reader) never run.
	v.registerResponders()

	return v
}

func (v *vtHost) write(p []byte) {
	if len(p) == 0 {
		return
	}
	_, _ = v.emu.Write(p)
	// Drain any suppressed-mode re-feed queued by a handler during Write.
	for len(v.pendingFeed) > 0 {
		pf := v.pendingFeed
		v.pendingFeed = nil
		_, _ = v.emu.Write(pf)
	}
}

func (v *vtHost) resize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	v.emu.Resize(cols, rows)
	// Resize can itself emit an in-band-resize report to the response pipe when
	// ?2048 is active; because decPrivateSet suppresses ?2048, that mode is never
	// set here and Resize never writes to the pipe.
}

func (v *vtHost) setScrollbackSize(maxLines int) { v.emu.SetScrollbackSize(maxLines) }

// ---- reflection binding (duty 4) -------------------------------------------

func (v *vtHost) bindReflection() {
	ev := reflect.ValueOf(v.emu).Elem()
	if f := ev.FieldByName("atPhantom"); f.IsValid() && f.CanAddr() {
		v.atPhantom = (*bool)(unsafe.Pointer(f.UnsafeAddr())) //nolint:gosec
	}
	scrs := ev.FieldByName("scrs")
	if scrs.IsValid() && scrs.Len() >= 2 {
		v.primaryScr = (*vt.Screen)(unsafe.Pointer(scrs.Index(0).UnsafeAddr())) //nolint:gosec
		v.altScr = (*vt.Screen)(unsafe.Pointer(scrs.Index(1).UnsafeAddr()))     //nolint:gosec
		if sv := scrs.Index(0).FieldByName("saved"); sv.IsValid() && sv.CanAddr() {
			v.savedCursor = (*vt.Cursor)(unsafe.Pointer(sv.UnsafeAddr())) //nolint:gosec
		}
	}
}

func (v *vtHost) pendingWrap() bool {
	if v.atPhantom == nil {
		return false
	}
	return *v.atPhantom
}

// ---- modes (duty 1) --------------------------------------------------------

func (v *vtHost) setMode(m ansi.Mode, on bool) {
	dm, ok := m.(ansi.DECMode)
	if !ok {
		return
	}
	switch int(dm) {
	case 1:
		v.m.appCursorKeys = on
	case 9:
		v.m.mouseX10 = on
	case 1000:
		v.m.mouseNormal = on
	case 1001:
		v.m.mouseHighlight = on
	case 1002:
		v.m.mouseButton = on
	case 1003:
		v.m.mouseAny = on
	case 1004:
		v.m.focusEvent = on
	case 1005:
		v.m.mouseUTF8 = on
	case 1006:
		v.m.mouseSGR = on
	case 2004:
		v.m.bracketedPaste = on
	}
}

// modesByte packs the tracked modes into the §12.1 modes bitmap (pendingWrap is
// added by the caller from live emulator state).
func (v *vtHost) modesByte() (modes, mouseProto uint8) {
	if v.m.bracketedPaste {
		modes |= 0x01 // ModeBracketedPaste
	}
	if v.m.appCursorKeys {
		modes |= 0x02 // ModeAppCursorKeys
	}
	if v.m.focusEvent {
		modes |= 0x10 // ModeFocusEvent
	}
	track := v.mouseTrack()
	if track != 0 || v.m.mouseX10 {
		modes |= 0x08     // ModeMouseTracking
		enc := uint8(0x0) // X10
		if v.m.mouseSGR {
			enc = 0x2
		} else if v.m.mouseUTF8 {
			enc = 0x1
		}
		mouseProto = (enc << 4) | (track & 0x0F)
	}
	return modes, mouseProto
}

// mouseTrack picks the §12.1 low-nibble tracking code (?1000..?1003). ?9 (X10)
// has no low-nibble code, so it registers only the ModeMouseTracking bit.
func (v *vtHost) mouseTrack() uint8 {
	switch {
	case v.m.mouseAny:
		return 0x4 // ?1003
	case v.m.mouseButton:
		return 0x3 // ?1002
	case v.m.mouseHighlight:
		return 0x2 // ?1001
	case v.m.mouseNormal:
		return 0x1 // ?1000
	default:
		return 0x0
	}
}

// ---- responders (duties 2 & 3) ---------------------------------------------

func (v *vtHost) registerResponders() {
	e := v.emu

	// DSR / CPR (CSI n).
	e.RegisterCsiHandler('n', func(params ansi.Params) bool {
		n := paramAt(params, 0, 1)
		switch n {
		case 5: // operating status: always OK
			io.WriteString(v.resp, "\x1b[0n") //nolint:errcheck,gosec
			return true
		case 6: // Cursor Position Report
			p := e.CursorPosition()
			io.WriteString(v.resp, ansi.CursorPositionReport(p.Y+1, p.X+1)) //nolint:errcheck,gosec
			return true
		}
		return false
	})

	// DECXCPR (CSI ? n), n=6.
	e.RegisterCsiHandler(ansi.Command('?', 0, 'n'), func(params ansi.Params) bool {
		if paramAt(params, 0, 1) != 6 {
			return false
		}
		p := e.CursorPosition()
		io.WriteString(v.resp, ansi.ExtendedCursorPositionReport(p.Y+1, p.X+1, 0)) //nolint:errcheck,gosec
		return true
	})

	// DA1 (CSI c).
	e.RegisterCsiHandler('c', func(params ansi.Params) bool {
		if paramAt(params, 0, 0) != 0 {
			return false
		}
		io.WriteString(v.resp, ansi.PrimaryDeviceAttributes(62, 1, 6, 22)) //nolint:errcheck,gosec
		return true
	})

	// DA2 (CSI > c).
	e.RegisterCsiHandler(ansi.Command('>', 0, 'c'), func(params ansi.Params) bool {
		if paramAt(params, 0, 0) != 0 {
			return false
		}
		io.WriteString(v.resp, ansi.SecondaryDeviceAttributes(1, 10, 0)) //nolint:errcheck,gosec
		return true
	})

	// DECRQM (CSI $ p / CSI ? $ p): reply "mode not recognized". Tracking every
	// mode's live setting to answer precisely is deferred (named tech-debt); a
	// not-recognized reply is a valid, non-blocking answer that keeps a probing
	// TUI from hanging.
	decrqm := func(params ansi.Params) bool {
		io.WriteString(v.resp, "\x1b["+strconv.Itoa(paramAt(params, 0, 0))+";0$y") //nolint:errcheck,gosec
		return true
	}
	e.RegisterCsiHandler(ansi.Command(0, '$', 'p'), decrqm)
	e.RegisterCsiHandler(ansi.Command('?', '$', 'p'), decrqm)

	// OSC 10/11/12 color queries (…;? form). A set form (…;rgb:… ) returns false
	// so the emulator's default handler applies the color; a query is answered to
	// the master. Real TUIs probe OSC 11 (background) to detect a dark theme.
	oscColor := func(osc int, cur func() color.Color) vt.OscHandler {
		return func(data []byte) bool {
			if !isOSCQuery(data) {
				return false
			}
			io.WriteString(v.resp, oscColorReport(osc, cur())) //nolint:errcheck,gosec
			return true
		}
	}
	e.RegisterOscHandler(10, oscColor(10, e.ForegroundColor))
	e.RegisterOscHandler(11, oscColor(11, e.BackgroundColor))
	e.RegisterOscHandler(12, oscColor(12, e.CursorColor))

	// DECRQSS ($q DCS): the emulator ships no handler; answer common requests
	// (duty 3).
	e.RegisterDcsHandler(ansi.Command(0, '$', 'q'), func(_ ansi.Params, data []byte) bool {
		var reply string
		switch string(data) {
		case "m": // SGR
			reply = "\x1bP1$r0m\x1b\\"
		case "r": // DECSTBM full-screen region
			reply = "\x1bP1$r1;" + strconv.Itoa(e.Height()) + "r\x1b\\"
		default:
			reply = "\x1bP0$r\x1b\\" // invalid request
		}
		io.WriteString(v.resp, reply) //nolint:errcheck,gosec
		return true
	})

	// Suppress ?2048 (in-band resize) SET so its unavoidable pipe write cannot
	// deadlock the feeder; preserve any other modes batched in the same sequence
	// by re-feeding them.
	e.RegisterCsiHandler(ansi.Command('?', 0, 'h'), v.decPrivateSet)
}

// decPrivateSet intercepts `CSI ? … h`. It returns false (letting the default
// DEC set-mode handler run) unless mode 2048 is present. When 2048 is present it
// suppresses the whole sequence and re-feeds every OTHER mode individually so no
// fidelity is lost for the common case where 2048 is sent alone.
func (v *vtHost) decPrivateSet(params ansi.Params) bool {
	has2048 := false
	var others []int
	for _, p := range params {
		val := paramValue(p)
		if val == 2048 {
			has2048 = true
		} else if val >= 0 {
			others = append(others, val)
		}
	}
	if !has2048 {
		return false // no ?2048 → default handler is safe (no pipe write)
	}
	for _, m := range others {
		v.pendingFeed = append(v.pendingFeed, "\x1b[?"...)
		v.pendingFeed = append(v.pendingFeed, strconv.Itoa(m)...)
		v.pendingFeed = append(v.pendingFeed, 'h')
	}
	return true
}

// ---- snapshot materialization ----------------------------------------------

// vtRaw is the materialized VT state a snapshot is serialized from. Grids are
// cell slices in row-major order, exactly cols*rows each.
type vtRaw struct {
	cols, rows    int
	altActive     bool
	cursorX       int
	cursorY       int
	cursorVisible bool
	cursorShape   uint8 // attachwire CursorShape*
	pendingWrap   bool
	savedX        int
	savedY        int
	modes         uint8
	mouseProto    uint8
	primary       []*uv.Cell
	alt           []*uv.Cell // nil unless altActive
	scrollback    [][]*uv.Cell
}

func (v *vtHost) raw() vtRaw {
	e := v.emu
	cols, rows := e.Width(), e.Height()
	alt := e.IsAltScreen()
	pos := e.CursorPosition()

	r := vtRaw{
		cols:        cols,
		rows:        rows,
		altActive:   alt,
		cursorX:     pos.X,
		cursorY:     pos.Y,
		pendingWrap: v.pendingWrap(),
		primary:     readGrid(v.primaryScr, cols, rows),
	}

	// Cursor visibility + shape from the active screen's cursor.
	cur := v.activeScreen().Cursor()
	r.cursorVisible = !cur.Hidden
	r.cursorShape = cursorShape(cur.Style)

	r.modes, r.mouseProto = v.modesByte()
	if r.pendingWrap {
		r.modes |= 0x04 // ModePendingWrap
	}

	if alt {
		r.alt = readGrid(v.altScr, cols, rows)
		// Saved primary cursor = the ?1049 restore point.
		if v.savedCursor != nil {
			r.savedX, r.savedY = v.savedCursor.X, v.savedCursor.Y
		}
	} else {
		// Primary active: saved cursor equals the active cursor (§12.1).
		r.savedX, r.savedY = pos.X, pos.Y
	}

	// Scrollback tail comes from the primary screen only (alt buffers have no
	// scrollback, §12.1).
	if sb := v.primaryScr.Scrollback(); sb != nil {
		n := sb.Len()
		for y := 0; y < n; y++ {
			line := make([]*uv.Cell, 0, cols)
			for x := 0; x < cols; x++ {
				line = append(line, sb.CellAt(x, y))
			}
			r.scrollback = append(r.scrollback, line)
		}
	}
	return r
}

func (v *vtHost) activeScreen() *vt.Screen {
	if v.emu.IsAltScreen() {
		return v.altScr
	}
	return v.primaryScr
}

// readGrid materializes a screen's cells row-major (cols*rows).
func readGrid(scr *vt.Screen, cols, rows int) []*uv.Cell {
	if scr == nil {
		return make([]*uv.Cell, cols*rows)
	}
	out := make([]*uv.Cell, 0, cols*rows)
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			out = append(out, scr.CellAt(x, y))
		}
	}
	return out
}

// ---- small helpers ---------------------------------------------------------

// cursorShape maps a vt.CursorStyle to the §12.1 cursorShape byte.
func cursorShape(s vt.CursorStyle) uint8 {
	switch s {
	case vt.CursorBlock:
		return 0x01 // CursorShapeBlock
	case vt.CursorUnderline:
		return 0x02 // CursorShapeUnderline
	case vt.CursorBar:
		return 0x03 // CursorShapeBar
	default:
		return 0x00 // CursorShapeDefault
	}
}

func paramAt(params ansi.Params, idx, def int) int {
	n, _, _ := params.Param(idx, def)
	return n
}

func paramValue(p ansi.Param) int { return p.Param(-1) }

func isOSCQuery(data []byte) bool {
	// A color query is the "?" form, e.g. "11;?".
	for _, b := range data {
		if b == '?' {
			return true
		}
	}
	return false
}

// oscColorReport renders an xterm color report ESC ] <osc> ; rgb:RRRR/GGGG/BBBB ST.
func oscColorReport(osc int, c color.Color) string {
	if c == nil {
		c = color.Black
	}
	r, g, b, _ := c.RGBA() // 16-bit per channel, already scaled
	hex := func(v uint32) string {
		const digits = "0123456789abcdef"
		v &= 0xFFFF
		return string([]byte{
			digits[(v>>12)&0xF], digits[(v>>8)&0xF], digits[(v>>4)&0xF], digits[v&0xF],
		})
	}
	return "\x1b]" + strconv.Itoa(osc) + ";rgb:" + hex(r) + "/" + hex(g) + "/" + hex(b) + "\x1b\\"
}
