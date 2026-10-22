package linear

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRetryJitterCap bounds the upward jitter added to a server-supplied
// Retry-After delay, so an extreme header value cannot schedule an absurd
// spread on top of an already long wait.
const maxRetryJitterCap = 30 * time.Second

// retryJitterIntn is the randomness seam behind Retry-After jitter. A
// var so tests can pin both jitter bounds deterministically.
var retryJitterIntn = rand.Int64N

// retryJitterExtra returns the deterministic upper bound of the added jitter
// for delay: half the delay, capped. Kept separate so the request-budget
// check compares against the worst case without drawing.
func retryJitterExtra(delay time.Duration) time.Duration {
	extra := delay / 2
	if extra > maxRetryJitterCap {
		extra = maxRetryJitterCap
	}
	if extra < 0 {
		return 0
	}
	return extra
}

// retryDelayUpperBound returns the worst-case sleep for a Retry-After delay:
// the server value plus the full capped jitter spread, saturating instead
// of overflowing on extreme header values.
func retryDelayUpperBound(delay time.Duration) time.Duration {
	extra := retryJitterExtra(delay)
	if delay > time.Duration(math.MaxInt64-int64(extra)) {
		return time.Duration(math.MaxInt64)
	}
	return delay + extra
}

// jitteredRetryDelay adds bounded upward jitter to a server-supplied
// Retry-After delay: a sleep in [delay, delay + min(delay/2, cap)].
// Retry-After is a minimum wait, so the sleep never lands before the
// server's deadline — denied callers bursting together spread out after it
// instead of waking in lock-step and stampeding the still-empty bucket. A
// zero delay stays zero: a bare 429 with no guidance still fails fast
// through the bounded attempt loop rather than sleeping.
func jitteredRetryDelay(delay time.Duration) time.Duration {
	extra := retryJitterExtra(delay)
	if extra <= 0 {
		return delay
	}
	// rand.Int64N(n) draws from [0, n): requesting extra+1 keeps the top of
	// the window reachable so the full [delay, delay+extra] range is live.
	draw := retryJitterIntn(int64(extra) + 1)
	if draw < 0 {
		draw = 0
	}
	if draw > int64(extra) {
		draw = int64(extra)
	}
	return delay + time.Duration(draw) //nolint:gosec // G404: jitter, not crypto
}

// retryDenial retains the upstream error and retry guidance for CLI embedders.
// ExitCode follows EX_TEMPFAIL, allowing callers to distinguish back-pressure
// from permanent failures without importing an internal type.
type retryDenial struct {
	cause error
	delay time.Duration
	retry bool
}

func (e *retryDenial) Error() string { return e.cause.Error() }
func (e *retryDenial) Unwrap() error { return e.cause }
func (e *retryDenial) ExitCode() int { return 75 }

func graphQLErrorText(entries []graphqlError) string {
	messages := make([]string, 0, len(entries))
	for _, entry := range entries {
		message := entry.Message
		code := entry.Code
		if code == "" {
			code = entry.Extensions.Code
		}
		if code != "" {
			message += " (code: " + code + ")"
		}
		messages = append(messages, message)
	}
	return strings.Join(messages, "; ")
}

func responseError(resp *http.Response, body []byte, entries []graphqlError, decodeErr error) error {
	cause := statusToError(resp.StatusCode)
	if decodeErr == nil && len(entries) > 0 {
		cause = fmt.Errorf("%w: %w: %s", cause, ErrGraphQLError, graphQLErrorText(entries))
	} else if text := strings.TrimSpace(string(body)); text != "" {
		// Some proxies return a flat JSON error rather than a GraphQL envelope.
		var flat graphqlError
		if json.Unmarshal(body, &flat) == nil && flat.Message != "" {
			text = graphQLErrorText([]graphqlError{flat})
		}
		cause = fmt.Errorf("%w: %s", cause, text)
	}
	delay, valid := retryAfter(resp.Header.Get("Retry-After"), time.Now())
	if resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode == http.StatusServiceUnavailable && valid) {
		if resp.StatusCode != http.StatusTooManyRequests {
			cause = fmt.Errorf("%w: %w", ErrRateLimited, cause)
		}
		if valid {
			cause = fmt.Errorf("%w; retry after %.0fs", cause, delay.Seconds())
		}
		return &retryDenial{cause: cause, delay: delay, retry: valid}
	}
	return cause
}

func retryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
		return time.Duration(seconds) * time.Second, true
	}
	if date, err := http.ParseTime(value); err == nil {
		delay := date.Sub(now)
		if delay < 0 {
			delay = 0
		}
		// Round up so a date-form header is never retried before its deadline.
		return time.Duration((delay+time.Second-1)/time.Second) * time.Second, true
	}
	return 0, false
}
