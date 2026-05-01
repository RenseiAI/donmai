package result

import "fmt"

// PermanentError is returned by [Poster.Post] when the platform
// rejected the request with a 4xx status code. Permanent errors are
// not retried — a 401 / 403 / 404 / 422 indicates a programmer error
// (wrong session id, expired auth token, invalid wire shape) that
// retrying will not fix.
type PermanentError struct {
	// StatusCode is the HTTP status the platform returned.
	StatusCode int
	// Body is up to the first 4 KiB of the response body, trimmed.
	Body string
}

// Error implements the error interface.
func (e *PermanentError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("permanent http %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("permanent http %d", e.StatusCode)
}

// TransientError is returned by [Poster.Post] when retries were
// exhausted on a transient failure (5xx, connection refused, DNS,
// context-deadline timeout that exceeded the per-attempt window).
type TransientError struct {
	// Attempts is the number of attempts made before giving up.
	Attempts int
	// Last is the last error observed.
	Last error
}

// Error implements the error interface.
func (e *TransientError) Error() string {
	return fmt.Sprintf("transient: %d attempts exhausted: %v", e.Attempts, e.Last)
}

// Unwrap returns the last underlying error so errors.Is / errors.As
// callers can match the inner cause.
func (e *TransientError) Unwrap() error { return e.Last }
