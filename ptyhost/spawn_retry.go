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
// apart from outside. The churn reading is a heuristic, not a proof: the
// same EPERM is also the persistent answer of a pool whose confinement
// profile forbids the exec outright (see isTransientSpawnError), and such a
// refusal fails here after exactly spawnRetryAttempts attempts — about
// 50ms of backoff — with the attempt count in the error, not silently.
// Production shims start real harnesses through this same path, so a refusal
// here is a launch failure, not only a test flake — and a launch failure that
// clears on the next attempt a second later is a retryable one. The bound
// keeps a genuinely broken command (missing binary, bad argv) failing fast:
// only the transient errnos below retry, everything else returns at once.
// A refusal that survives the retries is diagnosed where it happens: the
// shim helper's failure hook (logShimPTYFailure, Linux) records the errno,
// session ids, rlimits, and seccomp/cgroup state alongside the error, so
// the next triage can tell pressure apart from confinement.
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
// container filter answering an otherwise-allowed call under load — states
// of the launcher, not of the command, and usually gone on retry. The
// reading is deliberately heuristic: the same EPERM is also the persistent
// answer of a pool whose confinement profile forbids the exec (the
// confinement hypothesis for the observed flake was never ruled out — no
// deterministic local reproduction exists). That case is NOT absorbed: it
// retries spawnRetryAttempts times and then fails with the attempt count in
// the error, and the shim helper's failure hook (logShimPTYFailure, Linux)
// records the errno, rlimits, and seccomp/cgroup state, which is what
// distinguishes pressure from confinement on the next triage. A permanent
// failure (ENOENT, EACCES, ENOEXEC — the binary is missing, non-executable,
// or not runnable here) never retries: retrying those would only slow down
// an answer that cannot change.
func isTransientSpawnError(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EAGAIN)
}

// startPTYWithRetry runs makeCmd under a PTY sized ws, retrying transiently
// refused starts. It returns the master side; the started command's Process
// is set on success exactly as pty.StartWithSize leaves it. The started
// command is reported through onStart so Spawn can keep teardown ownership
// of it (the Session must signal and Wait the exact *exec.Cmd that started).
//
// A fresh *exec.Cmd starts every attempt: os/exec marks a Cmd started on the
// first Start call even when that call fails ("It is an error to call Start
// twice even if the first call did not create a process"), so reusing one
// Cmd across attempts would deterministically fail the retry with
// "exec: already started" and mask the transient refusal it was built to
// absorb. A fresh PTY pair is likewise opened per attempt: the master/slave
// from a refused attempt is closed before the next one is opened, so
// attempts never share terminal state and a refused attempt leaks no fd.
// The slave is closed in the parent once the child has it (creack/pty's own
// contract); a refused child never reaches exec, so there is no stray
// process to reap — the kernel reaps the failed fork itself.
func startPTYWithRetry(makeCmd func() *exec.Cmd, ws *pty.Winsize, logger *slog.Logger, onStart func(*exec.Cmd)) (*os.File, error) {
	return startPTYWithStarter(makeCmd, logger, func(cmd *exec.Cmd) (*os.File, error) {
		return pty.StartWithSize(cmd, ws)
	}, onStart)
}

// startPTYWithStarter is the injectable core of startPTYWithRetry: starter
// performs one PTY start of cmd. Production passes pty.StartWithSize bound
// to the spawn's winsize; tests pass a fault-injecting starter that refuses
// transiently before delegating to the real one. The retry contract is
// identical either way: every attempt starts a FRESH *exec.Cmd from
// makeCmd, never a reused one.
func startPTYWithStarter(makeCmd func() *exec.Cmd, logger *slog.Logger, starter func(*exec.Cmd) (*os.File, error), onStart func(*exec.Cmd)) (*os.File, error) {
	var err error
	var ptmx *os.File
	for attempt := 1; ; attempt++ {
		cmd := makeCmd()
		ptmx, err = starter(cmd)
		if err == nil {
			if onStart != nil {
				onStart(cmd)
			}
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
