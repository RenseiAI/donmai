package sanitize

import "bytes"

// Named byte constants for the control characters the parser recognizes.
const (
	bel = 0x07 // BEL
	bs  = 0x08 // BS  (C0 formatting — passed)
	ht  = 0x09 // HT  (C0 formatting — passed)
	lf  = 0x0A // LF  (C0 formatting — passed)
	cr  = 0x0D // CR  (C0 formatting — passed)
	esc = 0x1B // ESC
	can = 0x18 // CAN — aborts an in-flight sequence
	sub = 0x1A // SUB — aborts an in-flight sequence
	del = 0x7F // DEL — stripped

	// C1 8-bit control introducers (§9: treated equivalently to their
	// ESC-prefixed forms).
	c1DCS = 0x90 // ESC P
	c1SOS = 0x98 // ESC X
	c1CSI = 0x9B // ESC [
	c1ST  = 0x9C // ESC \  (string terminator)
	c1OSC = 0x9D // ESC ]
	c1PM  = 0x9E // ESC ^
	c1APC = 0x9F // ESC _
)

// Configuration defaults. The hold cap is the spec's sanitizerHoldMaxBytes
// (§9, value v1-draft; existence frozen).
const (
	// DefaultHoldMaxBytes is the frozen-named sanitizerHoldMaxBytes: the sanity
	// cap on a dangling or oversized string-type escape sequence
	// (OSC/DCS/APC/PM/SOS). At the cap the entire held sequence is stripped and
	// the parser resynchronizes (§9). Value is v1-draft.
	DefaultHoldMaxBytes = 8192

	// DefaultSixelMaxBytes caps a passed Sixel DCS payload (§9: "pass,
	// size-capped"; the cap is bounded by backpressure, §11.2). A Sixel over
	// this size is stripped whole.
	DefaultSixelMaxBytes = 2 << 20 // 2 MiB

	// TitleChipMaxLen is the rune cap applied (via UIString) to a neutralized
	// OSC 0/1/2 title before it is offered to Options.OnTitle for an optional
	// session-title chip (§9: "length-capped, control-char-stripped").
	TitleChipMaxLen = 256

	// csiMaxBytes bounds a single CSI sequence (params + intermediates). A CSI
	// that grows past this without a final byte is malformed and stripped.
	csiMaxBytes = 256
)

// Options configures a Sanitizer. The zero value is valid and equivalent to the
// frozen reference defaults.
type Options struct {
	// HoldMaxBytes overrides DefaultHoldMaxBytes when > 0.
	HoldMaxBytes int
	// SixelMaxBytes overrides DefaultSixelMaxBytes when > 0.
	SixelMaxBytes int
	// StripHyperlinks, when true, strips OSC 8 hyperlink sequences instead of
	// passing them through. The frozen reference disposition for OSC 8 is
	// display-only — the sequence REACHES the viewer, which renders link text
	// and never auto-navigates (a viewer-UI duty, §9). This option is a
	// defense-in-depth hook for a deployment that prefers to drop hyperlinks
	// outright; it defaults to false (pass-through).
	StripHyperlinks bool
	// OnTitle, when non-nil, is invoked with the UI-safe title text each time an
	// OSC 0/1/2 title-set sequence is neutralized. The title has already been
	// passed through UIString(title, TitleChipMaxLen). The sequence itself is
	// still stripped from the byte stream; the callback lets a viewer OPTIONALLY
	// show a capped title chip without ever retitling its own window (§9).
	OnTitle func(title string)
}

// parser state.
type state uint8

const (
	stGround    state = iota // normal text
	stEsc                    // seen ESC
	stEscInt                 // ESC + intermediate byte(s) 0x20-0x2F
	stCSI                    // collecting a CSI sequence
	stCSIIgnore              // malformed/oversized CSI — swallow until final, drop
	stDCS                    // collecting a DCS header (params/intermediates/final)
	stStr                    // collecting an OSC/DCS-data/APC/PM/SOS string body
)

// strKind identifies the string-type sequence being collected in stStr, for
// terminator handling and classification.
type strKind uint8

