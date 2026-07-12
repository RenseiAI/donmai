package attachwire

// §3.1 typed payload codecs. Each event type's payload has encode + decode +
// validation. Snapshot's snap serialization (§12.1) lives in snapshot.go; the
// Exit convention helpers (§12.2) live in exit.go.

// --- Output (0x01) -----------------------------------------------------------

// OutputPayload carries raw terminal output bytes (§3.1). On the host→relay leg
// these are raw PTY master bytes; on the relay→viewer leg they MUST have passed
// the §9 sanitization allowlist (enforced outside this framing library).
type OutputPayload struct {
	Data []byte
}

// EncodeOutput returns the Output payload: the data bytes are the entire
// payload (the frame boundary supplies the length).
func EncodeOutput(data []byte) []byte { return cloneBytes(data) }

// Encode serializes the Output payload.
func (p OutputPayload) Encode() []byte { return EncodeOutput(p.Data) }

// DecodeOutput interprets a frame payload as Output: the whole payload is the
// data. It never fails.
func DecodeOutput(payload []byte) OutputPayload {
	return OutputPayload{Data: cloneBytes(payload)}
}

// --- Input (0x02) ------------------------------------------------------------

// InputPayload is the §5 Input layout:
// [inputSeq][penGeneration][userIdLen][userId][dataLen][data].
type InputPayload struct {
	// InputSeq is the producing connection's monotonic input sequence (§4/§5).
	InputSeq uint64
	// PenGeneration is the pen generation the sender believes current — a
	// staleness guard, NOT an authorization credential (§5).
	PenGeneration uint64
	// UserID is relay-stamped and never client-supplied (§5): a viewer MUST send
	// it empty (the "empty-userId form", UserID == nil / len 0); the relay
	// stamps the verified id before forwarding to the host. The host MUST reject
	// any Input arriving unstamped.
	UserID []byte
	// Data is the already-encoded terminal input bytes, written verbatim to the
	// PTY stdin sink and NOT re-sanitized (§5).
	Data []byte
}

// Stamped reports whether UserID has been relay-stamped (§5). The host rejects
// unstamped Input (Stamped() == false).
func (p InputPayload) Stamped() bool { return len(p.UserID) > 0 }

// Encode serializes the Input payload (§5).
func (p InputPayload) Encode() []byte {
	buf := make([]byte, 0, 4*MaxVarintLen+len(p.UserID)+len(p.Data))
	buf = AppendUvarint(buf, p.InputSeq)
	buf = AppendUvarint(buf, p.PenGeneration)
	buf = AppendUvarint(buf, uint64(len(p.UserID)))
	buf = append(buf, p.UserID...)
	buf = AppendUvarint(buf, uint64(len(p.Data)))
	buf = append(buf, p.Data...)
	return buf
}

// EncodeViewerInput builds the client-side (unstamped) Input a viewer sends:
// UserID is empty (userIdLen = 0) per §5.
func EncodeViewerInput(inputSeq, penGeneration uint64, data []byte) []byte {
	return InputPayload{InputSeq: inputSeq, PenGeneration: penGeneration, Data: data}.Encode()
}

// DecodeInput parses an Input payload (§5). userIdLen = 0 yields UserID == nil
// (the empty-userId form). A declared length past the buffer is a framing
// error.
func DecodeInput(payload []byte) (InputPayload, error) {
	r := newReader(payload)
	inputSeq, err := r.uvarint()
	if err != nil {
		return InputPayload{}, err
	}
	penGen, err := r.uvarint()
	if err != nil {
		return InputPayload{}, err
	}
	userID, err := r.lenPrefixed()
	if err != nil {
		return InputPayload{}, err
	}
	data, err := r.lenPrefixed()
	if err != nil {
		return InputPayload{}, err
	}
	if err := r.expectDone(); err != nil {
		return InputPayload{}, err
	}
	return InputPayload{InputSeq: inputSeq, PenGeneration: penGen, UserID: userID, Data: data}, nil
}

// --- Resize (0x03) -----------------------------------------------------------

// ResizePayload is terminal geometry (§3.1, §8). pxWidth/pxHeight MAY be 0 when
// unknown; cols == 0 || rows == 0 is a framing error on every leg.
type ResizePayload struct {
	Cols     uint64
	Rows     uint64
	PxWidth  uint64
	PxHeight uint64
}

// Validate enforces the §3.1/§8 geometry rule: cols == 0 || rows == 0 is a
// framing error (a 0×N terminal does not exist; sub-1×1 is a DoS vector).
func (p ResizePayload) Validate() error {
	if p.Cols == 0 || p.Rows == 0 {
		return newFramingf("resize with cols=%d rows=%d (cols==0 || rows==0)", p.Cols, p.Rows)
	}
	return nil
}

// Encode serializes the Resize payload, rejecting an invalid geometry (§8).
func (p ResizePayload) Encode() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	buf := make([]byte, 0, 4*MaxVarintLen)
	buf = AppendUvarint(buf, p.Cols)
	buf = AppendUvarint(buf, p.Rows)
	buf = AppendUvarint(buf, p.PxWidth)
	buf = AppendUvarint(buf, p.PxHeight)
	return buf, nil
}

