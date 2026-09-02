package shimwire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/RenseiAI/donmai/attachwire"
)

// Generation is the monotonic controller-fencing number (§D4). The SHIM is
// authoritative for it: a daemon PROPOSES a strictly greater value in Welcome
// and the shim commits it before replying Adopted. A generation is a fencing
// value only — it can never create, release, or re-key a session (§D2).
type Generation uint64

// Hello is the shim's opening frame. It states what the shim is, what it can
// speak, and where its replay window currently sits, so the daemon can decide
// adopt-vs-quarantine WITHOUT mutating anything first.
type Hello struct {
	Protocol string `json:"protocol"`
	Min      uint32 `json:"protocolMin"`
	Max      uint32 `json:"protocolMax"`

	// OrgID + SessionID are the sole lifecycle identity (§D2). Everything else
	// on this struct is correlation or fencing.
	OrgID     string `json:"orgId"`
	SessionID string `json:"sessionId"`

	ShimID       string `json:"shimId"`
	ProcessEpoch uint64 `json:"processEpoch"`
	PID          int    `json:"pid"`
	// ProcessStartedAt is the OS-reported process start identity in Unix
	// nanoseconds. It is paired with PID everywhere because PID reuse is normal
	// and a bare PID match is not evidence (§D2).
	ProcessStartedAt int64 `json:"processStartedAt"`

	// HarnessPID and HarnessStartedAt identify the OWNED harness process, as
	// distinct from the shim that owns it.
	//
	// This pair is the operative evidence that a session survived a daemon
	// restart: adoption succeeding proves the shim is reachable, but only an
	// unchanged harness identity proves the WORKLOAD continued rather than being
	// restarted underneath a reused session id. It is deliberately not in the
	// discovery record — the record is what a dead shim leaves behind, while this
	// is what a live one asserts about the process it is still parenting.
	HarnessPID       int   `json:"harnessPid,omitempty"`
	HarnessStartedAt int64 `json:"harnessStartedAt,omitempty"`

	// WorkareaPath is the workarea the harness was launched against. The daemon
	// compares it to the session's own record so a shim cannot be adopted into a
	// different workarea than the one it is actually running in.
	WorkareaPath string `json:"workareaPath,omitempty"`

	Phase Phase `json:"phase"`

	// Generation is the generation the shim currently holds. A daemon must
	// propose strictly greater.
	Generation Generation `json:"generation"`

	// FirstSeq / LastSeq bound what the ring can still replay. FirstSeq is 0 when
	// nothing is buffered.
	FirstSeq uint64 `json:"firstSeq"`
	LastSeq  uint64 `json:"lastSeq"`

	// OrphanDeadlineUnixNano is when the shim will self-terminate if no
	// controller adopts it (§D8). Zero means no deadline is armed.
	OrphanDeadlineUnixNano int64 `json:"orphanDeadlineAt,omitempty"`

	Extensions Extensions `json:"extensions,omitempty"`
}

// Welcome is the daemon's adoption proposal.
type Welcome struct {
	Protocol string `json:"protocol"`
	// Selected is the single version chosen from the overlap.
	Selected uint32 `json:"selected"`

	// ControllerID identifies the controller process for diagnostics only.
	ControllerID string `json:"controllerId"`

	// ProposedGeneration MUST be strictly greater than Hello.Generation.
	ProposedGeneration Generation `json:"proposedGeneration"`

	// ResumeFrom is the first sequence the daemon still needs, i.e.
	// last_forwarded_seq + 1. The daemon never allocates sequence; it only
	// states where its durable record stopped (§D5).
	ResumeFrom uint64 `json:"resumeFrom"`

	Extensions Extensions `json:"extensions,omitempty"`
}

// Adopted is the shim's commit. It reports the generation it actually committed
// (never merely the one proposed) and the EXACT replay disposition, so the
// daemon learns "you will get a gap" before any output arrives.
type Adopted struct {
	Generation Generation `json:"generation"`
	// Extensions exactly echo Welcome.Extensions, proving the shim committed the
	// carrier correlation paired with this controller generation.
	Extensions Extensions `json:"extensions,omitempty"`
	// Contiguous is true when the requested ResumeFrom is servable from the ring.
	// False means a Gap + Snapshot precede live output.
	Contiguous bool `json:"contiguous"`
	// ReplayFrom / ReplayTo bound what will actually be replayed. Equal-to-zero
	// means nothing is buffered to replay.
	ReplayFrom uint64 `json:"replayFrom"`
	ReplayTo   uint64 `json:"replayTo"`
	Phase      Phase  `json:"phase"`
}

