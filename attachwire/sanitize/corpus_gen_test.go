package sanitize

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// fixture is a hand-authored conformance case. input and want are raw bytes
// (Go string literals carry the exact bytes via \x escapes). The generator
// verifies that the reference sanitizer maps input -> want before emitting the
// corpus, so the shared JSON is both hand-authored AND implementation-verified.
type fixture struct {
	name string
	desc string
	in   string
	want string
	disp string
	row  string
}

// corpusFixtures is the authoritative set: at least one per §9 table row (pass
// rows included) plus the required edge cases. Byte-exact; \x escapes are single
// bytes in a Go interpreted string literal.
func corpusFixtures() []fixture {
	bigOSC := "\x1b]0;" + strings.Repeat("A", 9000) // > DefaultHoldMaxBytes, no terminator

	return []fixture{
		// --- one per §9 table row -------------------------------------------
		{
			"printable_c0_formatting", "printable text plus the passed C0 formatting controls HT LF CR BS",
			"Hi\tthere\r\nline\bX", "Hi\tthere\r\nline\bX", "pass", "printable-c0-formatting",
		},
		{
			"bel_neutralize", "standalone BEL is neutralized (byte removed); surrounding text passes",
			"a\x07b", "ab", "neutralize", "bel",
		},
		{
			"sgr_pass", "SGR color/attribute sequence passes (cosmetic, cell-grid bounded)",
			"\x1b[1;31m", "\x1b[1;31m", "pass", "sgr",
		},
		{
			"cursor_erase_scroll_pass", "cursor addressing, erase and scroll-region CSI ops pass",
			"\x1b[2J\x1b[10;5H\x1b[K\x1b[1;24r", "\x1b[2J\x1b[10;5H\x1b[K\x1b[1;24r", "pass", "cursor-erase-scroll",
		},
		{
			"private_modes_pass", "private-mode set/reset (alt-screen ?1049, bracketed paste ?2004, mouse ?1000, cursor ?25) pass",
			"\x1b[?1049h\x1b[?2004h\x1b[?1000h\x1b[?25l", "\x1b[?1049h\x1b[?2004h\x1b[?1000h\x1b[?25l", "pass", "private-modes",
		},
		{
			"osc52_strip", "OSC 52 clipboard write is stripped (paste-jacking / clipboard theft)",
			"\x1b]52;c;SGVsbG8=\x07", "", "strip", "osc-52",
		},
		{
			"osc8_hyperlink_display_only", "OSC 8 hyperlink passes through (display-only; viewer renders link, no auto-navigation)",
			"\x1b]8;;https://example.com\x1b\\link text\x1b]8;;\x1b\\", "\x1b]8;;https://example.com\x1b\\link text\x1b]8;;\x1b\\", "display-only", "osc-8",
		},
		{
			"osc_title_neutralize", "OSC 0 title-set is neutralized: stripped from the stream (viewer may show a capped chip)",
			"\x1b]0;my window title\x07", "", "neutralize", "osc-title",
		},
		{
			"osc_color_set_pass", "OSC 4/10/11/12 color SET forms pass (cosmetic)",
			"\x1b]11;#1e1e2e\x07\x1b]10;rgb:ff/ff/ff\x1b\\", "\x1b]11;#1e1e2e\x07\x1b]10;rgb:ff/ff/ff\x1b\\", "pass", "osc-color-set",
		},
		{
			"osc_color_query_strip", "OSC color/title QUERY forms (payload ?) are stripped (make the terminal reply on input)",
			"\x1b]10;?\x07\x1b]4;1;?\x07", "", "strip", "osc-color-query",
		},
		{
			"osc_7_9_777_1337_strip", "OSC 7 (cwd), 9 (notify), 777, 1337 (proprietary file/clipboard/exec) are stripped",
			"\x1b]7;file:///home/u\x07\x1b]9;notify body\x07\x1b]1337;File=name=x:AAAA\x07", "", "strip", "osc-7-9-777-1337",
		},
		{
			"dsr_da_reports_strip", "DSR/DA/status report triggers (CSI 6n, CSI c, CSI >c, CSI 5n, CSI ?6n) are stripped at the viewer",
			"\x1b[6n\x1b[c\x1b[>c\x1b[5n\x1b[?6n", "", "strip", "dsr-da-reports",
		},
		{
			"window_manip_strip", "xterm window-manipulation CSI ...t (resize/report and title stack 22/23t) are stripped",
			"\x1b[8;24;80t\x1b[14t\x1b[22;0t\x1b[23;0t", "", "strip", "window-manip",
		},
		{
			"dcs_decudk_decrqss_strip", "DCS DECUDK (programmable keys) and DECRQSS (setting reports) are stripped",
			"\x1b\x501;1|17/48\x1b\\\x1b\x50$qm\x1b\\", "", "strip", "dcs-decudk-decrqss",
		},
		{
			"dcs_sixel_pass", "DCS Sixel graphics pass (display-only image, size-capped)",
			"\x1b\x50q#0;2;0;0;0#1~~@@vv@@~~$-#1?\x1b\\", "\x1b\x50q#0;2;0;0;0#1~~@@vv@@~~$-#1?\x1b\\", "pass", "dcs-sixel",
		},
		{
			"apc_pm_strip", "APC (Kitty-graphics) and PM (multiplexer passthrough) strings are stripped",
			"\x1b_Gi=1,a=T;AAAA\x1b\\\x1b^tmux;passthrough\x1b\\", "", "strip", "apc-pm",
		},

		// --- required edge cases --------------------------------------------
		{
			"osc52_split_mid_introducer", "the spec's split example: OSC 52 delivered so a boundary can fall inside 'ESC ] 5 2 ;' — still stripped",
			"\x1b]52;c;QUJD\x07", "", "strip", "osc-52-split",
		},
		{
			"osc52_c1_introducer", "OSC 52 introduced by the 8-bit C1 OSC (0x9D) and terminated by C1 ST (0x9C) is stripped",
			"\x9d52;c;QUJD\x9c", "", "strip", "osc-52-c1",
		},
		{
			"osc_bel_vs_st_terminator", "a passed OSC (11 set) with BEL and with ST terminators — both terminate and pass verbatim",
			"\x1b]11;#000000\x07\x1b]11;#000000\x1b\\", "\x1b]11;#000000\x07\x1b]11;#000000\x1b\\", "pass", "osc-terminators",
		},
		{
			"dcs_decrqss_explicit", "DCS DECRQSS ($ q ... ST) setting-report request is stripped",
			"\x1b\x50$qr\x1b\\", "", "strip", "dcs-decrqss",
		},
		{
			"sixel_under_cap", "a small Sixel is under the size cap and passes",
			"\x1b\x50q#1~~\x1b\\", "\x1b\x50q#1~~\x1b\\", "pass", "dcs-sixel",
		},
		{
			"alt_screen_enter_exit", "alt-screen enter (?1049h) and exit (?1049l) both pass",
			"\x1b[?1049h\x1b[HTUI body\x1b[?1049l", "\x1b[?1049h\x1b[HTUI body\x1b[?1049l", "pass", "private-modes",
		},
		{
			"truecolor_sgr", "24-bit truecolor SGR passes",
			"\x1b[38;2;255;128;0mX\x1b[0m", "\x1b[38;2;255;128;0mX\x1b[0m", "pass", "sgr",
		},
		{
			"hold_max_dangling_osc", "a dangling OSC exceeding sanitizerHoldMaxBytes with no terminator is stripped at the cap, never flushed",
			bigOSC, "", "strip", "hold-max-overflow",
		},
		{
			"c1_csi_sgr_pass", "an SGR introduced by the 8-bit C1 CSI (0x9B) is classified and passed like ESC [",
			"\x9b1mX", "\x9b1mX", "pass", "c1-csi",
		},
		{
			"stray_c1_strip", "stray C1 controls (IND 0x84, lone ST 0x9C) are stripped; surrounding text passes",
			"a\x84\x9cb", "ab", "strip", "stray-c1",
		},
		{
			"decrqm_xtversion_strip", "DECRQM mode report (CSI ?…$p) and XTVERSION (CSI >q) reply triggers are stripped",
			"\x1b[?2004$p\x1b[>0q", "", "strip", "dsr-da-reports",
		},
		{
			"esc_decid_strip", "ESC Z (DECID) device-attributes reply trigger is stripped; text passes",
			"a\x1bZb", "ab", "strip", "dsr-da-reports",
		},
		{
			"escape_misc_pass", "DECSC/DECRC (ESC 7/8), charset designation (ESC ( B) and RIS (ESC c) pass",
			"\x1b7\x1b8\x1b(B\x1bc", "\x1b7\x1b8\x1b(B\x1bc", "pass", "escape-misc",
		},
		{
			"decscusr_set_pass", "DECSCUSR cursor-style set (CSI Ps SP q) passes — distinguished from XTVERSION",
			"\x1b[2 q", "\x1b[2 q", "pass", "sgr",
		},
		{
			"sos_strip", "an SOS string is stripped; following text passes",
			"\x1bXstart-of-string data\x1b\\ok", "ok", "strip", "apc-pm",
		},
		{
			"del_nul_strip", "DEL (0x7F) and NUL (0x00) are stripped; text passes",
			"a\x7fb\x00c", "abc", "strip", "printable-c0-formatting",
		},
		{
			"utf8_c1_in_continuation", "a multibyte rune whose UTF-8 continuation byte equals a C1 introducer (0x9B) is passed as text, not parsed as CSI",
			"x\xd9\x9by", "x\xd9\x9by", "pass", "printable-c0-formatting",
		},
		{
			"focus_decckm_pass", "focus reporting (?1004h) and application cursor keys DECCKM (?1h) private modes pass",
			"\x1b[?1004h\x1b[?1h", "\x1b[?1004h\x1b[?1h", "pass", "private-modes",
		},
		{
			"mixed_stream", "a realistic mixed stream: text + SGR pass, BEL + title + CPR stripped, CRLF passes",
			"user@host:~$ \x1b[32mok\x1b[0m\x07\x1b]0;title\x07\x1b[6n\r\n",
			"user@host:~$ \x1b[32mok\x1b[0m\r\n", "mixed", "mixed",
		},
	}
}

