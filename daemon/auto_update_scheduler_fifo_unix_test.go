//go:build unix

package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	sched := d.autoUpdateScheduler
	_ = d.Stop(stopCtx)
	if elapsed := time.Since(start); elapsed > 3500*time.Millisecond {
		t.Fatalf("Stop took %v with a 50ms deadline, want within the attempt bound", elapsed)
	}
	// Pin the pre-check construction bound, not just the Stop join: the
	// waiter must observe the scheduler cancel and release inFlight,
	// letting run exit and close done even though the construction
	// goroutine stays blocked on the FIFO until unblocked below. Without
	// the select on ctx.Done() the attempt wedges with inFlight true
	// forever and every later tick is skipped.
	waitForUpdateCondition(t, 5*time.Second, "pre-check released inFlight after Stop cancel", func() bool {
		return !sched.inFlight.Load()
	})
	waitForUpdateCondition(t, 5*time.Second, "scheduler done closed after Stop cancel", func() bool {
		select {
		case <-sched.done:
			return true
		default:
			return false
		}
	})
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

// The Update phase construction is bounded by the same attempt deadline as
// the pre-check: with a newer manifest, the second NewUpdater (trust-root
// read while the lifecycle lease is held) must not hold the attempt past
// its bound, and Stop must reach its deadline-aware scheduler join instead
// of waiting for that lease first. The FIFO supplies one writer for the
// pre-check read; the Update read has no writer and would block forever
// without the bound. Drives production Start/tick/Stop.
func TestAutoUpdateSchedulerUpdateConstructionBoundedByAttemptDeadline(t *testing.T) {
	fifo := t.TempDir() + "/trustroot-update"
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
	d.autoUpdateRunBound = 5 * time.Second
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	d.mu.Lock()
	d.config.AutoUpdate.TrustRootPath = fifo
	d.config.AutoUpdate.DrainTimeoutSeconds = 5
	d.mu.Unlock()
	if d.autoUpdateScheduler == nil {
		t.Fatal("opted-in config started no scheduler through Start")
	}
	// One writer for the pre-check construction only: it blocks until the
	// first reader opens, writes the embedded root, then exits so the
	// Update construction has no writer and would block indefinitely.
	go func() {
		fd, err := syscall.Open(fifo, syscall.O_WRONLY, 0)
		if err != nil {
			return
		}
		data := append([]byte(nil), embeddedTrustRoot...)
		for len(data) > 0 {
			n, err := syscall.Write(fd, data)
			if err != nil {
				break
			}
			data = data[n:]
		}
		_ = syscall.Close(fd)
	}()
	waitForUpdateCondition(t, 5*time.Second, "scheduler tick wait", func() bool {
		return clock.waiterCount() > 0
	})
	clock.fire()
	// The tick entered Update: pre-check saw the newer release (one
	// manifest hit) and Update now holds the lease while blocked in the
	// second construction.
	waitForUpdateCondition(t, 5*time.Second, "update entered", func() bool {
		return d.State() == StateUpdating
	})
	// Stop while the Update construction is blocked must still terminate
	// within the caller's deadline: the early scheduler cancel (before
	// waiting for the lifecycle lease) releases the construction waiter,
	// which releases the lease. The attempt bound is drain 5s + run 5s
	// (10s); the Stop deadline below (5s) is shorter, so without the
	// early cancel Stop would wait for the full attempt and exceed its
	// deadline.
	start := time.Now()
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Stop(stopCtx); err != nil {
		t.Fatalf("Stop while Update construction blocked: %v (state=%q)", err, d.State())
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("Stop took %v while Update construction blocked, want within the caller deadline", elapsed)
	}
	if got := d.State(); got != StateStopped {
		t.Fatalf("state = %q, want stopped after bounded Stop", got)
	}
	// The pre-check used the single writer; the Update construction never
	// reached its manifest re-check.
	if got := probe.manifestHits.Load(); got != 1 {
		t.Fatalf("manifest checks = %d, want exactly the pre-check (Update blocked in construction)", got)
	}
	if got := probe.binaryHits.Load(); got != 0 {
		t.Fatalf("binary downloads = %d, want 0 (Update never reached downloads)", got)
	}
	// Unblock the leaked construction goroutine so it can finish.
	go func() {
		fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return
		}
		_ = syscall.Close(fd)
	}()
	time.Sleep(200 * time.Millisecond)
}

// A live reload that swaps the trust root to a FIFO between the pre-check
// and the Update construction must not block the in-flight attempt: the
// tick reuses the pre-checked snapshot for Update, so both constructions
// read the embedded root. Without the snapshot the second construction
// would block on the FIFO with no writer. Drives production attemptOnce
// with a real reload race injected on the first manifest fetch.
func TestAutoUpdateSchedulerSnapshotSurvivesTrustRootReload(t *testing.T) {
	fifo := t.TempDir() + "/trustroot-reload"
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	binary := []byte("new-binary")
	var d *Daemon
	var mutated atomic.Bool
	var manifestHits atomic.Int32
	var binaryHits atomic.Int32
	var sigHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/latest.json") {
			manifestHits.Add(1)
			if !mutated.Swap(true) && d != nil {
				// Simulate a live reload to a FIFO trust root after the
				// pre-check construction but before the Update
				// construction. The in-flight attempt must keep its
				// snapshot and never open the FIFO.
				d.mu.Lock()
				if d.config != nil {
					d.config.AutoUpdate.TrustRootPath = fifo
				}
				d.mu.Unlock()
			}
			_ = json.NewEncoder(w).Encode(VersionManifest{Version: "9.9.9", SHA256: hexSHA256(binary)})
			return
		}
		if strings.HasSuffix(r.URL.Path, ".sigstore") {
			sigHits.Add(1)
			_, _ = w.Write([]byte("not-a-bundle"))
			return
		}
		binaryHits.Add(1)
		_, _ = w.Write(binary)
	}))
	t.Cleanup(srv.Close)
	d = newRestartTestDaemon(t, nil)
	d.opts.Version = "0.1.0"
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, DrainTimeoutSeconds: 1,
		Source: srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	d.autoUpdateRunBound = time.Second
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s.ctx = ctx
	start := time.Now()
	s.attemptOnce()
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("snapshot attempt took %v, want within the drain+run bound", elapsed)
	}
	// Both constructions used the embedded snapshot: the Update re-check
	// ran and downloads followed. A live-config re-read would have blocked
	// the second construction on the FIFO with no writer (one manifest,
	// zero binaries, attempt timeout).
	if got := d.State(); got != StateDraining {
		t.Fatalf("state after snapshot attempt = %q, want draining (Update entered)", got)
	}
	if got := manifestHits.Load(); got < 2 {
		t.Fatalf("manifest checks = %d, want pre-check plus Update re-check (snapshot survived reload)", got)
	}
	if got := binaryHits.Load(); got != 1 {
		t.Fatalf("binary downloads = %d, want 1 (Update reached downloads despite reload to FIFO)", got)
	}
	if got := sigHits.Load(); got != 1 {
		t.Fatalf("bundle downloads = %d, want 1", got)
	}
}
