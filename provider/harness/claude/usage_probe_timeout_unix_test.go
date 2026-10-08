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
