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

// A FIFO trust-root path must not hold shutdown past the caller's
// deadline. loadUpdateTrustRoot refuses non-regular files without
// opening them, so construction never blocks on the FIFO: the attempt
// runs to fail-closed verification and Stop joins promptly. Without the
// stat guard the pre-check construction blocks inside os.ReadFile until
// a writer appears, leaking the worker even after the attempt deadline.
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
	// Stub the restart-preflight audit writer: the production writer
	// chmods the RegistryDir TempDir, which races TempDir cleanup under
	// -race when the scheduler fires concurrently. The other
	// Start-based scheduler tests avoid this by asserting only manifest
	// traffic; here the FIFO attempt must reach Update downloads, so
	// preflight must succeed deterministically.
	d.shims.restartStateWriter = func(restartPreparationAudit) error { return nil }
	d.shims.restartID = func() (string, error) { return "rp_test", nil }
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
	// The FIFO trust root is refused without opening it, so the attempt
	// completes (fail-closed verification, draining) instead of wedging
	// with inFlight true. Without the stat guard this wait times out:
	// construction blocks on the FIFO with no writer.
	waitForUpdateCondition(t, 10*time.Second, "FIFO attempt completed to draining", func() bool {
		return d.State() == StateDraining
	})
	if got := probe.manifestHits.Load(); got < 2 {
		t.Fatalf("manifest checks = %d, want pre-check plus Update re-check (FIFO refused without blocking)", got)
	}
	if got := probe.binaryHits.Load(); got != 1 {
		t.Fatalf("binary downloads = %d, want 1 (Update reached downloads despite FIFO trust root)", got)
	}
	start := time.Now()
	stopCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	sched := d.autoUpdateScheduler
	_ = d.Stop(stopCtx)
	if elapsed := time.Since(start); elapsed > 3500*time.Millisecond {
		t.Fatalf("Stop took %v with a 50ms deadline, want within the attempt bound", elapsed)
	}
	// The attempt already completed, so inFlight is clear and the loop is
	// joinable: Stop must terminate well within the attempt bound even
	// with the short caller deadline below.
	waitForUpdateCondition(t, 5*time.Second, "pre-check released inFlight", func() bool {
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
	// No writer is needed: construction never opened the FIFO, so no
	// leaked goroutine remains to unblock.
}

// The Update phase construction never blocks on a FIFO trust root:
// loadUpdateTrustRoot refuses non-regular files without opening them,
// so both the pre-check and the Update construction fail closed to
// verification and the attempt runs to draining with downloads. Without
// the guard the second NewUpdater blocks on the FIFO with no writer
// while holding the lifecycle lease, and Stop waits for the full attempt
// bound. Drives production Start/tick/Stop.
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
	// See the stub above: deterministic preflight so the FIFO attempt
	// reaches downloads.
	d.shims.restartStateWriter = func(restartPreparationAudit) error { return nil }
	d.shims.restartID = func() (string, error) { return "rp_test", nil }
	d.mu.Lock()
	d.config.AutoUpdate.TrustRootPath = fifo
	d.config.AutoUpdate.DrainTimeoutSeconds = 5
	d.mu.Unlock()
	if d.autoUpdateScheduler == nil {
		t.Fatal("opted-in config started no scheduler through Start")
	}
	waitForUpdateCondition(t, 5*time.Second, "scheduler tick wait", func() bool {
		return clock.waiterCount() > 0
	})
	clock.fire()
	// Both constructions refuse the FIFO without opening it: the tick
	// reaches Update (draining), performs the manifest re-check and the
	// downloads, then fails closed at verification. A blocking
	// construction would wedge here in StateUpdating with one manifest
	// hit and zero binaries.
	waitForUpdateCondition(t, 10*time.Second, "update reached draining despite FIFO", func() bool {
		return d.State() == StateDraining
	})
	// Stop after the completed attempt terminates promptly within the
	// caller's deadline.
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
	// Both constructions ran: pre-check plus the Update re-check, then
	// the binary download on the way to fail-closed verification. A
	// blocking Update construction would leave exactly one manifest hit
	// and zero binaries.
	if got := probe.manifestHits.Load(); got < 2 {
		t.Fatalf("manifest checks = %d, want pre-check plus Update re-check (FIFO refused without blocking)", got)
	}
	if got := probe.binaryHits.Load(); got != 1 {
		t.Fatalf("binary downloads = %d, want 1 (Update reached downloads despite FIFO trust root)", got)
	}
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