const (
	kOSC strKind = iota
	kDCS         // DCS data phase (Sixel pass-through)
	kAPC
	kPM
	kSOS
)

// Sanitizer is a stateful, streaming §9 escape-sequence filter. Escape state
// carries across Write calls, so a single Sanitizer sanitizes one leg's byte
// stream frame-by-frame. It is NOT safe for concurrent use; use one per leg.
type Sanitizer struct {
	holdMax   int
	sixelMax  int
	stripLink bool
	onTitle   func(string)

	st       state
	pending  []byte  // buffered bytes of the current sequence (incl. introducer)
	drop     bool    // current string is being stripped: don't buffer or emit
	curCap   int     // buffer cap for the current string body
	oscBEL   bool    // current string accepts BEL as a terminator (OSC only)
	kind     strKind // string kind, for classification
	introLen int     // introducer length within pending (1 for C1, 2 for ESC X)
	sawEsc   bool    // inside a string: saw ESC, expecting '\' to complete ST
	utf8Rem  int     // UTF-8 continuation bytes still expected in ground
	utf8Buf  []byte  // partial multibyte UTF-8 rune held until complete
}

// New returns a Sanitizer with the frozen reference defaults.
func New() *Sanitizer { return NewWithOptions(Options{}) }

// NewWithOptions returns a Sanitizer configured by opts. Zero-valued fields take
// their documented defaults.
func NewWithOptions(opts Options) *Sanitizer {
	s := &Sanitizer{
		holdMax:   opts.HoldMaxBytes,
		sixelMax:  opts.SixelMaxBytes,
		stripLink: opts.StripHyperlinks,
		onTitle:   opts.OnTitle,
	}
	if s.holdMax <= 0 {
		s.holdMax = DefaultHoldMaxBytes
	}
	if s.sixelMax <= 0 {
		s.sixelMax = DefaultSixelMaxBytes
	}
	return s
}

// Reset returns the Sanitizer to ground state and discards any held partial
// sequence, so it can be reused for a fresh leg. Options are preserved.
func (s *Sanitizer) Reset() {
	s.st = stGround
	s.pending = s.pending[:0]
	s.drop = false
	s.oscBEL = false
	s.sawEsc = false
	s.utf8Rem = 0
	s.utf8Buf = s.utf8Buf[:0]
	s.introLen = 0
	s.curCap = 0
}

// Write feeds one chunk of host-produced terminal bytes and returns a freshly
// allocated slice of the sanitized bytes that are safe to render. Partial
// escape state carries over to the next Write; a dangling introducer at the end
// of p produces no output for it until a later Write completes or caps it.
//
// The returned slice is owned by the caller and never aliases p or any internal
// buffer.
func (s *Sanitizer) Write(p []byte) []byte {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); {
		if s.step(p[i], &out) {
			i++
		}
		// A false return means the state changed and the same byte must be
		// re-dispatched. Each such transition moves toward ground/esc (states
		// that always consume), so termination is guaranteed.
	}
	return out
}

// step processes one input byte. It appends any emitted bytes to *out and
// returns true if the byte was consumed, false if it must be re-dispatched in
// the new state.
func (s *Sanitizer) step(b byte, out *[]byte) bool {
	switch s.st {
	case stGround:
		return s.stepGround(b, out)
	case stEsc:
		return s.stepEsc(b, out)
	case stEscInt:
		return s.stepEscInt(b, out)
	case stCSI:
		return s.stepCSI(b, out)
	case stCSIIgnore:
		return s.stepCSIIgnore(b)
	case stDCS:
		return s.stepDCS(b, out)
	case stStr:
		return s.stepStr(b, out)
	default:
		s.st = stGround
		return true
	}
}

// --- ground -----------------------------------------------------------------

