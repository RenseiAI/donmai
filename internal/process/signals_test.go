//go:build !windows

package process_test

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/internal/process"
)

func TestInstallSignalHandlers_CancelsOnSIGTERM(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	process.InstallSignalHandlers(ctx, cancel)

	// Send SIGTERM to ourselves.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("Kill(SIGTERM): %v", err)
	}

	select {
	case <-ctx.Done():
		// Expected — context was canceled by the signal handler.
	case <-time.After(2 * time.Second):
		t.Fatal("context not canceled within deadline after SIGTERM")
	}
}

func TestInstallSignalHandlers_CancelsOnSIGINT(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	process.InstallSignalHandlers(ctx, cancel)

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("Kill(SIGINT): %v", err)
	}

	select {
	case <-ctx.Done():
		// Expected.
	case <-time.After(2 * time.Second):
		t.Fatal("context not canceled within deadline after SIGINT")
	}
}

func TestInstallSignalHandlers_StopsOnContextDone(t *testing.T) {
	// Verify that when the context is already canceled, the goroutine
	// exits cleanly without blocking.
	ctx, cancel := context.WithCancel(context.Background())
	process.InstallSignalHandlers(ctx, cancel)

	cancel() // trigger the ctx.Done() path in the goroutine

	// If the goroutine leaked, the test would hang or race; this just
	// ensures we can call the function without panicking.
}
