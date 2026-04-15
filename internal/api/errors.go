package api

import "errors"

// Sentinel errors for expected API failure modes.
var (
	ErrNotAuthenticated = errors.New("not authenticated")
	ErrNotFound         = errors.New("not found")
	ErrUnauthorized     = errors.New("unauthorized")
	ErrRateLimited      = errors.New("rate limited")
	ErrTimeout          = errors.New("request timeout")
	ErrServerError      = errors.New("server error")
)
