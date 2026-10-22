package statusbeacon_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/runtime/statusbeacon"
)

func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func TestNewValidatesRequiredFields(t *testing.T) {
	t.Parallel()
	if _, err := statusbeacon.New(statusbeacon.Config{BaseURL: "x"}); err == nil {
		t.Fatal("expected error for missing SessionID")
	}
	if _, err := statusbeacon.New(statusbeacon.Config{SessionID: "s"}); err == nil {
		t.Fatal("expected error for missing BaseURL")
	}
}

func TestNormalizeStep(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", "streaming", "streaming", true},
		{"collapses whitespace", "line one\nline two", "line one line two", true},
		{"trims", "  spawning  ", "spawning", true},
		{"blank skipped", "   ", "", false},
		{"empty skipped", "", "", false},
		{"tabs and carriage returns", "a\tb\rc", "a b c", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := statusbeacon.NormalizeStep(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("NormalizeStep(%q) = (%q, %v); want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestNormalizeStepTruncatesLongSteps(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("s", 200)
	got, ok := statusbeacon.NormalizeStep(long)
	if !ok {
		t.Fatal("long step must still post (truncated), not be skipped")
	}
	if len(got) > statusbeacon.MaxStepLength {
		t.Fatalf("normalized step is %d bytes; want at most %d", len(got), statusbeacon.MaxStepLength)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated step %q does not carry the ellipsis marker", got)
	}
}

// TestRequestShape asserts the POST path, auth header, and JSON body — the
// exact wire contract the companion route must accept: workerId +
// beaconStep + beaconAt, beacon-only (no display message).
func TestRequestShape(t *testing.T) {
	t.Parallel()

	var (
		capturedPath   atomic.Pointer[string]
		capturedAuth   atomic.Pointer[string]
		capturedMethod atomic.Pointer[string]
		capturedBody   atomic.Pointer[string]
	)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		capturedPath.Store(&path)
		auth := r.Header.Get("Authorization")
		capturedAuth.Store(&auth)
		method := r.Method
		capturedMethod.Store(&method)
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		capturedBody.Store(&s)
		w.WriteHeader(http.StatusOK)
	})

	b, err := statusbeacon.New(statusbeacon.Config{
		SessionID:  "s1",
		WorkerID:   "w1",
		AuthToken:  "tok",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Now:        func() time.Time { return time.Date(2026, 10, 5, 17, 59, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !b.Report(context.Background(), "streaming") {
		t.Fatal("Report returned false on a 200 OK")
	}

	if got := capturedMethod.Load(); got == nil || *got != http.MethodPost {
		t.Fatalf("method = %v, want POST", got)
	}
	if got := capturedPath.Load(); got == nil || *got != "/api/sessions/s1/progress" {
		t.Fatalf("path = %v, want /api/sessions/s1/progress", got)
	}
	if got := capturedAuth.Load(); got == nil || *got != "Bearer tok" {
		t.Fatalf("Authorization = %v, want Bearer tok", got)
	}
	got := capturedBody.Load()
	if got == nil {
		t.Fatal("no body captured")
	}
	var body struct {
		WorkerID   string `json:"workerId"`
		BeaconStep string `json:"beaconStep"`
		BeaconAt   string `json:"beaconAt"`
	}
	if err := json.Unmarshal([]byte(*got), &body); err != nil {
		t.Fatalf("body not JSON: %q (%v)", *got, err)
	}
	if body.WorkerID != "w1" {
		t.Errorf("workerId = %q, want w1", body.WorkerID)
	}
	if body.BeaconStep != "streaming" {
		t.Errorf("beaconStep = %q, want streaming", body.BeaconStep)
	}
	if body.BeaconAt != "2026-10-05T17:59:00Z" {
		t.Errorf("beaconAt = %q, want RFC3339 2026-10-05T17:59:00Z", body.BeaconAt)
	}
	if _, err := time.Parse(time.RFC3339, body.BeaconAt); err != nil {
		t.Errorf("beaconAt %q not RFC3339: %v", body.BeaconAt, err)
	}
	// Beacon-only: no display message key may ride the post, or the route
	// would stream to the activity buffer and forward a permanent comment.
	if strings.Contains(*got, `"message"`) {
		t.Errorf("beacon body carries a message key (would forward a comment): %s", *got)
	}
}

// TestRepeatStepIsNoPost covers the step-change trigger: a repeat of the
// already-posted step performs no POST.
func TestRepeatStepIsNoPost(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	b, err := statusbeacon.New(statusbeacon.Config{
		SessionID: "s1", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if !b.Report(ctx, "streaming") {
		t.Fatal("first Report failed")
	}
	if !b.Report(ctx, "streaming") {
		t.Fatal("repeat Report must return true (already posted)")
	}
	if hits.Load() != 1 {
		t.Fatalf("repeat step posted %d times; want exactly 1 POST", hits.Load())
	}
	if !b.Report(ctx, "backstop") {
		t.Fatal("changed step Report failed")
	}
	if hits.Load() != 2 {
		t.Fatalf("changed step posted %d times total; want 2", hits.Load())
	}
}

// TestWhitespaceVariantsOfSameStepAreOnePost pins the normalization seam:
// steps that fold to the same single line are one post, not two.
func TestWhitespaceVariantsOfSameStepAreOnePost(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	b, err := statusbeacon.New(statusbeacon.Config{
		SessionID: "s1", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b.Report(ctx, "line one\nline two")
	b.Report(ctx, "line one line two")
	if hits.Load() != 1 {
		t.Fatalf("whitespace variants posted %d times; want 1", hits.Load())
	}
}

// TestBlankStepSkipsPostSilently covers the empty-step path: no POST, false.
func TestBlankStepSkipsPostSilently(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	b, err := statusbeacon.New(statusbeacon.Config{
		SessionID: "s1", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Report(context.Background(), "   ") {
		t.Fatal("blank step Report must return false")
	}
	if hits.Load() != 0 {
		t.Fatalf("blank step posted %d times; want 0", hits.Load())
	}
}

// TestBestEffortFailuresAreSwallowed covers the failure-tolerance contract:
// every failure shape returns false, logs, and never surfaces an error —
// the run continues with no failure.
func TestBestEffortFailuresAreSwallowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{"not found", http.StatusNotFound},
		{"server error", http.StatusInternalServerError},
		{"bad request", http.StatusBadRequest},
		{"forbidden", http.StatusForbidden},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "no", tc.status)
			})
			b, err := statusbeacon.New(statusbeacon.Config{
				SessionID: "s1", WorkerID: "w1", BaseURL: srv.URL, HTTPClient: srv.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			// No panic, no error return — false is the only signal, and
			// the step stays unposted so a later step retries.
			if b.Report(context.Background(), "streaming") {
				t.Fatalf("Report on HTTP %d must return false", tc.status)
			}
			if got := b.LastStep(); got != "" {
				t.Fatalf("LastStep = %q after a failed post; want empty (retry stays armed)", got)
			}
		})
	}
}

// TestFailedPostDoesNotAdvanceLastStep proves the retry seam: a failed post
// leaves the step unposted, so the terminal beacon still fires even when
// every mid-run post failed.
func TestFailedPostDoesNotAdvanceLastStep(t *testing.T) {
	t.Parallel()

	var fail atomic.Bool
	fail.Store(true)
	var bodiesMu sync.Mutex
	var bodies []string
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		body, _ := io.ReadAll(r.Body)
		bodiesMu.Lock()
		bodies = append(bodies, string(body))
		bodiesMu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	b, err := statusbeacon.New(statusbeacon.Config{
		SessionID: "s1", WorkerID: "w1", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if b.Report(ctx, "streaming") {
		t.Fatal("Report must fail while the endpoint 500s")
	}
	fail.Store(false)
	if !b.Report(ctx, "completed") {
		t.Fatal("terminal Report must succeed once the endpoint recovers")
	}
	bodiesMu.Lock()
	defer bodiesMu.Unlock()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"beaconStep":"completed"`) {
		t.Fatalf("terminal beacon bodies = %v; want the single completed post", bodies)
	}
}

// TestAuthRejectionLogsDistinctlyAtWarn asserts that a 401/403 is surfaced
// at WARN with an auth-shaped message, not folded into the routine
// best-effort Debug line every other failure gets. The outage stays
// swallowed either way.
func TestAuthRejectionLogsDistinctlyAtWarn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{"unauthorized", http.StatusUnauthorized},
		{"forbidden", http.StatusForbidden},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})
			var buf strings.Builder
			var mu sync.Mutex
			w := &lockedWriter{mu: &mu, b: &buf}
			b, err := statusbeacon.New(statusbeacon.Config{
				SessionID: "s1", BaseURL: srv.URL, HTTPClient: srv.Client(),
				Logger: slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})),
			})
			if err != nil {
				t.Fatal(err)
			}
			b.Report(context.Background(), "streaming")
			logs := muLockedString(&mu, &buf)
			if !strings.Contains(logs, "auth rejected") {
				t.Fatalf("expected a distinct auth-rejected line for %d, got: %s", tc.status, logs)
			}
			if !strings.Contains(logs, `"level":"WARN"`) {
				t.Fatalf("expected WARN level, got: %s", logs)
			}
		})
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func muLockedString(mu *sync.Mutex, b *strings.Builder) string {
	mu.Lock()
	defer mu.Unlock()
	return b.String()
}

// TestCredentialProviderSuppliesFreshCredentials proves the provider is
// consulted before every POST so mid-session rotations propagate.
func TestCredentialProviderSuppliesFreshCredentials(t *testing.T) {
	t.Parallel()

	var capturedAuth atomic.Pointer[string]
	var capturedBody atomic.Pointer[string]
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		capturedAuth.Store(&auth)
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		capturedBody.Store(&s)
		w.WriteHeader(http.StatusOK)
	})
	b, err := statusbeacon.New(statusbeacon.Config{
		SessionID: "s1",
		WorkerID:  "wkr_old",
		AuthToken: "old-token",
		BaseURL:   srv.URL,
		CredentialProvider: func(context.Context) (statusbeacon.RuntimeCredentials, error) {
			return statusbeacon.RuntimeCredentials{WorkerID: "wkr_fresh", AuthToken: "fresh-token"}, nil
		},
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	b.Report(context.Background(), "streaming")
	if got := capturedAuth.Load(); got == nil || *got != "Bearer fresh-token" {
		t.Fatalf("Authorization = %v, want Bearer fresh-token", got)
	}
	if got := capturedBody.Load(); got == nil || !strings.Contains(*got, `"workerId":"wkr_fresh"`) {
		t.Fatalf("body = %v, want fresh worker id", got)
	}
}

// TestShouldPost mirrors the runner's step-change gate without posting.
func TestShouldPost(t *testing.T) {
	t.Parallel()
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	b, err := statusbeacon.New(statusbeacon.Config{
		SessionID: "s1", BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !b.ShouldPost("streaming") {
		t.Fatal("ShouldPost(new step) = false; want true")
	}
	b.Report(context.Background(), "streaming")
	if b.ShouldPost("streaming") {
		t.Fatal("ShouldPost(same step) = true; want false")
	}
	if !b.ShouldPost("backstop") {
		t.Fatal("ShouldPost(changed step) = false; want true")
	}
	if b.ShouldPost("   ") {
		t.Fatal("ShouldPost(blank) = true; want false")
	}
}
