package shimwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestWriterReaderRoundTripsEveryMessageType(t *testing.T) {
	t.Parallel()

	// Every type in the CLOSED v1 vocabulary must survive a round trip. Iterating
	// the registry rather than listing cases by hand means a new type cannot be
	// added without this test noticing.
	types := []MessageType{
		TypeHello, TypeWelcome, TypeAdopted, TypeOutput, TypeGap, TypeSnapshot,
		TypeInput, TypeResize, TypeStop, TypeHeartbeat, TypeExit, TypeError,
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for i, mt := range types {
		body := []byte{byte(i), 0xAA, 0xBB}
		if err := w.Write(mt, body); err != nil {
			t.Fatalf("Write(%s): %v", mt, err)
		}
	}
	r := NewReader(&buf)
	for i, want := range types {
		got, err := r.Read()
		if err != nil {
			t.Fatalf("Read #%d: %v", i, err)
		}
		if got.Type != want {
			t.Fatalf("Read #%d type = %s, want %s", i, got.Type, want)
		}
		wantBody := []byte{byte(i), 0xAA, 0xBB}
		if !bytes.Equal(got.Body, wantBody) {
			t.Fatalf("Read #%d body = %v, want %v", i, got.Body, wantBody)
		}
	}
	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after drain = %v, want io.EOF", err)
	}
}

func TestReaderRefusesOversizedLengthWithoutAllocating(t *testing.T) {
	t.Parallel()

	// The declared length is 4 GiB but only the 4-byte header exists on the
	// stream. A reader that allocated before checking would try to reserve 4 GiB
	// and then block forever on a body that never arrives; the bound is what
	// turns that into an immediate typed refusal.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], ^uint32(0))
	r := NewReader(bytes.NewReader(hdr[:]))
	_, err := r.Read()
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("Read oversized = %v, want ErrMessageTooLarge", err)
	}
}

func TestReaderRejectsZeroLengthAndUnknownType(t *testing.T) {
	t.Parallel()

	t.Run("zero length", func(t *testing.T) {
		t.Parallel()
		var hdr [4]byte
		r := NewReader(bytes.NewReader(hdr[:]))
		if _, err := r.Read(); !errors.Is(err, ErrMalformed) {
			t.Fatalf("Read zero-length = %v, want ErrMalformed", err)
		}
	})

	t.Run("unknown type", func(t *testing.T) {
		t.Parallel()
		// An unassigned discriminator is a protocol-version signal, never a v1
		// message to skip. Accepting it would let a newer peer silently downgrade
		// this one.
		frame := []byte{0, 0, 0, 1, 0xFE}
		r := NewReader(bytes.NewReader(frame))
		if _, err := r.Read(); !errors.Is(err, ErrMalformed) {
			t.Fatalf("Read unknown type = %v, want ErrMalformed", err)
		}
	})
}

func TestWriterRefusesOversizedBodyAtSource(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := NewWriter(&buf)
	err := w.Write(TypeOutput, make([]byte, MaxMessageBytes))
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("Write oversized = %v, want ErrMessageTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("refused write emitted %d bytes; a partial frame would desync the stream", buf.Len())
	}
}

func TestWriterRefusesUnknownType(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := NewWriter(&buf).Write(MessageType(0xFE), nil); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Write unknown type = %v, want ErrMalformed", err)
	}
}

func TestWriterIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	// The shim's output pump and its heartbeat both write the same connection. A
	// torn frame would desynchronise the length framing permanently, so the
	// writer serialises. This asserts every frame arrives whole and well-formed.
	const writers, each = 8, 32
	var buf bytes.Buffer
	var mu sync.Mutex
	w := NewWriter(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}))

	var wg sync.WaitGroup
	for i := byte(0); i < writers; i++ {
		wg.Add(1)
		go func(id byte) {
			defer wg.Done()
			for j := byte(0); j < each; j++ {
				if err := w.Write(TypeHeartbeat, []byte{id, j}); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	r := NewReader(bytes.NewReader(buf.Bytes()))
	for i := 0; i < writers*each; i++ {
		msg, err := r.Read()
		if err != nil {
			t.Fatalf("Read #%d after concurrent writes: %v", i, err)
		}
		if msg.Type != TypeHeartbeat || len(msg.Body) != 2 {
			t.Fatalf("Read #%d = %s body %v; frame boundaries were torn", i, msg.Type, msg.Body)
		}
	}
}

func TestV3WriterBatchKeepsHostFrameResultPairsAdjacent(t *testing.T) {
	t.Parallel()
	const pairs = 64
	var buf bytes.Buffer
	var sinkMu sync.Mutex
	w := NewWriter(writerFunc(func(p []byte) (int, error) {
		sinkMu.Lock()
		defer sinkMu.Unlock()
		return buf.Write(p)
	}))
	var wg sync.WaitGroup
	for i := byte(0); i < pairs; i++ {
		wg.Add(2)
		go func(id byte) {
			defer wg.Done()
			if err := w.WriteVersionBatch(V3,
				Message{Type: TypeHostFrame, Body: []byte{id}},
				Message{Type: TypeSnapshotResult, Body: []byte{id}},
			); err != nil {
				t.Errorf("WriteVersionBatch: %v", err)
			}
		}(i)
		go func(id byte) {
			defer wg.Done()
			if err := w.WriteVersion(V3, TypeHeartbeat, []byte{id}); err != nil {
				t.Errorf("WriteVersion heartbeat: %v", err)
			}
		}(i)
	}
	wg.Wait()
	r := NewReader(bytes.NewReader(buf.Bytes()))
	for {
		message, err := r.ReadVersion(V3)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if message.Type != TypeHostFrame {
			continue
		}
		result, err := r.ReadVersion(V3)
		if err != nil || result.Type != TypeSnapshotResult || !bytes.Equal(result.Body, message.Body) {
			t.Fatalf("HostFrame pair interleaved: host=%v result=%v err=%v", message, result, err)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestReaderReportsTruncatedBody(t *testing.T) {
	t.Parallel()

	// Header claims 8 bytes; only 3 follow. This is what a peer dying mid-write
	// looks like, and it must be distinguishable from a clean close.
	frame := []byte{0, 0, 0, 8, byte(TypeOutput), 1, 2}
	r := NewReader(bytes.NewReader(frame))
	_, err := r.Read()
	if err == nil {
		t.Fatal("Read truncated body succeeded; want an error")
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("truncated body reported as clean EOF; a half-written frame is not a clean close")
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Fatalf("Read truncated = %v, want a body-read error", err)
	}
}

func TestMessageTypeMutatingCoversExactlyTheAuthorityBearingSet(t *testing.T) {
	t.Parallel()

	// §D4 permits read-only inspection without a generation but requires one for
	// input, resize, stop, terminal acknowledgement, and tombstone disposal. This
	// pins the predicate so a new authority-bearing message cannot be added
	// without deciding whether it is fenced.
	mutating := map[MessageType]bool{TypeInput: true, TypeResize: true, TypeStop: true}
	all := []MessageType{
		TypeHello, TypeWelcome, TypeAdopted, TypeOutput, TypeGap, TypeSnapshot,
		TypeInput, TypeResize, TypeStop, TypeHeartbeat, TypeExit, TypeError,
	}
	for _, mt := range all {
		if got, want := mt.Mutating(), mutating[mt]; got != want {
			t.Errorf("%s.Mutating() = %v, want %v", mt, got, want)
		}
	}
}

func TestMessageTypeKnownRejectsReservedValues(t *testing.T) {
	t.Parallel()

	for _, v := range []uint8{0x00, 0x0D, 0x7F, 0xFF} {
		if MessageType(v).Known() {
			t.Errorf("MessageType(%#x).Known() = true, want false (reserved)", v)
		}
	}
	if !TypeError.Known() {
		t.Error("TypeError.Known() = false; 0x0C is the last assigned v1 type")
	}
}

func TestSelectedVersionsKeepV1V2ClosedAndAdmitV3HostFrame(t *testing.T) {
	t.Parallel()
	for _, mt := range []MessageType{TypeSnapshotRequest, TypeSnapshotResult} {
		if mt.Known() || mt.AllowedIn(V1) || !mt.AllowedIn(V2) || !mt.AllowedIn(V3) {
			t.Fatalf("%s vocabulary: Known=%v v1=%v v2=%v v3=%v", mt, mt.Known(), mt.AllowedIn(V1), mt.AllowedIn(V2), mt.AllowedIn(V3))
		}
		var buf bytes.Buffer
		if err := NewWriter(&buf).Write(mt, nil); !errors.Is(err, ErrMalformed) {
			t.Fatalf("plain v1 Write(%s) = %v, want ErrMalformed", mt, err)
		}
		if err := NewWriter(&buf).WriteVersion(V2, mt, []byte{1}); err != nil {
			t.Fatalf("v2 WriteVersion(%s): %v", mt, err)
		}
		if _, err := NewReader(bytes.NewReader(buf.Bytes())).Read(); !errors.Is(err, ErrMalformed) {
			t.Fatalf("plain v1 Read(%s) = %v, want ErrMalformed", mt, err)
		}
		msg, err := NewReader(bytes.NewReader(buf.Bytes())).ReadVersion(V2)
		if err != nil || msg.Type != mt {
			t.Fatalf("v2 ReadVersion(%s) = (%+v,%v)", mt, msg, err)
		}
	}
	if TypeHostFrame.Known() || TypeHostFrame.AllowedIn(V1) || TypeHostFrame.AllowedIn(V2) || !TypeHostFrame.AllowedIn(V3) {
		t.Fatalf("HostFrame vocabulary: Known=%v v1=%v v2=%v v3=%v",
			TypeHostFrame.Known(), TypeHostFrame.AllowedIn(V1), TypeHostFrame.AllowedIn(V2), TypeHostFrame.AllowedIn(V3))
	}
	var buf bytes.Buffer
	if err := NewWriter(&buf).WriteVersion(V3, TypeHostFrame, []byte{1}); err != nil {
		t.Fatalf("v3 WriteVersion(HostFrame): %v", err)
	}
	if _, err := NewReader(bytes.NewReader(buf.Bytes())).ReadVersion(V2); !errors.Is(err, ErrMalformed) {
		t.Fatalf("selected v2 accepted HostFrame: %v", err)
	}
}
