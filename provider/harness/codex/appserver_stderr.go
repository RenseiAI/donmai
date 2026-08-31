package codex

import (
	"io"
	"regexp"
	"strings"
	"sync"
)

// This file gives every `codex app-server` child this package spawns —
// the shared headless process in codex.go and the per-session interactive
// naming bootstrap in interactive_name.go — a bounded, concurrency-safe
// record of its own stderr, instead of the discard drainStderr used to be.
//
// Context: the interactive naming bootstrap spawns `codex app-server
// --listen unix://<socket>` and keeps it alive for the life of a named
// session; the PTY child then attaches to that SAME process over the
// socket. When the app-server dies (observed in production during MCP
// server startup), the PTY's `codex resume --remote <socket>` client only
// ever sees its socket peer disappear and exits 0 — a clean-looking exit
// that hides a crash. Before this file existed, the app-server's own
// stderr — which is where codex actually prints its fatal error — was
// piped straight to /dev/null (see the old drainStderr's "Discard" comment
// this replaces), so recovering the real cause meant a multi-hour forensic
// reconstruction instead of one log line.

// appServerStderrRetentionBytes bounds how much of a codex app-server
// child's stderr this package retains in memory. 64KiB comfortably covers a
// fatal panic/backtrace plus the MCP-server-startup lines immediately
// preceding it, without letting a long-lived headless app-server (or a
// runaway interactive one) pin unbounded memory on a chatty child.
const appServerStderrRetentionBytes = 64 * 1024

// appServerStderrExcerptBytes bounds how much of the retained stderr this
// package actually surfaces in a structured log line or a returned error.
// The buffer itself retains more (appServerStderrRetentionBytes) so a
// slow-draining crash is not already evicted by the time something asks,
// but a session's failure evidence should not become its own multi-KB blob.
// The TAIL is what is kept: a fatal error or panic is almost always the
// LAST thing a dying process prints, right before the excerpt is read.
const appServerStderrExcerptBytes = 4 * 1024

// captureAppServerStderr starts draining r in the background into a new
// bounded buffer and returns the buffer immediately. r is closed once the
// drain goroutine's io.Copy returns (EOF, a pipe error, or the process
// tearing the pipe down) — the same ownership contract the retired
// drainStderr(r io.ReadCloser) had, so every existing caller still gets its
// pipe closed exactly once, exactly as before.
//
// Draining never stops for as long as the child writes: boundedBuffer.Write
// never blocks or returns an error, so once the buffer is full it just
// starts overwriting its own oldest bytes. The child is never
// backpressured by whether anyone ever reads the captured excerpt — that is
// the whole point of draining stderr into a sink in the first place.
func captureAppServerStderr(r io.ReadCloser) *boundedBuffer {
	buf := newBoundedBuffer(appServerStderrRetentionBytes)
	go func() {
		defer func() { _ = r.Close() }()
		_, _ = io.Copy(buf, r)
	}()
	return buf
}

// boundedBuffer accumulates the last `limit` bytes written, dropping the
// oldest data once that limit is reached. Goroutine-safe. This is the same
// drop-oldest tail-buffer shape provider/harness/clijsonl and
// provider/harness/opencode already use for their own stderr diagnostics
// (each package keeps its own copy rather than sharing one, per this
// repo's existing convention for this exact utility) — codex's copy adds
// Excerpt(), the redaction-aware accessor every caller in this package
// actually uses.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit, buf: make([]byte, 0, limit)}
}

