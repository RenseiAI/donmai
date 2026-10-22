package linear

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetryAfterRecovery(t *testing.T) {
	for _, status := range []int{429, 503} {
		for _, proxied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/proxy=%t", status, proxied), func(t *testing.T) {
				calls := 0
				var firstBody string
				var firstTime time.Time
				c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					calls++
					auth := "test-fixture-key-not-a-secret"
					if proxied {
						auth = "Bearer " + auth
					}
					if r.Header.Get("Authorization") != auth || r.Header.Get("Content-Type") != "application/json" {
						t.Error("retry lost request headers")
					}
					if calls == 1 {
						firstBody = string(body)
						firstTime = time.Now()
						w.Header().Set("Retry-After", "1")
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"errors":[{"message":"budget exhausted","extensions":{"code":"BUDGET_EXHAUSTED"}}]}`)
						return
					}
					if string(body) != firstBody {
						t.Error("retry changed query or variables")
					}
					if time.Since(firstTime) < time.Second {
						t.Error("retried before Retry-After")
					}
					writeGQLData(w, `{"teams":{"nodes":[{"id":"team-a","key":"A","name":"Alpha"}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
				})
				c.ProxyMode = proxied
				teams, err := c.ListTeams(context.Background())
				if err != nil || len(teams) != 1 || calls != 2 {
					t.Fatalf("teams=%v err=%v calls=%d", teams, err, calls)
				}
			})
		}
	}
}

func TestResponseFailures(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		header    string
		body      string
		want      []string
		calls     int
		sentinel  error
		temporary bool
	}{
		{"exhausted", 429, "0", `{"errors":[{"message":"budget exhausted","code":"BUDGET_EXHAUSTED"}]}`, []string{"budget exhausted", "BUDGET_EXHAUSTED", "rate limited", "retry after 0s"}, 3, ErrRateLimited, true},
		{"unavailable", 503, "0", `{"errors":[{"message":"quota denied","extensions":{"code":"QUOTA"}}]}`, []string{"quota denied", "QUOTA", "retry after 0s"}, 3, ErrServerError, true},
		{"no header", 429, "", `{"errors":[{"message":"too many requests","extensions":{"code":"RATE_LIMITED"}}]}`, []string{"too many requests", "RATE_LIMITED"}, 1, ErrRateLimited, true},
		{"invalid header", 503, "invalid", `{"errors":[{"message":"maintenance","code":"UNAVAILABLE"}]}`, []string{"maintenance", "UNAVAILABLE"}, 1, ErrServerError, false},
		{"negative header", 429, "-1", `{"message":"denied","code":"QUOTA"}`, []string{"denied", "QUOTA"}, 1, ErrRateLimited, true},
		{"too long", 429, "61", `{"errors":[{"message":"budget exhausted","code":"BUDGET_EXHAUSTED"}]}`, []string{"budget exhausted", "retry after 61s"}, 1, ErrRateLimited, true},
		{"huge header", 503, "4294967295", `busy`, []string{"busy", "retry after 4294967295s"}, 1, ErrServerError, true},
		{"plain body", 500, "0", "upstream service failed", []string{"upstream service failed"}, 1, ErrServerError, false},
		{"graphql body", 500, "", `{"errors":[{"message":"database unavailable","extensions":{"code":"DATABASE"}},{"message":"second error","code":"SECOND"}]}`, []string{"database unavailable", "DATABASE", "second error", "SECOND"}, 1, ErrServerError, false},
		{"json body", 500, "", `{"error":"proxy unavailable"}`, []string{`{"error":"proxy unavailable"}`}, 1, ErrServerError, false},
		{"auth", 403, "0", `{"errors":[{"message":"access denied","code":"DENIED"}]}`, []string{"access denied", "DENIED"}, 1, ErrForbidden, false},
		{"success status error", 200, "", `{"errors":[{"message":"query rejected","extensions":{"code":"BAD_QUERY"}}]}`, []string{"query rejected", "BAD_QUERY"}, 1, ErrGraphQLError, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Retry-After", tt.header)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			_, err := c.ListTeams(context.Background())
			if !errors.Is(err, tt.sentinel) {
				t.Fatalf("err=%v want %v", err, tt.sentinel)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err=%v missing %q", err, want)
				}
			}
			if calls != tt.calls {
				t.Errorf("calls=%d want %d", calls, tt.calls)
			}
			var coded interface{ ExitCode() int }
			if errors.As(fmt.Errorf("command: %w", err), &coded) != tt.temporary {
				t.Fatalf("temporary classification = %v", err)
			}
			if tt.temporary && coded.ExitCode() != 75 {
				t.Errorf("exit code=%d", coded.ExitCode())
			}
		})
	}
}

