package ptyhost

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
)

func TestSnapshotNeutralizesOSCTitles(t *testing.T) {
	sequences := []struct {
		name string
		seq  []byte
	}{
		{"ascii-bel", []byte("\x1b]0;stale title\x07")},
		{"unicode-bel", []byte("\x1b]0;✳ stale title\x07")},
		{"unicode-st", []byte("\x1b]1;✳ stale title\x1b\\")},
		{"c1-osc-st", append([]byte{oscTitleC1OSC}, append([]byte("2;✳ stale title"), oscTitleC1ST)...)},
	}

	for _, tc := range sequences {
		t.Run(tc.name, func(t *testing.T) {
			for split := 0; split <= len(tc.seq); split++ {
				v := newVTHost(80, 4, DefaultScrollback, io.Discard, nil)
				v.write([]byte("prompt> "))
				v.write(tc.seq[:split])
				v.write(tc.seq[split:])
				v.write([]byte("ready"))

				got := snapshotText(t, v)
				if strings.Contains(got, "stale title") {
					t.Fatalf("split %d leaked OSC title into snapshot: %q", split, got)
				}
				if !strings.Contains(got, "prompt> ready") {
					t.Fatalf("split %d lost ordinary cell output: %q", split, got)
				}
			}
		})
	}
}

func TestSnapshotTitleFilterPreservesNonTitleOSC(t *testing.T) {
	queries := [][]byte{
		[]byte("\x1b]11;?\x07"),
		append([]byte{oscTitleC1OSC}, append([]byte("11;?"), oscTitleC1ST)...),
	}
	for _, query := range queries {
		for split := 0; split <= len(query); split++ {
			var resp bytes.Buffer
			v := newVTHost(80, 4, DefaultScrollback, &resp, nil)
			v.write(query[:split])
			v.write(query[split:])
			if !strings.Contains(resp.String(), "\x1b]11;rgb:") {
				t.Fatalf("split %d lost OSC color query response: %q", split, resp.String())
			}
		}
	}
}

func TestSnapshotTitleFilterPreservesNonTitleBytes(t *testing.T) {
	inputs := [][]byte{
		[]byte("plain ✳ text\r\n\x1b[31mred\x1b[0m"),
		[]byte("\x1b]8;;https://example.com/✳\x1b\\link\x1b]8;;\x1b\\"),
		[]byte("\x1b]11;rgb:1111/2222/3333\x07"),
		[]byte("\x1b]99;✳ payload\x07"),
	}
	for _, input := range inputs {
		for split := 0; split <= len(input); split++ {
			f := oscTitleFilter{}
			got := append(f.Write(input[:split]), f.Write(input[split:])...)
			if !bytes.Equal(got, input) {
				t.Fatalf("split %d mutated non-title bytes:\n got %q\nwant %q", split, got, input)
			}
		}
	}
}

func TestSessionOutputPreservesOSCTitleBytes(t *testing.T) {
	raw := []byte("before\x1b]0;✳ stale title\x07after")
	s := &Session{
		spawnAt: time.Now(),
		vt:      newVTHost(80, 4, DefaultScrollback, io.Discard, nil),
		nextSeq: attachwire.HostSeqStart,
		ring:    newRing(DefaultRingBytes),
		subs:    make(map[*subscription]struct{}),
	}
	s.onOutput(raw)

	if len(s.ring.frames) != 1 {
		t.Fatalf("output produced %d frames, want 1", len(s.ring.frames))
	}
	got := attachwire.DecodeOutput(s.ring.frames[0].Payload).Data
	if !bytes.Equal(got, raw) {
		t.Fatalf("live output mutated: got %q want %q", got, raw)
	}
	if snapshot := snapshotText(t, s.vt.(*vtHost)); strings.Contains(snapshot, "stale title") {
		t.Fatalf("snapshot leaked title while live output stayed raw: %q", snapshot)
	}
}

func snapshotText(t *testing.T, v *vtHost) string {
	t.Helper()
	scr := serializeScreen(t, v)
	var b strings.Builder
	for _, cell := range scr.Primary {
		b.Write(cell.RuneBytes)
	}
	return b.String()
}
