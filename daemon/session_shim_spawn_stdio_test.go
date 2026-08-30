package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestShimChildStdoutStderrLandInThePerSessionLogFile pins the un-blinding
// fix: a launched shim's stdout/stderr used to be pointed at /dev/null
// (startShimProcess), so every runner/provider error the child process wrote
// — including the exact class of tool/lifecycle adaptation refusal
// repository_sandbox_reconcile.go's doc comment describes — was invisible.
// startShimProcess now captures both streams to
// shimChildLogPath(registryDir, identity), a plain file rather than a
// daemon-owned pipe (a shim is a DETACHED process that outlives this daemon
// — worker_spawner.go's pipe-to-log pattern would hand the shim EPIPE the
// moment this daemon exits or restarts). The filename is the fixed-length
// digest sessionshim.Identity.LogName() produces — the same convention as
// every sibling registry artifact — never the raw session id.
//
// This drives the launch through AcceptWork with a WorkerCommand that writes
// one distinguishable line to each of stdout and stderr and exits
// immediately, without ever publishing a shim discovery record — the exact
// same "launch never announced itself" shape
// TestLaunchFailureFailsTheAcceptClosed already covers for the accept's own
// error contract. That failure is irrelevant here: this test's own concern
// is only whether the child's stdio reached the log file, not whether
// adoption completed.
func TestShimChildStdoutStderrLandInThePerSessionLogFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	registryDir := dir + "/registry"
	const sessionID = "sess-stdio-capture"
	const stdoutLine = "shim-child-stdout-marker"
	const stderrLine = "shim-child-stderr-marker"

	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableOwnership: true,
			OrgID:           "test-org",
			RegistryDir:     registryDir,
			LaunchTimeout:   750 * time.Millisecond,
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		// A worker that writes to both streams and exits immediately without
		// ever publishing a shim discovery record.
		WorkerCommand: []string{
			"/bin/sh", "-c",
			"echo " + stdoutLine + "; echo " + stderrLine + " 1>&2; exit 0",
		},
		WorktreeParentDir: dir,
	})
	d.spawner.opts.ShimSpawn = d.launchSessionShim
	d.spawner.Resume()

	if _, err := d.spawner.AcceptWork(SessionSpec{
		SessionID: sessionID, ProjectID: "p1",
		Repository: "https://example.invalid/x/y", Mode: interactiveRunMode,
	}); err == nil {
		t.Fatal("AcceptWork = nil error; a shim launch that never announced itself must fail the accept (see TestLaunchFailureFailsTheAcceptClosed)")
	}

	logPath := shimChildLogPath(registryDir, sessionshim.Identity{OrgID: "test-org", SessionID: sessionID})
	var content []byte
	deadline := time.Now().Add(2 * time.Second)
	for {
		var err error
		content, err = os.ReadFile(logPath)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("read %s: %v", logPath, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := string(content)
	if !strings.Contains(got, stdoutLine) {
		t.Errorf("shim child log %q does not contain the stdout marker %q", got, stdoutLine)
	}
	if !strings.Contains(got, stderrLine) {
		t.Errorf("shim child log %q does not contain the stderr marker %q", got, stderrLine)
	}
}
