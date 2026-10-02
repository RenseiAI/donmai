package daemon

import (
	"bytes"
	"encoding/base64"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestRedactShimChildLogMasksSecretShapes pins the redaction pass: every
// shimChildLogSecretPatterns shape is masked (KEY=VALUE env lines keep
// their key name and mask only the value), and content the guard has no
// business touching — bytes past the snapshot size it was given, simulating
// a concurrent append from the shim child's own O_APPEND writes — is left
// completely untouched.
func TestRedactShimChildLogMasksSecretShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		secret string
	}{
		{"bearer token", "Authorization: Bearer abcDEF012345678.ghiJKL901234"},
		{"jwt", "auth eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJVadQssw5c done"},
		{"secret-like KEY=VALUE env line", "DATABASE_PASSWORD=hunter2supersecretvalue"},
		{"exported secret-like KEY=VALUE env line", "export PRIVATE_KEY=AbCdEfGhIjKlMnOpQrStUvWxYz123456"},
		{"secret-like JSON key with a short value", `{"db_password": "hunter2-is-short"}`},
		{"openai-style sk- key", "OPENAI_API_KEY=sk-abcdefghijklmnopqrstuvwx"},
		{"rensei rsk_ token", "token=rsk_abcdefghijklmnopqrstuvwxyz0123"},
		{"rensei rsp_ token", "reg=rsp_abcdefghijklmnopqrstuvwxyz0123"},
		{"donmai dmk_ token", "DMK=dmk_0123456789abcdef0123456789abcdef01234567"},
		{"generic 32+ char hex run", "digest=" + strings.Repeat("a1", 20)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "child.log")
			line := "before-marker " + tc.secret + " after-marker\n"
			if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(path, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()

			if err := redactShimChildLog(f, int64(len(line))); err != nil {
				t.Fatalf("redactShimChildLog: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(line) {
				t.Fatalf("redaction changed length: got %d bytes, want %d (same-offset in-place substitution must never shift content)", len(got), len(line))
			}
			if !bytes.HasPrefix(got, []byte("before-marker ")) || !bytes.HasSuffix(got, []byte(" after-marker\n")) {
				t.Fatalf("redaction touched surrounding non-secret content: %q", got)
			}
			if bytes.Contains(got, []byte(tc.secret)) {
				t.Fatalf("secret-shaped content survived redaction: %q", got)
			}
			if !bytes.Contains(got, bytes.Repeat([]byte("x"), 8)) {
				t.Fatalf("expected a masked run of 'x' characters in %q", got)
			}
		})
	}
}

