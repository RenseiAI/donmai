package process

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// InstallSignalHandlers starts a goroutine that listens for SIGINT and SIGTERM.
// On receipt of either signal, cancel is called. The goroutine stops cleanly
// when ctx is done and signal.Stop is called for cleanup.
//
// InstallSignalHandlers does not block — it returns immediately after starting
// the background goroutine.
func InstallSignalHandlers(ctx context.Context, cancel context.CancelFunc) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer signal.Stop(ch)
		select {
		case <-ctx.Done():
		case <-ch:
			cancel()
		}
	}()
}
