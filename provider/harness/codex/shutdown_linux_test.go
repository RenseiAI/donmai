package codex

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/sessionshim"
	"golang.org/x/sys/unix"
)

const shutdownSubreaperEnv = "DONMAI_CODEX_SHUTDOWN_TEST_SUBREAPER"

// A dedicated subprocess adopts the launcher's orphaned writer and deliberately
// postpones reaping until after Shutdown's assertions. This makes the distinction
// between a zombie and a live writer deterministic without changing host init or
// the test runner's child ownership.
func TestProvider_ShutdownWithZombieDescendant(t *testing.T) {
	if os.Getenv(shutdownSubreaperEnv) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		output, err := os.CreateTemp(t.TempDir(), "subreaper-output")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = output.Close() })
		// Use the same direct self-exec pattern as the launcher fixture.
		process, err := os.StartProcess(executable, []string{
			executable,
			"-test.run=^TestProvider_ShutdownWithZombieDescendant$/^launcher(-exit)?$", "-test.v",
		}, &os.ProcAttr{
			Env:   append(os.Environ(), shutdownSubreaperEnv+"=1"),
			Files: []*os.File{os.Stdin, output, output},
		})
		if err != nil {
			t.Fatal(err)
		}
		state, waitErr := process.Wait()
		out, err := os.ReadFile(output.Name())
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("subreaper lifecycle:\n%s", out)
		if waitErr != nil || !state.Success() {
			t.Fatalf("subreaper lifecycle: state=%v err=%v", state, waitErr)
		}
		return
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	// Every adopted child belongs to this fixture-only process. No signals or
	// process-table sweep are needed to discharge its remaining reap obligation.
	t.Cleanup(func() {
		for {
			var status unix.WaitStatus
			pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if err == unix.ECHILD || (err == nil && pid == 0) {
				return
			}
			if err != nil {
				t.Errorf("reap fixture descendant: %v", err)
				return
			}
		}
	})
	TestProvider_ShutdownWaitsForFinalWrites(t)
}

// Hold a real child live, then wait without reaping, then reap it. These three
// kernel states distinguish writer quiescence from start-identity existence.
func TestShutdownWriterRunning_LiveZombieAndReaped(t *testing.T) {
	cmd := exec.Command("sh", "-c", "read line")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	identity, err := sessionshim.ProcessIdentityFor(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if running, err := shutdownWriterRunning(identity); err != nil || !running {
		t.Fatalf("live child: running=%v err=%v", running, err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := observeOwnedProcessExit(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	state, err := shutdownProcessState(identity.PID)
	if err != nil || !strings.HasPrefix(state, "Z") {
		t.Fatalf("unreaped child: state=%q err=%v", state, err)
	}
	alive, err := identity.Alive()
	if err != nil || !alive {
		t.Fatalf("zombie must retain its start identity: alive=%v err=%v", alive, err)
	}
	if running, err := shutdownWriterRunning(identity); err != nil || running {
		t.Fatalf("zombie cannot write: running=%v err=%v", running, err)
	}
	t.Logf("unreaped child: identity matches=%v state=%s writerRunning=false", alive, state)
	_ = cmd.Wait() // read sees EOF and exits nonzero; Wait still must reap it.
	if running, err := shutdownWriterRunning(identity); err != nil || running {
		t.Fatalf("reaped child: running=%v err=%v", running, err)
	}
	state, err = shutdownProcessState(identity.PID)
	if err != nil || state != "" {
		t.Fatalf("child was not reaped: state=%q err=%v", state, err)
	}
}
