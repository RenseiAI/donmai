package afclient

import "errors"

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

	// ErrUnimplemented is returned by client methods whose wire shape is
	// canonical (the request/response types are stable) but whose
	// implementation has not yet landed. Used as a placeholder during
	// staged migrations so downstream consumers compile against the
	// final signature without depending on a half-finished call site.
	ErrUnimplemented = errors.New("unimplemented")
)