// TestGenerateCorpus regenerates testdata/corpus.json. It is skipped unless
// GEN_CORPUS is set, and it fails loudly if any hand-authored `want` disagrees
// with the reference sanitizer (contiguous OR split), so the emitted corpus is
// always consistent with this implementation.
func TestGenerateCorpus(t *testing.T) {
	if os.Getenv("GEN_CORPUS") == "" {
		t.Skip("set GEN_CORPUS=1 to regenerate testdata/corpus.json")
	}
	fx := corpusFixtures()
	seen := map[string]bool{}
	entries := make([]Entry, 0, len(fx))
	for _, f := range fx {
		if seen[f.name] {
			t.Fatalf("duplicate fixture name %q", f.name)
		}
		seen[f.name] = true

		got := string(New().Write([]byte(f.in)))
		if got != f.want {
			t.Fatalf("fixture %q: contiguous mismatch\n in=%q\nwant=%q\n got=%q", f.name, f.in, f.want, got)
		}
		// Verify a two-way split at every interior offset agrees with `want`.
		for off := 1; off < len(f.in); off++ {
			s := New()
			var b []byte
			b = append(b, s.Write([]byte(f.in)[:off])...)
			b = append(b, s.Write([]byte(f.in)[off:])...)
			if string(b) != f.want {
				t.Fatalf("fixture %q: split at %d mismatch: got %q want %q", f.name, off, b, f.want)
			}
		}
		entries = append(entries, Entry{
			Name:           f.name,
			Description:    f.desc,
			Input:          base64.StdEncoding.EncodeToString([]byte(f.in)),
			ExpectedOutput: base64.StdEncoding.EncodeToString([]byte(f.want)),
			Disposition:    f.disp,
			SpecRow:        f.row,
		})
	}

	buf, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile("testdata/corpus.json", buf, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d entries to testdata/corpus.json", len(entries))
}
