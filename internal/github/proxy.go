package github

import "net/http"

// NewProxiedClient creates a GitHub client that routes REST requests through
// the platform's /api/cli/github/rest proxy. The platform authenticates the
// request via the rsk_ token and forwards it under the org's GitHub App
// installation credential.
//
// baseURL is the platform's base URL (e.g. "https://app.rensei.ai").
// token is the rsk_* session token from afclient.CredentialsFromDataSource.
func NewProxiedClient(baseURL, token string) *Client {
	c := NewClient(token)
	// Override the base URL to point at the platform proxy.
	// The proxy path mirrors the linear proxy pattern: REST calls are made to
	// <baseURL>/api/cli/github/rest/<path> where <path> is the GitHub REST path
	// (e.g. /repos/owner/repo/issues/1).
	c.BaseURL = baseURL + "/api/cli/github/rest"
	// Replace the HTTP client to add the Bearer auth header for the platform.
	c.httpClient = &http.Client{Timeout: c.httpClient.Timeout}
	return c
}
