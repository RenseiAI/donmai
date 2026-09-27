//go:build codex_integration

package codex

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// nativeFixtureProcess uses the same sole waiter and group ownership protocol
// as the headless provider. A returned direct PID does not prove that Codex's
// plugin and MCP descendants have stopped writing to the fixture home.
type nativeFixtureProcess struct {
	owned *ownedProcess
	done  chan error
}

func startNativeFixtureProcess(cmd *exec.Cmd) (*nativeFixtureProcess, error) {
	configureOwnedProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start native fixture: %w", err)
	}
	p := &nativeFixtureProcess{owned: newOwnedProcess(cmd), done: make(chan error, 1)}
	go func() { p.done <- p.owned.wait() }()
	return p, nil
}

// TestNativeFixtureProcessChild is entered only by the synthetic descendant
// control. Its writer inherits a pipe from the test, so an unsuccessful control
// can release it without signalling a process outside its own isolated group.
func TestNativeFixtureProcessChild(t *testing.T) {
	switch os.Getenv("DONMAI_NATIVE_FIXTURE_CHILD") {
	case "":
		return
	case "leader":
		cmd := exec.Command(os.Args[0], "-test.run=^TestNativeFixtureProcessChild$") //nolint:gosec // the current test executable
		cmd.Env = append(os.Environ(), "DONMAI_NATIVE_FIXTURE_CHILD=writer")
		cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
		cmd.ExtraFiles = []*os.File{os.NewFile(3, "writer-exit-receipt")}
		writerReady, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ready, err := bufio.NewReader(writerReady).ReadString('\n')
		if err != nil || ready != "writer-ready\n" {
			t.Fatalf("synthetic writer readiness=%q err=%v", ready, err)
		}
		fmt.Fprint(os.Stdout, ready)
	case "writer":
		stopped := make(chan os.Signal, 1)
		signal.Notify(stopped, syscall.SIGTERM)
		defer signal.Stop(stopped)
		fmt.Fprintln(os.Stdout, "writer-ready")
		stdinClosed := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			close(stdinClosed)
		}()
		select {
		case <-stopped:
		case <-stdinClosed:
		}
		if err := os.WriteFile(os.Getenv("DONMAI_NATIVE_FIXTURE_EXIT_MARKER"), []byte("writer-exited"), 0o600); err != nil {
			t.Fatal(err)
		}
		exitReceipt := os.NewFile(3, "writer-exit-receipt")
		if _, err := fmt.Fprintln(exitReceipt, "writer-exited"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("invalid native fixture child mode")
	}
}

func testNativeFixtureDescendantCleanup(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix process group control does not apply to Windows")
	}
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlRead.Close()
	defer controlWrite.Close()
	exitRead, exitWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer exitRead.Close()
	marker := t.TempDir() + "/writer-exit"
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeFixtureProcessChild$") //nolint:gosec // the current test executable
	cmd.Env = append(os.Environ(), "DONMAI_NATIVE_FIXTURE_CHILD=leader", "DONMAI_NATIVE_FIXTURE_EXIT_MARKER="+marker)
	cmd.Stdin = controlRead
	cmd.ExtraFiles = []*os.File{exitWrite}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	cmd.Stdout = readyWrite
	cmd.Stderr = os.Stderr
	process, err := startNativeFixtureProcess(cmd)
	_ = readyWrite.Close()
	_ = exitWrite.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The pipe remains open until the assertion: even a failed group cleanup
	// leaves only our own writer, which EOF can release without a broad kill.
	joined := false
	defer func() {
		_ = controlWrite.Close()
		_, _ = io.ReadAll(exitRead)
		if !joined {
			<-process.done
		}
	}()
	ready, err := bufio.NewReader(readyRead).ReadString('\n')
	if err != nil || ready != "writer-ready\n" {
		t.Fatalf("synthetic descendant readiness=%q err=%v", ready, err)
	}
	if _, err := io.ReadAll(readyRead); err != nil {
		t.Fatalf("synthetic leader stdout: %v", err)
	}
	stopErr := process.owned.stop(time.Second)
	if stopErr != nil {
		t.Fatalf("stop isolated synthetic descendants: %v", stopErr)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "writer-exited" {
		t.Fatalf("descendant exited before fixture cleanup: marker=%q err=%v", body, err)
	}
	if receipt, err := io.ReadAll(exitRead); err != nil || string(receipt) != "writer-exited\n" {
		t.Fatalf("descendant exit receipt=%q err=%v", receipt, err)
	}
	// The sole waiter must have joined the leader as well as the writer.
	err = <-process.done
	joined = true
	if err != nil {
		t.Fatalf("synthetic leader exit: %v", err)
	}
}
