package codeintelhost

import "errors"

// Sentinel errors returned by this package. Callers (notably Handler) use
// errors.Is to classify a failure into the correct wire response: an
// authentication/binding-mismatch failure may be returned as a 4xx with a
// frozen ToolResult body; every other failure listed here is a semantic MCP
// operation error (HTTP 200, ToolResult.IsError == true) per the design's
// authentication and serving contract.
var (
	// ErrUnauthorized reports an untrusted bearer token: missing, malformed,
	// wrong alg, bad signature, wrong issuer/audience, or expired/future-dated.
	ErrUnauthorized = errors.New("code intel host: unauthorized")

	// ErrBindingMismatch reports a trusted token whose subject or claimed
	// binding fields do not exactly equal the request body (or, on the
	// held-binding recheck, the binding actually leased for the request).
	ErrBindingMismatch = errors.New("code intel host: binding mismatch")

	// ErrAtCapacity reports that the resident-workarea pool is full and no
	// evictable (idle, unleased, not-in-flight) entry exists to make room for
	// a new binding. It is a fail-fast backpressure signal, never a queue.
	ErrAtCapacity = errors.New("code intel host: at capacity")

	// ErrRepositoryNotFound reports a repositoryPathId absent from the
	// configured Catalog.
	ErrRepositoryNotFound = errors.New("code intel host: repository not found in catalog")

	// ErrProjectMismatch reports a repositoryPathId whose catalog projectId
	// does not equal the request binding's projectId.
	ErrProjectMismatch = errors.New("code intel host: project mismatch")

	// ErrRevisionUnavailable reports a revision absent from the configured
	// repository source (and its trusted local mirror) after a fetch attempt.
	ErrRevisionUnavailable = errors.New("code intel host: revision unavailable")

	// ErrClosed reports that the pool has begun (or finished) a graceful
	// shutdown drain and refuses new acquisitions.
	ErrClosed = errors.New("code intel host: pool closed")

	// ErrInsecureSource reports a catalog repository source that embeds
	// userinfo/credentials in an http(s) URL (e.g. https://user:pass@host/…).
	// The catalog is an operator-owned config file that may itself be
	// world-readable, logged, or checked into a repo; this error text
	// deliberately never echoes the offending source value.
	ErrInsecureSource = errors.New("code intel host: repository source must not embed credentials in an http(s) URL")

	// ErrMirrorOriginMismatch reports that an already-on-disk bare mirror's
	// recorded origin does not match the catalog's currently configured
	// source for that repository path ID — a fail-closed refusal to silently
	// reuse/mix content from the wrong remote. Neither URL is echoed in the
	// error text.
	ErrMirrorOriginMismatch = errors.New("code intel host: existing mirror origin does not match configured repository source")
)
