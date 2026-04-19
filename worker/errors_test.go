package worker

import (
	"errors"
	"fmt"
	"testing"
)

// TestSentinels_Distinct guards against any two sentinels accidentally
// collapsing to the same value (which would break errors.Is routing).
func TestSentinels_Distinct(t *testing.T) {
	sentinels := []error{
		ErrRegistrationFailed,
		ErrPollFailed,
		ErrHeartbeatFailed,
		ErrInvalidProvisioningToken,
		ErrRuntimeJWTExpired,
		ErrRuntimeJWTInvalid,
		ErrRateLimited,
		ErrServerError,
		ErrNotFound,
	}
	for i := range sentinels {
		for j := range sentinels {
			if i == j {
				continue
			}
			if errors.Is(sentinels[i], sentinels[j]) {
				t.Errorf("sentinel %d (%v) matches sentinel %d (%v)", i, sentinels[i], j, sentinels[j])
			}
		}
	}
}

// TestSentinels_WrapUnwrap verifies that every sentinel is unwrappable from
// a fmt.Errorf %w wrap, which is how the package wraps errors in helpers.
func TestSentinels_WrapUnwrap(t *testing.T) {
	sentinels := []error{
		ErrRegistrationFailed,
		ErrPollFailed,
		ErrHeartbeatFailed,
		ErrInvalidProvisioningToken,
		ErrRuntimeJWTExpired,
		ErrRuntimeJWTInvalid,
		ErrRateLimited,
		ErrServerError,
		ErrNotFound,
	}
	for _, s := range sentinels {
		wrapped := fmt.Errorf("ctx: %w", s)
		if !errors.Is(wrapped, s) {
			t.Errorf("errors.Is on wrapped %v failed", s)
		}
	}
}

// TestSentinels_HaveMessages guards against a sentinel being accidentally
// declared without a message (errors.New("")).
func TestSentinels_HaveMessages(t *testing.T) {
	for _, s := range []error{
		ErrRegistrationFailed,
		ErrPollFailed,
		ErrHeartbeatFailed,
		ErrInvalidProvisioningToken,
		ErrRuntimeJWTExpired,
		ErrRuntimeJWTInvalid,
		ErrRateLimited,
		ErrServerError,
		ErrNotFound,
	} {
		if s.Error() == "" {
			t.Errorf("sentinel has empty message: %v", s)
		}
	}
}
