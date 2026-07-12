package ptyhost

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
)

// fixtureMeta mirrors the recorded-fixture sidecar JSON (copied from the vt
// spike). Only the fields the ptyhost tests consume are declared.
type fixtureMeta struct {
	Name        string `json:"name"`
	Cols        int    `json:"cols"`
	Rows        int    `json:"rows"`
	Bytes       int    `json:"bytes"`
	Checkpoints []struct {
		Label  string `json:"label"`
		Offset int    `json:"offset"`
	} `json:"checkpoints"`
	Reference *struct {
		Panes []struct {
			ID       string `json:"id"`
			Left     int    `json:"left"`
			Top      int    `json:"top"`
			Width    int    `json:"width"`
			Height   int    `json:"height"`
			Active   bool   `json:"active"`
			CursorX  int    `json:"cursor_x"`
			CursorY  int    `json:"cursor_y"`
			CaptureE string `json:"capture_e"`
		} `json:"panes"`
	} `json:"reference,omitempty"`
}

func loadFixture(t *testing.T, name string) ([]byte, fixtureMeta) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".raw"))
	if err != nil {
		t.Fatalf("read fixture %s.raw: %v", name, err)
	}
	metaBytes, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatalf("read fixture %s.json: %v", name, err)
	}
	var m fixtureMeta
	if err := json.Unmarshal(metaBytes, &m); err != nil {
		t.Fatalf("parse %s.json: %v", name, err)
	}
	return raw, m
}

func (m fixtureMeta) offset(label string) (int, bool) {
	for _, c := range m.Checkpoints {
		if c.Label == label {
			return c.Offset, true
		}
	}
	return 0, false
}

// feedVT builds a vtHost at the fixture geometry and feeds raw bytes through the
// full VT path (the same path Session.onOutput uses). Query answers drain to a
// discard writer so a query byte never deadlocks the feed.
func feedVT(t *testing.T, cols, rows int, raw []byte) *vtHost {
	t.Helper()
	if cols == 0 || rows == 0 {
		cols, rows = 80, 24
	}
	v := newVTHost(cols, rows, DefaultScrollback, io.Discard, nil)
	v.write(raw)
	return v
}

// serializeScreen runs the full VT→attachwire.Screen path and round-trips it
// through the framing library so the encoded snapshot is proven escape-safe
// (attachwire.DecodeScreen enforces §12.1 on decode).
func serializeScreen(t *testing.T, v *vtHost) attachwire.Screen {
	t.Helper()
	scr := buildScreen(v.raw(), 0, attachwire.EchoUnknown, nil)
	enc, err := scr.Encode()
	if err != nil {
		t.Fatalf("screen encode: %v", err)
	}
	dec, err := attachwire.DecodeScreen(enc)
	if err != nil {
		t.Fatalf("screen decode (escape-safety violated): %v", err)
	}
	return dec
}

// gridRowText reconstructs the text of a serialized grid row over columns
// [left, left+width), honoring wide-glyph continuation cells, and right-trims
// trailing blanks — matching the vt spike's subRowText/tmux comparison policy.
func gridRowText(grid []attachwire.Cell, cols, left, y, width int) string {
	var b strings.Builder
	for x := left; x < left+width && x < cols; x++ {
		c := grid[y*cols+x]
		if c.Style&attachwire.StyleWideContinuation != 0 {
			continue // spacer half of a wide glyph — already emitted by its base
		}
		if len(c.RuneBytes) == 0 {
			b.WriteByte(' ')
			continue
		}
		b.Write(c.RuneBytes)
	}
	return trimRightSpaces(b.String())
}

// stripSGR removes ANSI SGR/escape sequences from a capture-pane -e string,
// leaving per-cell text (copied from the vt spike).
func stripSGR(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == 0x1b {
			j := i + 1
			switch {
			case j < len(s) && s[j] == '[':
				j++
				for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
					j++
				}
				j++
			case j < len(s) && s[j] == ']':
				for j < len(s) && s[j] != 0x07 {
					j++
				}
				j++
			default:
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func trimRightSpaces(s string) string { return strings.TrimRight(s, " \t\x00") }