// TestRedactShimChildLogOpaqueRuns pins the generic catch-all: which runs of
// 32+ base64/base64url bytes are masked, and exactly which bytes. mask is the
// exact substring that must come back as a same-length run of 'x' with every
// other byte untouched; an empty mask means the line must survive
// byte-for-byte. Only two shapes are exempt — canonical UUIDs and filesystem
// paths — and the masked half of the table proves neither exemption can be
// stretched to cover a token.
func TestRedactShimChildLogOpaqueRuns(t *testing.T) {
	t.Parallel()

	const (
		b64      = "AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"     // 36 bytes of the base64 alphabet
		b64url   = "abcDEF0123456789-_abcDEF0123456789-_ab"   // 38 bytes with base64url '-' and '_'
		b64url40 = "Xk9_pQ2-vL7mN4zR8sT1-uW6yA3bC5dE0fG_hJ2k" // 40 bytes with base64url '-' and '_'
		padded   = "QWxh/ZGRp+bjpvcGVuIHNlc2FtZSBhbmQgbW9yZQ=="
		hex33    = "9f1c8a2b3d4e5f60718293a4b5c6d7e8f"
		hex40    = "0123456789abcdef0123456789abcdef01234567"
		pathLike = "Zq8vLmN2pR4s/T7uWx9yAbC3/dE5fG6hJ8kM" // standard base64 whose '/' split it into short pieces
		uuid     = "550e8400-e29b-41d4-a716-446655440000"
	)

	cases := []struct {
		name string
		line string
		mask string
	}{
		// Both base64 alphabets, standard and url-safe, padded or not.
		{"base64 run", "token " + b64 + " end\n", b64},
		{"base64url run with - and _", "token " + b64url + " end\n", b64url},
		{"padded standard base64 with + and /", "blob " + padded + " end\n", padded},
		{"exactly 32 bytes", "t " + b64[:32] + " end\n", b64[:32]},
		{"long hex run", "commit " + hex40 + " end\n", hex40},

		// The bytes around a run never take part in the decision.
		{"sentence-final period after base64", "the key is " + b64 + ".\n", b64},
		{"sentence-final period after base64url", "the key is " + b64url + ".\n", b64url},
		{"ellipsis before base64", "..." + b64 + " done\n", b64},
		{"ellipsis before base64url", "..." + b64url + " done\n", b64url},
		{"double quotes", `value "` + b64url + `"` + "\n", b64url},
		{"single quotes", "value '" + b64url + "'\n", b64url},
		{"parentheses", "(" + b64url + ")\n", b64url},
		{"colon", "token:" + b64url + "\n", b64url},
		{"json value", `{"session":"` + b64url + `","ok":true}` + "\n", b64url},
		{"file extension", "kept " + hex33 + ".log beside the record\n", hex33},

		// Neither exemption stretches to cover a token.
		{"base64url segment in a path masks the whole path", "open /var/lib/donmai/" + b64url40 + "/state.json\n", "/var/lib/donmai/" + b64url40 + "/state"},
		{"hex segment in a path masks the whole path", "/var/cache/donmai/" + hex40 + "/out.log\n", "/var/cache/donmai/" + hex40 + "/out"},
		{"standard base64 that splits into short pieces at its own '/'", "secret " + pathLike + " end\n", pathLike},
		{"uuid glued to a long token", "id " + uuid + "-" + b64 + " end\n", uuid + "-" + b64},
		{"hyphenated words outside a path", "step acceptance-checklist-for-the-integration-suite failed\n", "acceptance-checklist-for-the-integration-suite"},

		// Readable: short runs, canonical UUIDs, filesystem paths.
		{"31 bytes is short", "t " + b64[:31] + " end\n", ""},
		{"short base64url token", "t abcDEF0123456789-_abc end\n", ""},
		{"uuid", "launch " + uuid + " failed\n", ""},
		{"upper-case uuid", "launch " + strings.ToUpper(uuid) + " failed\n", ""},
		{"session id", "session sess-" + uuid + " never published a record\n", ""},
		{"missing scenario file path", "scenario file is missing: /var/lib/donmai/scenarios/acceptance-checklist.json\n", ""},
		{"dot-directory path", "open /tmp/.donmai/scenarios/run-42.json: no such file\n", ""},
		{"source path", "/home/runner/work/donmai/donmai/daemon/session_shim_spawn_log_guard_test.go:42: boom\n", ""},
		{"path with a uuid segment", "/opt/Library/Developer/CoreSimulator/Devices/" + strings.ToUpper(uuid) + "/data\n", ""},
		{"url path with a session id", "GET https://example.com/api/v1/sessions/sess-" + uuid + "/events 404\n", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := tc.line
			if tc.mask != "" {
				if strings.Count(tc.line, tc.mask) != 1 {
					t.Fatalf("bad case: mask %q must occur exactly once in %q", tc.mask, tc.line)
				}
				want = strings.Replace(tc.line, tc.mask, strings.Repeat("x", len(tc.mask)), 1)
			}
			buf := []byte(tc.line)
			changed := redactShimChildLogBytes(buf)
			if string(buf) != want {
				t.Fatalf("redaction of %q:\n got  %q\n want %q", tc.line, buf, want)
			}
			if changed != (tc.mask != "") {
				t.Fatalf("redactShimChildLogBytes reported changed=%v, want %v", changed, tc.mask != "")
			}
		})
	}
}