// GapMsg declares a missing INCLUSIVE range and why it is missing. Emitting this
// rather than renumbering is the honesty requirement in §D5: no daemon or
// carrier may invent missing output while claiming continuity.
type GapMsg struct {
	FromSeq uint64    `json:"fromSeq"`
	ToSeq   uint64    `json:"toSeq"`
	Reason  GapReason `json:"reason"`
}

// SnapshotMsg carries an attachwire-encoded screen and the host sequence it
// reflects. After a Gap, AtSeq is always > GapMsg.ToSeq.
type SnapshotMsg struct {
	AtSeq  uint64 `json:"atSeq"`
	Screen []byte `json:"screen"`
}

// SnapshotMode is the closed v2 authoritative-snapshot operation set.
type SnapshotMode uint8

const (
	// SnapshotInspect reads exact screen bytes without allocating sequence.
	SnapshotInspect SnapshotMode = 1
	// SnapshotEmit allocates/delivers one exact interactive-attach frame.
	SnapshotEmit SnapshotMode = 2
)

// Known reports whether m is an assigned selected-v2 snapshot mode.
func (m SnapshotMode) Known() bool { return m == SnapshotInspect || m == SnapshotEmit }

func (m SnapshotMode) String() string {
	if m == SnapshotInspect {
		return "inspect"
	}
	if m == SnapshotEmit {
		return "emit"
	}
	return fmt.Sprintf("unknown(%d)", m)
}

// SnapshotRequest is the selected-v2 request correlation. RequestID is local
// to one controller connection and never creates lifecycle identity.
type SnapshotRequest struct {
	RequestID  uint64
	Generation Generation
	Mode       SnapshotMode
}

// SnapshotResult carries an opaque exact byte result. For inspect, Bytes is the
// attachwire Screen encoding and AtSeq names the state it describes. For emit,
// Bytes is the complete encoded interactive-attach Snapshot frame.
type SnapshotResult struct {
	RequestID  uint64
	Generation Generation
	Mode       SnapshotMode
	Code       ErrorCode
	AtSeq      uint64
	InStream   bool
	Bytes      []byte
}

// HostFrame is the selected-v3 exact observation. RequestID is zero for
// ordinary/replayed frames and non-zero only for the live Snapshot emitted by
// that connection-local SnapshotRequest. FrameBytes are the complete canonical
// interactive-attach frame bytes.
type HostFrame struct {
	RequestID  uint64
	FrameBytes []byte
}

const hostFrameHeaderLen = 8

// MaxHostFrameBytes is the largest exact attach frame that fits below the
// existing shimwire message ceiling after the type byte and v3 request header.
const MaxHostFrameBytes = MaxMessageBytes - 1 - hostFrameHeaderLen

// EncodeHostFrame validates and preserves one exact canonical attach frame.
func EncodeHostFrame(hostFrame HostFrame) ([]byte, error) {
	if err := validateHostFrame(hostFrame); err != nil {
		return nil, err
	}
	body := make([]byte, hostFrameHeaderLen+len(hostFrame.FrameBytes))
	binary.BigEndian.PutUint64(body[:hostFrameHeaderLen], hostFrame.RequestID)
	copy(body[hostFrameHeaderLen:], hostFrame.FrameBytes)
	return body, nil
}

// DecodeHostFrame strictly decodes and independently owns one v3 observation.
func DecodeHostFrame(body []byte) (HostFrame, error) {
	if len(body) <= hostFrameHeaderLen {
		return HostFrame{}, fmt.Errorf("shimwire: %w: HostFrame body %d bytes, need > %d", ErrMalformed, len(body), hostFrameHeaderLen)
	}
	hostFrame := HostFrame{
		RequestID:  binary.BigEndian.Uint64(body[:hostFrameHeaderLen]),
		FrameBytes: append([]byte(nil), body[hostFrameHeaderLen:]...),
	}
	if err := validateHostFrame(hostFrame); err != nil {
		return HostFrame{}, err
	}
	return hostFrame, nil
}

