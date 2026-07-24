package afcli

import (
	"context"
	"fmt"
	"os/signal"
	"strings"
	"syscall"
)

// interruptContext returns a context that is cancelled on SIGINT or SIGTERM.
// The caller is responsible for calling the returned cancel function to release
// resources.
func interruptContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM) //nolint:govet
	return ctx
}

// binaryName returns the configured binary name, defaulting to "donmai".
// Prefer using this over hardcoding "donmai" in error strings so embedders
// like rensei-tui get the correct binary name automatically.
func binaryName(cfg Config) string {
	if cfg.BinaryName != "" {
		return cfg.BinaryName
	}
	return "donmai"
}

// daemonDownErr returns a user-facing error for daemon connectivity failures.
// binary is the calling binary name (from binaryName(cfg)).
func daemonDownErr(binary string, underlying error) error {
	if isConnectionRefused(underlying) {
		return fmt.Errorf("daemon is not running — start it with `%s daemon install` then `%s daemon start`", binary, binary)
	}
	return fmt.Errorf("daemon unreachable: %w", underlying)
}

// isConnectionRefused reports whether err is a TCP connection-refused failure.
func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") || strings.Contains(s, "ECONNREFUSED")
}
