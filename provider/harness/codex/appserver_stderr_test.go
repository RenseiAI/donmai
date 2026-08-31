package codex

import (
	"strings"
	"testing"
)

// TestBoundedBuffer_DropsOldestOnOverflow pins the ring-buffer contract the
// bounded app-server stderr capture depends on: once the buffer is full,
// writing more never blocks or errors — it silently drops the OLDEST bytes
// so the most recent output (where a fatal error almost always lands)
// survives.
func TestBoundedBuffer_DropsOldestOnOverflow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		limit   int
		writes  []string
		want    string
		wantLen int
	}{
		{
			name:   "fits entirely",
			limit:  32,
			writes: []string{"hello ", "world"},
			want:   "hello world",
		},
		{
			name:   "single write exceeds limit, keeps the tail",
			limit:  5,
			writes: []string{"abcdefgh"},
			want:   "defgh",
		},
		{
			name:   "accumulated writes overflow, oldest dropped",
			limit:  10,
			writes: []string{"0123456789", "ABCDE"},
			want:   "56789ABCDE",
		},
		{
			name:   "many small writes overflow one byte at a time",
			limit:  4,
			writes: []string{"a", "b", "c", "d", "e", "f"},
			want:   "cdef",
		},
		{
			name:   "empty write is a no-op",
			limit:  8,
			writes: []string{"abc", "", "def"},
			want:   "abcdef",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			buf := newBoundedBuffer(tc.limit)
			for _, w := range tc.writes {
				n, err := buf.Write([]byte(w))
				if err != nil {
					t.Fatalf("Write(%q): unexpected error %v", w, err)
				}
				if n != len(w) {
					t.Fatalf("Write(%q) = %d, want %d (Write must never report a short write)", w, n, len(w))
				}
			}
			if got := buf.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
			if len(buf.String()) > tc.limit {
				t.Fatalf("String() length %d exceeds limit %d", len(buf.String()), tc.limit)
			}
		})
	}
}

// TestBoundedBuffer_ExcerptNilSafe pins that a nil *boundedBuffer — every
// namedInteractiveAppServer built outside the real spawn path (e.g. the
// close()/closeClient() concurrency test) never sets one — is a safe,
// silent "" rather than a nil-pointer panic.
func TestBoundedBuffer_ExcerptNilSafe(t *testing.T) {
	t.Parallel()
	var buf *boundedBuffer
	if got := buf.Excerpt(); got != "" {
		t.Fatalf("Excerpt() on a nil buffer = %q, want \"\"", got)
	}
}

// TestBoundedBuffer_ExcerptRedactsAndBoundsTail pins Excerpt's two jobs at
// once: it must scrub a secret this test plants deep inside the captured
// stderr, and it must never exceed appServerStderrExcerptBytes even when the
// underlying buffer holds far more (keeping the TAIL, since a fatal error is
// almost always the last thing a dying process prints).
//
// RED proof: replace Excerpt's body with `return b.String()` and this test
// fails on the redaction assertion (the bearer token is still present
// verbatim) — see the codex-appserver-stderr worktree's implementation
// commit message for the actual revert/run/restore this pins.
func TestBoundedBuffer_ExcerptRedactsAndBoundsTail(t *testing.T) {
	t.Parallel()
	buf := newBoundedBuffer(appServerStderrRetentionBytes)
	secret := "Bearer sk-live-do-not-print-this-1234567890"
	filler := strings.Repeat("x", appServerStderrExcerptBytes+1024)
	if _, err := buf.Write([]byte(filler)); err != nil {
		t.Fatalf("Write filler: %v", err)
	}
	if _, err := buf.Write([]byte("\nfatal: app-server crashed while starting MCP server\nAuthorization header was: " + secret + "\n")); err != nil {
		t.Fatalf("Write secret: %v", err)
	}

	excerpt := buf.Excerpt()
	if strings.Contains(excerpt, "sk-live-do-not-print-this-1234567890") {
		t.Fatalf("Excerpt leaked the bearer token verbatim: %q", excerpt)
	}
	if !strings.Contains(excerpt, "[REDACTED]") {
		t.Fatalf("Excerpt does not show a redaction marker at all: %q", excerpt)
	}
	if !strings.Contains(excerpt, "fatal: app-server crashed while starting MCP server") {
		t.Fatalf("Excerpt dropped the diagnostic message it exists to preserve: %q", excerpt)
	}
	if len(excerpt) > appServerStderrExcerptBytes {
		t.Fatalf("Excerpt length %d exceeds appServerStderrExcerptBytes %d", len(excerpt), appServerStderrExcerptBytes)
	}
}

