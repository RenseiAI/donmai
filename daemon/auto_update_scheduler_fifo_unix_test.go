//go:build unix

package daemon

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// A blocked trust-root read must not hold shutdown past the caller's
// deadline. Construction runs under the attempt context, so Stop's cancel
// releases the waiter even though the construction goroutine stays blocked
// until the path unblocks; the Stop join itself is bounded by the Stop
// context.
func TestAutoUpdateSchedulerBlockedTrustRootRespectsStopDeadline(t *testing.T) {
	fifo := t.TempDir() + "/trustroot"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
	path := writeAutoUpdateDaemonConfig(t, string(ScheduleOnRelease), probe.srv.URL, true)
	clock := newManualUpdateClock(time.Now())
	d := New(Options{
		ConfigPath:       path,
		JWTPath:          t.TempDir() + "/jwt",
		SkipWizard:       true,
		SkipRegistration: true,
		Version:          "0.1.0",
		SpawnerOptions:   SpawnerOptions{WorkerCommand: []string{"/bin/false"}},
		SessionShim:      SessionShimConfig{RegistryDir: t.TempDir()},
	})
	d.autoUpdateClock = clock
	d.autoUpdateRunBound = 3 * time.Second
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	d.mu.Lock()
	d.config.AutoUpdate.TrustRootPath = fifo
	d.config.AutoUpdate.DrainTimeoutSeconds = 1
	d.mu.Unlock()
	if d.autoUpdateScheduler == nil {
		t.Fatal("opted-in config started no scheduler through Start")
	}
	waitForUpdateCondition(t, 5*time.Second, "scheduler tick wait", func() bool {
		return clock.waiterCount() > 0
	})
	clock.fire()
	waitForUpdateCondition(t, 5*time.Second, "attempt in flight", func() bool {
		s := d.autoUpdateScheduler
		return s != nil && s.inFlight.Load()
	})
	start := time.Now()
	stopCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = d.Stop(stopCtx)
	if elapsed := time.Since(start); elapsed > 3500*time.Millisecond {
		t.Fatalf("Stop took %v with a 50ms deadline, want within the attempt bound", elapsed)
	}
	// Unblock the construction read so the leaked goroutine can finish.
	go func() {
		fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return
		}
		_ = syscall.Close(fd)
	}()
	time.Sleep(200 * time.Millisecond)
}
