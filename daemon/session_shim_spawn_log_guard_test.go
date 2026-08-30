package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestRedactShimChildLogMasksSecretShapes pins the redaction pass: every
// shimChildLogSecretPatterns shape is masked to same-length 'x' runs, and
// content the guard has no business touching — bytes past the snapshot size
// it was given, simulating a concurrent append from the shim child's own
// O_APPEND writes — is left completely untouched.
func TestRedactShimChildLogMasksSecretShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		secret string
	}{
		{"bearer token", "Authorization: Bearer abcDEF012345678.ghiJKL901234"},
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
