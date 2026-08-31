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
// This calls startShimProcess directly rather than driving a full
// AcceptWork/launchSessionShim launch: launchSessionShim's own cleanup (F1
// — see TestFailedShimLaunchLeavesNoLogFileOrGuardGoroutine) removes this
// exact log file the moment a launch fails to ever announce itself, which
// is unavoidably true of a bare "echo and exit" worker with no real shim
// protocol behind it — so asserting on the file's content through that
// full path would be racing the very cleanup that neighbor test pins.
// Calling startShimProcess directly isolates the concern this test
// actually has (does the child's stdio reach the file at all) from
// launchSessionShim's separate adoption-outcome-dependent lifecycle.
func TestShimChildStdoutStderrLandInThePerSessionLogFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	registryDir := dir + "/registry"
	const sessionID = "sess-stdio-capture"
	const stdoutLine = "shim-child-stdout-marker"
	const stderrLine = "shim-child-stderr-marker"

	d := &Daemon{}
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		// A worker that writes to both streams and exits immediately.
		WorkerCommand: []string{
			"/bin/sh", "-c",
			"echo " + stdoutLine + "; echo " + stderrLine + " 1>&2; exit 0",
		},
		WorktreeParentDir: dir,
	})

	identity := sessionshim.Identity{OrgID: "test-org", SessionID: sessionID}
	launch := sessionshim.Launch{
		Identity:     identity,
		RegistryDir:  registryDir,
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 1,
	}
	started, err := d.startShimProcess(SessionSpec{SessionID: sessionID}, launch, nil)
	if err != nil {
		t.Fatalf("startShimProcess: %v", err)
	}
	if started.PID == 0 {
		t.Fatal("startShimProcess returned pid 0")
	}

	logPath := shimChildLogPath(registryDir, identity)
	var got string
	deadline := time.Now().Add(2 * time.Second)
	for {
		content, readErr := os.ReadFile(logPath)
		if readErr == nil {
			got = string(content)
			if strings.Contains(got, stdoutLine) && strings.Contains(got, stderrLine) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("shim child log %q does not contain both markers (stdout=%q stderr=%q), read err=%v", got, stdoutLine, stderrLine, readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
