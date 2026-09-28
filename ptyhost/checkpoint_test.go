package ptyhost

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	uv "github.com/charmbracelet/ultraviolet"
)

func checkpointSession(t *testing.T, history string) *Session {
	t.Helper()
	v := newVTHost(20, 8, 100, io.Discard, nil)
	t.Cleanup(func() { _ = v.emu.Close() })
	v.write([]byte(history))
	s := &Session{vt: v, epoch: 7, nextSeq: 19}
	s.closedFlag.Store(true) // Pure VT fixture: no PTY master; ECHO is unknown.
	return s
}

func TestContinuationCheckpointSuffix(t *testing.T) {
	for _, tc := range []struct{ name, history, suffix string }{
		{"pen", "\x1b[31m", "X"},
		{"saved", "\x1b[3;5H\x1b7\x1b[H", "\x1b8X"},
		{"margins", "\x1b[2;4r\x1b[H", "\x1b[4;1H\nX"},
		{"csi", "\x1b[3", "1mX"},
		{"utf8", "\xe7\x95", "\x8cX"},
		{"wrap", strings.Repeat("A", 20), "B"},
		{"alternate", "primary\x1b[?1049halt", "\x1b[?1049lX"},
		{"title_selector", "\x1b]", "2;ignored title\x07X"},
		{"title_body", "\x1b]2;partial", "ignored\x1b\\X"},
		{"title_esc", "\x1b]2;partial\x1b", "\\X"},
		{"mode_callbacks", "\x1b[?2004;1006;1003h", "X"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := checkpointSession(t, tc.history)
			c, err := s.ContinuationCheckpoint(attachwire.ContinuationSchema)
			if err != nil {
				t.Fatal(err)
			}
			if c.Epoch != 7 || c.AtSeq != 18 || s.nextSeq != 19 {
				t.Fatal("checkpoint changed or lost sequence boundary")
			}
			wire, err := c.Encode(attachwire.ContinuationSchema)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := attachwire.DecodeContinuationCheckpoint(wire, attachwire.ContinuationSchema)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := RestoreContinuation(decoded)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restored.Close() })
			s.vt.write([]byte(tc.suffix))
			if err := restored.ApplyRawFrame(attachwire.Frame{Type: attachwire.TypeOutput, Seq: uint64(restored.AtSeq()) + 1, Payload: attachwire.EncodeOutput([]byte(tc.suffix))}); err != nil {
				t.Fatal(err)
			}
			expected, err := s.buildScreenLocked().Encode()
			if err != nil {
				t.Fatal(err)
			}
			actual, err := restored.Screen().Encode()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(expected, actual) {
				t.Fatal("uninterrupted and restored identical suffix diverged")
			}
		})
	}
}

