package worker

import "errors"

// Sentinel errors for expected worker-protocol failure modes. Callers should
// test for these with errors.Is. These are intentionally duplicated from
// afclient so the worker package stays self-contained.
var (
	// ErrRegistrationFailed indicates a failure during POST /api/workers/register
	// that is not covered by a more specific sentinel (e.g. ErrInvalidProvisioningToken).
	ErrRegistrationFailed = errors.New("worker registration failed")

	// ErrPollFailed indicates a failure during GET /api/workers/{id}/poll that
	// is not covered by a more specific sentinel.
	ErrPollFailed = errors.New("worker poll failed")

	// ErrHeartbeatFailed indicates a failure during POST /api/workers/{id}/heartbeat
	// that is not covered by a more specific sentinel.
	ErrHeartbeatFailed = errors.New("worker heartbeat failed")

	// ErrInvalidProvisioningToken is returned when the rsp_live_ provisioning
	// token is rejected by the coordinator (HTTP 401 on the register call).
	ErrInvalidProvisioningToken = errors.New("invalid provisioning token")

	// ErrRuntimeJWTExpired is returned when the short-lived runtime JWT has
	// expired (HTTP 401 on a poll or heartbeat call). The worker should
	// re-register to obtain a fresh token.
	ErrRuntimeJWTExpired = errors.New("runtime jwt expired")

	// ErrRuntimeJWTInvalid is returned when the runtime JWT is present but
	// refused by the coordinator (HTTP 403). This typically indicates the
	// worker has been revoked or the token is structurally invalid and a
	// fresh register will not help.
	ErrRuntimeJWTInvalid = errors.New("runtime jwt invalid")

	// ErrRateLimited is returned when the coordinator responds with HTTP 429.
	ErrRateLimited = errors.New("rate limited")

	// ErrServerError is returned for 5xx responses from the coordinator.
	ErrServerError = errors.New("server error")

	// ErrNotFound is returned when the coordinator responds with HTTP 404.
	ErrNotFound = errors.New("not found")
)
