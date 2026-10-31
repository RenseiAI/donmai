package daemon

import (
	"context"
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
