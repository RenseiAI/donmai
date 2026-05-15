package github

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned when a GitHub resource does not exist.
var ErrNotFound = errors.New("github: not found")

// ErrUnauthorized is returned when the request is rejected due to authentication.
var ErrUnauthorized = errors.New("github: unauthorized — check GITHUB_TOKEN or app installation token")

// ErrForbidden is returned when the authenticated identity lacks permission.
var ErrForbidden = errors.New("github: forbidden — token lacks required scope")

// APIError represents an error response from the GitHub REST API.
type APIError struct {
	StatusCode int
	Message    string `json:"message"`
	DocURL     string `json:"documentation_url"`
}

func (e *APIError) Error() string {
	if e.DocURL != "" {
		return fmt.Sprintf("github API error %d: %s (see %s)", e.StatusCode, e.Message, e.DocURL)
	}
	return fmt.Sprintf("github API error %d: %s", e.StatusCode, e.Message)
}
