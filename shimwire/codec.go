package shimwire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// MaxMessageBytes bounds one wire message (type + body).
//
// The bound sits well above the PTY host's 32 KiB per-Output cap so an ordinary
// frame is never near it, and far below anything that would let a malformed or
// hostile length field drive an unbounded allocation. The length is validated
// BEFORE the body buffer is allocated, which is the whole reason the bound
// exists.
const MaxMessageBytes = 1 << 20 // 1 MiB

// lengthPrefixLen is the u32 big-endian length prefix.
const lengthPrefixLen = 4

// Message is one decoded wire message. Body is the type-specific body; use the
// typed Decode* helpers in payloads.go to interpret it.
type Message struct {
	Type MessageType
	Body []byte
}

// Writer serialises messages onto a stream. It is safe for concurrent use: the
// shim's output pump and its heartbeat both write to the same connection, and a
// torn message would desynchronise the length framing permanently.
type Writer struct {
	mu  sync.Mutex
	w   io.Writer
	hdr [lengthPrefixLen + 1]byte
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write emits one length-delimited message. A body above MaxMessageBytes is
// refused locally rather than emitted for the peer to reject, so an oversized
// message is a bug at its source.
func (x *Writer) Write(t MessageType, body []byte) error {
	if !t.AllowedIn(V1) {
		return fmt.Errorf("shimwire: write: %w: unknown message type 0x%s", ErrMalformed, hexByte(byte(t)))
	}
	return x.writeAssigned(t, body)
}

func (x *Writer) writeAssigned(t MessageType, body []byte) error {
	total := 1 + len(body)
	if total > MaxMessageBytes {
		return fmt.Errorf("shimwire: write %s: %w: %d bytes", t, ErrMessageTooLarge, total)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.writeAssignedLocked(t, body, total)
}

func (x *Writer) writeAssignedLocked(t MessageType, body []byte, total int) error {
	//nolint:gosec // G115: total is bounded by MaxMessageBytes (1 MiB) three lines above
	binary.BigEndian.PutUint32(x.hdr[0:lengthPrefixLen], uint32(total))
	x.hdr[lengthPrefixLen] = byte(t)
	if _, err := x.w.Write(x.hdr[:]); err != nil {
		return fmt.Errorf("shimwire: write %s header: %w", t, err)
	}
	if len(body) > 0 {
		if _, err := x.w.Write(body); err != nil {
			return fmt.Errorf("shimwire: write %s body: %w", t, err)
		}
	}
	return nil
}

// WriteMessage is the Message-shaped form of Write.
func (x *Writer) WriteMessage(m Message) error { return x.Write(m.Type, m.Body) }

// WriteVersion emits only a type legal in the selected closed vocabulary.
func (x *Writer) WriteVersion(version uint32, t MessageType, body []byte) error {
	if !t.AllowedIn(version) {
		return fmt.Errorf("shimwire: write %s: %w: type is not legal in selected v%d", t, ErrMalformed, version)
	}
	return x.writeAssigned(t, body)
}

// WriteVersionBatch validates every message first, then writes the complete
// batch under one stream lock. Selected-v3 uses this to make the live
// HostFrame/SnapshotResult pair adjacent to every other shim write.
func (x *Writer) WriteVersionBatch(version uint32, messages ...Message) error {
	totals := make([]int, len(messages))
	for i, message := range messages {
		if !message.Type.AllowedIn(version) {
			return fmt.Errorf("shimwire: write %s: %w: type is not legal in selected v%d", message.Type, ErrMalformed, version)
		}
		totals[i] = 1 + len(message.Body)
		if totals[i] > MaxMessageBytes {
			return fmt.Errorf("shimwire: write %s: %w: %d bytes", message.Type, ErrMessageTooLarge, totals[i])
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for i, message := range messages {
		if err := x.writeAssignedLocked(message.Type, message.Body, totals[i]); err != nil {
			return err
		}
	}
	return nil
}

// Reader deserialises messages from a stream. Unlike Writer it is NOT safe for
// concurrent use — a protocol stream has exactly one reader by construction.
type Reader struct {
	r   *bufio.Reader
	hdr [lengthPrefixLen]byte
}

// NewReader wraps r with its own buffering.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 64<<10)}
}

// Read returns the next message. A clean end of stream is reported as io.EOF
// verbatim so a caller can distinguish "the peer closed" (an ordinary
// controller-loss event) from "the peer sent garbage" (a quarantine trigger).
func (x *Reader) Read() (Message, error) {
	return x.readVersion(V1)
}

func (x *Reader) readVersion(version uint32) (Message, error) {
	if _, err := io.ReadFull(x.r, x.hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return Message{}, io.EOF
		}
		return Message{}, fmt.Errorf("shimwire: read length: %w", err)
	}
	total := binary.BigEndian.Uint32(x.hdr[:])
	if total == 0 {
		return Message{}, fmt.Errorf("shimwire: read: %w: zero-length message", ErrMalformed)
	}
	if total > MaxMessageBytes {
		// Refuse BEFORE allocating: this is the anti-amplification bound.
		return Message{}, fmt.Errorf("shimwire: read: %w: declared %d bytes, max %d", ErrMessageTooLarge, total, MaxMessageBytes)
	}
	buf := make([]byte, total)
	if _, err := io.ReadFull(x.r, buf); err != nil {
		return Message{}, fmt.Errorf("shimwire: read body: %w", err)
	}
	t := MessageType(buf[0])
	if !t.AllowedIn(version) {
		return Message{}, fmt.Errorf("shimwire: read: %w: unknown message type 0x%s", ErrMalformed, hexByte(buf[0]))
	}
	return Message{Type: t, Body: buf[1:]}, nil
}

// ReadVersion reads one message and refuses a type outside the selected closed
// vocabulary. Handshake callers use Read until a version has been selected.
func (x *Reader) ReadVersion(version uint32) (Message, error) {
	return x.readVersion(version)
}