func TestContinuationCheckpointRetainsOSC8RawLinkThroughSameSuffix(t *testing.T) {
	want := append([]byte("https://example.invalid/"), 0xff)
	history := append([]byte("\x1b]8;;"), want...)
	history = append(history, []byte("\x1b\\")...)
	s := checkpointSession(t, string(history))
	producer := s.vt.(*vtHost)
	if got := []byte(producer.emu.PrimaryScreen().Cursor().Link.Params); !bytes.Equal(got, want) {
		t.Fatalf("fixture did not reach raw OSC8 link: got=%x want=%x", got, want)
	}
	checkpoint, err := s.ContinuationCheckpoint(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := checkpoint.Encode(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := attachwire.DecodeContinuationCheckpoint(wire, attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := RestoreContinuation(decoded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	if got := []byte(mirror.host.emu.PrimaryScreen().Cursor().Link.Params); !bytes.Equal(got, want) {
		t.Fatalf("restored cursor link changed: got=%x want=%x", got, want)
	}
	producer.write([]byte("X"))
	if err := mirror.ApplyRawFrame(attachwire.Frame{
		Type: attachwire.TypeOutput,
		Seq:  uint64(mirror.AtSeq()) + 1, Payload: attachwire.EncodeOutput([]byte("X")),
	}); err != nil {
		t.Fatal(err)
	}
	p, m := producer.emu.PrimaryScreen().CellAt(0, 0), mirror.host.emu.PrimaryScreen().CellAt(0, 0)
	if p == nil || m == nil || !bytes.Equal([]byte(p.Link.Params), []byte(m.Link.Params)) || !bytes.Equal([]byte(p.Link.Params), want) {
		t.Fatalf("same-suffix cell link diverged: producer=%+v mirror=%+v", p, m)
	}
	producerScreen, err := s.buildScreenLocked().Encode()
	if err != nil {
		t.Fatal(err)
	}
	mirrorScreen, err := mirror.Screen().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(producerScreen, mirrorScreen) {
		t.Fatal("same-suffix safe Screen diverged")
	}
}

func TestContinuationCheckpointFailClosed(t *testing.T) {
	s := checkpointSession(t, "\x1b[31m")
	if _, err := s.ContinuationCheckpoint(""); err == nil {
		t.Fatal("missing selection accepted")
	}
	c, err := s.ContinuationCheckpoint(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Encode(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{nil, b[:len(b)-1], append(append([]byte{}, b...), 0)} {
		if _, err := attachwire.DecodeContinuationCheckpoint(bad, attachwire.ContinuationSchema); err == nil {
			t.Fatal("malformed checkpoint accepted")
		}
	}
	if _, err := attachwire.DecodeContinuationCheckpoint(b, ""); err == nil {
		t.Fatal("unnegotiated checkpoint accepted")
	}
	c.State = []byte("{}")
	if restored, err := RestoreContinuation(c); err == nil {
		_ = restored.Close()
		t.Fatal("incomplete state accepted")
	}
	c, err = s.ContinuationCheckpoint(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	c.Picture.CursorCol = 2
	if restored, err := RestoreContinuation(c); err == nil {
		_ = restored.Close()
		t.Fatal("inconsistent picture accepted")
	}
	c.Epoch++
	if _, err := c.Encode(attachwire.ContinuationSchema); err == nil {
		t.Fatal("epoch mismatch accepted")
	}
}

func TestContinuationRawTailPreservesParserAcrossPicture(t *testing.T) {
	s := checkpointSession(t, "\x1b[31m\x1b]0;")
	c, err := s.ContinuationCheckpoint(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := RestoreContinuation(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	picture, err := c.Picture.Encode()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := attachwire.Frame{Type: attachwire.TypeSnapshot, Seq: 19, Payload: attachwire.SnapshotEnvelope{AtSeq: 18, SnapFormat: attachwire.SnapFormatScreen, Snap: picture}.Encode()}
	if err := mirror.ApplyRawFrame(snapshot); err != nil {
		t.Fatal(err)
	}
	raw := []byte("HELLO\x07X")
	s.vt.write(raw)
	if err := mirror.ApplyRawFrame(attachwire.Frame{Type: attachwire.TypeOutput, Seq: 20, Payload: attachwire.EncodeOutput(raw)}); err != nil {
		t.Fatal(err)
	}
	want, _ := s.buildScreenLocked().Encode()
	got, _ := mirror.Screen().Encode()
	if !bytes.Equal(want, got) {
		t.Fatal("raw tail or picture event reset complete state")
	}
	if err := mirror.ApplyRawFrame(attachwire.Frame{Type: attachwire.TypeOutput, Seq: 22, Payload: attachwire.EncodeOutput([]byte("gap"))}); err == nil {
		t.Fatal("tail gap accepted")
	}
	if mirror.AtSeq() != 20 {
		t.Fatal("failed tail advanced anchor")
	}
}

func TestContinuationAtomicTailSurvivesRingEviction(t *testing.T) {
	s := checkpointSession(t, "\x1b[31m")
	s.ring = newRing(16)
	s.subs = make(map[*subscription]struct{})
	c, sub, err := s.OpenContinuation(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	mirror, err := RestoreContinuation(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	produced := make(chan error, 1)
	go func() {
		for i := 0; i < 200; i++ {
			s.onOutput([]byte("X"))
		}
		produced <- s.EmitMarker("complete")
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	finished := false
	for !finished {
		select {
		case f := <-sub.Frames():
			if err := mirror.ApplyRawFrame(f); err != nil {
				t.Fatal(err)
			}
			finished = f.Type == attachwire.TypeMarker
		case <-deadline.C:
			t.Fatal("atomic tail stalled")
		}
	}
	if err := <-produced; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Subscribe(c.AtSeq); err == nil {
		t.Fatal("fixture did not actually evict checkpoint boundary")
	}
	expected, seq, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := expected.Encode()
	b, _ := mirror.Screen().Encode()
	if seq != mirror.AtSeq() || !bytes.Equal(a, b) {
		t.Fatal("atomic checkpoint plus exact raw tail diverged")
	}
}

func TestContinuationProjectionConfinesControlContent(t *testing.T) {
	s := checkpointSession(t, "")
	v := s.vt.(*vtHost)
	v.emu.SetCell(0, 0, &uv.Cell{Content: "\x1b]52;clipboard\x07", Width: 1})
	c, err := s.ContinuationCheckpoint(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := RestoreContinuation(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	screen := mirror.Screen()
	if _, err := screen.Encode(); err != nil {
		t.Fatal("mirror projection was not escape-safe")
	}
	if bytes.Contains(screen.Primary[0].RuneBytes, []byte{0x1b}) || bytes.Contains(screen.Primary[0].RuneBytes, []byte{0x07}) {
		t.Fatal("raw cell control reached renderer projection")
	}
}

func TestContinuationRepeatedConstructCloseAndMalformedRestore(t *testing.T) {
	s := checkpointSession(t, "\x1b[31mready")
	c, err := s.ContinuationCheckpoint(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	malformed := c
	malformed.State = []byte("{}")
	for i := 0; i < 64; i++ {
		if mirror, err := RestoreContinuation(malformed); err == nil || mirror != nil {
			if mirror != nil {
				_ = mirror.Close()
			}
			t.Fatal("malformed restore retained a usable mirror")
		}
		mirror, err := RestoreContinuation(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := mirror.Close(); err != nil {
			t.Fatal(err)
		}
		if err := mirror.Close(); err != nil {
			t.Fatal(err)
		}
		if err := mirror.ApplyRawFrame(attachwire.Frame{Type: attachwire.TypeOutput, Seq: uint64(c.AtSeq) + 1, Payload: attachwire.EncodeOutput([]byte("after close"))}); err == nil {
			t.Fatal("closed mirror accepted a frame")
		}
		if mirror.AtSeq() != c.AtSeq {
			t.Fatal("closed mirror advanced its cursor")
		}
		if _, err := mirror.Screen().Encode(); err != nil {
			t.Fatal("safe final display lost after Close")
		}
	}
}

func TestContinuationSnapshotUpdatesEchoWithoutRepaint(t *testing.T) {
	s := checkpointSession(t, "\x1b[31m")
	c, err := s.ContinuationCheckpoint(attachwire.ContinuationSchema)
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := RestoreContinuation(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	picture := c.Picture
	picture.EchoMode = attachwire.EchoOff
	encoded, err := picture.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := mirror.ApplyRawFrame(attachwire.Frame{Type: attachwire.TypeSnapshot, Seq: uint64(c.AtSeq) + 1, Payload: attachwire.SnapshotEnvelope{AtSeq: uint64(c.AtSeq), SnapFormat: attachwire.SnapFormatScreen, Snap: encoded}.Encode()}); err != nil {
		t.Fatal(err)
	}
	if mirror.Screen().EchoMode != attachwire.EchoOff {
		t.Fatal("authoritative echo metadata stayed stale")
	}
	if err := mirror.ApplyRawFrame(attachwire.Frame{Type: attachwire.TypeOutput, Seq: uint64(c.AtSeq) + 2, Payload: attachwire.EncodeOutput([]byte("X"))}); err != nil {
		t.Fatal(err)
	}
	if mirror.Screen().Primary[0].FG != attachwire.IndexedColor(1) {
		t.Fatal("picture reset the saved SGR pen")
	}
	final := attachwire.NewNormalExit(0)
	c.Exit = &final
	c.AtSeq = 0
	if got, err := RestoreContinuation(c); err == nil || got != nil {
		if got != nil {
			_ = got.Close()
		}
		t.Fatal("direct struct accepted Exit without a sequence")
	}
	c.AtSeq = 18
	c.Exit = &attachwire.ExitPayload{Signal: "\x1b]52;unsafe"}
	if got, err := RestoreContinuation(c); err == nil || got != nil {
		if got != nil {
			_ = got.Close()
		}
		t.Fatal("direct struct accepted unsafe Exit metadata")
	}
}
