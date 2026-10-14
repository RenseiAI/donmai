package linear

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
