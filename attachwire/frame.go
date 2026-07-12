package attachwire

// EventType is the one-byte §3 event-type discriminator. Values 0x00 and
// 0x08–0xFF are reserved; a receiver MUST treat an unknown type as a framing
// error (a new type is a protocol-version bump, not a v1 addition).
type EventType uint8

// The v1-frozen event-type registry (§3).
const (
	TypeOutput   EventType = 0x01 // host: raw terminal output bytes
	TypeInput    EventType = 0x02 // viewer: keystrokes + inputSeq + penGeneration + userId
	TypeResize   EventType = 0x03 // terminal geometry (policy hook, §8)
	TypeMarker   EventType = 0x04 // host: asciinema-style annotation label
	TypeExit     EventType = 0x05 // host: process exit code + optional signal (§12.2)
	TypeSnapshot EventType = 0x06 // host: current-screen snapshot at a sequence (§12)
	TypeControl  EventType = 0x07 // any: JSON control-plane message (§7)
)

// Known reports whether t is an assigned v1 event type. Anything else is a
// framing error at decode time (§3).
func (t EventType) Known() bool { return t >= TypeOutput && t <= TypeControl }

func (t EventType) String() string {
	switch t {
	case TypeOutput:
		return "Output"
	case TypeInput:
		return "Input"
	case TypeResize:
		return "Resize"
	case TypeMarker:
		return "Marker"
	case TypeExit:
		return "Exit"
	case TypeSnapshot:
		return "Snapshot"
	case TypeControl:
		return "Control"
	default:
		return "Unknown(0x" + hexByte(byte(t)) + ")"
	}
}

func hexByte(b byte) string {
	const digits = "0123456789ABCDEF"
	return string([]byte{digits[b>>4], digits[b&0x0F]})
}

// Frame is a single §2 wire message:
//
//	+----------+--------------+------------------+-------------------+
//	| type:u8  | seq:varint   | rel_time:varint  | payload:bytes...  |
//	+----------+--------------+------------------+-------------------+
//
// seq is interpreted in the producer's sequence namespace (§4); rel_time is
// microseconds since the producer's stream epoch anchor. On out-of-namespace
// frames (all Control in every direction, non-host-produced Resize, and the
// post-Exit Snapshot) both header fields are zero and receivers MUST ignore
// them (§2) — see NewControlFrame / NewViewportResizeFrame and
// RequiresZeroedHeaders.
type Frame struct {
	Type    EventType
	Seq     uint64
	RelTime uint64
	Payload []byte
}

// Encode serializes the frame to a fresh byte slice (§2). The WebSocket layer
// already delimits the message, so no total length is prepended.
func (f Frame) Encode() []byte {
	buf := make([]byte, 0, capHint(1+2*MaxVarintLen, len(f.Payload)))
	buf = append(buf, byte(f.Type))
	buf = AppendUvarint(buf, f.Seq)
	buf = AppendUvarint(buf, f.RelTime)
	buf = append(buf, f.Payload...)
	return buf
}

// EncodeFrame is the package-level form of Frame.Encode.
func EncodeFrame(f Frame) []byte { return f.Encode() }

// DecodeFrame parses a §2 frame. It enforces the two framing-layer rules:
// an unknown type byte is a framing error (§3), and a truncated or overflowing
// header varint is a framing error (§2.1). The payload is returned as an
// independent copy and is NOT further parsed here — use the DecodeXxx payload
// codecs (§3.1) for that.
func DecodeFrame(buf []byte) (Frame, error) {
	r := newReader(buf)
	tb, err := r.readByte()
	if err != nil {
		return Frame{}, newFraming("empty frame: missing type byte")
	}
	t := EventType(tb)
	if !t.Known() {
		return Frame{}, newFramingf("unknown event type 0x%02X", tb)
	}
	seq, err := r.uvarint()
	if err != nil {
		return Frame{}, err
	}
	rel, err := r.uvarint()
	if err != nil {
		return Frame{}, err
	}
	return Frame{Type: t, Seq: seq, RelTime: rel, Payload: r.remainingCopy()}, nil
}

// NewControlFrame builds a Control frame (§2 out-of-namespace rule): Control
// frames in every direction carry seq = 0 and rel_time = 0, and receivers MUST
// ignore both header fields. This constructor zeroes the headers for you.
func NewControlFrame(payload []byte) Frame {
	return Frame{Type: TypeControl, Payload: payload}
}

// NewViewportResizeFrame builds a non-host-produced Resize frame — a viewer
// viewport advertisement or the relay's authoritative geometry (§2, §8). These
// are out-of-namespace: seq = 0, rel_time = 0, headers ignored by receivers.
// The host's own applied-geometry Resize echo IS in the host output namespace
// and must instead be built with an explicit Seq/RelTime.
func NewViewportResizeFrame(payload []byte) Frame {
	return Frame{Type: TypeResize, Payload: payload}
}

// RequiresZeroedHeaders reports whether a frame of the given type, produced by
// the given party, is out-of-namespace and MUST carry seq = 0 / rel_time = 0
// with receivers ignoring both (§2). Control is always out-of-namespace;
// Resize is out-of-namespace only when NOT produced by the host (a host's
// applied-geometry echo rides the host output sequence). All other host-produced
// types are in-namespace. The post-Exit Snapshot is a separate §12.2 case the
// producer handles explicitly.
func RequiresZeroedHeaders(t EventType, producedByHost bool) bool {
	switch t {
	case TypeControl:
		return true
	case TypeResize:
		return !producedByHost
	default:
		return false
	}
}
