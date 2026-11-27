//go:build unix

package claude

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// writeForkingUsageCLI publishes a fake CLI that forks a grandchild
// past the probe timeout: a background sleeper that records its pid,
// then a foreground sleep the probe context outlives. With the whole
// process group killed on expiry, both sleeps die with the leader;
// with only the leader killed, the background sleeper is orphaned and
// keeps running — the stray this test hunts.
// The file's unix build tag already excludes platforms without /bin/sh.
func writeForkingUsageCLI(t *testing.T, pidFile string) string {
	t.Helper()
	if fakeCLIDispatcherErr != "" {
		t.Fatalf("prepare fake CLI dispatcher: %s", fakeCLIDispatcherErr)
	}
	script := "#!/bin/sh\n" +
		"sleep 30 &\n" +
		"echo $! > '" + pidFile + "'\n" +
		"sleep 30\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude-forking.sh")
	if err := writeFakeCLIFile(path+".fixture", script); err != nil {
		t.Fatalf("write fake cli fixture: %v", err)
	}
	if err := os.Link(fakeCLIDispatcher, path); err != nil { //nolint:gosec // atomically publishes a hard-linked test fixture
		t.Fatalf("publish fake cli dispatcher: %v", err)
	}
	return path
}

// forkedChildPID reads the grandchild pid the fixture recorded, waiting
// briefly for the background fork to land it.
func forkedChildPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(pidFile) //nolint:gosec // test-owned fixture output
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 1 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("forking fixture never recorded its grandchild pid: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitProcessGone polls until kill(pid, 0) reports the process gone.
// A SIGKILLed group member is reaped promptly; a process that survives
// the probe timeout is still signalable after the grace below.
func awaitProcessGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) survived the probe timeout; the kill left a stray process", what, pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestProbeUsageTimeoutKillsProcessGroup drives the production probe
// entry point against a CLI whose answer never comes: the probe
// context expires while a forked grandchild still sleeps. The probe
// must report an unanswered failure — a read that never answered
// carries no login verdict — and no member of the probe's process
// group may survive the timeout.
func TestProbeUsageTimeoutKillsProcessGroup(t *testing.T) {
	t.Parallel()

	pidFile := filepath.Join(t.TempDir(), "forked-child.pid")
	binary := writeForkingUsageCLI(t, pidFile)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	accountID, probed, _ := ProbeUsage(ctx, binary)
	if probed.Unavailable == nil || probed.Unavailable.Reason != agent.UsageUnavailableProbeFailed {
		t.Fatalf("probed = %+v, want probeFailed from the timed-out read", probed.Unavailable)
	}
	if probed.Unavailable.Answered {
		t.Error("timed-out read marked answered; a read that never answered carries no verdict")
	}
	if accountID != "" {
		t.Errorf("account id = %q, want empty when the read produced nothing", accountID)
	}

	pid := forkedChildPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	awaitProcessGone(t, pid, "forked usage-probe grandchild")
}

