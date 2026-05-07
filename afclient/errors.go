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

	// ErrUnimplemented is returned by client methods whose wire shape is
	// canonical (the request/response types are stable) but whose
	// implementation has not yet landed. Used as a placeholder during
	// staged migrations so downstream consumers compile against the
	// final signature without depending on a half-finished call site.
	ErrUnimplemented = errors.New("unimplemented")
)