func validateHostFrame(hostFrame HostFrame) error {
	if len(hostFrame.FrameBytes) == 0 {
		return fmt.Errorf("shimwire: %w: HostFrame has no frame bytes", ErrMalformed)
	}
	if len(hostFrame.FrameBytes) > MaxHostFrameBytes {
		return fmt.Errorf("shimwire: %w: HostFrame has %d frame bytes", ErrMessageTooLarge, len(hostFrame.FrameBytes))
	}
	frame, err := attachwire.DecodeFrame(hostFrame.FrameBytes)
	if err != nil || !bytes.Equal(frame.Encode(), hostFrame.FrameBytes) {
		return fmt.Errorf("shimwire: %w: HostFrame is not one canonical attach frame", ErrMalformed)
	}
	if frame.Seq == 0 {
		return fmt.Errorf("shimwire: %w: HostFrame sequence must be positive", ErrMalformed)
	}
	switch frame.Type {
	case attachwire.TypeOutput:
		if hostFrame.RequestID != 0 {
			return fmt.Errorf("shimwire: %w: non-Snapshot HostFrame carries request id", ErrMalformed)
		}
	case attachwire.TypeResize:
		if hostFrame.RequestID != 0 {
			return fmt.Errorf("shimwire: %w: non-Snapshot HostFrame carries request id", ErrMalformed)
		}
		if _, err := attachwire.DecodeResize(frame.Payload); err != nil {
			return fmt.Errorf("shimwire: %w: invalid applied Resize HostFrame", ErrMalformed)
		}
	case attachwire.TypeMarker:
		if hostFrame.RequestID != 0 {
			return fmt.Errorf("shimwire: %w: non-Snapshot HostFrame carries request id", ErrMalformed)
		}
		if _, err := attachwire.DecodeMarker(frame.Payload); err != nil {
			return fmt.Errorf("shimwire: %w: invalid Marker HostFrame", ErrMalformed)
		}
	case attachwire.TypeSnapshot:
		if _, err := attachwire.DecodeSnapshotEnvelope(frame.Payload); err != nil {
			return fmt.Errorf("shimwire: %w: invalid Snapshot HostFrame", ErrMalformed)
		}
	case attachwire.TypeExit:
		if hostFrame.RequestID != 0 {
			return fmt.Errorf("shimwire: %w: non-Snapshot HostFrame carries request id", ErrMalformed)
		}
		if _, err := attachwire.DecodeExit(frame.Payload); err != nil {
			return fmt.Errorf("shimwire: %w: invalid Exit HostFrame", ErrMalformed)
		}
	default:
		return fmt.Errorf("shimwire: %w: attach type %s is not a host-sequence observation", ErrMalformed, frame.Type)
	}
	return nil
}

const (
	snapshotRequestLen      = 17
	snapshotResultHeaderLen = 27
)

// EncodeSnapshotRequest uses a fixed binary header; arbitrary result bytes are
// never routed through JSON/base64.
func EncodeSnapshotRequest(r SnapshotRequest) ([]byte, error) {
	if r.RequestID == 0 || r.Generation == 0 || !r.Mode.Known() {
		return nil, fmt.Errorf("shimwire: %w: invalid snapshot request id=%d generation=%d mode=%s", ErrMalformed, r.RequestID, r.Generation, r.Mode)
	}
	b := make([]byte, snapshotRequestLen)
	binary.BigEndian.PutUint64(b[0:8], r.RequestID)
	binary.BigEndian.PutUint64(b[8:16], uint64(r.Generation))
	b[16] = byte(r.Mode)
	return b, nil
}

