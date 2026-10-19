package linear

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// retryJitterIntn is the randomness seam behind Retry-After equal jitter. A
// var so tests can pin both jitter bounds deterministically.
var retryJitterIntn = rand.Int64N

// jitteredRetryDelay applies equal jitter to a server-supplied Retry-After
// delay: a sleep in [delay/2, delay], never above the server's deadline so a
// burst of denied callers does not stampede back in lock-step, and never
// below half the deadline so the server's back-pressure is honored. A zero
// delay stays zero — a bare 429 with no guidance still fails fast through
// the bounded attempt loop rather than sleeping.
func jitteredRetryDelay(delay time.Duration) time.Duration {
	half := delay / 2
	if half <= 0 {
		return delay
	}
	// rand.Int64N(n) draws from [0, n): requesting half+1 keeps the top of
	// the window reachable so the full [delay/2, delay] range is live.
	draw := retryJitterIntn(int64(half) + 1)
	if draw > int64(half) {
		draw = int64(half)
	}
	return half + time.Duration(draw) //nolint:gosec // G404: jitter, not crypto
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