func (s *Sanitizer) stepGround(b byte, out *[]byte) bool {
	// Mid-UTF-8: buffer continuation bytes (0x80-0xBF) and emit the multibyte
	// rune only once it is COMPLETE. Buffering (rather than eager emission) does
	// two things: it keeps a C1 introducer value (0x90/0x9B/0x9D/…) that occurs
	// INSIDE a multibyte rune from being mistaken for a control, and it makes the
	// filter idempotent — an incomplete or invalid UTF-8 lead is never flushed on
	// its own, so it can never absorb an unrelated following byte on a re-scan.
	if s.utf8Rem > 0 {
		if b >= 0x80 && b <= 0xBF {
			s.utf8Buf = append(s.utf8Buf, b)
			s.utf8Rem--
			if s.utf8Rem == 0 {
				*out = append(*out, s.utf8Buf...)
				s.utf8Buf = s.utf8Buf[:0]
			}
			return true
		}
		// Invalid continuation: drop the incomplete rune and re-dispatch b.
		s.utf8Buf = s.utf8Buf[:0]
		s.utf8Rem = 0
		return false
	}

	switch {
	case b == esc:
		s.beginEsc()
		return true
	case b == ht, b == lf, b == cr, b == bs:
		*out = append(*out, b) // C0 formatting — pass (§9)
		return true
	case b < 0x20:
		return true // other C0 (incl. BEL): neutralize/strip
	case b == del:
		return true // DEL — strip
	case b <= 0x7E:
		*out = append(*out, b) // printable ASCII — pass
		return true
	}

	// b >= 0x80.
	switch b {
	case c1CSI:
		s.beginCSI(b)
		return true
	case c1OSC:
		s.beginStr(kOSC, b, true)
		return true
	case c1DCS:
		s.beginDCS(b)
		return true
	case c1APC:
		s.beginStr(kAPC, b, false)
		return true
	case c1PM:
		s.beginStr(kPM, b, false)
		return true
	case c1SOS:
		s.beginStr(kSOS, b, false)
		return true
	}
	if b <= 0x9F {
		return true // stray C1 control (IND/NEL/ST/…) — strip
	}

	// b >= 0xA0: a UTF-8 lead byte begins a buffered multibyte rune; every other
	// high byte (lone continuation 0xA0-0xBF, invalid lead 0xC0/0xC1/0xF5-0xFF)
	// is invalid UTF-8 and is dropped. Dropping — rather than passing a lone
	// invalid byte — keeps the output well-formed and the filter idempotent.
	switch {
	case b >= 0xC2 && b <= 0xDF:
		s.utf8Rem = 1
	case b >= 0xE0 && b <= 0xEF:
		s.utf8Rem = 2
	case b >= 0xF0 && b <= 0xF4:
		s.utf8Rem = 3
	default:
		return true // invalid lead / lone continuation — strip
	}
	s.utf8Buf = append(s.utf8Buf[:0], b)
	return true
}

// --- escape -----------------------------------------------------------------

func (s *Sanitizer) beginEsc() {
	s.st = stEsc
	s.pending = append(s.pending[:0], esc)
}

func (s *Sanitizer) stepEsc(b byte, out *[]byte) bool {
	switch b {
	case '[':
		s.pending = append(s.pending, b)
		s.st = stCSI
		s.introLen = len(s.pending)
		return true
	case ']':
		s.pending = append(s.pending, b)
		s.beginStrFromEsc(kOSC, true)
		return true
	case 'P':
		s.pending = append(s.pending, b)
		s.st = stDCS
		s.introLen = len(s.pending)
		return true
	case '_':
		s.beginStrFromEsc(kAPC, false)
		return true
	case '^':
		s.beginStrFromEsc(kPM, false)
		return true
	case 'X':
		s.beginStrFromEsc(kSOS, false)
		return true
	case '\\':
		s.st = stGround // lone ST outside any string — strip
		return true
	case esc:
		s.pending = append(s.pending[:0], esc) // restart escape
		return true
	case can, sub:
		s.st = stGround
		return true
	}
	if b >= 0x20 && b <= 0x2F { // intermediate → nF sequence
		s.pending = append(s.pending, b)
		s.st = stEscInt
		return true
	}
	if b >= 0x30 && b <= 0x7E { // final of a two-byte escape
		if b == 'Z' { // DECID — triggers a device-attributes reply; strip (§9)
			s.st = stGround
			return true
		}
		*out = append(*out, s.pending...)
		*out = append(*out, b)
		s.st = stGround
		return true
	}
	// Unexpected byte (C0/DEL/high): abort the escape and re-dispatch in ground.
	s.st = stGround
	return false
}

