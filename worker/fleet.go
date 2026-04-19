package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Worker process states reported by Fleet.Status.
const (
	// WorkerStateRunning indicates the child process has been spawned and
	// has not yet exited.
	WorkerStateRunning = "running"
	// WorkerStateStopping indicates Stop or Scale has begun tearing the
	// child down (SIGTERM sent) but the child has not yet been reaped.
	WorkerStateStopping = "stopping"
	// WorkerStateExited indicates the child has exited (either because the
	// Fleet told it to or because it crashed).
	WorkerStateExited = "exited"
)

// scaleGrace is the per-child graceful-stop window used by Scale when
// shrinking. Stop() takes an explicit graceful duration from the caller.
const scaleGrace = 5 * time.Second

// workerEntry is the internal bookkeeping record for a single supervised
// child. Access is serialised via Fleet.mu.
type workerEntry struct {
	proc      *os.Process
	pid       int
	startedAt time.Time
	state     string
	// done is closed by the per-child wait goroutine once os.Process.Wait
	// returns. Stop uses it to observe graceful exit within the grace
	// window.
	done chan struct{}
}

// Fleet supervises a set of worker child processes. It is safe for
// concurrent use by a single owner (one goroutine calling Start/Scale/Stop
// plus optional readers calling Status).
type Fleet struct {
	binaryPath string
	baseArgs   []string

	// Env is the environment passed to every spawned child. When nil the
	// child inherits the parent's environment (exec.Cmd default). Tests
	// set this to inject helper-mode flags.
	Env []string

	mu        sync.Mutex
	processes []*workerEntry
	logger    *slog.Logger
}

// WorkerProcess is a snapshot of a supervised worker's state returned by
// Fleet.Status. It is a plain value — mutating it does not affect the
// Fleet. The name intentionally includes the "Worker" prefix so callers
// outside the worker package read it as a worker-owned process type.
//
//revive:disable-next-line:exported
type WorkerProcess struct {
	PID       int
	StartedAt time.Time
	State     string // "running" | "stopping" | "exited"
}

// NewFleet constructs a Fleet that will spawn binaryPath with baseArgs
// appended by each child. The caller supplies binaryPath (typically from
// os.Executable or exec.LookPath).
func NewFleet(binaryPath string, baseArgs []string) *Fleet {
	args := make([]string, len(baseArgs))
	copy(args, baseArgs)
	return &Fleet{
		binaryPath: binaryPath,
		baseArgs:   args,
		logger:     slog.Default(),
	}
}

// SetLogger overrides the default slog logger. Primarily for tests.
func (f *Fleet) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.Default()
	}
	f.mu.Lock()
	f.logger = l
	f.mu.Unlock()
}

// Start spawns exactly count worker child processes. Returns an error if
// any spawn fails; partial state is left in place (callers should Stop if
// they want a clean rollback). After a successful Start the fleet PID
// file is written with the active PIDs.
func (f *Fleet) Start(ctx context.Context, count int) error {
	if count < 0 {
		return fmt.Errorf("fleet: start: negative count %d", count)
	}
	for i := 0; i < count; i++ {
		if err := f.spawnOne(ctx); err != nil {
			return fmt.Errorf("fleet: start: spawn %d/%d: %w", i+1, count, err)
		}
	}
	if err := f.writePIDFile(); err != nil {
		return fmt.Errorf("fleet: start: %w", err)
	}
	return nil
}

// Scale grows or shrinks the fleet to target processes. Uses
// graceful-stop (SIGTERM, then SIGKILL after scaleGrace) when shrinking.
// After a successful scale the PID file is rewritten.
func (f *Fleet) Scale(ctx context.Context, target int) error {
	if target < 0 {
		return fmt.Errorf("fleet: scale: negative target %d", target)
	}

	current := f.liveCount()
	switch {
	case target > current:
		for i := current; i < target; i++ {
			if err := f.spawnOne(ctx); err != nil {
				return fmt.Errorf("fleet: scale: spawn %d/%d: %w", i+1, target, err)
			}
		}
	case target < current:
		toKill := current - target
		if err := f.shrink(toKill, scaleGrace); err != nil {
			return fmt.Errorf("fleet: scale: %w", err)
		}
	}

	if err := f.writePIDFile(); err != nil {
		return fmt.Errorf("fleet: scale: %w", err)
	}
	return nil
}

// Stop signals all supervised children with SIGTERM, waits up to graceful
// for them to exit, then SIGKILLs survivors. Clears the process table
// and removes the PID file.
func (f *Fleet) Stop(_ context.Context, graceful time.Duration) error {
	// Snapshot entries and mark stopping under the lock.
	f.mu.Lock()
	entries := make([]*workerEntry, 0, len(f.processes))
	for _, e := range f.processes {
		if e.state == WorkerStateExited {
			continue
		}
		e.state = WorkerStateStopping
		entries = append(entries, e)
	}
	f.mu.Unlock()

	// SIGTERM all non-exited children.
	for _, e := range entries {
		if err := e.proc.Signal(syscall.SIGTERM); err != nil {
			// Process may have already exited between mark and signal.
			if !errors.Is(err, os.ErrProcessDone) {
				f.logger.Warn("fleet: sigterm failed", "pid", e.pid, "err", err)
			}
		}
	}

	// Wait for graceful exit or fire SIGKILL on survivors.
	f.waitOrKill(entries, graceful)

	// Clear the table.
	f.mu.Lock()
	f.processes = nil
	f.mu.Unlock()

	if err := RemoveFleetPIDFile(); err != nil {
		return fmt.Errorf("fleet: stop: %w", err)
	}
	return nil
}

