package sanitize

import (
	"bytes"
	"strings"
	"testing"
)

// xrng is a tiny deterministic xorshift64 PRNG used to drive reproducible
// chunk-boundary selection in the streaming tests. A hand-rolled generator
// (rather than math/rand) keeps the chunking fully reproducible while avoiding a
// weak-RNG dependency; it is never used for anything security-sensitive.
type xrng struct{ s uint64 }

func newXRNG(seed uint64) *xrng {
	if seed == 0 {
		seed = 0x9E3779B97F4A7C15
	}
	return &xrng{s: seed}
}

func (r *xrng) next() uint64 {
	r.s ^= r.s << 13
	r.s ^= r.s >> 7
	r.s ^= r.s << 17
	return r.s
}

// intn returns a pseudo-random int in [0, n). The random word is masked to 31
// bits (which always fits an int) before the modulo, so no wide conversion is
// needed.
func (r *xrng) intn(n int) int {
	v := int(r.next() & 0x7FFFFFFF)
	return v % n
}

// contiguous is a convenience: sanitize a whole input with a fresh Sanitizer.
func contiguous(in []byte, opts ...Options) []byte {
	var s *Sanitizer
	if len(opts) > 0 {
		s = NewWithOptions(opts[0])
	} else {
		s = New()
	}
	return s.Write(in)
}

// TestCorpusContiguous verifies every shared fixture maps Input -> ExpectedOutput
// under the reference defaults when delivered in one Write.
func TestCorpusContiguous(t *testing.T) {
	entries, err := ConformanceCorpus()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("empty conformance corpus")
	}
	for _, e := range entries {
		t.Run(e.Name, func(t *testing.T) {
			in, err := e.InputBytes()
			if err != nil {
				t.Fatalf("decode input: %v", err)
			}
			want, err := e.ExpectedOutputBytes()
			if err != nil {
				t.Fatalf("decode expected: %v", err)
			}
			got := contiguous(in)
			if !bytes.Equal(got, want) {
				t.Fatalf("disposition %s / row %s\n got=%q\nwant=%q", e.Disposition, e.SpecRow, got, want)
			}
		})
	}
}

// TestCorpusSplitEveryOffset is the frozen conformance requirement (§9): for
// EVERY fixture, a two-way split at EVERY interior byte offset MUST yield the
// same disposition as contiguous delivery. No sampling.
func TestCorpusSplitEveryOffset(t *testing.T) {
	entries, err := ConformanceCorpus()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		e := e
		t.Run(e.Name, func(t *testing.T) {
			in, _ := e.InputBytes()
			want, _ := e.ExpectedOutputBytes()
			for off := 1; off < len(in); off++ {
				s := New()
				var got []byte
				got = append(got, s.Write(in[:off])...)
				got = append(got, s.Write(in[off:])...)
				if !bytes.Equal(got, want) {
					t.Fatalf("split at offset %d: got=%q want=%q", off, got, want)
				}
			}
		})
	}
}

// TestCorpusRandomChunking is the frozen random multi-way chunking pass: each
// fixture fed in random-sized chunks MUST equal contiguous delivery.
func TestCorpusRandomChunking(t *testing.T) {
	entries, err := ConformanceCorpus()
	if err != nil {
		t.Fatal(err)
	}
	rng := newXRNG(0xC0FFEE)
	for _, e := range entries {
		in, _ := e.InputBytes()
		want, _ := e.ExpectedOutputBytes()
		for trial := 0; trial < 64; trial++ {
			s := New()
			var got []byte
			for pos := 0; pos < len(in); {
				n := 1 + rng.intn(7)
				if pos+n > len(in) {
					n = len(in) - pos
				}
				got = append(got, s.Write(in[pos:pos+n])...)
				pos += n
			}
			if len(in) == 0 {
				got = s.Write(nil)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s trial %d: chunked=%q want=%q", e.Name, trial, got, want)
			}
		}
	}
}

