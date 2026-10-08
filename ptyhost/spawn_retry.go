package ptyhost

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// spawnRetryAttempts bounds how many times Spawn offers a transiently refused
// PTY start before giving up. The refusal this absorbs is the rare
// `fork/exec <argv0>: operation not permitted` seen under heavy parallel spawn
// churn (CI and container stress runs, roughly 1 in a few hundred launches):
// Go reports the child-side errno of the whole fork → setsid → TIOCSCTTY →
// execve sequence as one opaque error, so the failing step cannot be told
// apart from outside, and every step in that sequence is transient-capable
// under load (process-table and PTY-index pressure resolve in milliseconds).
// Production shims start real harnesses through this same path, so a refusal
// here is a launch failure, not only a test flake — and a launch failure that
// clears on the next attempt a second later is a retryable one. The bound
// keeps a genuinely broken command (missing binary, bad argv) failing fast:
// only the transient errnos below retry, everything else returns at once.
const spawnRetryAttempts = 3

// spawnRetryDelay spaces the retry attempts. Unloaded spawns take single-digit
// milliseconds, so even the full backoff is noise against a session lifetime
// while being long enough for momentary kernel-side pressure to clear.
const spawnRetryDelay = 25 * time.Millisecond

// isTransientSpawnError reports whether a PTY start failure is worth one more
// attempt. EPERM and EAGAIN are the two errnos a fork/exec child can carry
// back transiently: EAGAIN is fork's documented answer to process-count and
// RLIMIT_NPROC pressure, and EPERM is what the same pressure returns through
// the session/controlling-terminal steps (setsid, TIOCSCTTY) and through a
// container filter answering an otherwise-allowed call under load — all
// states of the launcher, not of the command, and all gone on retry. A
// permanent failure (ENOENT, EACCES, ENOEXEC — the binary is missing,
// non-executable, or not runnable here) never retries: retrying those would
// only slow down an answer that cannot change.
func isTransientSpawnError(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EAGAIN)
}

// startPTYWithRetry runs cmd under a PTY sized ws, retrying transiently
// refused starts. It returns the master side; cmd.Process is set on success
// exactly as pty.StartWithSize leaves it.
//
// A fresh PTY pair is opened per attempt: the master/slave from a refused
// attempt is closed before the next one is opened, so attempts never share
// terminal state and a refused attempt leaks no fd. The slave is closed in
// the parent once the child has it (creack/pty's own contract); a refused
// child never reaches exec, so there is no stray process to reap — the
// kernel reaps the failed fork itself.
func startPTYWithRetry(cmd *exec.Cmd, ws *pty.Winsize, logger *slog.Logger) (*os.File, error) {
	var err error
	var ptmx *os.File
	for attempt := 1; ; attempt++ {
		ptmx, err = pty.StartWithSize(cmd, ws)
		if err == nil {
			if attempt > 1 && logger != nil {
				logger.Warn("ptyhost: pty start refused transiently, retry succeeded",
					"attempt", attempt, "command", cmd.Args)
			}
			return ptmx, nil
		}
		if !isTransientSpawnError(err) || attempt >= spawnRetryAttempts {
			return nil, fmt.Errorf("ptyhost: pty start: %w (attempt %d of %d)", err, attempt, spawnRetryAttempts)
		}
		if logger != nil {
			logger.Warn("ptyhost: pty start refused transiently, retrying",
				"attempt", attempt, "error", err, "command", cmd.Args)
		}
		time.Sleep(spawnRetryDelay)
	}
}
