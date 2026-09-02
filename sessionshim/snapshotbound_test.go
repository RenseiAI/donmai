package sessionshim

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/shimwire"
)

// screenWithHistory builds a §12.1 screen of the given geometry with lines of
// scrollback, each cell a distinct-but-plain glyph so the encoding is realistic
// rather than degenerate.
func screenWithHistory(cols, rows, lines uint64) attachwire.Screen {
	cell := func(r byte) attachwire.Cell {
		return attachwire.Cell{RuneBytes: []byte{r}, FG: attachwire.IndexedColor(7), BG: attachwire.DefaultColor}
	}
	row := func(seed uint64) []attachwire.Cell {
		out := make([]attachwire.Cell, cols)
		for i := range out {
			out[i] = cell(byte('a' + (seed+uint64(i))%26))
		}
		return out
	}
	screen := attachwire.Screen{
		EchoMode: attachwire.EchoOn, Cols: cols, Rows: rows,
		ActiveBuffer: attachwire.BufferPrimary, CursorShape: attachwire.CursorShapeBlock,
		Primary: make([]attachwire.Cell, 0, cols*rows),
	}
	for r := range rows {
		screen.Primary = append(screen.Primary, row(r)...)
	}
	for l := range lines {
		screen.Scrollback = append(screen.Scrollback, row(l))
	}
	return screen
}

func encodedScreen(t *testing.T, screen attachwire.Screen) []byte {
	t.Helper()
	encoded, err := screen.Encode()
	if err != nil {
		t.Fatalf("encode screen: %v", err)
	}
	return encoded
}

func snapshotFrame(t *testing.T, seq, atSeq uint64, screen attachwire.Screen) attachwire.Frame {
	t.Helper()
	return attachwire.Frame{
		Type: attachwire.TypeSnapshot, Seq: seq, RelTime: 4242,
		Payload: attachwire.SnapshotEnvelope{
			AtSeq: atSeq, SnapFormat: attachwire.SnapFormatScreen, Snap: encodedScreen(t, screen),
		}.Encode(),
	}
}