// TestCorpusByteWise feeds every fixture one byte at a time (the maximal split)
// and checks equality with the contiguous result — the tightest streaming case.
func TestCorpusByteWise(t *testing.T) {
	entries, _ := ConformanceCorpus()
	for _, e := range entries {
		in, _ := e.InputBytes()
		want, _ := e.ExpectedOutputBytes()
		s := New()
		var got []byte
		for i := range in {
			got = append(got, s.Write(in[i:i+1])...)
		}
		if len(in) == 0 {
			got = s.Write(nil)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s byte-wise: got=%q want=%q", e.Name, got, want)
		}
	}
}

// TestCorpusIdempotent asserts re-sanitizing any fixture's output with a fresh
// Sanitizer is the identity — the output already contains no forbidden sequence.
func TestCorpusIdempotent(t *testing.T) {
	entries, _ := ConformanceCorpus()
	for _, e := range entries {
		want, _ := e.ExpectedOutputBytes()
		again := contiguous(want)
		if !bytes.Equal(again, want) {
			t.Fatalf("%s: not idempotent: %q -> %q", e.Name, want, again)
		}
	}
}

// TestDispositions is a focused table over the §9 rows expressed directly (a
// second, human-readable spelling of the corpus intent).
func TestDispositions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"pass_text", "hello world", "hello world"},
		{"pass_tab_newline", "a\tb\nc", "a\tb\nc"},
		{"strip_bel", "x\x07y", "xy"},
		{"strip_multiple_bel", "\x07\x07a\x07", "a"},
		{"pass_sgr", "\x1b[0;1;4;31m", "\x1b[0;1;4;31m"},
		{"pass_cup", "\x1b[5;10H", "\x1b[5;10H"},
		{"pass_ed_el", "\x1b[2J\x1b[K", "\x1b[2J\x1b[K"},
		{"pass_scroll_region", "\x1b[2;23r", "\x1b[2;23r"},
		{"pass_altscreen", "\x1b[?1049h", "\x1b[?1049h"},
		{"pass_bracketed_paste", "\x1b[?2004h", "\x1b[?2004h"},
		{"pass_mouse", "\x1b[?1006h\x1b[?1000h", "\x1b[?1006h\x1b[?1000h"},
		{"strip_osc52", "\x1b]52;c;QQ==\x07", ""},
		{"pass_osc8", "\x1b]8;;https://x\x1b\\t\x1b]8;;\x1b\\", "\x1b]8;;https://x\x1b\\t\x1b]8;;\x1b\\"},
		{"strip_osc_title", "\x1b]2;t\x07", ""},
		{"pass_osc_color_set", "\x1b]4;1;rgb:00/00/00\x07", "\x1b]4;1;rgb:00/00/00\x07"},
		{"strip_osc_color_query", "\x1b]11;?\x07", ""},
		{"strip_osc7", "\x1b]7;file:///x\x07", ""},
		{"strip_osc1337", "\x1b]1337;x\x07", ""},
		{"strip_cpr", "\x1b[6n", ""},
		{"strip_da", "\x1b[c", ""},
		{"strip_da_secondary", "\x1b[>c", ""},
		{"strip_dsr5", "\x1b[5n", ""},
		{"strip_window_t", "\x1b[18t", ""},
		{"strip_title_stack", "\x1b[22;2t\x1b[23;2t", ""},
		{"strip_decudk", "\x1b\x50|17/48\x1b\\", ""},
		{"strip_decrqss", "\x1b\x50$qm\x1b\\", ""},
		{"pass_sixel", "\x1b\x50q#1~\x1b\\", "\x1b\x50q#1~\x1b\\"},
		{"strip_apc", "\x1b_x\x1b\\", ""},
		{"strip_pm", "\x1b^x\x1b\\", ""},
		{"strip_sos", "\x1bXx\x1b\\", ""},
		{"strip_esc_decid", "\x1bZ", ""},
		{"pass_decsc_decrc", "\x1b7\x1b8", "\x1b7\x1b8"},
		{"pass_ris", "\x1bc", "\x1bc"},
		{"pass_charset", "\x1b(B\x1b)0", "\x1b(B\x1b)0"},
		{"strip_del", "a\x7fb", "ab"},
		{"strip_nul", "a\x00b", "ab"},
		{"strip_stray_c1", "a\x90\x9cb", "ab"}, // lone C1 DCS-introducer with immediate C1 ST, then stray
		{"pass_c1_csi", "\x9b1m", "\x9b1m"},
		{"strip_c1_osc52", "\x9d52;c;QQ==\x9c", ""},
		{"strip_decrqm", "\x1b[?2004$p", ""},
		{"strip_xtversion", "\x1b[>0q", ""},
		{"pass_decscusr", "\x1b[4 q", "\x1b[4 q"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(contiguous([]byte(tc.in)))
			if got != tc.want {
				t.Fatalf("in=%q got=%q want=%q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDanglingIntroducerHeld verifies a dangling OSC introducer at end of input
// is held (never emitted) and only resolves when the terminator arrives later.
func TestDanglingIntroducerHeld(t *testing.T) {
	s := New()
	// Feed a partial OSC 52; nothing should be emitted yet.
	if got := s.Write([]byte("before\x1b]52;c;")); string(got) != "before" {
		t.Fatalf("held: got %q, want %q", got, "before")
	}
	// Feed the rest; the whole OSC 52 is stripped, trailing text passes.
	if got := s.Write([]byte("QUJD\x07after")); string(got) != "after" {
		t.Fatalf("resume: got %q, want %q", got, "after")
	}
}

// TestDanglingSplitOSC8Pass verifies a PASS-disposition string held across a
// boundary is emitted verbatim once terminated.
func TestDanglingSplitOSC8Pass(t *testing.T) {
	s := New()
	got := s.Write([]byte("\x1b]8;;https://ex"))
	got = append(got, s.Write([]byte("ample.com\x1b\\X"))...)
	want := "\x1b]8;;https://example.com\x1b\\X"
	if string(got) != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestHoldMaxOverflowStrip checks the sanitizerHoldMaxBytes disposition: an
// over-cap held string is stripped whole and the parser resyncs on the
// terminator, then normal text passes again.
func TestHoldMaxOverflowStrip(t *testing.T) {
	s := NewWithOptions(Options{HoldMaxBytes: 16})
	// OSC 8 (would pass) but the URL blows the cap -> stripped whole.
	in := "\x1b]8;;" + strings.Repeat("a", 64) + "\x1b\\tail"
	got := string(s.Write([]byte(in)))
	if got != "tail" {
		t.Fatalf("overflow: got %q want %q", got, "tail")
	}
	// After resync the sanitizer is back in ground and passes normally.
	if got := string(s.Write([]byte("ok"))); got != "ok" {
		t.Fatalf("post-resync: got %q", got)
	}
}

// TestHoldMaxOverflowNoTerminator: an over-cap dangling string with no
// terminator ever is never flushed.
func TestHoldMaxOverflowNoTerminator(t *testing.T) {
	s := NewWithOptions(Options{HoldMaxBytes: 32})
	got := string(s.Write([]byte("\x1b]0;" + strings.Repeat("Z", 500))))
	if got != "" {
		t.Fatalf("expected no output, got %q", got)
	}
}

// TestSixelUnderAndOverCap checks the Sixel size cap: under-cap passes,
// over-cap is stripped whole.
func TestSixelUnderAndOverCap(t *testing.T) {
	under := "\x1b\x50q" + strings.Repeat("~", 100) + "\x1b\\"
	if got := string(contiguous([]byte(under), Options{SixelMaxBytes: 4096})); got != under {
		t.Fatalf("under-cap sixel should pass, got %q", got)
	}
	over := "\x1b\x50q" + strings.Repeat("~", 4096) + "\x1b\\next"
	got := string(contiguous([]byte(over), Options{SixelMaxBytes: 128}))
	if got != "next" {
		t.Fatalf("over-cap sixel should be stripped, got %q", got)
	}
}

// TestStripHyperlinksOption verifies the OSC 8 defense-in-depth hook.
func TestStripHyperlinksOption(t *testing.T) {
	in := "\x1b]8;;https://ex\x1b\\link\x1b]8;;\x1b\\"
	if got := string(contiguous([]byte(in), Options{StripHyperlinks: true})); got != "link" {
		t.Fatalf("StripHyperlinks: got %q want %q", got, "link")
	}
	// Default passes it through.
	if got := string(contiguous([]byte(in))); got != in {
		t.Fatalf("default OSC8 should pass, got %q", got)
	}
}

// TestOnTitleCallback verifies OSC 0/1/2 neutralization strips the sequence but
// surfaces the UI-safe title via the callback.
func TestOnTitleCallback(t *testing.T) {
	var titles []string
	s := NewWithOptions(Options{OnTitle: func(tt string) { titles = append(titles, tt) }})
	// A title carrying a non-terminating control byte (SOH 0x01); UIString must
	// strip it. (BEL would terminate the OSC, so it is not used here.)
	got := string(s.Write([]byte("\x1b]0;Build\x01OK\x07rest")))
	if got != "rest" {
		t.Fatalf("title should be stripped from stream, got %q", got)
	}
	if len(titles) != 1 || titles[0] != "BuildOK" {
		t.Fatalf("title callback: got %#v", titles)
	}
}

// TestOnTitleAllThree checks OSC 0, 1, and 2 all fire the callback.
func TestOnTitleAllThree(t *testing.T) {
	for _, ps := range []string{"0", "1", "2"} {
		var got string
		s := NewWithOptions(Options{OnTitle: func(tt string) { got = tt }})
		s.Write([]byte("\x1b]" + ps + ";hello\x07"))
		if got != "hello" {
			t.Fatalf("OSC %s: title=%q", ps, got)
		}
	}
}

// TestResetReuse verifies Reset clears held state so the Sanitizer can serve a
// fresh leg.
func TestResetReuse(t *testing.T) {
	s := New()
	s.Write([]byte("\x1b]52;c;")) // leave it mid-OSC
	s.Reset()
	if got := string(s.Write([]byte("QUJD\x07hello"))); got != "QUJD\x07hello" {
		// After Reset the leftover 'QUJD...' is plain ground text (BEL stripped).
		if got != "QUJDhello" {
			t.Fatalf("after reset: got %q", got)
		}
	}
}

// TestC1Terminators verifies BEL terminates OSC while ST terminates DCS/APC, and
// that a C1 ST (0x9C) terminates any string kind.
func TestC1Terminators(t *testing.T) {
	// BEL does NOT terminate a DCS; it is body. The DCS runs to ST.
	got := string(contiguous([]byte("\x1b\x50q~\x07~\x1b\\Z")))
	if got != "\x1b\x50q~\x07~\x1b\\Z" {
		t.Fatalf("BEL inside sixel should be body: got %q", got)
	}
}

// TestEmptyAndNil confirms empty inputs are safe and produce no output.
func TestEmptyAndNil(t *testing.T) {
	s := New()
	if got := s.Write(nil); len(got) != 0 {
		t.Fatalf("nil: got %q", got)
	}
	if got := s.Write([]byte{}); len(got) != 0 {
		t.Fatalf("empty: got %q", got)
	}
}

// TestNoAliasing confirms the returned slice does not alias the input.
func TestNoAliasing(t *testing.T) {
	in := []byte("hello")
	out := New().Write(in)
	if len(out) > 0 && &out[0] == &in[0] {
		t.Fatal("output aliases input")
	}
}

// TestUTF8Passthrough verifies multibyte UTF-8 (including continuation bytes in
// the C1 range) passes intact and is not misparsed as controls.
func TestUTF8Passthrough(t *testing.T) {
	cases := []string{
		"café",
		"日本語",
		"emoji 👍🏽 mix",
		"x\xd9\x9by",     // U+065B: continuation byte 0x9B
		"a\xe2\x80\x8bb", // ZWSP U+200B
	}
	for _, c := range cases {
		if got := string(contiguous([]byte(c))); got != c {
			t.Fatalf("utf8 %q -> %q", c, got)
		}
	}
}

// TestInvalidUTF8Dropped verifies invalid UTF-8 is dropped (not passed) so the
// output is well-formed and the filter stays idempotent. It also covers the
// held-partial-rune-at-EOF case.
func TestInvalidUTF8Dropped(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\xffb", "ab"},                     // invalid lead 0xFF dropped
		{"a\xc0\x80b", "ab"},                 // overlong / invalid lead 0xC0 dropped, 0x80 lone cont dropped
		{"a\x80b", "ab"},                     // lone continuation dropped
		{"a\xe8\x10b", "ab"},                 // truncated 3-byte lead + non-continuation
		{"caf\xc3", "caf"},                   // valid lead truncated at EOF -> held, never flushed
		{"x\xe2\x80\x8by", "x\xe2\x80\x8by"}, // valid ZWSP passes intact
	}
	for _, c := range cases {
		if got := string(contiguous([]byte(c.in))); got != c.want {
			t.Fatalf("invalid utf8 %q -> %q, want %q", c.in, got, c.want)
		}
	}
	// A valid lead held across a Write boundary completes on the next Write.
	s := New()
	got := s.Write([]byte("x\xc3"))
	got = append(got, s.Write([]byte("\xa9y"))...) // 0xC3 0xA9 = é
	if string(got) != "x\xc3\xa9y" {
		t.Fatalf("split multibyte: got %q", got)
	}
}

// TestMalformedCSIStripped ensures an oversized/malformed CSI does not leak.
func TestMalformedCSIStripped(t *testing.T) {
	// Very long parameter run with no final byte, then a final.
	in := "\x1b[" + strings.Repeat("1;", 400) + "m" + "tail"
	got := string(contiguous([]byte(in)))
	if got != "tail" {
		t.Fatalf("oversized CSI should be stripped: got %q", got)
	}
}

// TestEscAbortInString verifies an ESC that is not part of ST aborts the current
// string and starts a new sequence.
func TestEscAbortInString(t *testing.T) {
	// OSC 8 (pass) aborted mid-way by ESC [ 0 m (a new SGR that passes).
	in := "\x1b]8;;http://x\x1b[0m"
	got := string(contiguous([]byte(in)))
	// The aborted OSC is dropped; the SGR passes.
	if got != "\x1b[0m" {
		t.Fatalf("esc-abort: got %q want %q", got, "\x1b[0m")
	}
}

// TestCANabort verifies CAN/SUB abort an in-flight sequence.
func TestCANabort(t *testing.T) {
	if got := string(contiguous([]byte("\x1b]52;c\x18tail"))); got != "tail" {
		t.Fatalf("CAN abort OSC: got %q", got)
	}
	if got := string(contiguous([]byte("\x1b[31\x1amX"))); got != "mX" {
		// SUB aborts the CSI mid-params; 'mX' is then ground text.
		t.Fatalf("SUB abort CSI: got %q", got)
	}
}

// TestEscIntTransitions exercises the nF-escape (ESC + intermediates) state:
// pass on final, ESC restart, CAN abort, and oversized-run strip.
func TestEscIntTransitions(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b#8", "\x1b#8"},      // DECALN — pass
		{"\x1b(\x1b7", "\x1b7"},   // ESC ( aborted by ESC restart -> DECSC passes
		{"\x1b(\x18X", "X"},       // ESC ( aborted by CAN -> X ground text
		{"\x1b " + "F", "\x1b F"}, // S7C1T (ESC SP F) — pass
		{"\x1b(\x01Y", "Y"},       // unexpected C0 aborts nF, Y re-dispatched
	}
	for _, c := range cases {
		if got := string(contiguous([]byte(c.in))); got != c.want {
			t.Fatalf("escInt %q -> %q, want %q", c.in, got, c.want)
		}
	}
	// Oversized intermediate run with no final -> stripped.
	over := "\x1b(" + strings.Repeat(" ", 300) + "Bx"
	if got := string(contiguous([]byte(over))); got != "x" {
		t.Fatalf("oversized nF: got %q", got)
	}
}

// TestDCSTransitions exercises DCS header aborts, restarts, and overflow.
func TestDCSTransitions(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b\x50\x1b7", "\x1b7"},           // DCS aborted by ESC restart -> DECSC passes
		{"\x1b\x50\x18Z", "Z"},               // DCS aborted by CAN
		{"\x1b\x50q~\x9c", "\x1b\x50q~\x9c"}, // Sixel terminated by C1 ST -> pass
		{"\x1b\x90\x9c", ""},                 // empty C1 DCS terminated by C1 ST -> strip
		{"\x1b\x50\x07~\x1b\\", ""},          // BEL in DCS header is malformed -> strip to ST
	}
	for _, c := range cases {
		if got := string(contiguous([]byte(c.in))); got != c.want {
			t.Fatalf("dcs %q -> %q, want %q", c.in, got, c.want)
		}
	}
	// DCS header exceeding holdMax with no final -> strip.
	s := NewWithOptions(Options{HoldMaxBytes: 16})
	over := "\x1b\x50" + strings.Repeat("1", 64) + "q~\x1b\\tail"
	if got := string(s.Write([]byte(over))); got != "tail" {
		t.Fatalf("oversized DCS header: got %q", got)
	}
}

// TestCSIIgnoreTransitions exercises the malformed-CSI ignore state exits.
func TestCSIIgnoreTransitions(t *testing.T) {
	// Oversized CSI aborted by ESC starting a new (passing) sequence.
	over := "\x1b[" + strings.Repeat("1;", 400) + "\x1b7"
	if got := string(contiguous([]byte(over))); got != "\x1b7" {
		t.Fatalf("csiIgnore ESC restart: got %q", got)
	}
	// Oversized CSI aborted by CAN.
	over2 := "\x1b[" + strings.Repeat("1;", 400) + "\x18Q"
	if got := string(contiguous([]byte(over2))); got != "Q" {
		t.Fatalf("csiIgnore CAN: got %q", got)
	}
	// Unexpected C0 inside a CSI -> malformed -> stripped to the final byte.
	if got := string(contiguous([]byte("\x1b[3\x011mX"))); got != "X" {
		t.Fatalf("csi C0 malformed: got %q", got)
	}
}

// TestNonNumericOSCStripped verifies a non-numeric OSC (old letter forms) and an
// absurd numeric OSC default to strip.
func TestNonNumericOSCStripped(t *testing.T) {
	cases := []string{
		"\x1b]l window\x07", // non-numeric OSC l -> strip
		"\x1b]999999;x\x07", // > 5 digit OSC number -> not known -> strip
		"\x1b];empty\x07",   // empty leading number -> strip
	}
	for _, in := range cases {
		if got := string(contiguous([]byte(in))); got != "" {
			t.Fatalf("non-numeric OSC %q -> %q, want empty", in, got)
		}
	}
}

// TestCorpusCoverageAllRows asserts each expected §9 row appears in the corpus.
func TestCorpusCoverageAllRows(t *testing.T) {
	entries, _ := ConformanceCorpus()
	rows := map[string]bool{}
	for _, e := range entries {
		rows[e.SpecRow] = true
	}
	required := []string{
		"printable-c0-formatting", "bel", "sgr", "cursor-erase-scroll",
		"private-modes", "osc-52", "osc-8", "osc-title", "osc-color-set",
		"osc-color-query", "osc-7-9-777-1337", "dsr-da-reports", "window-manip",
		"dcs-decudk-decrqss", "dcs-sixel", "apc-pm",
	}
	for _, r := range required {
		if !rows[r] {
			t.Errorf("§9 row %q not covered by any corpus fixture", r)
		}
	}
}
