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
			// B1: a single-quoted value defeated the previous double-quote-only
			// character class outright — the match simply failed to start,
			// leaking the secret verbatim.
			name:          "single-quoted value bypasses the old double-quote-only class",
			in:            "api_key='sk-single-quoted-secret-1234'",
			wantRedacted:  []string{"sk-single-quoted-secret-1234"},
			wantSurviving: []string{"api_key", "[REDACTED]"},
		},
		{
			// B3: bare "token" label, absent from the original alternation.
			name:          "bare token label",
			in:            `token: "raw-token-secret-654321"`,
			wantRedacted:  []string{"raw-token-secret-654321"},
			wantSurviving: []string{"token", "[REDACTED]"},
		},
		{
			// B3: bare "password" label.
			name:          "bare password label",
			in:            "password=hunter2-is-not-really-the-password",
			wantRedacted:  []string{"hunter2-is-not-really-the-password"},
			wantSurviving: []string{"password", "[REDACTED]"},
		},
		{
			// B3: bare "passwd" label (distinct spelling from "password").
			name:          "bare passwd label",
			in:            "passwd=another-secret-value-000111",
			wantRedacted:  []string{"another-secret-value-000111"},
			wantSurviving: []string{"passwd", "[REDACTED]"},
		},
		{
			// B3: bare "credential" label.
			name:          "bare credential label",
			in:            "credential=cred-secret-abcdef123456",
			wantRedacted:  []string{"cred-secret-abcdef123456"},
			wantSurviving: []string{"credential", "[REDACTED]"},
		},
		{
			// B3: "private_key" label.
			name:          "private key label",
			in:            `private_key: "pk-material-abcdef123456"`,
			wantRedacted:  []string{"pk-material-abcdef123456"},
			wantSurviving: []string{"private_key", "[REDACTED]"},
		},
		{
			// B3: bare "cookie" label.
			name:          "bare cookie label",
			in:            "cookie=session-cookie-value-abcdef99",
			wantRedacted:  []string{"session-cookie-value-abcdef99"},
			wantSurviving: []string{"cookie", "[REDACTED]"},
		},
		{
			// B3: bare "session" label.
			name:          "bare session label",
			in:            "session=session-token-abcdef123456",
			wantRedacted:  []string{"session-token-abcdef123456"},
			wantSurviving: []string{"session", "[REDACTED]"},
		},
		{
			// B2: a realistic env_http_headers JSON key ("X-Donmai-Token") —
			// the label match is unanchored so it catches the tail of a
			// compound JSON key, not just an exact bare word.
			name:          "env_http_headers JSON key carrying a token",
			in:            `"X-Donmai-Token": "header-secret-value-778899"`,
			wantRedacted:  []string{"header-secret-value-778899"},
			wantSurviving: []string{"X-Donmai-Token", "[REDACTED]"},
		},
		{
			// B2: an env-var dump, KEY=VALUE shape, with a compound
			// identifier no exact-label match would catch.
			name:          "env-var dump KEY=VALUE shape",
			in:            "DONMAI_MCP_TOKEN=env-dump-secret-abcxyz99",
			wantRedacted:  []string{"env-dump-secret-abcxyz99"},
			wantSurviving: []string{"DONMAI_MCP_TOKEN", "[REDACTED]"},
		},
		{
			// B2: a URL query token embedded in a panic line — the highest-
			// value capture target per review. The `&` boundary must stop
			// the match before the NEXT query parameter, which must survive
			// so the excerpt keeps other diagnostic detail readable.
			name:          "URL query token in a panic line",
			in:            "panic: dial failed: https://api.example.com/mcp?token=panic-query-secret&retry=1",
			wantRedacted:  []string{"panic-query-secret"},
			wantSurviving: []string{"[REDACTED]", "retry=1", "https://api.example.com/mcp?token="},
		},
		{
			// B2: URL userinfo credentials, user:password form — only the
			// password is redacted; the username and host stay visible.
			name:          "URL userinfo credentials, user:password form",
			in:            "dial tcp failed: https://svc:userinfo-secret-pw@mcp.internal:443/socket",
			wantRedacted:  []string{"userinfo-secret-pw"},
			wantSurviving: []string{"svc", "[REDACTED]", "mcp.internal"},
		},
		{
			// B2: URL userinfo credentials, bare-token form (no
			// username:password split).
			name:          "URL userinfo credentials, bare-token form",
			in:            "https://bare-userinfo-secret-token@mcp.internal/socket",
			wantRedacted:  []string{"bare-userinfo-secret-token"},
			wantSurviving: []string{"[REDACTED]", "mcp.internal"},
		},
		{
			// Shape-based rule (point 4 of the review): a well-known PUBLIC
			// token format (OpenAI sk-) with no label anywhere nearby.
			name:          "shape-based OpenAI-style secret, no label",
			in:            "leaked: sk-thisisnotarealopenaikeyfixture",
			wantRedacted:  []string{"sk-thisisnotarealopenaikeyfixture"},
			wantSurviving: []string{"[REDACTED]"},
		},
		{
			// Shape-based rule: a well-known PUBLIC GitHub token format.
			name:          "shape-based GitHub-style token, no label",
			in:            "found ghp_thisisnotarealgithubtokenfixture in stderr",
			wantRedacted:  []string{"ghp_thisisnotarealgithubtokenfixture"},
			wantSurviving: []string{"[REDACTED]", "found", "in stderr"},
		},
		{
			// Shape-based rule: a well-known PUBLIC Slack token format.
			name:          "shape-based Slack-style token, no label",
			in:            "xoxb-thisisnotarealslacktokenfixture",
			wantRedacted:  []string{"xoxb-thisisnotarealslacktokenfixture"},
			wantSurviving: []string{"[REDACTED]"},
		},
		{
			// C1: a multi-segment OpenAI-style key in the "sk-proj-..."
			// shape (the most likely unlabeled credential in the codex/
			// OpenAI harness this package spawns). The pre-C1 prefix
			// pattern required an unbroken 10+ char alnum run right after
			// ONE separator, so a short first segment ("proj") shorter
			// than that bound made the whole match fail — the token
			// leaked in full, not just partially.
			name:          "shape-based multi-segment OpenAI key (sk-proj- shape)",
			in:            "leaked: sk-proj-thisisnotarealsegmentone-thisisnotarealsegmenttwo",
			wantRedacted:  []string{"sk-proj-thisisnotarealsegmentone-thisisnotarealsegmenttwo"},
			wantSurviving: []string{"[REDACTED]", "leaked:"},
		},
		{
			// C1: a multi-segment Slack token — the pre-C1 pattern redacted
			// only the FIRST dash-delimited segment, leaving every
			// subsequent segment (where a real xoxb token's actual secret
			// material lives) exposed.
			name: "shape-based multi-segment Slack token, no label",
			in:   "found xoxb-notarealfirstseg-notarealsecondseg-notarealthirdseg in stderr",
			wantRedacted: []string{
				"xoxb-notarealfirstseg-notarealsecondseg-notarealthirdseg",
				"-notarealsecondseg-notarealthirdseg", // the part a first-segment-only match would have left exposed
			},
			wantSurviving: []string{"[REDACTED]", "found", "in stderr"},
		},
		{
			// C1 (non-regression): pk_organization_id is a FIELD NAME, not
			// a secret — the fixed pattern allows `-` inside the matched
			// run but never `_`, so an underscore-joined identifier that
			// merely starts with a matching prefix stops at the first `_`
			// and is not swept up whole. (The literal "pk_" itself is
			// still shape-matched per the label; what must NOT happen is
			// the rest of the identifier disappearing into one blob.)
			name:           "pk_ prefixed field NAME is not swallowed whole",
			in:             "config field pk_organization_id is required",
			wantUnmodified: true,
		},
		{
			// C2: the app-server died mid-write, after the opening quote
			// and part of the secret but before the closing quote — the
			// EXACT tail shape this excerpt is built to capture (a crash
			// truncates output, it doesn't tidy up after itself). The
			// pre-C2 value pattern required a closing quote to match a
			// quoted value AT ALL, so an unterminated quote made the whole
			// labeled match fail and the partial secret leaked untouched.
			name:          "unterminated double-quote value (crash-mid-write truncation)",
			in:            `api_key: "sk-truncated-mid-write-secret-fixture`,
			wantRedacted:  []string{"sk-truncated-mid-write-secret-fixture"},
			wantSurviving: []string{"api_key", "[REDACTED]"},
		},
		{
			// C2: same truncation shape, single-quoted.
			name:          "unterminated single-quote value (crash-mid-write truncation)",
			in:            `token='sk-truncated-mid-write-secret-fixture2`,
			wantRedacted:  []string{"sk-truncated-mid-write-secret-fixture2"},
			wantSurviving: []string{"token", "[REDACTED]"},
		},
		{
			// Safety net requested alongside C2: a structured log line
			// where "token" appears only INSIDE a quoted, already-closed
			// message value (not as a label at all) must not trigger any
			// redaction, and an unrelated trailing field must survive
			// completely intact.
			name:           "the word token inside a closed msg value, unrelated trailing field survives",
			in:             `msg="token refresh ok" next=1`,
			wantUnmodified: true,
		},
		{
			name:           "plain diagnostic text is untouched",
			in:             "fatal: failed to start MCP server \"fixture\": exit status 1",
			wantUnmodified: true,
		},
		{
			// Safety net: the new bare "session" label must not sweep up
			// ordinary prose that happens to use the word with no [:=]
			// immediately after it.
			name:           "bare word 'session' in ordinary prose is untouched",
			in:             "session ended without error",
			wantUnmodified: true,
		},
		{
			// Safety net: same, for "api_key" mentioned descriptively rather
			// than assigned.
			name:           "label word mentioned descriptively is untouched",
			in:             "the api_key parameter is optional",
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
