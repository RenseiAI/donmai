// Package linear provides a lightweight Linear GraphQL client using stdlib net/http.
package linear

import "errors"

// Sentinel errors for expected Linear API failure modes.
var (
	ErrInvalidAPIKey  = errors.New("invalid api key")
	ErrUnauthorized   = errors.New("unauthorized")
	ErrForbidden      = errors.New("forbidden")
	ErrNotFound       = errors.New("not found")
	ErrRateLimited    = errors.New("rate limited")
	ErrServerError    = errors.New("server error")
	ErrGraphQLError   = errors.New("graphql error")
	ErrMutationFailed = errors.New("mutation failed")

	// ErrInvalidPlatformURL marks a proxied-client origin that is not a bare,
	// credential-free HTTP(S) origin. It fails the constructor, so no request
	// is ever issued against a malformed or credential-bearing base URL.
	// Errors wrapping it never quote the offending value.
	ErrInvalidPlatformURL = errors.New("invalid platform base URL")
)