// TestRedactShimChildLogMasksRandomTokensInEveryContext is the bypass proof
// for the exemptions. Seeded random tokens from both base64 alphabets, padded
// and unpadded, 32-64 bytes long, are dropped into every context the
// exemptions or the old neighbour skip could have mistaken for a path, a
// filename or an identifier; every byte of every token must come back masked.
// The seed is fixed, so a failure names a reproducible token.
func TestRedactShimChildLogMasksRandomTokensInEveryContext(t *testing.T) {
	t.Parallel()

	contexts := []string{
		"%s",
		"the key is %s.",
		"...%s done",
		`"%s"`,
		"'%s'",
		"(%s)",
		"token:%s",
		`{"value":"%s"}`,
		"kept %s.log",
		"open /%s",
		"/var/lib/donmai/%s/state.json",
		"/home/runner/%s/sess-550e8400-e29b-41d4-a716-446655440000/log",
	}
	encodings := []struct {
		name string
		enc  *base64.Encoding
	}{
		{"std", base64.StdEncoding},
		{"raw std", base64.RawStdEncoding},
		{"url", base64.URLEncoding},
		{"raw url", base64.RawURLEncoding},
	}

	for _, e := range encodings {
		t.Run(e.name, func(t *testing.T) {
			t.Parallel()
			var seed [32]byte
			copy(seed[:], "redaction bypass proof "+e.name)
			src := rand.NewChaCha8(seed) //nolint:gosec // G404: a fixed, non-cryptographic seed is the point; a failure names a reproducible token
			rng := rand.New(src)         //nolint:gosec // G404: same deterministic source as above
			for range 1000 {
				raw := make([]byte, 24+rng.IntN(25))
				_, _ = src.Read(raw)
				token := e.enc.EncodeToString(raw)
				for _, context := range contexts {
					line := strings.Replace(context, "%s", token, 1)
					buf := []byte(line)
					redactShimChildLogBytes(buf)
					at := strings.Index(line, token)
					if strings.Trim(string(buf[at:at+len(token)]), "x") != "" {
						t.Fatalf("token survived redaction:\n in  %q\n out %q", line, buf)
					}
				}
			}
		})
	}
}

// TestRedactShimChildLogNeverTouchesBytesPastSize is the race-safety control:
// redactShimChildLog must be a no-op on content beyond the size it was
// given, since that range may be a concurrent write from the shim child's
// own O_APPEND fd that has not been snapshotted yet.
func TestRedactShimChildLogNeverTouchesBytesPastSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "child.log")
	prefix := "plain line, no secret here\n"
	tail := "sk-" + strings.Repeat("a", 40) + "\n"
	if err := os.WriteFile(path, []byte(prefix+tail), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	// Only "see" the prefix — as if the tail had not been written yet.
	if err := redactShimChildLog(f, int64(len(prefix))); err != nil {
		t.Fatalf("redactShimChildLog: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(prefix+tail)) {
		t.Fatalf("redaction touched content beyond the given size: got %q, want unchanged %q", got, prefix+tail)
	}
}

// TestCapShimChildLogTruncatesAndMarksOnce pins the size bound: a file over
// shimChildLogCapBytes is trimmed back to the cap and gets exactly one
// truncation marker line naming the bytes dropped; a file at or under the
// cap is left untouched.
func TestCapShimChildLogTruncatesAndMarksOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "child.log")
	oversized := bytes.Repeat([]byte("a"), shimChildLogCapBytes+1024)
	if err := os.WriteFile(path, oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	if err := capShimChildLog(f, int64(len(oversized))); err != nil {
		t.Fatalf("capShimChildLog: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= shimChildLogCapBytes || info.Size() > shimChildLogCapBytes+256 {
		t.Fatalf("capped size = %d, want just over %d (cap plus one short marker line)", info.Size(), shimChildLogCapBytes)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("truncated at")) {
		t.Fatalf("capped log %q does not carry a truncation marker", got[len(got)-200:])
	}
}

// TestCapShimChildLogNoopUnderCap is the control: a file at or under the cap
// is never truncated or marked.
func TestCapShimChildLogNoopUnderCap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "child.log")
	content := []byte("well under the cap\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	if err := capShimChildLog(f, int64(len(content))); err != nil {
		t.Fatalf("capShimChildLog: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("an under-cap file must be left untouched: got %q, want %q", got, content)
	}
}

// TestGuardShimChildLogOnceStopsAfterTerminalRemoval pins the guard
// goroutine's self-termination contract: once removeShimChildLog disposes
// of the file at terminal cleanup, the next guard tick must report false so
// runShimChildLogGuard's loop exits instead of spinning on a missing file
// forever.
func TestGuardShimChildLogOnceStopsAfterTerminalRemoval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id := sessionshim.Identity{OrgID: "org", SessionID: "sess-guard-stop"}
	logPath := shimChildLogPath(dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("some output\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !guardShimChildLogOnce(logPath) {
		t.Fatal("guardShimChildLogOnce = false while the log file still exists")
	}

	removeShimChildLog(dir, id)

	if guardShimChildLogOnce(logPath) {
		t.Fatal("guardShimChildLogOnce = true after removeShimChildLog disposed of the file; the guard goroutine would spin forever")
	}
}