// Status returns a point-in-time snapshot of the supervised processes.
func (f *Fleet) Status() []WorkerProcess {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]WorkerProcess, 0, len(f.processes))
	for _, e := range f.processes {
		out = append(out, WorkerProcess{
			PID:       e.pid,
			StartedAt: e.startedAt,
			State:     e.state,
		})
	}
	return out
}

// spawnOne starts a single child, registers it in the process table, and
// launches the per-child wait goroutine.
func (f *Fleet) spawnOne(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, f.binaryPath, f.baseArgs...) //nolint:gosec // binaryPath is caller-supplied and intentional
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	// Setpgid gives the child its own process group so a future Signal
	// to the group would reach any grandchildren. We still signal the
	// immediate pid in Stop, but the group isolation prevents the child
	// from receiving terminal signals meant for the parent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if f.Env != nil {
		cmd.Env = f.Env
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("fleet: spawn: %w", err)
	}

	entry := &workerEntry{
		proc:      cmd.Process,
		pid:       cmd.Process.Pid,
		startedAt: time.Now(),
		state:     WorkerStateRunning,
		done:      make(chan struct{}),
	}

	f.mu.Lock()
	f.processes = append(f.processes, entry)
	logger := f.logger
	f.mu.Unlock()

	logger.Info("fleet: spawned worker", "pid", entry.pid)

	go f.waitChild(cmd, entry)

	return nil
}

// waitChild runs cmd.Wait in its own goroutine and records the terminal
// state of the child. It closes entry.done to signal completion.
func (f *Fleet) waitChild(cmd *exec.Cmd, entry *workerEntry) {
	err := cmd.Wait()

	f.mu.Lock()
	prev := entry.state
	entry.state = WorkerStateExited
	logger := f.logger
	f.mu.Unlock()

	close(entry.done)

	switch {
	case err == nil:
		logger.Info("fleet: worker exited", "pid", entry.pid)
	case prev == WorkerStateStopping:
		// Expected: we asked it to stop.
		logger.Info("fleet: worker stopped", "pid", entry.pid, "err", err)
	default:
		logger.Warn("fleet: worker exited unexpectedly", "pid", entry.pid, "err", err)
	}
}

// liveCount returns the number of processes not yet in the "exited"
// state.
func (f *Fleet) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.processes {
		if e.state != WorkerStateExited {
			n++
		}
	}
	return n
}

// shrink terminates n running processes (last-in-first-out) with a
// graceful window, then reaps them from the table. The PID file is NOT
// rewritten here; the caller (Scale) does that.
func (f *Fleet) shrink(n int, graceful time.Duration) error {
	f.mu.Lock()
	victims := make([]*workerEntry, 0, n)
	for i := len(f.processes) - 1; i >= 0 && len(victims) < n; i-- {
		e := f.processes[i]
		if e.state == WorkerStateExited {
			continue
		}
		e.state = WorkerStateStopping
		victims = append(victims, e)
	}
	f.mu.Unlock()

	for _, e := range victims {
		if err := e.proc.Signal(syscall.SIGTERM); err != nil {
			if !errors.Is(err, os.ErrProcessDone) {
				f.logger.Warn("fleet: sigterm failed", "pid", e.pid, "err", err)
			}
		}
	}

	f.waitOrKill(victims, graceful)

	// Reap exited entries from the table.
	f.reapExited()
	return nil
}

// waitOrKill waits up to graceful for every entry's wait goroutine to
// complete, then SIGKILLs any survivors and waits briefly for them to be
// reaped.
func (f *Fleet) waitOrKill(entries []*workerEntry, graceful time.Duration) {
	if len(entries) == 0 {
		return
	}
	deadline := time.Now().Add(graceful)
	for _, e := range entries {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		select {
		case <-e.done:
			// Child exited gracefully.
		case <-time.After(remaining):
			// Deadline hit for this child — SIGKILL and wait for reap.
			f.logger.Warn("fleet: graceful window elapsed, sending SIGKILL", "pid", e.pid)
			if err := e.proc.Kill(); err != nil {
				if !errors.Is(err, os.ErrProcessDone) {
					f.logger.Error("fleet: sigkill failed", "pid", e.pid, "err", err)
				}
			}
			// Block until reaped; Wait must return for the per-child
			// goroutine to close done. Bound this with a hard cap so a
			// truly stuck kernel doesn't hang Stop forever.
			select {
			case <-e.done:
			case <-time.After(5 * time.Second):
				f.logger.Error("fleet: child did not reap after SIGKILL", "pid", e.pid)
			}
		}
	}
}

// reapExited removes entries in the "exited" state from the process
// table. Called after a Scale shrink to keep the table compact.
func (f *Fleet) reapExited() {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.processes[:0]
	for _, e := range f.processes {
		if e.state != WorkerStateExited {
			kept = append(kept, e)
		}
	}
	// Zero out any tail entries so the GC can collect them.
	for i := len(kept); i < len(f.processes); i++ {
		f.processes[i] = nil
	}
	f.processes = kept
}

// writePIDFile persists the current live PIDs to the fleet PID file.
func (f *Fleet) writePIDFile() error {
	f.mu.Lock()
	pids := make([]int, 0, len(f.processes))
	for _, e := range f.processes {
		if e.state == WorkerStateExited {
			continue
		}
		pids = append(pids, e.pid)
	}
	f.mu.Unlock()
	if err := WriteFleetPIDs(pids); err != nil {
		return err
	}
	return nil
}