// TestBoundSnapshotFrameKeepsTheNewestHistoryThatFits is the unit-level pin for
// the production failure: a Snapshot whose serialized screen carries a long
// history does not fit one local-wire message, and before this it was refused at
// its source — which closed the connection carrying it.
//
// Reverting hostFrameBody to the plain EncodeHostFrame call turns the oversized
// rows RED with shimwire.ErrMessageTooLarge, which is exactly the error the
// production shim answered with before it dropped a healthy carrier.
func TestBoundSnapshotFrameKeepsTheNewestHistoryThatFits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		cols        uint64
		rows        uint64
		lines       uint64
		wantTrimmed bool
	}{
		{name: "ordinary screen passes through verbatim", cols: 80, rows: 24, lines: 8},
		{name: "long history is trimmed to fit", cols: 200, rows: 24, lines: 1500, wantTrimmed: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			original := screenWithHistory(tc.cols, tc.rows, tc.lines)
			frame := snapshotFrame(t, 91, 90, original)
			shim := &Shim{logger: slog.New(slog.DiscardHandler), id: Identity{OrgID: "o", SessionID: "s"}}

			body, err := shim.hostFrameBody(0, frame)
			if err != nil {
				t.Fatalf("hostFrameBody: %v", err)
			}
			if got := messageBytes(body); got > shimwire.MaxMessageBytes {
				t.Fatalf("encoded host frame = %d bytes, want <= %d", got, shimwire.MaxMessageBytes)
			}

			// The carrier's own decode path, verbatim: what the controller does
			// with the bytes on the wire.
			hostFrame, err := shimwire.DecodeHostFrame(body)
			if err != nil {
				t.Fatalf("DecodeHostFrame: %v", err)
			}
			decoded, err := attachwire.DecodeFrame(hostFrame.FrameBytes)
			if err != nil {
				t.Fatalf("DecodeFrame: %v", err)
			}
			if decoded.Type != attachwire.TypeSnapshot || decoded.Seq != 91 || decoded.RelTime != 4242 {
				t.Fatalf("frame header = %s/%d/%d, want Snapshot/91/4242", decoded.Type, decoded.Seq, decoded.RelTime)
			}
			envelope, err := attachwire.DecodeSnapshotEnvelope(decoded.Payload)
			if err != nil {
				t.Fatalf("DecodeSnapshotEnvelope: %v", err)
			}
			if envelope.AtSeq != 90 || envelope.SnapFormat != attachwire.SnapFormatScreen {
				t.Fatalf("envelope = atSeq %d format %d, want 90/screen", envelope.AtSeq, envelope.SnapFormat)
			}
			screen, err := attachwire.DecodeScreen(envelope.Snap)
			if err != nil {
				t.Fatalf("DecodeScreen: %v", err)
			}

			// The LIVE screen is never touched — only history is shortened.
			if screen.Cols != original.Cols || screen.Rows != original.Rows || len(screen.Primary) != len(original.Primary) {
				t.Fatalf("live grid changed: %dx%d/%d cells, want %dx%d/%d",
					screen.Cols, screen.Rows, len(screen.Primary),
					original.Cols, original.Rows, len(original.Primary))
			}
			if !tc.wantTrimmed {
				if uint64(len(screen.Scrollback)) != tc.lines {
					t.Fatalf("scrollback = %d lines, want the original %d untouched", len(screen.Scrollback), tc.lines)
				}
				return
			}
			if len(screen.Scrollback) == 0 || uint64(len(screen.Scrollback)) >= tc.lines {
				t.Fatalf("scrollback = %d lines, want a non-empty tail shorter than %d", len(screen.Scrollback), tc.lines)
			}
			// The NEWEST lines survive: scrollback is oldest-first, so the last
			// retained line must still be the last line of the original.
			want := original.Scrollback[len(original.Scrollback)-1]
			got := screen.Scrollback[len(screen.Scrollback)-1]
			if len(got) != len(want) || string(got[0].RuneBytes) != string(want[0].RuneBytes) {
				t.Fatal("bounded snapshot dropped the newest history instead of the oldest")
			}
		})
	}
}

// TestBoundSnapshotFrameRefusesWhatHasNoHistoryToDrop pins the documented
// residual: when the LIVE grid alone exceeds the wire there is nothing
// shortenable left, and grinding the live screen down would start deleting what
// the session currently shows. That is a refusal with its own sentinel, not a
// silent half-screen.
func TestBoundSnapshotFrameRefusesWhatHasNoHistoryToDrop(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		frame func(t *testing.T) attachwire.Frame
	}{
		{
			name: "live grid larger than the wire",
			frame: func(t *testing.T) attachwire.Frame {
				t.Helper()
				return snapshotFrame(t, 5, 4, screenWithHistory(1000, 400, 0))
			},
		},
		{
			name: "a non-Snapshot frame has no scrollback at all",
			frame: func(t *testing.T) attachwire.Frame {
				t.Helper()
				return attachwire.Frame{
					Type: attachwire.TypeOutput, Seq: 5,
					Payload: attachwire.EncodeOutput(make([]byte, 2*shimwire.MaxMessageBytes)),
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shim := &Shim{logger: slog.New(slog.DiscardHandler), id: Identity{OrgID: "o", SessionID: "s"}}
			_, err := shim.hostFrameBody(0, tc.frame(t))
			if !errors.Is(err, ErrSnapshotUnboundable) {
				t.Fatalf("hostFrameBody = %v, want ErrSnapshotUnboundable", err)
			}
		})
	}
}