func TestRetryAfterDateAndCancellation(t *testing.T) {
	for _, date := range []bool{false, true} {
		t.Run(fmt.Sprintf("date=%t", date), func(t *testing.T) {
			calls := 0
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				header := "30"
				if date {
					header = time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
				}
				w.Header().Set("Retry-After", header)
				w.WriteHeader(503)
				_, _ = io.WriteString(w, `{"errors":[{"message":"budget exhausted","code":"QUOTA"}]}`)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := c.ListTeams(ctx)
			if calls != 1 || time.Since(start) > time.Second || !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "retry after 30s") {
				t.Fatalf("calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
			}
		})
	}
	// Cancellation during an allowed wait must stop the retries and preserve cause.
	ctx, cancel := context.WithCancel(context.Background())
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"errors":[{"message":"limited"}]}`)
	})
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()
	_, err := c.ListTeams(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err=%v", err)
	}
}

func TestWriteOnceDenialDoesNotRetry(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"errors":[{"message":"budget exhausted","code":"QUOTA"}]}`)
			})
			_, err := c.CreateDocument(context.Background(), CreateDocumentInput{Title: "fixture", IssueID: StringValue("issue")})
			if calls != 1 || !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "QUOTA") {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestRetryDeadlinePreservesDenial(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"errors":[{"message":"budget exhausted","code":"QUOTA"}]}`)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := c.ListTeams(ctx)
	var coded interface{ ExitCode() int }
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrRateLimited) || !errors.As(err, &coded) || coded.ExitCode() != 75 {
		t.Fatalf("deadline lost denial: %v", err)
	}
}

func TestRetryAfterHTTPDateRecovery(t *testing.T) {
	for _, future := range []bool{false, true} {
		t.Run(fmt.Sprintf("future=%t", future), func(t *testing.T) {
			calls := 0
			var retryAt time.Time
			c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					retryAt = time.Now().Add(-time.Minute)
					if future {
						retryAt = time.Now().Add(2 * time.Second).Truncate(time.Second)
					}
					w.Header().Set("Retry-After", retryAt.UTC().Format(http.TimeFormat))
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"errors":[{"message":"temporarily unavailable","code":"UNAVAILABLE"}]}`)
					return
				}
				if time.Now().Before(retryAt) {
					t.Error("retried before HTTP date")
				}
				writeGQLData(w, `{"teams":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}`)
			})
			_, err := c.ListTeams(context.Background())
			if err != nil || calls != 2 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestRetryRequestDeadline(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"errors":[{"message":"budget exhausted","code":"QUOTA"}]}`)
	})
	transport := c.HTTPClient.Transport
	c.HTTPClient.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > time.Minute {
			t.Error("request has no 60-second total deadline")
		}
		return transport.RoundTrip(r)
	})
	start := time.Now()
	_, err := c.ListTeams(context.Background())
	if calls != 1 || time.Since(start) > time.Second || !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "retry after 60s") {
		t.Fatalf("calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
	}
}

// TestRetryBudgetComparesWorstCaseSleep pins the request-budget guard to the
// worst-case jittered sleep. A 2s Retry-After with maximum upward jitter
// draws a 3s sleep, which cannot finish inside a 2.5s caller budget, so the
// client must return the denial immediately instead of starting a sleep that
// runs into the deadline and surfaces a context error.
func TestRetryBudgetComparesWorstCaseSleep(t *testing.T) {
	defer func(restore func(int64) int64) { retryJitterIntn = restore }(retryJitterIntn)
	retryJitterIntn = func(n int64) int64 { return n - 1 }
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"errors":[{"message":"budget exhausted","code":"QUOTA"}]}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.ListTeams(ctx)
	elapsed := time.Since(start)
	if calls != 1 {
		t.Fatalf("calls=%d want 1", calls)
	}
	if elapsed >= time.Second {
		t.Fatalf("retry waited %v for an unaffordable worst-case sleep, want an immediate denial", elapsed)
	}
	if !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "retry after 2s") {
		t.Fatalf("err=%v want rate-limited denial with retry after 2s", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("unaffordable retry surfaced a context error: %v", err)
	}
}
