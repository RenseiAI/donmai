package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// registerPath is the coordinator endpoint used to exchange a provisioning
// token for a worker identity and runtime JWT.
const registerPath = "/api/workers/register"

// Register exchanges the Client's provisioning token for a worker identity
// and runtime JWT.
//
// Register authenticates with the long-lived provisioning token carried on
// the Client (c.ProvisioningToken) and must be called before any Poll or
// Heartbeat calls. The request body is encoded as JSON from req.
//
// On success, Register mutates the Client: c.WorkerID and c.RuntimeJWT are
// populated from the response so subsequent Poll/Heartbeat calls can use
// the runtime JWT. The returned *RegisterResponse carries the same values
// together with the coordinator's heartbeat cadence.
//
// On failure, the Client's WorkerID and RuntimeJWT fields are left
// unchanged. A 401 response is mapped to ErrInvalidProvisioningToken, 429
// to ErrRateLimited, and any 5xx to ErrRegistrationFailed. All other
// errors (transport, decode, context cancellation) are wrapped with a
// "register: " prefix and returned verbatim.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	var resp RegisterResponse
	if err := c.postWithProvisioning(ctx, registerPath, req, &resp); err != nil {
		// 5xx responses surface as ErrServerError from the shared status
		// mapping; remap to the package-level ErrRegistrationFailed so
		// callers have a single sentinel for "registration did not
		// succeed for a server-side reason".
		if errors.Is(err, ErrServerError) {
			return nil, fmt.Errorf("register: %w", ErrRegistrationFailed)
		}
		return nil, fmt.Errorf("register: %w", err)
	}

	c.WorkerID = resp.WorkerID
	c.RuntimeJWT = resp.RuntimeJWT

	slog.Default().Info("worker registered", "worker_id", resp.WorkerID)

	return &resp, nil
}
