package vt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func checkpointTerminal(t *testing.T) *Emulator {
	t.Helper()
	e := NewEmulator(20, 8)
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func checkpointWrite(t *testing.T, e *Emulator, s string) {
	t.Helper()
	if _, err := e.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func checkpointBytes(t *testing.T, e *Emulator) []byte {
	t.Helper()
	b, err := e.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckpointContinuation(t *testing.T) {
	cases := []struct {
		name, history, suffix string
		resize                bool
	}{
		{"pen", "\x1b[31;44;1;3;4m", "X", false},
		{"saved", "\x1b[3;5H\x1b[31m\x1b7\x1b[H\x1b[34m", "\x1b8X", false},
		{"margin", "\x1b[2;4r\x1b[H", "\x1b[4;1H\nX", false},
		{"csi", "\x1b[3", "1mX", false},
		{"utf8", "\xe7\x95", "\x8cX", false},
		{"alternate", "primary\x1b[?1049h\x1b[31malt", "\x1b[?1049lX", false},
		{"alternate_active", "primary\x1b[?1049halt", "X", false},
		{"wrap", strings.Repeat("A", 20), "B", false},
		{"resize", strings.Repeat("A", 20), "B", true},
		{"tabs", "\x1b[3g\x1b[6G\x1bH\x1b[H", "\tX", false},
		{"charset", "\x1b(0", "lqk", false},
		{"locking_shift", "\x1b*0\x1bn", "qX", false},
		{"single_shift", "\x1b*0\x8e", "qX", false},
		{"origin", "\x1b[2;5r\x1b[?6h", "\x1b[2;1HX", false},
		{"insert", "ABCDE\x1b[H\x1b[4h", "X", false},
		{"repeat", "Z", "\x1b[3b", false},
		{"link", "\x1b]8;id=one;https://example.org\x1b\\", "X\x1b]8;;\x1b\\Y", false},
		{"osc_partial", "\x1b]4;1;rgb:ff/", "00/00\x1b\\\x1b[31mX", false},
		{"dcs_partial", "\x1bP1;2", "qpayload\x1b\\X", false},
		{"grapheme", "\x1b[?2027h👩\xe2", "\x80\x8d💻X", false},
		{"scrollback", strings.Repeat("line\r\n", 12), "next\r\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := checkpointTerminal(t), checkpointTerminal(t)
			checkpointWrite(t, a, tc.history)
			image := checkpointBytes(t, a)
			if err := b.RestoreCheckpoint(image); err != nil {
				t.Fatal(err)
			}
			if tc.resize {
				a.Resize(24, 10)
				b.Resize(24, 10)
			}
			checkpointWrite(t, a, tc.suffix)
			checkpointWrite(t, b, tc.suffix)
			if !bytes.Equal(checkpointBytes(t, a), checkpointBytes(t, b)) {
				t.Fatalf("continuation diverged: uninterrupted=%q restored=%q", a.Render(), b.Render())
			}
		})
	}
}

func TestCheckpointEverySplit(t *testing.T) {
	streams := []string{"\x1b[31;4:3m界👩‍💻X", "\x1b]8;id=x;https://example.org\x1b\\link", "\x1b(0lqk\x1b(Btext", "\x1b[?1049hhello\x1b[?1049lworld", "\x1bP1;2qabc\x1b\\X", "\x1b]4;1;rgb:ff/00/00\x1b\\\x1b[31mX"}
	for _, stream := range streams {
		for split := 0; split <= len(stream); split++ {
			a, b := checkpointTerminal(t), checkpointTerminal(t)
			checkpointWrite(t, a, stream[:split])
			if err := b.RestoreCheckpoint(checkpointBytes(t, a)); err != nil {
				t.Fatalf("split%d: %v", split, err)
			}
			checkpointWrite(t, a, stream[split:])
			checkpointWrite(t, b, stream[split:])
			if !bytes.Equal(checkpointBytes(t, a), checkpointBytes(t, b)) {
				t.Fatalf("split%d: continuation differs for %q", split, stream)
			}
		}
	}
}

func TestCheckpointMalformedAtomic(t *testing.T) {
	e := checkpointTerminal(t)
	checkpointWrite(t, e, "unchanged")
	before := checkpointBytes(t, e)
	var state checkpointState
	if err := json.Unmarshal(before, &state); err != nil {
		t.Fatal(err)
	}
	malformed := [][]byte{nil, []byte("{}"), append(append([]byte{}, before...), 0), []byte(strings.Repeat("x", MaxCheckpointBytes+1)), bytes.Replace(before, []byte(`"Phantom":false`), []byte(`"Phantom":null`), 1)}
	for _, mutate := range []func(*checkpointState){func(c *checkpointState) { c.Screens[0].Lines[0][0] = ^uint32(0) }, func(c *checkpointState) { c.Schema = "future" }, func(c *checkpointState) { c.Width = 1 << 30 }, func(c *checkpointState) { c.Active = 9 }, func(c *checkpointState) { c.GL = 9 }, func(c *checkpointState) { c.Parser.State = 255 }, func(c *checkpointState) { c.Screens[0].Lines = nil }, func(c *checkpointState) { c.Screens[0].Scroll.Max.Y = 1000 }} {
		var c checkpointState
		_ = json.Unmarshal(before, &c)
		mutate(&c)
		b, _ := json.Marshal(c)
		malformed = append(malformed, b)
	}
	for i, b := range malformed {
		if err := e.RestoreCheckpoint(b); err == nil {
			t.Fatalf("malformed%d accepted", i)
		}
		if !bytes.Equal(before, checkpointBytes(t, e)) {
			t.Fatalf("malformed%d mutated terminal", i)
		}
	}
}

func TestCheckpointDefaultScrollback(t *testing.T) {
	for _, width := range []int{80, 160} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			e := NewEmulator(width, 24)
			t.Cleanup(func() { _ = e.Close() })
			for line := 0; line < DefaultScrollbackSize+30; line++ {
				checkpointWrite(t, e, fmt.Sprintf("%08d %s\r\n", line, strings.Repeat("x", width-9)))
			}
			if e.scr.scrollback.Len() != DefaultScrollbackSize {
				t.Fatalf("scrollback=%d", e.scr.scrollback.Len())
			}
			b := checkpointBytes(t, e)
			t.Logf("default scrollback width=%d lines=%d encoded=%d bytes", width, e.scr.scrollback.Len(), len(b))
			restored := NewEmulator(1, 1)
			t.Cleanup(func() { _ = restored.Close() })
			if err := restored.RestoreCheckpoint(b); err != nil {
				t.Fatal(err)
			}
			checkpointWrite(t, e, "\x1b[31mcontinued\r\n")
			checkpointWrite(t, restored, "\x1b[31mcontinued\r\n")
			if !bytes.Equal(checkpointBytes(t, e), checkpointBytes(t, restored)) {
				t.Fatal("full-history suffix diverged")
			}
		})
	}
}

func TestCheckpointQueryContinuation(t *testing.T) {
	for _, tc := range []struct{ name, history, suffix string }{
		{"foreground", "\x1b]10;#ff0000\x07", "\x1b]10;?\x07"},
		{"background", "\x1b]11;#0033aa\x07", "\x1b]11;?\x07"},
		{"cursor_color", "\x1b]12;#114477\x07", "\x1b]12;?\x07"},
		{"private_mode", "\x1b[?1004h", "\x1b[?1004$p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := checkpointTerminal(t), checkpointTerminal(t)
			var ar, br bytes.Buffer
			a.SetResponseWriter(&ar)
			b.SetResponseWriter(&br)
			checkpointWrite(t, a, tc.history)
			if err := b.RestoreCheckpoint(checkpointBytes(t, a)); err != nil {
				t.Fatal(err)
			}
			ar.Reset()
			checkpointWrite(t, a, tc.suffix)
			checkpointWrite(t, b, tc.suffix)
			if ar.Len() == 0 || !bytes.Equal(ar.Bytes(), br.Bytes()) {
				t.Fatalf("query suffix replies diverged: uninterrupted=%q restored=%q", ar.String(), br.String())
			}
		})
	}
}