func (s *Sanitizer) stepEscInt(b byte, out *[]byte) bool {
	switch {
	case b >= 0x20 && b <= 0x2F:
		s.pending = append(s.pending, b)
		if len(s.pending) > csiMaxBytes {
			s.st = stCSIIgnore // malformed run — swallow to the final byte, drop
		}
		return true
	case b >= 0x30 && b <= 0x7E: // final: charset designation, DECALN, S7C1T… — pass
		*out = append(*out, s.pending...)
		*out = append(*out, b)
		s.st = stGround
		return true
	case b == esc:
		s.pending = append(s.pending[:0], esc)
		s.st = stEsc
		return true
	case b == can || b == sub:
		s.st = stGround
		return true
	}
	s.st = stGround
	return false
}

// --- CSI --------------------------------------------------------------------

func (s *Sanitizer) beginCSI(introducer byte) {
	s.st = stCSI
	s.pending = append(s.pending[:0], introducer)
	s.introLen = len(s.pending)
}

func (s *Sanitizer) stepCSI(b byte, out *[]byte) bool {
	switch {
	case b >= 0x30 && b <= 0x3F: // parameter bytes (incl. private prefixes ? > = <)
		s.pending = append(s.pending, b)
	case b >= 0x20 && b <= 0x2F: // intermediate bytes
		s.pending = append(s.pending, b)
	case b >= 0x40 && b <= 0x7E: // final byte — classify
		if csiPasses(s.pending[s.introLen:], b) {
			*out = append(*out, s.pending...)
			*out = append(*out, b)
		}
		s.st = stGround
		return true
	case b == esc:
		s.pending = append(s.pending[:0], esc)
		s.st = stEsc
		return true
	case b == can || b == sub:
		s.st = stGround
		return true
	default: // unexpected C0/DEL/high byte inside CSI — malformed
		s.st = stCSIIgnore
		return true
	}
	if len(s.pending) > csiMaxBytes {
		s.st = stCSIIgnore // oversized — swallow to the final and drop
	}
	return true
}

func (s *Sanitizer) stepCSIIgnore(b byte) bool {
	switch {
	case b >= 0x40 && b <= 0x7E: // final — drop the whole malformed CSI
		s.st = stGround
	case b == esc:
		s.pending = append(s.pending[:0], esc)
		s.st = stEsc
	case b == can || b == sub:
		s.st = stGround
	}
	return true
}

// csiPasses decides whether a CSI with the given parameter/intermediate content
// and final byte is passed. It strips exactly the reply-triggering and
// out-of-grid forms enumerated in §9 plus the reply closure required by the
// "when in doubt, a sequence that could emit input is stripped" default:
//
//   - final 'n' — DSR / CPR / status reports (CSI 5n, CSI 6n, CSI ?…n)
//   - final 'c' — device attributes (CSI c, CSI >c, CSI =c, CSI ?…c)
//   - final 't' — xterm window manipulation, all forms incl. title stack 22/23t
//   - final 'q' with a '>' private prefix — XTVERSION (replies with a DCS)
//   - final 'p' with a '$' intermediate — DECRQM mode report (replies)
//
// Everything else — SGR, cursor addressing/erase/scroll region, private
// mode set/reset (?1049, ?2004, ?1000-?1006, ?1004, ?25, DECCKM…), DECSCUSR
// cursor-style set — is a cell-grid operation and passes (§9 default).
func csiPasses(content []byte, final byte) bool {
	switch final {
	case 'n', 'c', 't':
		return false
	case 'q':
		if len(content) > 0 && content[0] == '>' {
			return false // XTVERSION
		}
	case 'p':
		if bytes.IndexByte(content, '$') >= 0 {
			return false // DECRQM
		}
	}
	return true
}

// --- DCS header -------------------------------------------------------------

func (s *Sanitizer) beginDCS(introducer byte) {
	s.st = stDCS
	s.pending = append(s.pending[:0], introducer)
	s.introLen = len(s.pending)
}

