package terminalparser

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi/parser"
)

func checkpointRecorder(p *Parser) *[]string {
	events := new([]string)
	p.SetHandler(Handler{
		Print:   func(r rune) { *events = append(*events, fmt.Sprintf("print:%U", r)) },
		Execute: func(b byte) { *events = append(*events, fmt.Sprintf("execute:%x", b)) },
		HandleCsi: func(cmd Cmd, params Params) {
			*events = append(*events, fmt.Sprintf("csi:%d:%v", cmd, append([]Param(nil), params...)))
		},
		HandleOsc: func(cmd int, data []byte) {
			*events = append(*events, fmt.Sprintf("osc:%d:%x", cmd, data))
		},
		HandleDcs: func(cmd Cmd, params Params, data []byte) {
			*events = append(*events, fmt.Sprintf("dcs:%d:%v:%x", cmd, append([]Param(nil), params...), data))
		},
	})
	return events
}

func TestParserCheckpointResume(t *testing.T) {
	tests := []struct {
		name, prefix, suffix string
		unlimited            bool
	}{
		{name: "CSI in-progress parameter", prefix: "\x1b[12;3", suffix: "4mZ"},
		{name: "OSC command and data", prefix: "\x1b]12;hel", suffix: "lo\x07Z"},
		{name: "DCS parameter and data", prefix: "\x1bP1;2qhel", suffix: "lo\x1b\\Z"},
		{name: "UTF-8 lead", prefix: "\xf0", suffix: "\x9f\x8c\x8dZ"},
		{name: "UTF-8 continuation", prefix: "\xf0\x9f", suffix: "\x8c\x8dZ"},
		{name: "UTF-8 invalid second byte", prefix: "\xf0A", suffix: "BCZ"},
		{name: "UTF-8 invalid third byte", prefix: "\xf0\x9f\x1b", suffix: "CZ"},
		{name: "UTF-8 overlong prefix", prefix: "\xf0\x80", suffix: "\x80\x80Z"},
		{name: "finite data limit", prefix: "\x1b]1;abc", suffix: "def\x07", unlimited: false},
		{name: "empty unlimited before OSC", prefix: "", suffix: "\x1b]1;abc\x07", unlimited: true},
		{name: "unlimited data", prefix: "\x1b]1;abc", suffix: "def\x07", unlimited: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			continuous := NewParser()
			resumed := NewParser()
			if tt.name == "finite data limit" {
				continuous.SetDataSize(5)
			}
			if tt.unlimited {
				continuous.SetDataSize(0)
			}
			wantEvents := checkpointRecorder(continuous)
			gotEvents := checkpointRecorder(resumed)
			for i := range []byte(tt.prefix) {
				continuous.Advance(tt.prefix[i])
			}
			checkpoint, err := continuous.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			if err := resumed.RestoreCheckpoint(checkpoint); err != nil {
				t.Fatal(err)
			}
			for i := range []byte(tt.suffix) {
				continuous.Advance(tt.suffix[i])
				resumed.Advance(tt.suffix[i])
			}
			if !reflect.DeepEqual(*gotEvents, *wantEvents) {
				t.Fatalf("events after resume = %v, continuous = %v", *gotEvents, *wantEvents)
			}
			want, err := continuous.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			got, err := resumed.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("final checkpoint differs: resumed %+v, continuous %+v", got, want)
			}
		})
	}
}

func TestParserCheckpointClonesBuffers(t *testing.T) {
	p := NewParser()
	for _, b := range []byte("\x1b]1;abc") {
		p.Advance(b)
	}
	checkpoint, err := p.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	beforeParams := append([]int(nil), checkpoint.Params...)
	beforeData := append([]byte(nil), checkpoint.Data...)
	p.params[0] = 123
	p.data[0] = 'X'
	if !reflect.DeepEqual(checkpoint.Params, beforeParams) || !reflect.DeepEqual(checkpoint.Data, beforeData) {
		t.Fatal("exported checkpoint aliases parser buffers")
	}
	restored := NewParser()
	if err := restored.RestoreCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint.Params[0] = 456
	checkpoint.Data[0] = 'Y'
	if !reflect.DeepEqual(restored.params, beforeParams) || !reflect.DeepEqual(restored.data[:len(beforeData)], beforeData) {
		t.Fatal("restored parser aliases checkpoint buffers")
	}
}

func TestParserCheckpointValidateAndAtomicRestore(t *testing.T) {
	p := NewParser()
	events := checkpointRecorder(p)
	for _, b := range []byte("\x1b[12;") {
		p.Advance(b)
	}
	valid, err := p.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*ParserCheckpoint)
	}{
		{"version", func(c *ParserCheckpoint) { c.Version++ }},
		{"state", func(c *ParserCheckpoint) { c.State = 255 }},
		{"empty params", func(c *ParserCheckpoint) { c.Params = nil }},
		{"oversize params", func(c *ParserCheckpoint) { c.Params = make([]int, parser.MaxParamsSize+1) }},
		{"parameter length", func(c *ParserCheckpoint) { c.ParamsLen = len(c.Params) + 1 }},
		{"data capacity", func(c *ParserCheckpoint) { c.DataCapacity = maxCheckpointDataSize + 1 }},
		{"data length", func(c *ParserCheckpoint) { c.DataLen = 1 }},
		{"unlimited length", func(c *ParserCheckpoint) { c.DataLen = -1; c.DataCapacity = 1 }},
		{"oversize data", func(c *ParserCheckpoint) { c.Data = make([]byte, maxCheckpointDataSize+1) }},
		{"UTF-8 lead", func(c *ParserCheckpoint) { c.State = parser.Utf8State; c.ParamsLen = 1; c.Command = 0x80 }},
		{"UTF-8 complete", func(c *ParserCheckpoint) { c.State = parser.Utf8State; c.ParamsLen = 2; c.Command = 0x80c2 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := valid
			tt.mutate(&bad)
			if err := bad.Validate(); err == nil {
				t.Fatal("malformed checkpoint validated")
			}
			if err := p.RestoreCheckpoint(bad); err == nil {
				t.Fatal("malformed checkpoint restored")
			}
			after, err := p.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, valid) {
				t.Fatal("invalid restore changed parser")
			}
		})
	}
	for _, b := range []byte("3m") {
		p.Advance(b)
	}
	if !strings.Contains(strings.Join(*events, ","), "csi:") {
		t.Fatal("handler lost after invalid restore")
	}
}