// DecodeSnapshotRequest strictly decodes the fixed v2 request body.
func DecodeSnapshotRequest(body []byte) (SnapshotRequest, error) {
	if len(body) != snapshotRequestLen {
		return SnapshotRequest{}, fmt.Errorf("shimwire: %w: snapshot request body %d bytes, want %d", ErrMalformed, len(body), snapshotRequestLen)
	}
	r := SnapshotRequest{RequestID: binary.BigEndian.Uint64(body[0:8]), Generation: Generation(binary.BigEndian.Uint64(body[8:16])), Mode: SnapshotMode(body[16])}
	if r.RequestID == 0 || r.Generation == 0 {
		return SnapshotRequest{}, fmt.Errorf("shimwire: %w: invalid snapshot request id=%d generation=%d mode=%s", ErrMalformed, r.RequestID, r.Generation, r.Mode)
	}
	return r, nil
}

var snapshotResultCodes = map[ErrorCode]byte{
	"": 0, CodeMalformed: 1, CodeStaleGeneration: 2, CodeDuplicateChanged: 3,
	CodeRequestLedgerFull: 4, CodeExited: 5, CodeInternal: 6, CodeRequestMismatch: 7, CodeTimeout: 8,
}

var snapshotResultCodesByByte = func() map[byte]ErrorCode {
	m := make(map[byte]ErrorCode, len(snapshotResultCodes))
	for code, value := range snapshotResultCodes {
		m[value] = code
	}
	return m
}()

// MaxSnapshotResultBytes is the largest verbatim payload a SnapshotResult can
// carry and still fit one wire message, after the type byte and fixed header.
//
// It exists so a producer can decide whether a result fits by ARITHMETIC rather
// than by encoding it to find out: the header is fixed-width and the payload is
// copied verbatim, so the encoded size is exactly known in advance. The bounded
// Snapshot path measures millions of bytes per emit; making it encode twice to
// learn a number it can compute is pure waste.
const MaxSnapshotResultBytes = MaxMessageBytes - 1 - snapshotResultHeaderLen

// SnapshotResultMessageBytes is the exact framed size — type byte plus body —
// that EncodeSnapshotResult will produce for a payload of payloadLen bytes. It
// is the number Writer compares against MaxMessageBytes.
func SnapshotResultMessageBytes(payloadLen int) int {
	return 1 + snapshotResultHeaderLen + payloadLen
}

// EncodeSnapshotResult preserves Bytes verbatim after a fixed binary header.
func EncodeSnapshotResult(r SnapshotResult) ([]byte, error) {
	status, ok := snapshotResultCodes[r.Code]
	if !ok || r.RequestID == 0 || r.Generation == 0 || (r.Code == "" && !r.Mode.Known()) {
		return nil, fmt.Errorf("shimwire: %w: invalid snapshot result correlation/status", ErrMalformed)
	}
	if r.Code != "" && (r.AtSeq != 0 || r.InStream || len(r.Bytes) != 0) {
		return nil, fmt.Errorf("shimwire: %w: refused snapshot result carries success fields", ErrMalformed)
	}
	b := make([]byte, snapshotResultHeaderLen+len(r.Bytes))
	binary.BigEndian.PutUint64(b[0:8], r.RequestID)
	binary.BigEndian.PutUint64(b[8:16], uint64(r.Generation))
	b[16], b[17] = byte(r.Mode), status
	binary.BigEndian.PutUint64(b[18:26], r.AtSeq)
	if r.InStream {
		b[26] = 1
	}
	copy(b[snapshotResultHeaderLen:], r.Bytes)
	return b, nil
}

// DecodeSnapshotResult strictly decodes the fixed header and opaque tail.
func DecodeSnapshotResult(body []byte) (SnapshotResult, error) {
	if len(body) < snapshotResultHeaderLen {
		return SnapshotResult{}, fmt.Errorf("shimwire: %w: snapshot result body %d bytes, need >= %d", ErrMalformed, len(body), snapshotResultHeaderLen)
	}
	code, ok := snapshotResultCodesByByte[body[17]]
	r := SnapshotResult{RequestID: binary.BigEndian.Uint64(body[0:8]), Generation: Generation(binary.BigEndian.Uint64(body[8:16])), Mode: SnapshotMode(body[16]), Code: code, AtSeq: binary.BigEndian.Uint64(body[18:26]), InStream: body[26] == 1, Bytes: append([]byte(nil), body[snapshotResultHeaderLen:]...)}
	if !ok || body[26] > 1 || r.RequestID == 0 || r.Generation == 0 || (r.Code == "" && !r.Mode.Known()) {
		return SnapshotResult{}, fmt.Errorf("shimwire: %w: invalid snapshot result correlation/status", ErrMalformed)
	}
	if r.Code != "" && (r.AtSeq != 0 || r.InStream || len(r.Bytes) != 0) {
		return SnapshotResult{}, fmt.Errorf("shimwire: %w: refused snapshot result carries success fields", ErrMalformed)
	}
	return r, nil
}