func (s *Sanitizer) stepDCS(b byte, _ *[]byte) bool {
	switch {
	case b >= 0x30 && b <= 0x3F: // parameter bytes
		s.pending = append(s.pending, b)
	case b >= 0x20 && b <= 0x2F: // intermediate bytes
		s.pending = append(s.pending, b)
	case b >= 0x40 && b <= 0x7E: // final byte — classify the DCS type
		s.pending = append(s.pending, b)
		s.classifyDCS(b)
		return true
	case b == esc:
		s.pending = append(s.pending[:0], esc)
		s.st = stEsc
		return true
	case b == c1ST: // 8-bit ST ends an empty/malformed DCS header — strip, resync
		s.st = stGround
		return true
	case b == can || b == sub:
		s.st = stGround
		return true
	default:
		s.dropStringUntilST() // malformed header — strip until ST
		return true
	}
	if len(s.pending) > s.holdMax {
		s.dropStringUntilST()
	}
	return true
}

// classifyDCS is called with the DCS final byte already appended to pending. It
// routes into the string-body phase either as a passed Sixel or as a stripped
// string (DECUDK, DECRQSS, or any unenumerated DCS — §9 default strip).
func (s *Sanitizer) classifyDCS(final byte) {
	header := s.pending[s.introLen:]
	hasDollar := bytes.IndexByte(header, '$') >= 0
	if final == 'q' && !hasDollar {
		// Sixel graphics: pass, size-capped. Keep buffering the body.
		s.st = stStr
		s.kind = kDCS
		s.drop = false
		s.oscBEL = false
		s.sawEsc = false
		s.curCap = s.sixelMax
		return
	}
	// DECUDK ('|'), DECRQSS ('$' 'q'), or anything else — strip.
	s.dropStringUntilST()
}

// --- string body (OSC / Sixel-DCS / APC / PM / SOS) -------------------------

// beginStr starts a string-body sequence introduced by a single C1 byte.
func (s *Sanitizer) beginStr(kind strKind, introducer byte, oscBEL bool) {
	s.pending = append(s.pending[:0], introducer)
	s.introLen = len(s.pending)
	s.enterStr(kind, oscBEL)
}

// beginStrFromEsc starts a string-body sequence whose introducer bytes are
// already in pending (ESC plus the second byte for OSC; ESC only for APC/PM/SOS,
// which are always stripped so their introducer need not be retained).
func (s *Sanitizer) beginStrFromEsc(kind strKind, oscBEL bool) {
	if kind == kOSC {
		s.introLen = len(s.pending) // pending already holds ESC ']'
	} else {
		s.introLen = 0
	}
	s.enterStr(kind, oscBEL)
}

func (s *Sanitizer) enterStr(kind strKind, oscBEL bool) {
	s.st = stStr
	s.kind = kind
	s.oscBEL = oscBEL
	s.sawEsc = false
	switch kind {
	case kOSC:
		s.drop = false
		s.curCap = s.holdMax
	default: // APC/PM/SOS — always stripped
		s.drop = true
		s.curCap = s.holdMax
	}
}

// dropStringUntilST switches the current sequence into strip-and-resync mode:
// buffered bytes are discarded and subsequent bytes are swallowed until the
// string terminator, then the parser returns to ground. Used for the HoldMax
// overflow disposition and for strip-classified DCS.
func (s *Sanitizer) dropStringUntilST() {
	s.st = stStr
	s.drop = true
	s.sawEsc = false
	s.pending = s.pending[:0]
	// oscBEL/kind retain whatever terminator acceptance the sequence had.
}

