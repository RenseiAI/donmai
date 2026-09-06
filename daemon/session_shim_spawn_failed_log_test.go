package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

// TestAdoptedShimLeavesNoFailureLogAndStillUnlinksTheLiveOne is the
// discriminating control for the retention change: keeping a child log on
// FAILURE must not turn into keeping one on every session.
//
// Two properties, one launch. A launch that reaches trackLaunchedShim must
// leave no `.failed` sibling at all — the preservation defer is gated on
// launchAdopted, and a bug there would quietly accumulate one retained log per
// successful session. And the ordinary terminal disposal must still unlink the
// live log, exactly as before this change.
func TestAdoptedShimLeavesNoFailureLogAndStillUnlinksTheLiveOne(t *testing.T) {
	f := newShimSpawnFixture(t)

	spec := f.interactiveSpec("sess-adopted-no-failure-log")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)

	logPath := shimChildLogPath(f.registry, id)
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("live log missing after a successful launch: %v", err)
	}
	failedPath := shimFailedChildLogPath(f.registry, id)
	if _, err := os.Stat(failedPath); err == nil {
		t.Fatalf("an adopted session produced a retained failure log at %s; preservation must be gated on the launch failing", failedPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error stat'ing %s: %v", failedPath, err)
	}

	if err := f.daemon.StopAdoptedSessionShim(id.OrgID, id.SessionID, shimwire.StopHostShutdown); err != nil {
		t.Fatalf("StopAdoptedSessionShim: %v", err)
	}
	waitFor(t, 10*time.Second, "the terminal cleanup to unlink the adopted session's live log", func() bool {
		_, err := os.Stat(logPath)
		return os.IsNotExist(err)
	})
	if _, err := os.Stat(failedPath); !os.IsNotExist(err) {
		t.Fatalf("terminal cleanup left a %s behind (stat err %v); only a FAILED launch retains one", shimFailedChildLogSuffix, err)
	}
}

// TestPreserveShimChildLogRedactsBeforeRetaining is the reason preservation is
// safe to add at all.
//
// The guard goroutine only sweeps every shimChildLogGuardInterval, so up to one
// interval of the child's output has never been masked — and this file now
// survives for a day instead of being deleted in the next instruction. Both the
// retained FILE and the tail carried out on the error must be masked, and the
// two must agree, because they come from one pass over one buffer.
func TestPreserveShimChildLogRedactsBeforeRetaining(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	id := sessionshim.Identity{OrgID: "test-org", SessionID: "sess-redact-before-retain"}
	const secret = "sk-" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const marker = "provider said no"
	if err := os.WriteFile(shimChildLogPath(dir, id), []byte(marker+"\ntoken="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	kept, tail := preserveShimChildLog(dir, id)
	if kept != shimFailedChildLogPath(dir, id) {
		t.Fatalf("kept path = %q, want the .failed sibling %q", kept, shimFailedChildLogPath(dir, id))
	}
	if strings.Contains(tail, secret) {
		t.Errorf("the tail carried out on the error quotes a credential: %q", tail)
	}
	if !strings.Contains(tail, marker) {
		t.Errorf("tail = %q, want it to carry the child's own diagnosis", tail)
	}
	content, err := os.ReadFile(kept) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read retained log: %v", err)
	}
	if strings.Contains(string(content), secret) {
		t.Errorf("the RETAINED FILE still holds the credential:\n%s", content)
	}
	if !strings.Contains(string(content), "token=") {
		t.Errorf("redaction removed more than the secret span:\n%s", content)
	}
}

// TestPreserveShimChildLogWithoutAFile pins the ordinary case for a launch that
// failed before startShimProcess ever created one: no file, no error, no path,
// no tail. This runs on a path that is already returning a failure and must not
// manufacture a second one.
func TestPreserveShimChildLogWithoutAFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	kept, tail := preserveShimChildLog(dir, sessionshim.Identity{OrgID: "o", SessionID: "never-started"})
	if kept != "" || tail != "" {
		t.Fatalf("preserveShimChildLog = (%q, %q), want empty for a launch with no log", kept, tail)
	}
}

func TestShimChildLogTailExcerpt(t *testing.T) {
	t.Parallel()

	manyLines := make([]string, 0, shimFailedChildLogTailLines+5)
	for i := range shimFailedChildLogTailLines + 5 {
		manyLines = append(manyLines, "line-"+string(rune('a'+i%26))+string(rune('0'+i%10)))
	}

	tests := []struct {
		name    string
		buf     string
		partial bool
		want    string
	}{
		{
			name: "empty",
			buf:  "",
			want: "",
		},
		{
			name: "lines are joined on one line so a NACK reason stays one field",
			buf:  "first\nsecond\n",
			want: "first | second",
		},
		{
			name: "blank and whitespace-only lines are dropped",
			buf:  "first\n\n   \nsecond\n",
			want: "first | second",
		},
		{
			name: "CRLF is normalized rather than trailing every line with \\r",
			buf:  "first\r\nsecond\r\n",
			want: "first | second",
		},
		{
			// A read that started mid-file cannot know where its first line
			// began, so quoting it as though it were whole would be a lie.
			name:    "a partial read drops its truncated first line",
			buf:     "ncated\nwhole\n",
			partial: true,
			want:    "whole",
		},
		{
			name: "only the last N lines survive: a fatal error is the last thing a dying process prints",
			buf:  strings.Join(manyLines, "\n"),
			want: strings.Join(manyLines[len(manyLines)-shimFailedChildLogTailLines:], " | "),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shimChildLogTailExcerpt([]byte(tc.buf), tc.partial); got != tc.want {
				t.Errorf("excerpt = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRedactAndReadShimChildLogTailIsBoundedByBytes proves the byte ceiling is
// enforced by the READ and not only by the line count: a child that writes one
// enormous line must not be able to turn a NACK reason into a multi-kilobyte
// blob.
func TestRedactAndReadShimChildLogTailIsBoundedByBytes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "big.log")
	// Two lines: an enormous first one, then the fatal line at the end. Only
	// the tail window is read, so the first line is partially read and dropped.
	body := strings.Repeat("n", shimFailedChildLogTailBytes*2) + "\nthe last thing it printed\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	tail := redactAndReadShimChildLogTail(path)
	if tail != "the last thing it printed" {
		t.Fatalf("tail = %q, want only the final line: the truncated head must be dropped", tail)
	}
}

func TestSweepFailedShimChildLogs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Now()
	id := func(session string) sessionshim.Identity {
		return sessionshim.Identity{OrgID: "test-org", SessionID: session}
	}
	stale := shimFailedChildLogPath(dir, id("stale"))
	fresh := shimFailedChildLogPath(dir, id("fresh"))
	live := shimChildLogPath(dir, id("live"))
	record := filepath.Join(dir, "some-record.json")
	for _, path := range []string{stale, fresh, live, record} {
		if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-shimFailedChildLogRetention - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	sweepFailedShimChildLogs(dir, now)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("a failure log past the retention window survived (stat err %v)", err)
	}
	for _, path := range []string{fresh, live, record} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the sweep removed %s, which it must not touch: %v", filepath.Base(path), err)
		}
	}
}

// TestSweepFailedShimChildLogsToleratesAMissingDirectory pins the very first
// launch on a fresh host: the registry directory does not exist yet, and the
// sweep must be silent about it rather than failing a launch.
func TestSweepFailedShimChildLogsToleratesAMissingDirectory(t *testing.T) {
	t.Parallel()
	sweepFailedShimChildLogs(filepath.Join(t.TempDir(), "not-created-yet"), time.Now())
}