// ResizeMsg is authoritative geometry from the controller.
type ResizeMsg struct {
	Generation Generation `json:"generation"`
	Cols       uint32     `json:"cols"`
	Rows       uint32     `json:"rows"`
	PxWidth    uint32     `json:"pxWidth"`
	PxHeight   uint32     `json:"pxHeight"`
}

// StopMsg is a generation-fenced stop with a typed reason.
type StopMsg struct {
	Generation Generation `json:"generation"`
	Reason     StopReason `json:"reason"`
}

// HeartbeatMsg is bidirectional liveness. From the daemon it also ACKNOWLEDGES
// output: AckedSeq is the highest sequence the daemon has durably forwarded, and
// it is what a later adoption resumes from.
type HeartbeatMsg struct {
	Generation Generation `json:"generation,omitempty"`
	AckedSeq   uint64     `json:"ackedSeq,omitempty"`
	Phase      Phase      `json:"phase,omitempty"`
}

// ExitMsg is the immutable terminal observation. Once emitted it never changes,
// and it is the only thing that closes the lifecycle loop (§D10).
type ExitMsg struct {
	Seq      uint64 `json:"seq"`
	ExitCode uint64 `json:"exitCode"`
	Signal   string `json:"signal,omitempty"`
}

// ErrorMsg is a closed code plus display-only detail.
type ErrorMsg struct {
	Code   ErrorCode `json:"code"`
	Detail string    `json:"detail,omitempty"`
}

// ---- typed encode/decode ---------------------------------------------------

// encodeJSON marshals a control body. Marshalling these fixed structs cannot
// fail in practice, but the error is still surfaced rather than dropped.
func encodeJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("shimwire: encode: %w", err)
	}
	return b, nil
}

// decodeJSON decodes a control body STRICTLY: an unknown field is a protocol
// error. Strictness is the point — a field a peer does not understand is a
// capability it does not have, and accepting it silently is the "protocol
// mismatch becomes a silent downgrade" failure the ADR forbids.
func decodeJSON(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("shimwire: %w: %v", ErrMalformed, err)
	}
	// Prove EOF. Decoder.More reports array/object membership and does not reject
	// a second top-level document.
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("shimwire: %w: trailing document after message body", ErrMalformed)
		}
		return fmt.Errorf("shimwire: %w: trailing bytes after message body: %v", ErrMalformed, err)
	}
	return nil
}

// EncodeHello encodes a Hello body.
func EncodeHello(h Hello) ([]byte, error) { return encodeJSON(h) }

// DecodeHello strictly decodes a Hello body.
func DecodeHello(body []byte) (Hello, error) {
	var h Hello
	err := decodeJSON(body, &h)
	return h, err
}

// EncodeWelcome encodes a Welcome body.
func EncodeWelcome(w Welcome) ([]byte, error) { return encodeJSON(w) }

// DecodeWelcome strictly decodes a Welcome body.
func DecodeWelcome(body []byte) (Welcome, error) {
	var w Welcome
	err := decodeJSON(body, &w)
	return w, err
}

// EncodeAdopted encodes an Adopted body.
func EncodeAdopted(a Adopted) ([]byte, error) { return encodeJSON(a) }

// DecodeAdopted strictly decodes an Adopted body.
func DecodeAdopted(body []byte) (Adopted, error) {
	var a Adopted
	err := decodeJSON(body, &a)
	return a, err
}

// EncodeGap encodes a Gap body.
func EncodeGap(g GapMsg) ([]byte, error) { return encodeJSON(g) }

