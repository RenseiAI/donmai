package shimwire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
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
	// Reject trailing bytes: two concatenated JSON documents in one body would
	// otherwise decode as the first alone.
	if dec.More() {
		return fmt.Errorf("shimwire: %w: trailing bytes after message body", ErrMalformed)
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
