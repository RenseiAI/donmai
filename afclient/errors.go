package afclient

import (
	"errors"
	"fmt"
)

// Sentinel errors for expected API failure modes.
var (
	ErrNotAuthenticated = errors.New("not authenticated")
	ErrNotFound         = errors.New("not found")
	ErrUnauthorized     = errors.New("unauthorized")
	ErrRateLimited      = errors.New("rate limited")
	ErrTimeout          = errors.New("request timeout")
	ErrServerError      = errors.New("server error")
	// ErrConflict indicates the server returned 409 Conflict — the
	// requested operation collides with existing state (e.g. a workarea
	// restore targeting a session id that's already in use).
	ErrConflict = errors.New("conflict")
	// ErrUnavailable indicates the server returned 503 Service
	// Unavailable — capacity is exhausted; the caller should honour the
	// Retry-After header if present.
	ErrUnavailable = errors.New("service unavailable")
	// ErrBadRequest indicates the server returned 400 Bad Request — the
	// request payload was malformed, or referenced corrupted state (e.g.
	// a corrupted workarea archive). The wrapped error chain carries any
	// reason string the server attached.
	ErrBadRequest = errors.New("bad request")
	// ErrRestartPreflightRefused is returned when the daemon could not durably
	// prepare every authority scope for a planned restart. Callers must not invoke
	// the service manager after this error.
	ErrRestartPreflightRefused = errors.New("daemon restart preflight refused")
	// ErrNeedsSetup is the typed setup refusal the service entry point
	// returns when the host has no harness or repository profile yet
	// (fresh-host seed). The entry point maps it to ExitCodeNeedsSetup
	// so the supervisor holds the unit failed instead of restarting it
	// every RestartSec while it waits for `host setup`.
	ErrNeedsSetup = errors.New("daemon is not configured yet: run `host setup` to choose a harness and repository, then restart")
	// ErrInvalidRestartPreflightResponse reports a 2xx response that is not the
	// closed session-shim-restart-preflight-v1 permission schema. Unknown or
	// malformed success is a refusal, never implied permission.
	ErrInvalidRestartPreflightResponse = errors.New("invalid daemon restart preflight response")

	// ErrUnimplemented is returned by client methods whose wire shape is
	// canonical (the request/response types are stable) but whose
	// implementation has not yet landed. Used as a placeholder during
	// staged migrations so downstream consumers compile against the
	// final signature without depending on a half-finished call site.
	ErrUnimplemented = errors.New("unimplemented")
)

// StopSessionError is the bounded, whitelisted error receipt returned by the
// public session-stop endpoint. It deliberately does not retain the raw HTTP
// body: platform error payloads may contain fields that are not safe to echo.
type StopSessionError struct {
	HTTPStatus        int           `json:"httpStatus"`
	Stopped           bool          `json:"stopped"`
	SessionID         string        `json:"sessionId"`
	PreviousStatus    SessionStatus `json:"previousStatus"`
	Code              string        `json:"code"`
	Refusal           string        `json:"refusal"`
	Retryable         bool          `json:"retryable"`
	Disposition       string        `json:"disposition,omitempty"`
	OwnerLiveness     string        `json:"ownerLiveness,omitempty"`
	PreparedAgeMs     *int64        `json:"preparedAgeMs,omitempty"`
	MutationID        string        `json:"mutationId,omitempty"`
	RetryAfterSeconds *int          `json:"retryAfterSeconds,omitempty"`
}

func (e *StopSessionError) Error() string {
	if e.Disposition != "" {
		return fmt.Sprintf("%s (%s)", e.Refusal, e.Disposition)
	}
	return e.Refusal
}

// Unwrap preserves compatibility for callers that already branch on the
// coarse HTTP 409 sentinel.
func (e *StopSessionError) Unwrap() error { return ErrConflict }

// PromptSessionError is the bounded, whitelisted refusal receipt returned by
// the public session-prompt endpoint (POST /api/public/sessions/:id/prompt).
// It preserves the 409 code, reason and recovery hint without retaining the
// raw HTTP body and without echoing prompt text. The command layer renders
// the human line and the --json body from these fields.
type PromptSessionError struct {
	HTTPStatus     int           `json:"httpStatus"`
	Delivered      bool          `json:"delivered"`
	DeliveryTier   string        `json:"deliveryTier,omitempty"`
	Disposition    string        `json:"disposition,omitempty"`
	SessionID      string        `json:"sessionId,omitempty"`
	SessionStatus  SessionStatus `json:"sessionStatus,omitempty"`
	Code           string        `json:"code,omitempty"`
	Reason         string        `json:"error,omitempty"`
	RecoveryAction string        `json:"recoveryAction,omitempty"`
	MissingFields  []string      `json:"missingProjectionFields,omitempty"`
}

func (e *PromptSessionError) Error() string {
	if e.Code != "" && e.Reason != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Reason)
	}
	if e.Reason != "" {
		return e.Reason
	}
	if e.Code != "" {
		return e.Code
	}
	return "prompt refused"
}

// Unwrap preserves compatibility for callers that already branch on the
// coarse HTTP 409 sentinel.
func (e *PromptSessionError) Unwrap() error { return ErrConflict }