// DecodeGap strictly decodes a Gap body and rejects an unknown reason or an
// inverted range.
func DecodeGap(body []byte) (GapMsg, error) {
	var g GapMsg
	if err := decodeJSON(body, &g); err != nil {
		return g, err
	}
	if !g.Reason.Known() {
		return g, fmt.Errorf("shimwire: %w: unknown gap reason %q", ErrMalformed, g.Reason)
	}
	if g.FromSeq > g.ToSeq {
		return g, fmt.Errorf("shimwire: %w: inverted gap range [%d,%d]", ErrMalformed, g.FromSeq, g.ToSeq)
	}
	return g, nil
}

// EncodeSnapshot encodes a Snapshot body.
func EncodeSnapshot(s SnapshotMsg) ([]byte, error) { return encodeJSON(s) }

// DecodeSnapshot strictly decodes a Snapshot body.
func DecodeSnapshot(body []byte) (SnapshotMsg, error) {
	var s SnapshotMsg
	err := decodeJSON(body, &s)
	return s, err
}

// EncodeResize encodes a Resize body.
func EncodeResize(r ResizeMsg) ([]byte, error) { return encodeJSON(r) }

// DecodeResize strictly decodes a Resize body.
func DecodeResize(body []byte) (ResizeMsg, error) {
	var r ResizeMsg
	err := decodeJSON(body, &r)
	return r, err
}

// EncodeStop encodes a Stop body.
func EncodeStop(s StopMsg) ([]byte, error) { return encodeJSON(s) }

// DecodeStop strictly decodes a Stop body and rejects an unknown reason.
func DecodeStop(body []byte) (StopMsg, error) {
	var s StopMsg
	if err := decodeJSON(body, &s); err != nil {
		return s, err
	}
	if !s.Reason.Known() {
		return s, fmt.Errorf("shimwire: %w: unknown stop reason %q", ErrMalformed, s.Reason)
	}
	return s, nil
}

// EncodeHeartbeat encodes a Heartbeat body.
func EncodeHeartbeat(h HeartbeatMsg) ([]byte, error) { return encodeJSON(h) }

// DecodeHeartbeat strictly decodes a Heartbeat body.
func DecodeHeartbeat(body []byte) (HeartbeatMsg, error) {
	var h HeartbeatMsg
	err := decodeJSON(body, &h)
	return h, err
}

// EncodeExit encodes an Exit body.
func EncodeExit(e ExitMsg) ([]byte, error) { return encodeJSON(e) }

// DecodeExit strictly decodes an Exit body.
func DecodeExit(body []byte) (ExitMsg, error) {
	var e ExitMsg
	err := decodeJSON(body, &e)
	return e, err
}

// EncodeError encodes an Error body.
func EncodeError(e ErrorMsg) ([]byte, error) { return encodeJSON(e) }

// DecodeError strictly decodes an Error body.
func DecodeError(body []byte) (ErrorMsg, error) {
	var e ErrorMsg
	err := decodeJSON(body, &e)
	return e, err
}

// ---- byte-carrying messages ------------------------------------------------

// outputHeaderLen is seq(u64) + relTime(u64).
const outputHeaderLen = 16

// EncodeOutput frames one shim-produced output chunk: a fixed binary header
// carrying the SHIM-ALLOCATED host sequence, then the raw terminal bytes
// verbatim. Terminal bytes never pass through a text encoding here — they are
// arbitrary binary and re-encoding them is both wasteful and a corruption risk.
func EncodeOutput(seq, relTime uint64, data []byte) []byte {
	buf := make([]byte, outputHeaderLen+len(data))
	binary.BigEndian.PutUint64(buf[0:8], seq)
	binary.BigEndian.PutUint64(buf[8:16], relTime)
	copy(buf[outputHeaderLen:], data)
	return buf
}

// DecodeOutput splits an Output body. The returned slice ALIASES body; callers
// that retain it past the read loop must copy (the reader hands out a fresh
// buffer per message, so the common path needs no copy).
func DecodeOutput(body []byte) (seq, relTime uint64, data []byte, err error) {
	if len(body) < outputHeaderLen {
		return 0, 0, nil, fmt.Errorf("shimwire: %w: output body %d bytes, need >= %d", ErrMalformed, len(body), outputHeaderLen)
	}
	seq = binary.BigEndian.Uint64(body[0:8])
	relTime = binary.BigEndian.Uint64(body[8:16])
	return seq, relTime, body[outputHeaderLen:], nil
}