// DecodeResize parses and validates a Resize payload (§3.1, §8).
func DecodeResize(payload []byte) (ResizePayload, error) {
	r := newReader(payload)
	cols, err := r.uvarint()
	if err != nil {
		return ResizePayload{}, err
	}
	rows, err := r.uvarint()
	if err != nil {
		return ResizePayload{}, err
	}
	pxW, err := r.uvarint()
	if err != nil {
		return ResizePayload{}, err
	}
	pxH, err := r.uvarint()
	if err != nil {
		return ResizePayload{}, err
	}
	if err := r.expectDone(); err != nil {
		return ResizePayload{}, err
	}
	p := ResizePayload{Cols: cols, Rows: rows, PxWidth: pxW, PxHeight: pxH}
	if err := p.Validate(); err != nil {
		return ResizePayload{}, err
	}
	return p, nil
}

// --- Marker (0x04) -----------------------------------------------------------

// MarkerPayload is a UTF-8 annotation label for recording/replay (§3.1);
// display-only, never executed. When rendered as viewer UI text it is
// length-capped and control-char-stripped (§9) — that treatment is a viewer
// concern, not part of framing.
type MarkerPayload struct {
	Label string
}

// Encode serializes the Marker payload.
func (p MarkerPayload) Encode() []byte {
	buf := make([]byte, 0, MaxVarintLen+len(p.Label))
	buf = AppendUvarint(buf, uint64(len(p.Label)))
	buf = append(buf, p.Label...)
	return buf
}

// DecodeMarker parses a Marker payload (§3.1).
func DecodeMarker(payload []byte) (MarkerPayload, error) {
	r := newReader(payload)
	label, err := r.lenPrefixed()
	if err != nil {
		return MarkerPayload{}, err
	}
	if err := r.expectDone(); err != nil {
		return MarkerPayload{}, err
	}
	return MarkerPayload{Label: string(label)}, nil
}

// --- Snapshot envelope (0x06) ------------------------------------------------

// SnapFormatScreen is snapFormat 0x01 = VT-serialized screen (§12.1).
const SnapFormatScreen uint8 = 0x01

// SnapshotEnvelope is the §3.1 Snapshot layout:
// [atSeq][snapFormat:u8][snapLen][snap]. The envelope is v1-frozen; the snap
// serialization for a given snapFormat is v1-draft (see snapshot.go for
// snapFormat 0x01). atSeq is the host output sequence the screen reflects
// (§12); post-Exit snapshots carry header seq 0 but atSeq = the Exit seq
// (§12.2).
type SnapshotEnvelope struct {
	AtSeq      uint64
	SnapFormat uint8
	Snap       []byte
}

// Encode serializes the Snapshot envelope (§3.1).
func (e SnapshotEnvelope) Encode() []byte {
	buf := make([]byte, 0, 2*MaxVarintLen+1+len(e.Snap))
	buf = AppendUvarint(buf, e.AtSeq)
	buf = append(buf, e.SnapFormat)
	buf = AppendUvarint(buf, uint64(len(e.Snap)))
	buf = append(buf, e.Snap...)
	return buf
}

// DecodeSnapshotEnvelope parses the frozen Snapshot envelope (§3.1). The snap
// bytes are returned uninterpreted; decode them with DecodeScreen when
// SnapFormat == SnapFormatScreen.
func DecodeSnapshotEnvelope(payload []byte) (SnapshotEnvelope, error) {
	r := newReader(payload)
	atSeq, err := r.uvarint()
	if err != nil {
		return SnapshotEnvelope{}, err
	}
	sf, err := r.readByte()
	if err != nil {
		return SnapshotEnvelope{}, err
	}
	snap, err := r.lenPrefixed()
	if err != nil {
		return SnapshotEnvelope{}, err
	}
	if err := r.expectDone(); err != nil {
		return SnapshotEnvelope{}, err
	}
	return SnapshotEnvelope{AtSeq: atSeq, SnapFormat: sf, Snap: snap}, nil
}

// --- Control (0x07) ----------------------------------------------------------

// EncodeControlPayload wraps a JSON control object as the §3.1 Control payload:
// [jsonLen][json]. See control.go for the typed message set and BuildControlFrame.
func EncodeControlPayload(jsonObj []byte) []byte {
	buf := make([]byte, 0, MaxVarintLen+len(jsonObj))
	buf = AppendUvarint(buf, uint64(len(jsonObj)))
	buf = append(buf, jsonObj...)
	return buf
}

// DecodeControlPayload extracts the JSON bytes from a Control payload (§3.1).
// Decode the result with DecodeControl.
func DecodeControlPayload(payload []byte) ([]byte, error) {
	r := newReader(payload)
	j, err := r.lenPrefixed()
	if err != nil {
		return nil, err
	}
	if err := r.expectDone(); err != nil {
		return nil, err
	}
	return j, nil
}
