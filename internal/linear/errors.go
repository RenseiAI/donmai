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
)