// Write implements io.Writer. Always returns len(p), nil — drops oldest
// bytes when the buffer would exceed limit rather than blocking or erroring,
// so a caller draining a live child's stderr never has to apply
// backpressure to keep this sink bounded.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.limit {
		// The incoming chunk alone exceeds the limit — retain only its tail.
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	overflow := (len(b.buf) + len(p)) - b.limit
	if overflow > 0 {
		copy(b.buf, b.buf[overflow:])
		b.buf = b.buf[:len(b.buf)-overflow]
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// String returns a snapshot of the current, unredacted buffer contents.
// Production callers want Excerpt, not this — String exists mainly so tests
// can assert on the raw retained bytes (e.g. the drop-oldest boundary)
// without going through redaction.
func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Excerpt returns a redacted, bounded (<= appServerStderrExcerptBytes) tail
// of everything captured so far — safe to place directly in a slog line or
// a returned error. A nil buffer (every namedInteractiveAppServer built
// outside startNamedInteractiveAppServer's real spawn path, e.g. the
// close()/closeClient() concurrency test, never sets one) returns "".
func (b *boundedBuffer) Excerpt() string {
	if b == nil {
		return ""
	}
	redacted := redactAppServerStderr(b.String())
	if len(redacted) > appServerStderrExcerptBytes {
		redacted = redacted[len(redacted)-appServerStderrExcerptBytes:]
	}
	return strings.TrimSpace(redacted)
}

// appServerStderrValuePattern matches a secret-shaped value up to a REAL
// boundary — a matching closing quote (double OR single), or the next
// whitespace/quote/`&`/EOL when unquoted — instead of a fixed character
// class. A character class has to keep guessing at which punctuation a real
// secret might contain (base64 uses `+/=`, JWTs use `.`, API keys use
// `-_`); worse, a single-quoted value (api_key='SECRET') defeated an
// earlier double-quote-only version of this pattern outright, since the
// leading `'` was not in the class and the match simply failed to start.
// `&` is excluded from the unquoted branch so a URL query token
// (?token=SECRET&next=1) redacts only the token, not the rest of the
// query string.
const appServerStderrValuePattern = `(?:"[^"\r\n]*"?|'[^'\r\n]*'?|[^\s"'&\r\n]+)`

// appServerStderrRedaction pairs a pattern with its own replacement
// template (rather than inferring one from NumSubexp, which cannot express
// the two-group userinfo templates below).
type appServerStderrRedaction struct {
	pattern     *regexp.Regexp
	replacement string
}

// appServerStderrRedactions scrub secret-shaped substrings out of a captured
// excerpt before it is ever logged or placed in a returned error. The codex
// app-server this package spawns is handed real credentials — host-session
// auth, MCP server bearer tokens delivered via env_http_headers — so a
// panic or a debug trace that happens to echo one of those must not turn a
// forensic log line into a leak. Most patterns keep any label they matched
// (e.g. "Authorization:") and replace only the secret-shaped value that
// followed it, so the excerpt stays readable.
var appServerStderrRedactions = []appServerStderrRedaction{
	{
		// An Authorization header rendered inline in a log line. The value
		// runs to end-of-line (rather than stopping at a boundary) so a
		// scheme prefix — "Authorization: Bearer <token>" — is consumed as
		// one secret instead of leaving "Bearer" behind for the next
		// pattern to mangle. Ordered before the bare-bearer pattern below
		// for exactly that reason.
		regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*)[^\r\n]+`),
		"${1}[REDACTED]",
	},
	{
		// A bare "Bearer <token>" NOT already consumed above (no
		// "Authorization" label present at all).
		regexp.MustCompile(`(?i)(bearer[\s:=]+)\S+`),
		"${1}[REDACTED]",
	},
	{
		// Any secret-labeled value, KEY=VALUE or JSON shaped:
		// api_key: "sk-...", access_token=..., "client_secret":"...",
		// token='...', password=..., cookie: "...", session=.... Each
		// label also matches its plural (tokens, passwords, secrets, ...)
		// since a labeled COLLECTION line is just as capable of holding a
		// live secret as a labeled scalar.
		//
		// Deliberately UNANCHORED on the left — the label only needs to
		// appear as a substring immediately before the [:=], not as a
		// whole word — so this also catches shapes a stricter match would
		// miss without a dedicated pattern per shape:
		//   - env_http_headers JSON key names: "X-Donmai-Token": "..."
		//     (the "token" alternative matches the tail of the JSON key).
		//   - env-var dumps: DONMAI_MCP_TOKEN=... (same reasoning, applied
		//     to a KEY=VALUE shell-env line instead of a JSON key).
		//   - a bare token in a URL query string embedded in a panic line:
		//     .../mcp?token=SECRET&retry=1 (appServerStderrValuePattern's
		//     `&` boundary stops the match before the next query param).
		regexp.MustCompile(`(?i)((?:api[_-]?keys?|access[_-]?tokens?|refresh[_-]?tokens?|auth[_-]?tokens?|client[_-]?secrets?|private[_-]?keys?|secrets?|tokens?|passwords?|passwds?|credentials?|cookies?|sessions?)["']?\s*[:=]\s*)` + appServerStderrValuePattern),
		"${1}[REDACTED]",
	},
	{
		// URL userinfo credentials, user:password form:
		// scheme://user:PASSWORD@host/... — redact only the password,
		// keep the username and host visible.
		regexp.MustCompile(`(://[^/\s:@'"]*:)[^/\s@'"]+(@)`),
		"${1}[REDACTED]${2}",
	},
	{
		// URL userinfo credentials, bare-token form (no username:password
		// split): scheme://TOKEN@host/...
		regexp.MustCompile(`(://)[^/\s:@'"]+(@)`),
		"${1}[REDACTED]${2}",
	},
	{
		// Well-known PUBLIC token-format conventions — OpenAI sk-/pk-,
		// GitHub gh[oprsu]_, Slack xox[baprs]- — redacted by shape alone,
		// no label needed, the same way donmai's own dmk_ format already
		// was below. This is deliberately a short, well-documented,
		// vendor-public prefix list rather than a general high-entropy
		// scanner: RE2 (Go's regexp engine) has no practical way to
		// approximate entropy without also sweeping up ordinary git SHAs,
		// UUIDs, and content hashes that are NOT secrets and ARE exactly
		// the kind of diagnostic detail this excerpt exists to preserve.
		// It must never encode a closed-source/org-internal token prefix —
		// this file is OSS and ships to every downstream consumer of this
		// package, not just whichever closed-source product happens to
		// embed it.
		//
		// The body allows `-` (but never `_`) so a multi-segment real-world
		// token — sk-proj-..., a multi-segment xoxb-...-... — is captured
		// in full rather than just its first segment, while a `_`-joined
		// identifier that merely starts with a matching prefix (e.g.
		// pk_organization_id, which is a field NAME, not a secret) stops at
		// the first underscore and is left alone.
		regexp.MustCompile(`\b(?:sk|pk|gh[oprsu]|xox[baprs])[-_][A-Za-z0-9](?:[A-Za-z0-9-]{8,})[A-Za-z0-9]\b`),
		"[REDACTED]",
	},
	{
		// donmai's own machine-token format (dmk_<hex>) — no label needed,
		// the shape alone is distinctive enough to redact outright.
		regexp.MustCompile(`\bdmk_[0-9a-f]{16,}\b`),
		"[REDACTED]",
	},
}

// redactAppServerStderr applies every appServerStderrRedactions pattern in
// turn. Exported to tests as a lowercase package function (not a method) so
// the pure scrubbing behavior can be pinned without spinning up a buffer or
// a subprocess.
func redactAppServerStderr(s string) string {
	for _, r := range appServerStderrRedactions {
		s = r.pattern.ReplaceAllString(s, r.replacement)
	}
	return s
}
