//go:build unix

package muse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// fakeServeScript writes a stand-in host binary: it answers the version
// probe, forks a helper into its process group (recording the helper's
// pid), then either exits at once or idles without ever answering the
// handshake.
func fakeServeScript(t *testing.T, leaderExits bool) (bin, pidFile string) {
	t.Helper()
	dir := t.TempDir()
	pidFile = filepath.Join(dir, "helper.pid")
	tail := "exec sleep 300"
	if leaderExits {
		tail = "exit 0"
	}
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo 'Muse Code 1.4.6'; exit 0; fi\n" +
		"sleep 300 </dev/null >/dev/null 2>&1 &\n" +
		"echo $! > \"$MUSE_TEST_PIDFILE\"\n" +
		tail + "\n"
	bin = filepath.Join(dir, "muse")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write script: %v", err)
	}
	return bin, pidFile
}

func readHelperPid(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidFile) //nolint:gosec // test temp path
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("helper pid never recorded")
	return 0
}

// waitGone polls until pid no longer exists (or is a reaped zombie).
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("helper pid %d outlived the serve child (orphaned)", pid)
}

// TestSpawn_FailedHandshakeKillsProcessGroup covers both teardown
// orders: the leader still alive when the handshake times out, and the
// leader already exited (and reaped) while its helper lives on. In both
// cases Spawn must return and the helper must die with the group.
func TestSpawn_FailedHandshakeKillsProcessGroup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		leaderExits bool
	}{
		{"leader alive", false},
		{"leader exited", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, pidFile := fakeServeScript(t, tc.leaderExits)
			p, err := New(Options{
				MuseBin:    bin,
				RPCTimeout: 500 * time.Millisecond,
				Env:        map[string]string{"MUSE_TEST_PIDFILE": pidFile},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

			var spawnErr error
			returnsWithin(t, 15*time.Second, "Spawn", func() {
				_, spawnErr = p.Spawn(context.Background(), agent.Spec{Prompt: "x", Cwd: t.TempDir(), Autonomous: true})
			})
			if spawnErr == nil {
				t.Fatal("Spawn succeeded against a host that never handshakes")
			}
			waitGone(t, readHelperPid(t, pidFile))
		})
	}
}
