package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Queued watcher reloads must not recreate the scheduled loop after the
// daemon stopped. Production Start owns the loop; production Stop removes
// it and publishes the terminal state. A reload callback that waited behind
// the reload serialization while Stop completed must observe the stop
// generation and stay inert instead of installing a background-context
// loop the stopped daemon would never join.
func TestAutoUpdateSchedulerQueuedReloadStaysStopped(t *testing.T) {
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
	d.autoUpdateRunBound = time.Second
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if d.autoUpdateScheduler == nil {
		t.Fatal("opted-in config started no scheduler through Start")
	}
	// Hold reload serialization so the callback queues behind it.
	d.yamlReloadMu.Lock()
	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		cfg := d.Config()
		if cfg == nil {
			return
		}
		// A different but still opted-in schedule takes the start path.
		if cfg.AutoUpdate.Schedule == ScheduleOnRelease {
			cfg.AutoUpdate.Schedule = ScheduleNightly
		} else {
			cfg.AutoUpdate.Schedule = ScheduleOnRelease
		}
		d.onYamlChanged(cfg)
	}()
	time.Sleep(100 * time.Millisecond)
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := d.State(); got != StateStopped {
		t.Fatalf("state = %q, want stopped", got)
	}
	d.yamlReloadMu.Unlock()
	<-reloadDone
	if d.autoUpdateScheduler != nil {
		t.Fatalf("scheduler resurrected after Stop: state=%q", d.State())
	}
	// A stale tick body left over from before Stop stays inert: no
	// network after stopping began.
	stale := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	tickCtx, tickCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer tickCancel()
	stale.ctx = tickCtx
	base := probe.manifestHits.Load()
	stale.attemptOnce()
	if got := probe.manifestHits.Load(); got != base {
		t.Fatalf("stale tick manifest requests = %d, want %d (no calls after Stop)", got, base)
	}
}

