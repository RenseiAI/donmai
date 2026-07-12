package attachwire

import "encoding/base64"

// §14 degraded SSE fallback lane — batch envelope types for BOTH directions and
// the POST response taxonomy. This library carries the wire TYPES only; the
// transport (SSE/HTTP), endpoint derivation, retry timers, and batch sizing are
// out of scope here (sizing/timers are v1-draft; the contiguity, rejected-whole,
// batchId-idempotency, and single-inputSeq-space rules are v1-frozen and belong
// to the relay/client logic, not this codec). Frames travel base64-encoded.

// ViewerInputBatch is the viewer → relay upstream envelope (§14). Inputs are in
// contiguous inputSeq order and FirstInputSeq MUST equal lastAck + 1; a batch
// that opens a gap is rejected whole. Controls ride outside the contiguity rule
// and are not covered by ackInputSeq. A control-only batch omits
// FirstInputSeq/LastInputSeq (nil) and carries empty Inputs.
type ViewerInputBatch struct {
	BatchID       string   `json:"batchId"`
	FirstInputSeq *int64   `json:"firstInputSeq,omitempty"`
	LastInputSeq  *int64   `json:"lastInputSeq,omitempty"`
	Inputs        []string `json:"inputs"`   // base64(Input frame), contiguous inputSeq order
	Controls      []string `json:"controls"` // base64(Control frame), processed in array order
}

// HostFrameBatch is the host → relay upstream envelope (§14): the mirror of the
// viewer batch with directions inverted. Frames are in contiguous host seq order
// and FirstSeq MUST equal lastAck + 1; a gap-opening batch is rejected whole.
// OutOfSeq carries out-of-namespace frames (host Control such as subscribe and
// error, plus post-Exit Snapshot replies), outside the contiguity rule and
// uncovered by the ack.
type HostFrameBatch struct {
	BatchID  string   `json:"batchId"`
	FirstSeq int64    `json:"firstSeq"`
	LastSeq  int64    `json:"lastSeq"`
	Frames   []string `json:"frames"`   // base64(host frame), contiguous host seq order
	OutOfSeq []string `json:"outOfSeq"` // base64(Control or post-Exit Snapshot)
}

// InputBatchAccepted is the viewer POST 200 body (§14): success; advance the
// send window. AckInputSeq is the highest contiguous inputSeq applied and
// forwarded.
type InputBatchAccepted struct {
	BatchID     string `json:"batchId"`
	AckInputSeq int64  `json:"ackInputSeq"`
}

// InputBatchRejected is the viewer POST 409 body (§14): a gap; the batch was
// rejected whole; resend from AckInputSeq + 1.
type InputBatchRejected struct {
	BatchID     string `json:"batchId"`
	AckInputSeq int64  `json:"ackInputSeq"`
}

// HostBatchAccepted is the host POST 200 body (§14): success; AckSeq is the
// highest contiguous host seq applied.
type HostBatchAccepted struct {
	BatchID string `json:"batchId"`
	AckSeq  int64  `json:"ackSeq"`
}

// HostBatchRejected is the host POST 409 body (§14): a gap; the batch was
// rejected whole; resend from AckSeq + 1.
type HostBatchRejected struct {
	BatchID string `json:"batchId"`
	AckSeq  int64  `json:"ackSeq"`
}

// The §14 POST taxonomy also defines 401 (token invalid/expired → re-mint and
// retry) and 429 (backpressure/rate limit → back off, honor Retry-After). Those
// carry no body, so this package defines no type for them.

// FrameBase64Encoding is the base64 encoding used for the degraded lane's
// SSE data lines and batch frame arrays (§14): standard base64.
var FrameBase64Encoding = base64.StdEncoding

// EncodeFrameBase64 encodes a frame as a base64 string for a §14 SSE event or
// batch array element.
func EncodeFrameBase64(f Frame) string {
	return FrameBase64Encoding.EncodeToString(f.Encode())
}

// DecodeFrameBase64 decodes a base64 batch/SSE element back into a Frame.
// Invalid base64 or an undecodable frame is a FramingError.
func DecodeFrameBase64(s string) (Frame, error) {
	raw, err := FrameBase64Encoding.DecodeString(s)
	if err != nil {
		return Frame{}, &FramingError{Reason: "invalid base64 in degraded-lane element", cause: err}
	}
	return DecodeFrame(raw)
}