func (s *Sanitizer) stepStr(b byte, out *[]byte) bool {
	if s.sawEsc {
		s.sawEsc = false
		if b == '\\' { // ESC '\' = 7-bit ST — terminate
			s.finishStr(out, []byte{esc, '\\'})
			return true
		}
		// ESC not followed by '\' aborts the string and starts a new escape.
		s.st = stEsc
		s.pending = append(s.pending[:0], esc)
		return false // re-dispatch b in stEsc
	}

	switch {
	case b == esc:
		s.sawEsc = true
		return true
	case s.oscBEL && b == bel: // OSC BEL terminator
		s.finishStr(out, []byte{bel})
		return true
	case b == c1ST: // 8-bit ST terminator (any string kind)
		s.finishStr(out, []byte{c1ST})
		return true
	case b == can || b == sub: // abort — strip whatever we held
		s.st = stGround
		s.drop = false
		return true
	}

	// Body byte.
	if !s.drop {
		s.pending = append(s.pending, b)
		if len(s.pending) > s.curCap {
			s.dropStringUntilST() // over cap — strip whole and resync (§9)
		}
	}
	return true
}

// finishStr terminates the current string body. term is the terminator byte(s)
// consumed. On a pass disposition the original bytes plus the terminator are
// emitted verbatim; on strip/neutralize nothing is emitted.
func (s *Sanitizer) finishStr(out *[]byte, term []byte) {
	defer func() {
		s.st = stGround
		s.drop = false
	}()
	if s.drop {
		return // already stripping
	}
	switch s.kind {
	case kDCS: // Sixel — pass
		*out = append(*out, s.pending...)
		*out = append(*out, term...)
	case kOSC:
		s.finishOSC(out, term)
	default:
		// APC/PM/SOS never reach here with drop=false.
	}
}

func (s *Sanitizer) finishOSC(out *[]byte, term []byte) {
	content := s.pending[s.introLen:]
	disp, titleStart := classifyOSC(content, s.stripLink)
	switch disp {
	case oscPass:
		*out = append(*out, s.pending...)
		*out = append(*out, term...)
	case oscTitle:
		if s.onTitle != nil {
			title := ""
			if titleStart >= 0 && titleStart < len(content) {
				title = string(content[titleStart:])
			}
			s.onTitle(UIString(title, TitleChipMaxLen))
		}
		// neutralize: stripped from the stream regardless of the callback.
	case oscStrip:
		// stripped
	}
}

// OSC disposition results.
type oscDisp uint8

const (
	oscStrip oscDisp = iota
	oscPass
	oscTitle
)

// classifyOSC maps an OSC content body (the bytes between the introducer and the
// terminator, e.g. "52;c;<b64>") to a disposition per the §9 table. It returns
// the disposition and, for title sequences, the index within content where the
// title text begins (after the first ';'), or -1.
func classifyOSC(content []byte, stripHyperlinks bool) (oscDisp, int) {
	ps, nlen, ok := leadingOSCNumber(content)
	if !ok {
		return oscStrip, -1 // non-numeric or absurd OSC → default strip
	}
	switch ps {
	case 52: // clipboard — paste-jacking / theft
		return oscStrip, -1
	case 8: // hyperlink — display-only (reaches the viewer) unless hook set
		if stripHyperlinks {
			return oscStrip, -1
		}
		return oscPass, -1
	case 0, 1, 2: // title/icon set — neutralize, expose capped title chip
		start := -1
		if nlen < len(content) && content[nlen] == ';' {
			start = nlen + 1
		}
		return oscTitle, start
	case 4, 10, 11, 12: // palette/fg/bg/cursor color — set passes, query strips
		if bytes.IndexByte(content, '?') >= 0 {
			return oscStrip, -1 // query form makes the terminal reply on input
		}
		return oscPass, -1
	case 7, 9, 777, 1337: // cwd / notify / proprietary file-xfer-clipboard-exec
		return oscStrip, -1
	default:
		return oscStrip, -1 // unenumerated OSC string → default strip
	}
}

// leadingOSCNumber parses the leading run of ASCII digits as the OSC command
// number. A missing or implausibly long (> 5 digit) number is not a known OSC
// and reports ok=false.
func leadingOSCNumber(content []byte) (val, nlen int, ok bool) {
	i := 0
	for i < len(content) && content[i] >= '0' && content[i] <= '9' {
		i++
	}
	if i == 0 || i > 5 {
		return 0, i, false
	}
	v := 0
	for _, c := range content[:i] {
		v = v*10 + int(c-'0')
	}
	return v, i, true
}