// hangingProbe publishes a fixture whose status read records its leader and
// pipe-holding helper pids, signals readiness, then waits to be killed.
func hangingProbe(t *testing.T, variant string) (binary, selected, pidFile, helperFile string) {
	t.Helper()
	if fakeCLIDispatcherErr != "" {
		t.Fatalf("prepare fake CLI dispatcher: %s", fakeCLIDispatcherErr)
	}
	dir := t.TempDir()
	pidFile, helperFile = filepath.Join(dir, "status.pid"), filepath.Join(dir, "helper.pid")
	usage := `printf '{"type":"system","subtype":"init"}\n'`
	if variant == "ok" {
		usage = `printf '{"type":"control_response","response":{"subtype":"success","response":{"rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":54},"seven_day":{"utilization":18.4}}}}}\n'`
	}
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" auth status --json \"*)\n" +
		"  echo $$ > '" + pidFile + "'\n" +
		"  sleep 30 & echo $! > '" + helperFile + "'\n" +
		"  sleep 30;;\n" +
		"esac\n" + usage + "\nexit 0\n"
	path := filepath.Join(dir, "fake-claude-hanging-status.sh")
	if err := writeFakeCLIFile(path+".fixture", script); err != nil {
		t.Fatalf("write fake cli fixture: %v", err)
	}
	if err := os.Link(fakeCLIDispatcher, path); err != nil { //nolint:gosec // atomically publishes a hard-linked test fixture
		t.Fatalf("publish fake cli dispatcher: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "selected-home"), 0o700); err != nil {
		t.Fatalf("make selected home: %v", err)
	}
	return path, filepath.Join(dir, "selected-home"), pidFile, helperFile
}

// assertHangingStatusGone proves the owned stop reaped the group.
func assertHangingStatusGone(t *testing.T, pidFile, helperFile string) {
	t.Helper()
	t.Cleanup(func() { _ = syscall.Kill(forkedChildPID(t, pidFile), syscall.SIGKILL) })
	t.Cleanup(func() { _ = syscall.Kill(forkedChildPID(t, helperFile), syscall.SIGKILL) })
	awaitProcessGone(t, forkedChildPID(t, pidFile), "status leader")
	awaitProcessGone(t, forkedChildPID(t, helperFile), "status pipe holder")
}

// TestProbeUsageForConfigDir_CancelledStatusKeepsWindows proves the selected
// identity read owns its children: cancelling after readiness returns
// promptly with the windows preserved, an empty identity, and both fixture
// processes gone.
func TestProbeUsageForConfigDir_CancelledStatusKeepsWindows(t *testing.T) {
	t.Parallel()

	binary, selected, pidFile, helperFile := hangingProbe(t, "ok")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var gotID string
	var gotProbed agent.UsageLimits
	done := make(chan struct{}, 1)
	go func() {
		gotID, gotProbed, _ = ProbeUsageForConfigDir(ctx, binary, selected)
		close(done)
	}()
	// The helper pid lands only after the status child started and forked
	// its pipe holder, so cancelling now exercises the owned group stop.
	_ = forkedChildPID(t, helperFile)
	cancelledAt := time.Now()
	cancel()
	select {
	case <-done:
		if elapsed := time.Since(cancelledAt); elapsed > 2*time.Second {
			t.Errorf("cancelled probe held the caller for %s", elapsed)
		}
		if gotProbed.Unavailable != nil || len(gotProbed.Windows) != 2 {
			t.Fatalf("probed = %+v, want the answered windows preserved", gotProbed.Unavailable)
		}
		if gotID != "" {
			t.Errorf("account id = %q, want empty when the identity read was cancelled", gotID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled probe did not return within 2s")
	}
	assertHangingStatusGone(t, pidFile, helperFile)
}

// TestProbeUsageForConfigDir_ShortDeadlineSilentStatusIsUnanswered proves the
// same ownership on the silent usage path: a short caller deadline returns
// an unanswered failure with both fixture processes gone.
func TestProbeUsageForConfigDir_ShortDeadlineSilentStatusIsUnanswered(t *testing.T) {
	t.Parallel()

	binary, selected, pidFile, helperFile := hangingProbe(t, "silent")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	started := time.Now()
	accountID, probed, _ := ProbeUsageForConfigDir(ctx, binary, selected)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("short-deadline probe held the caller for %s", elapsed)
	}
	if probed.Unavailable == nil || probed.Unavailable.Reason != agent.UsageUnavailableProbeFailed {
		t.Fatalf("probed = %+v, want probeFailed from the silent read", probed.Unavailable)
	}
	if probed.Unavailable.Answered {
		t.Error("silent read with a hung status marked answered; nothing answered")
	}
	if accountID != "" {
		t.Errorf("account id = %q, want empty when the read produced nothing", accountID)
	}
	assertHangingStatusGone(t, pidFile, helperFile)
}
