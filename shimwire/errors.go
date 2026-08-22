package shimwire

import "errors"

// Sentinel errors. Every adoption-relevant failure maps to one of these so the
// classification path in the adopting daemon can switch on the CAUSE rather
// than on a message string.
var (
	// ErrVersionMismatch reports disjoint protocol ranges (§D3/§D7).
	ErrVersionMismatch = errors.New("shimwire: protocol version ranges do not overlap")

	// ErrExtensionUnsupported reports a peer-required extension this build does
	// not implement. Fail-closed by contract.
	ErrExtensionUnsupported = errors.New("shimwire: required extension unsupported")

	// ErrMalformed reports a frame this build cannot decode: bad length, unknown
	// message type, or a body that fails strict decoding.
	ErrMalformed = errors.New("shimwire: malformed message")

	// ErrMessageTooLarge reports a declared length above MaxMessageBytes. It is
	// returned BEFORE any allocation is made for the body.
	ErrMessageTooLarge = errors.New("shimwire: message exceeds maximum size")

	// ErrStaleGeneration reports a mutating frame carrying a controller
	// generation the shim has already superseded. This is the split-brain fence
	// (§D4): an old daemon whose packet is delivered late is refused, not obeyed.
	ErrStaleGeneration = errors.New("shimwire: stale controller generation")

	// ErrGenerationRequired reports a mutating frame that carried no generation
	// at all. Read-only inspection may omit it; input, resize, stop, terminal
	// acknowledgement, and tombstone disposal may not.
	ErrGenerationRequired = errors.New("shimwire: controller generation required for mutating frame")
)

// ErrorCode is the closed set of codes carried by an Error message. The detail
// string beside it is DISPLAY-ONLY: it is rendered to an operator and never
// parsed, so adding detail can never change a peer's control flow.
type ErrorCode string

// The closed v1 error-code registry.
const (
	CodeVersionMismatch    ErrorCode = "version_mismatch"
	CodeExtensionRequired  ErrorCode = "extension_required"
	CodeMalformed          ErrorCode = "malformed"
	CodeStaleGeneration    ErrorCode = "stale_generation"
	CodeGenerationRequired ErrorCode = "generation_required"
	CodeIdentityMismatch   ErrorCode = "identity_mismatch"
	CodeUnauthenticated    ErrorCode = "unauthenticated"
	CodePhaseUnknown       ErrorCode = "phase_unknown"
	CodeExited             ErrorCode = "exited"
	CodeInternal           ErrorCode = "internal"
)

// Known reports whether c is an assigned v1 code. An unknown code from a peer
// is itself a protocol defect, so callers surface it rather than guessing.
func (c ErrorCode) Known() bool {
	switch c {
	case CodeVersionMismatch, CodeExtensionRequired, CodeMalformed,
		CodeStaleGeneration, CodeGenerationRequired, CodeIdentityMismatch,
		CodeUnauthenticated, CodePhaseUnknown, CodeExited, CodeInternal:
		return true
	default:
		return false
	}
}