// TestBoundSnapshotAnnouncesTheTruncatedHistory pins the audit trail. The trim
// is deliberately NOT a new wire field — a Screen decoder rejects trailing bytes,
// so a receiver-visible marker would stop older controllers decoding a Snapshot
// at all — so the shim's log is the only place a shortened history is declared.
// A silent trim would be indistinguishable from a session that never scrolled.
func TestBoundSnapshotAnnouncesTheTruncatedHistory(t *testing.T) {
	t.Parallel()
	var log strings.Builder
	shim := &Shim{
		id:     Identity{OrgID: "org-trim", SessionID: "session-trim"},
		logger: slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	if _, err := shim.hostFrameBody(0, snapshotFrame(t, 91, 90, screenWithHistory(200, 24, 1500))); err != nil {
		t.Fatalf("hostFrameBody: %v", err)
	}
	line := log.String()
	for _, want := range []string{
		"snapshot history truncated to fit the local wire",
		"org-trim/session-trim",
		"droppedScrollbackLines",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("trim log = %q, want it to contain %q", line, want)
		}
	}
}

// TestBoundSnapshotResultBytesFitsEveryCarrier covers the two SnapshotResult
// shapes that carry a screen verbatim. Bounding runs where the result is BUILT,
// so the retry ledger's byte-for-byte replay of an exact retry cannot answer the
// same request id with two different payloads.
func TestBoundSnapshotResultBytesFitsEveryCarrier(t *testing.T) {
	t.Parallel()
	big := screenWithHistory(200, 24, 1500)
	tests := []struct {
		name   string
		result func(t *testing.T) shimwire.SnapshotResult
	}{
		{
			name: "inspect carries a raw screen",
			result: func(t *testing.T) shimwire.SnapshotResult {
				t.Helper()
				return shimwire.SnapshotResult{
					RequestID: 3, Generation: 2, Mode: shimwire.SnapshotInspect,
					AtSeq: 90, Bytes: encodedScreen(t, big),
				}
			},
		},
		{
			name: "emit carries an encoded Snapshot frame",
			result: func(t *testing.T) shimwire.SnapshotResult {
				t.Helper()
				return shimwire.SnapshotResult{
					RequestID: 4, Generation: 2, Mode: shimwire.SnapshotEmit, InStream: true,
					AtSeq: 90, Bytes: snapshotFrame(t, 91, 90, big).Encode(),
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shim := &Shim{logger: slog.New(slog.DiscardHandler), id: Identity{OrgID: "o", SessionID: "s"}}
			original := tc.result(t)
			bounded, err := shim.boundSnapshotResultBytes(original)
			if err != nil {
				t.Fatalf("boundSnapshotResultBytes: %v", err)
			}
			body, err := shimwire.EncodeSnapshotResult(bounded)
			if err != nil {
				t.Fatalf("EncodeSnapshotResult: %v", err)
			}
			if got := messageBytes(body); got > shimwire.MaxMessageBytes {
				t.Fatalf("encoded result = %d bytes, want <= %d", got, shimwire.MaxMessageBytes)
			}
			// Correlation is what a controller validates the pair on; bounding
			// must not disturb any of it.
			if bounded.RequestID != original.RequestID || bounded.Generation != original.Generation ||
				bounded.Mode != original.Mode || bounded.AtSeq != original.AtSeq ||
				bounded.InStream != original.InStream || bounded.Code != original.Code {
				t.Fatalf("bounded result correlation = %+v, want %+v", bounded, original)
			}
			// Deterministic: an exact retry must reproduce the stored bytes.
			again, err := shim.boundSnapshotResultBytes(tc.result(t))
			if err != nil {
				t.Fatalf("boundSnapshotResultBytes retry: %v", err)
			}
			if !snapshotResultsEqual(bounded, again) {
				t.Fatal("bounding the same result twice produced different bytes")
			}
		})
	}
}

// TestBoundSnapshotMessageFitsThePreV3JSONCarrier covers the released selected
// v1/v2 Snapshot message, whose body is JSON: the screen bytes are
// base64-inflated on the way out, so the ceiling is reached at roughly three
// quarters of the raw length. Measuring the raw screen instead of the encoded
// message would silently emit a frame the writer then refuses.
func TestBoundSnapshotMessageFitsThePreV3JSONCarrier(t *testing.T) {
	t.Parallel()
	raw := encodedScreen(t, screenWithHistory(200, 24, 1500))
	sizeOf := func(screen []byte) (int, error) {
		body, err := shimwire.EncodeSnapshot(shimwire.SnapshotMsg{AtSeq: 90, Screen: screen})
		if err != nil {
			return 0, err
		}
		return messageBytes(body), nil
	}
	if size, err := sizeOf(raw); err != nil || size <= shimwire.MaxMessageBytes {
		t.Fatalf("fixture screen encodes to %d bytes (err %v); it must exceed %d to exercise the bound",
			size, err, shimwire.MaxMessageBytes)
	}
	bounded, dropped, err := boundSnapshotScreen(raw, shimwire.MaxMessageBytes, sizeOf)
	if err != nil {
		t.Fatalf("boundSnapshotScreen: %v", err)
	}
	if dropped == 0 {
		t.Fatal("boundSnapshotScreen reported no dropped lines for a screen that did not fit")
	}
	size, err := sizeOf(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if size > shimwire.MaxMessageBytes {
		t.Fatalf("bounded Snapshot message = %d bytes, want <= %d", size, shimwire.MaxMessageBytes)
	}
	if _, err := attachwire.DecodeScreen(bounded); err != nil {
		t.Fatalf("bounded screen is not a canonical Screen: %v", err)
	}
}

// TestBoundedSnapshotIsAnOrdinaryScreenForAnyReceiver is the wire-compatibility
// pin. Nothing about the frame shape changes: a bounded Snapshot is a fully
// canonical attach frame carrying a fully canonical §12.1 Screen, so a receiver
// that knows nothing about this bound — including one negotiated at the released
// selected-v2 tier — decodes it exactly as it decodes any other Snapshot. The
// re-encode is byte-identical on the round trip, which is the property
// shimwire's own HostFrame validator enforces.
func TestBoundedSnapshotIsAnOrdinaryScreenForAnyReceiver(t *testing.T) {
	t.Parallel()
	frame := snapshotFrame(t, 91, 90, screenWithHistory(200, 24, 1500))
	bounded, dropped, err := boundSnapshotFrame(frame, shimwire.MaxHostFrameBytes,
		func(f attachwire.Frame) (int, error) { return len(f.Encode()), nil })
	if err != nil {
		t.Fatalf("boundSnapshotFrame: %v", err)
	}
	if dropped == 0 {
		t.Fatal("fixture did not exercise the bound")
	}
	encoded := bounded.Encode()
	roundTripped, err := attachwire.DecodeFrame(encoded)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if got := roundTripped.Encode(); string(got) != string(encoded) {
		t.Fatal("bounded frame does not round-trip to identical canonical bytes")
	}
	// shimwire's own strictness is the receiver's: it rejects anything that is
	// not one canonical attach frame.
	if _, err := shimwire.EncodeHostFrame(shimwire.HostFrame{FrameBytes: encoded}); err != nil {
		t.Fatalf("bounded frame refused by the HostFrame validator: %v", err)
	}

	// Determinism is what keeps §D5's byte-for-byte rule intact across replays:
	// the same retained frame bounds to the same wire bytes every time it is
	// delivered, so one host sequence never carries two different payloads —
	// not on a re-adoption ring hit, and not across a controller generation.
	again, _, err := boundSnapshotFrame(snapshotFrame(t, 91, 90, screenWithHistory(200, 24, 1500)),
		shimwire.MaxHostFrameBytes, func(f attachwire.Frame) (int, error) { return len(f.Encode()), nil })
	if err != nil {
		t.Fatalf("boundSnapshotFrame retry: %v", err)
	}
	if string(again.Encode()) != string(encoded) {
		t.Fatal("bounding the same frame twice produced different wire bytes")
	}
}