// inputHeaderLen is generation(u64).
const inputHeaderLen = 8

// EncodeInput frames controller input: the fencing generation, then the input
// bytes verbatim. The generation rides in the header rather than a side channel
// precisely so the shim cannot accept the bytes without also seeing the fence.
func EncodeInput(gen Generation, data []byte) []byte {
	buf := make([]byte, inputHeaderLen+len(data))
	binary.BigEndian.PutUint64(buf[0:8], uint64(gen))
	copy(buf[inputHeaderLen:], data)
	return buf
}

// DecodeInput splits an Input body. The returned slice aliases body.
func DecodeInput(body []byte) (Generation, []byte, error) {
	if len(body) < inputHeaderLen {
		return 0, nil, fmt.Errorf("shimwire: %w: input body %d bytes, need >= %d", ErrMalformed, len(body), inputHeaderLen)
	}
	return Generation(binary.BigEndian.Uint64(body[0:8])), body[inputHeaderLen:], nil
}

// attributedInputHeaderLen is generation(u64) + userIdLen(u16).
const attributedInputHeaderLen = 8 + 2

// maxAttributedInputUserID bounds the relay-stamped userId this frame can
// carry — comfortably above any real platform-issued id or the shared SYSTEM
// sentinel (attachwire.SystemNudgeUserID) — while keeping the length prefix a
// fixed 2 bytes.
const maxAttributedInputUserID = 1<<16 - 1

// EncodeAttributedInput frames v4 AttributedInput (TypeAttributedInput, legal
// only at selected v4+): the fencing generation, then the relay-stamped
// userId length-prefixed so a decoder never has to guess where it ends and
// the input bytes begin, then the input bytes verbatim.
//
// This is a NEW message type carrying a NEW payload shape — it does not
// change EncodeInput/DecodeInput or TypeInput's byte-identical selected
// v1/v2/v3 wire in any way (see the V4 doc in version.go for why: the corpus
// treats "change what an existing selected version's bytes mean" as the one
// unacceptable move, and "add a new type at a new selected version" as the
// compatible one).
func EncodeAttributedInput(gen Generation, userID, data []byte) ([]byte, error) {
	if len(userID) > maxAttributedInputUserID {
		return nil, fmt.Errorf("shimwire: %w: attributed input userId %d bytes, max %d", ErrMalformed, len(userID), maxAttributedInputUserID)
	}
	buf := make([]byte, attributedInputHeaderLen+len(userID)+len(data))
	binary.BigEndian.PutUint64(buf[0:8], uint64(gen))
	binary.BigEndian.PutUint16(buf[8:10], uint16(len(userID))) //nolint:gosec // G115: bounded by maxAttributedInputUserID above
	copy(buf[attributedInputHeaderLen:attributedInputHeaderLen+len(userID)], userID)
	copy(buf[attributedInputHeaderLen+len(userID):], data)
	return buf, nil
}

// DecodeAttributedInput splits an AttributedInput body. The returned
// userID/data slices alias body.
func DecodeAttributedInput(body []byte) (gen Generation, userID, data []byte, err error) {
	if len(body) < attributedInputHeaderLen {
		return 0, nil, nil, fmt.Errorf("shimwire: %w: attributed input body %d bytes, need >= %d", ErrMalformed, len(body), attributedInputHeaderLen)
	}
	gen = Generation(binary.BigEndian.Uint64(body[0:8]))
	userIDLen := int(binary.BigEndian.Uint16(body[8:10]))
	if len(body) < attributedInputHeaderLen+userIDLen {
		return 0, nil, nil, fmt.Errorf("shimwire: %w: attributed input declared userId length %d exceeds body", ErrMalformed, userIDLen)
	}
	userID = body[attributedInputHeaderLen : attributedInputHeaderLen+userIDLen]
	data = body[attributedInputHeaderLen+userIDLen:]
	return gen, userID, data, nil
}