// A successful scheduled swap fences the drain-exit: admission must not
// reopen after the snapshot that authorized the restart. The swap flag is
// latched while the update lease is still held, so a Resume that acquires
// the lease afterwards observes it with no window.
func TestAutoUpdateSchedulerAppliedSwapFencesResume(t *testing.T) {
	d := newRestartTestDaemon(t, nil)
	d.opts.Version = "0.1.0"
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, DrainTimeoutSeconds: 1,
		Source: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	var restarts int
	d.autoUpdateRestartHook = func() { restarts++ }
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		return &UpdateResult{Updated: true, Version: "9.9.9", Reason: "update-applied"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	s.ctx = ctx
	s.attemptOnce()
	if restarts != 1 {
		t.Fatalf("restart requests = %d, want 1", restarts)
	}
	if err := d.ResumeContext(context.Background()); err == nil {
		t.Fatalf("resume accepted at scheduled restart handoff: state=%q accepting=%v", d.State(), d.spawner.IsAccepting())
	}
	if got := d.State(); got != StateDraining {
		t.Fatalf("state = %q, want draining", got)
	}
}

// The production updater path latches the same post-swap fence as the
// prepared-update stub: a real signed-binary swap through NewUpdater +
// RunUpdate (no runPreparedUpdate) must set updateSwapApplied while the
// update lease is held, so Resume refuses and the restart handoff owns
// completion. Removing the shared latch leaves this test green only when
// the stub path is used; this test drives the production branch with a
// hermetic pass-through verifier and a temp binary so the swap never
// touches the test binary. It pins AC-2/AC-4 through the production
// entry point (scheduler tick → updateWithConfig → RunUpdate).
func TestAutoUpdateSchedulerProductionSwapFencesResume(t *testing.T) {
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "test-bundle")
	d := newRestartTestDaemon(t, nil)
	d.opts.Version = "0.1.0"
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, DrainTimeoutSeconds: 1,
		Source: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	// Hermetic production swap: pass-through verification plus a temp
	// binary path, so RunUpdate applies the probe binary without a real
	// sigstore bundle and without touching the test executable.
	current := filepath.Join(t.TempDir(), "donmai-daemon")
	if err := os.WriteFile(current, []byte("old-binary"), 0o600); err != nil {
		t.Fatalf("write current binary: %v", err)
	}
	d.autoUpdateVerifier = passVerifier{}
	d.autoUpdateBinaryPath = current
	d.autoUpdateRunBound = 5 * time.Second
	var restarts atomic.Int32
	d.autoUpdateRestartHook = func() { restarts.Add(1) }
	if d.runPreparedUpdate != nil {
		t.Fatal("runPreparedUpdate must stay nil to exercise the production updater branch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	s.ctx = ctx
	s.attemptOnce()
	if got := restarts.Load(); got != 1 {
		t.Fatalf("production restart requests = %d, want 1", got)
	}
	if !d.updateSwapApplied.Load() {
		t.Fatal("production swap did not latch updateSwapApplied")
	}
	swapped, err := os.ReadFile(current) //nolint:gosec
	if err != nil {
		t.Fatalf("read swapped binary: %v", err)
	}
	if string(swapped) != "new-binary" {
		t.Fatalf("swapped binary = %q, want probe binary (production swap never applied)", swapped)
	}
	if got := probe.manifestHits.Load(); got < 2 {
		t.Fatalf("manifest checks = %d, want pre-check plus Update re-check", got)
	}
	if err := d.ResumeContext(context.Background()); err == nil {
		t.Fatalf("resume accepted after production swap: state=%q accepting=%v", d.State(), d.spawner.IsAccepting())
	}
	if got := d.State(); got != StateDraining {
		t.Fatalf("state = %q, want draining", got)
	}
	// A second tick must not re-enter Update for the same release.
	baseManifest := probe.manifestHits.Load()
	s.attemptOnce()
	if got := restarts.Load(); got != 1 {
		t.Fatalf("production restart requests after second tick = %d, want still 1", got)
	}
	if got := probe.manifestHits.Load(); got != baseManifest {
		t.Fatalf("manifest checks after second tick = %d, want %d (applied flag stops re-apply)", got, baseManifest)
	}
}

// A successful manual HTTP update leaves Resume available: only the
// scheduled loop owns a restart handoff (exit code 3), so a fenced
// manual swap would strand the old process draining with no completion
// owner (no scheduler to request exit 3, explicit resume refused).
// Drives both real HTTP routes with the production updater branch
// (pass-through verifier plus temp binary): without the scheduled-only
// fence this test goes RED with resume 409 and updateSwapApplied set.
func TestManualHTTPUpdateSwapLeavesResumeAvailable(t *testing.T) {
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "test-bundle")
	d := newRestartTestDaemon(t, nil)
	d.opts.Version = "0.1.0"
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleManual, DrainTimeoutSeconds: 1,
		Source: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	current := filepath.Join(t.TempDir(), "donmai-daemon")
	if err := os.WriteFile(current, []byte("old-binary"), 0o600); err != nil {
		t.Fatalf("write current binary: %v", err)
	}
	d.autoUpdateVerifier = passVerifier{}
	d.autoUpdateBinaryPath = current
	if d.runPreparedUpdate != nil {
		t.Fatal("runPreparedUpdate must stay nil to exercise the production updater branch")
	}
	server := httptest.NewServer(NewServer(d).httpd.Handler)
	t.Cleanup(server.Close)
	res, err := http.Post(server.URL+"/api/daemon/update", "application/json", nil) //nolint:gosec
	if err != nil {
		t.Fatalf("POST update: %v", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST update = %d, want 200", res.StatusCode)
	}
	// The handler runs the production swap asynchronously; poll for the
	// on-disk replacement plus the Update re-check.
	waitForUpdateCondition(t, 10*time.Second, "manual production swap applied", func() bool {
		data, err := os.ReadFile(current) //nolint:gosec
		return err == nil && string(data) == "new-binary"
	})
	waitForUpdateCondition(t, 10*time.Second, "manual update reached downloads", func() bool {
		return probe.manifestHits.Load() >= 1 && probe.binaryHits.Load() == 1
	})
	waitForUpdateCondition(t, 10*time.Second, "manual update settled to draining", func() bool {
		return d.State() == StateDraining
	})
	if d.updateSwapApplied.Load() {
		t.Fatal("manual swap latched updateSwapApplied: scheduled-only fence leaked to the manual path")
	}
	if got := d.State(); got != StateDraining {
		t.Fatalf("state after manual swap = %q, want draining", got)
	}
	resume, err := http.Post(server.URL+"/api/daemon/resume", "application/json", nil) //nolint:gosec
	if err != nil {
		t.Fatalf("POST resume: %v", err)
	}
	_ = resume.Body.Close()
	if resume.StatusCode != http.StatusOK {
		t.Fatalf("POST resume after manual swap = %d, want 200 (manual completion owner is explicit resume)", resume.StatusCode)
	}
	if got := d.State(); got != StateRunning {
		t.Fatalf("state after manual resume = %q, want running", got)
	}
}

// Changing between two opted-in schedules restarts the loop so the
// outstanding timer is rearmed: keeping the old loop would retain its
// already-computed wait (nightly keeping a 23-hour wait after a reload
// to hourly; hourly still firing the old daytime tick after a reload to
// nightly). Drives the production yaml-watcher callback; without the
// stop-then-start reconcile the scheduler pointer is unchanged.
func TestAutoUpdateSchedulerReloadBetweenEnabledSchedulesRearmsTimer(t *testing.T) {
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
	path := writeAutoUpdateDaemonConfig(t, string(ScheduleNightly), probe.srv.URL, true)
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
	d.autoUpdateRunBound = time.Second
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = d.Stop(ctx)
	})
	if d.autoUpdateScheduler == nil {
		t.Fatal("opted-in config started no scheduler through Start")
	}
	waitForUpdateCondition(t, 5*time.Second, "scheduler tick wait", func() bool {
		return clock.waiterCount() > 0
	})
	first := d.autoUpdateScheduler
	// Nightly -> on-release: still opted in, but the cadence changed, so
	// the loop must restart (new instance, new timer).
	reloaded := *d.Config()
	reloaded.AutoUpdate.Schedule = ScheduleOnRelease
	d.onYamlChanged(&reloaded)
	if d.autoUpdateScheduler == nil {
		t.Fatal("scheduler missing after nightly -> on-release reload")
	}
	if d.autoUpdateScheduler == first {
		t.Fatal("scheduler instance unchanged after nightly -> on-release reload: old 23-hour timer retained")
	}
	// On-release -> nightly: the daytime hourly timer must not survive;
	// the loop restarts again for the 03:00 cadence.
	second := d.autoUpdateScheduler
	reloaded2 := *d.Config()
	reloaded2.AutoUpdate.Schedule = ScheduleNightly
	d.onYamlChanged(&reloaded2)
	if d.autoUpdateScheduler == nil {
		t.Fatal("scheduler missing after on-release -> nightly reload")
	}
	if d.autoUpdateScheduler == second {
		t.Fatal("scheduler instance unchanged after on-release -> nightly reload: old daytime timer retained")
	}
}
