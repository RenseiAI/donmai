package afcli

import (
	"context"
	"os/exec"
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

// runGitCommand executes a git subcommand and returns its trimmed stdout.
func runGitCommand(args ...string) (string, error) {
	out, err := exec.Command("git", args...).Output() //nolint:gosec
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
