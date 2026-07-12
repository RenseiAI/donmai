package attachclient

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachwire"
)

// launchRelay re-execs the test binary as the stub relay subprocess bound to
// addr ("127.0.0.1:0" for an ephemeral port), waiting for its readiness line. It
// returns the actual bound address (so a restart can reuse the same port) and a
// SIGKILL func. The subprocess (the sanctioned attachtest relay) owns the
// listener; the test itself opens none.
func launchRelay(t *testing.T, addr string) (boundAddr string, kill func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0]) //nolint:gosec // G204: re-exec of this very test binary; args are test constants
	cmd.Env = append(os.Environ(),
		"ATTACHTEST_RELAY_MAIN=1",
		"ATTACHTEST_RELAY_ARGS=-addr "+addr+" -ring 64",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start relay subprocess: %v", err)
	}
	br := bufio.NewReader(stdout)
	ready := make(chan string, 1)
	go func() {
		line, _ := br.ReadString('\n')
		ready <- strings.TrimSpace(line)
		_, _ = io.Copy(io.Discard, br)
	}()
	select {
	case boundAddr = <-ready:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("relay subprocess did not signal readiness")
	}
	if boundAddr == "" {
		_ = cmd.Process.Kill()
		t.Fatal("relay subprocess reported an empty address")
	}
	kill = func() {
		_ = cmd.Process.Kill() // SIGKILL on unix
		_, _ = cmd.Process.Wait()
	}
	return boundAddr, kill
}

// TestKillNineReconnectAndConverge is a wave exit criterion: the stub relay runs
// as a real subprocess, is SIGKILLed mid-stream, and restarts on the same port.
// The client must reconnect with backoff (same epoch), keep streaming with no
// host-seq regression, and a viewer on the restarted relay must converge via the
// § 13 snapshot+tail repair path (the ring is lost on stub death).
func TestKillNineReconnectAndConverge(t *testing.T) {
	if testing.Short() {
		t.Skip("kill -9 subprocess e2e skipped in -short")
	}
	addr, kill1 := launchRelay(t, "127.0.0.1:0")
	wsURL := "ws://" + addr + "/" + attachwire.VersionPathSegment + "/rooms/room-1"

	sess := newFakeSession(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunHost(ctx, HostConfig{
			AttachURL:            wsURL,
			TokenSource:          staticToken(mkHostToken(testSessionID, 1, "host-jti-kill", true), nil),
			Session:              sess,
			BackoffMin:           10 * time.Millisecond,
			BackoffMax:           100 * time.Millisecond,
			FinalScreenWindow:    200 * time.Millisecond,
			UpgradeProbeInterval: time.Hour,
		})
	}()

	// Stream before the kill and observe via viewer #1.
	for i := 0; i < 5; i++ {
		sess.PushOutput([]byte("pre"))
	}
	v1tok := mkViewerToken(testSessionID, "user-1", "vjti-1", "viewer")
	v1, err := attachtest.AttachViewer(ctx, wsURL, v1tok, attachwire.RoleViewer, nil)
	if err != nil {
		t.Fatalf("attach viewer 1: %v", err)
	}
	pre := collect(v1, 30, 3*time.Second)
	_ = v1.Close()
	var preMax uint64
	for _, f := range pre {
		if f.Seq > preMax {
			preMax = f.Seq
		}
	}
	if preMax == 0 {
		t.Fatal("viewer 1 observed no seq-bearing frames before the kill")
	}

	// SIGKILL the relay and restart it on the SAME port (ring lost).
	kill1()
	_, kill2 := launchRelay(t, addr)
	defer kill2()

	// Give the client time to reconnect (backoff ≤ 100ms) to the restarted relay.
	time.Sleep(900 * time.Millisecond)

	// Viewer #2 attaches to the restarted relay; then post-kill frames stream.
	v2tok := mkViewerToken(testSessionID, "user-2", "vjti-2", "viewer")
	v2, err := attachtest.AttachViewer(ctx, wsURL, v2tok, attachwire.RoleViewer, nil)
	if err != nil {
		t.Fatalf("attach viewer 2: %v", err)
	}
	defer func() { _ = v2.Close() }()
	for i := 0; i < 5; i++ {
		sess.PushOutput([]byte("post"))
	}

	frames := collect(v2, 60, 4*time.Second)
	assertSeqMonotonic(t, frames)

	var sawSnapshot bool
	var maxSeq uint64
	for _, f := range frames {
		if f.Type == attachwire.TypeSnapshot {
			sawSnapshot = true
		}
		if f.Seq > maxSeq {
			maxSeq = f.Seq
		}
	}
	if !sawSnapshot {
		t.Error("viewer 2 never received a Snapshot (snapshot+tail convergence after ring loss)")
	}
	if maxSeq <= preMax {
		t.Errorf("no forward progress after reconnect: viewer 2 max seq %d <= pre-kill max %d", maxSeq, preMax)
	}

	sess.PushExit(0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunHost returned %v, want nil after kill/reconnect/exit", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunHost did not return after Exit")
	}
}
