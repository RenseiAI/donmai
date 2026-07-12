package attachwire

import (
	"errors"
	"fmt"
)

// ErrorCode is a §7 control-plane error.code value. The message-type set is
// v1-frozen; the code registry itself is v1-draft (extendable) — but these are
// the v1 codes.
type ErrorCode string

// The v1 error.code registry (§7).
const (
	CodeFraming      ErrorCode = "framing"
	CodeAuth         ErrorCode = "auth"
	CodeRoomMismatch ErrorCode = "room-mismatch"
	CodePenDenied    ErrorCode = "pen-denied"
	CodeRingMiss     ErrorCode = "ring-miss"
	CodeBackpressure ErrorCode = "backpressure"
	CodeRateLimited  ErrorCode = "rate-limited"
	CodeEpochStale   ErrorCode = "epoch-stale"
	CodeInternal     ErrorCode = "internal"
)

// FramingError classifies a wire-format violation that §2.1/§3 mandate be
// answered by closing the connection with an error control message whose
// code == "framing" (CodeFraming). Every decode-time framing violation in this
// package — unknown event type, truncated or overflowing varint, a declared
// length past the buffer, a snapshot escape-safety violation — surfaces as a
// FramingError so callers can uniformly map it to the frozen disposition.
type FramingError struct {
	// Reason is a human-readable description of the violation.
	Reason string
	// cause is an optional wrapped underlying error.
	cause error
}

func (e *FramingError) Error() string {
	if e.cause != nil {
		return "attachwire: framing error: " + e.Reason + ": " + e.cause.Error()
	}
	return "attachwire: framing error: " + e.Reason
}

// Unwrap exposes any wrapped cause for errors.Is / errors.As.
func (e *FramingError) Unwrap() error { return e.cause }

// Code reports the §7 error.code a receiver MUST send when closing on this
// error. It is always CodeFraming.
func (e *FramingError) Code() ErrorCode { return CodeFraming }

func newFraming(reason string) *FramingError { return &FramingError{Reason: reason} }

func newFramingf(format string, args ...any) *FramingError {
	return &FramingError{Reason: fmt.Sprintf(format, args...)}
}

// IsFramingErr reports whether err is, or wraps, a FramingError — i.e. whether
// the disposition is "close the connection with error.code = framing" (§2.1).
func IsFramingErr(err error) bool {
	var fe *FramingError
	return errors.As(err, &fe)
}

// Varint framing errors (§2.1). Both are FramingErrors, so IsFramingErr reports
// true and errors.Is matches by identity. They are kept distinct so callers can
// tell an over-long value (overflow) from a buffer that ended mid-varint
// (truncation), even though both close the connection with code "framing".
var (
	// ErrVarintOverflow is returned when a varint does not terminate within the
	// 10-byte uint64 maximum, or its final byte carries bits above the single
	// legal top bit.
	ErrVarintOverflow = &FramingError{Reason: "varint exceeds the 10-byte uint64 maximum (overflow)"}

	// ErrVarintTruncated is returned when the buffer is exhausted while a
	// varint's continuation bit is still set (a frame that ends mid-varint).
	ErrVarintTruncated = &FramingError{Reason: "varint truncated: buffer exhausted before terminator"}
)

// ErrUnknownControlType is returned by DecodeControl for a Control message whose
// "type" discriminator is not one of the v1-frozen set (§7). It is deliberately
// NOT a FramingError: an unrecognized control type is handled softly for
// forward-compatibility (the relay MAY ignore it), whereas an unknown frame
// TYPE BYTE at the framing layer (§3) is a hard framing error. Those are two
// different layers and both rules hold.
var ErrUnknownControlType = errors.New("attachwire: unknown control message type")