// TestRedactAppServerStderr table-tests the pure scrubbing function in
// isolation, independent of any buffer or subprocess — every secret shape
// the wiring is required to catch, plus a plain-text case proving normal
// diagnostic output survives untouched.
func TestRedactAppServerStderr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		in             string
		wantRedacted   []string // substrings that must NOT survive
		wantSurviving  []string // substrings that MUST survive verbatim
		wantUnmodified bool
	}{
		{
			name:          "bearer token behind an Authorization label",
			in:            "connecting with Authorization: Bearer abcDEF123.token-value",
			wantRedacted:  []string{"abcDEF123.token-value"},
			wantSurviving: []string{"Authorization", "[REDACTED]"},
		},
		{
			name:          "bare bearer token, no Authorization label",
			in:            `stderr: "bearer abcDEF123.token-value" was rejected`,
			wantRedacted:  []string{"abcDEF123.token-value"},
			wantSurviving: []string{"bearer", "[REDACTED]", "was rejected"},
		},
		{
			name:          "bearer lowercase",
			in:            "bearer=sk-abcdefghijklmnop",
			wantRedacted:  []string{"sk-abcdefghijklmnop"},
			wantSurviving: []string{"[REDACTED]"},
		},
		{
			name:          "authorization header json",
			in:            `{"authorization":"secret-header-value-xyz"}`,
			wantRedacted:  []string{"secret-header-value-xyz"},
			wantSurviving: []string{"authorization", "[REDACTED]"},
		},
		{
			name:          "labeled api key",
			in:            `api_key: "sk-proj-abcdefghijklmnop"`,
			wantRedacted:  []string{"sk-proj-abcdefghijklmnop"},
			wantSurviving: []string{"api_key", "[REDACTED]"},
		},
		{
			name:          "labeled access token",
			in:            "access_token=ghp_abcdefghijklmnopqrstuvwxyz",
			wantRedacted:  []string{"ghp_abcdefghijklmnopqrstuvwxyz"},
			wantSurviving: []string{"access_token", "[REDACTED]"},
		},
		{
			name:          "donmai machine token",
			in:            "using credential dmk_0123456789abcdef0123456789abcdef for host auth",
			wantRedacted:  []string{"dmk_0123456789abcdef0123456789abcdef"},
			wantSurviving: []string{"using credential", "[REDACTED]", "for host auth"},
		},
		{
			name:           "plain diagnostic text is untouched",
			in:             "fatal: failed to start MCP server \"fixture\": exit status 1",
			wantUnmodified: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redactAppServerStderr(tc.in)
			if tc.wantUnmodified {
				if got != tc.in {
					t.Fatalf("redactAppServerStderr(%q) = %q, want unchanged", tc.in, got)
				}
				return
			}
			for _, secret := range tc.wantRedacted {
				if strings.Contains(got, secret) {
					t.Fatalf("redactAppServerStderr(%q) = %q, still contains secret %q", tc.in, got, secret)
				}
			}
			for _, want := range tc.wantSurviving {
				if !strings.Contains(got, want) {
					t.Fatalf("redactAppServerStderr(%q) = %q, missing expected substring %q", tc.in, got, want)
				}
			}
		})
	}
}
