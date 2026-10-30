package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// manualUpdateClock is a controllable updateClock: After returns a channel
// the test fires on demand, so scheduled ticks run deterministically
// through production Start/Stop.
type manualUpdateClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []chan time.Time
}

func newManualUpdateClock(now time.Time) *manualUpdateClock {
	return &manualUpdateClock{now: now}
}

func (m *manualUpdateClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

func (m *manualUpdateClock) After(_ time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	m.mu.Lock()
	m.waiters = append(m.waiters, ch)
	m.mu.Unlock()
	return ch
}

func (m *manualUpdateClock) waiterCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.waiters)
}

// fire delivers the current time to every registered waiter exactly once.
func (m *manualUpdateClock) fire() {
	m.mu.Lock()
	waiters := m.waiters
	m.waiters = nil
	now := m.now
	m.mu.Unlock()
	for _, w := range waiters {
		w <- now
	}
}

func waitForUpdateCondition(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", msg)
}

// updateProbeServer serves a fixed manifest/binary/bundle triple while
// counting hits per artifact kind, so tests observe exactly which update
// stages a tick or Update reached.
type updateProbeServer struct {
	srv          *httptest.Server
	manifestHits atomic.Int32
	binaryHits   atomic.Int32
	sigHits      atomic.Int32
	otherHits    atomic.Int32
}

func newUpdateProbeServer(t *testing.T, version string, binary []byte, bundle string) *updateProbeServer {
	t.Helper()
	p := &updateProbeServer{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/latest.json"):
			p.manifestHits.Add(1)
			_ = json.NewEncoder(w).Encode(VersionManifest{Version: version, SHA256: hexSHA256(binary)})
		case strings.HasSuffix(r.URL.Path, ".sigstore"):
			p.sigHits.Add(1)
			_, _ = w.Write([]byte(bundle))
		case strings.HasSuffix(r.URL.Path, "latest.json"):
			p.otherHits.Add(1)
			http.Error(w, "unexpected manifest path", http.StatusNotFound)
		default:
			p.binaryHits.Add(1)
			_, _ = w.Write(binary)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func writeAutoUpdateDaemonConfig(t *testing.T, schedule, source string, signers bool) string {
	t.Helper()
	dir := t.TempDir()
	signerBlock := ""
	if signers {
		signerBlock = `  signers:
    - san: releases@example.com
      issuer: https://issuer.example
`
	}
	body := fmt.Sprintf(`apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: scheduler-fixture
capacity:
  maxConcurrentSessions: 1
  maxVCpuPerSession: 1
  maxMemoryMbPerSession: 1024
  reservedForSystem:
    vCpu: 1
    memoryMb: 1024
orchestrator:
  url: https://platform.example.com
autoUpdate:
  channel: stable
  schedule: %s
  drainTimeoutSeconds: 1
  source: %s
%s`, schedule, source, signerBlock)
	path := filepath.Join(dir, "daemon.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAutoUpdateSource_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	body := []byte(`apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: test-machine
capacity:
  maxConcurrentSessions: 1
  maxVCpuPerSession: 1
  maxMemoryMbPerSession: 1024
  reservedForSystem:
    vCpu: 1
    memoryMb: 1024
orchestrator:
  url: https://platform.example.com
autoUpdate:
  channel: stable
  schedule: nightly
  drainTimeoutSeconds: 600
  source: https://updates.example.com/cdn
  signers:
    - san: releases@example.com
      issuer: https://issuer.example
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.AutoUpdate.Source; got != "https://updates.example.com/cdn" {
		t.Fatalf("Source = %q, want the configured origin", got)
	}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.AutoUpdate.Source != "https://updates.example.com/cdn" {
		t.Fatalf("round-trip Source = %q", reloaded.AutoUpdate.Source)
	}
}

func TestAutoUpdateSource_Validation(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		wantErr bool
	}{
		{"empty stays inert", "", false},
		{"https ok", "https://updates.example.com", false},
		{"http ok", "http://127.0.0.1:9", false},
		{"ftp rejected", "ftp://updates.example.com", true},
		{"missing scheme rejected", "updates.example.com", true},
		{"missing host rejected", "https://", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{
				Machine:      MachineConfig{ID: "m"},
				Orchestrator: OrchestratorConfig{URL: "https://platform.example.com"},
				AutoUpdate:   AutoUpdateConfig{Channel: ChannelStable, Schedule: ScheduleNightly, Source: c.source},
			}
			err := validateConfig(cfg)
			if c.wantErr && err == nil {
				t.Fatalf("validateConfig(%q) = nil, want rejection", c.source)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("validateConfig(%q) = %v, want acceptance", c.source, err)
			}
		})
	}
}

func TestAutoUpdateSchedulerWanted(t *testing.T) {
	withSource := "https://updates.example.com"
	signed := []UpdateSigner{{SAN: "releases@example.com", Issuer: "https://issuer.example"}}
	cases := []struct {
		name     string
		schedule UpdateSchedule
		source   string
		signers  []UpdateSigner
		want     bool
	}{
		{"nightly opted-in", ScheduleNightly, withSource, signed, true},
		{"on-release opted-in", ScheduleOnRelease, withSource, signed, true},
		{"manual never schedules", ScheduleManual, withSource, signed, false},
		{"empty schedule never schedules", "", withSource, signed, false},
		{"missing source stays inert", ScheduleNightly, "", signed, false},
		{"absent consent stays inert", ScheduleNightly, withSource, nil, false},
		{"unconfigured stays inert", "", "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{AutoUpdate: AutoUpdateConfig{
				Channel:  ChannelStable,
				Schedule: c.schedule,
				Source:   c.source,
				Signers:  c.signers,
			}}
			if got := autoUpdateSchedulerWanted(cfg); got != c.want {
				t.Fatalf("wanted = %v, want %v", got, c.want)
			}
		})
	}
	if autoUpdateSchedulerWanted(nil) {
		t.Fatal("wanted(nil) = true, want false")
	}
}

func TestNextAutoUpdateDelay(t *testing.T) {
	loc := time.Local
	morning := time.Date(2026, 5, 4, 1, 0, 0, 0, loc)
	if got := nextAutoUpdateDelay(morning, ScheduleNightly); got != 2*time.Hour {
		t.Fatalf("nightly at 01:00 waits %v, want 2h to 03:00", got)
	}
	after := time.Date(2026, 5, 4, 4, 30, 0, 0, loc)
	want := 22*time.Hour + 30*time.Minute
	if got := nextAutoUpdateDelay(after, ScheduleNightly); got != want {
		t.Fatalf("nightly at 04:30 waits %v, want %v", got, want)
	}
	if got := nextAutoUpdateDelay(morning, ScheduleOnRelease); got != time.Hour {
		t.Fatalf("on-release waits %v, want 1h", got)
	}
	// Manual/empty fall back to the nightly cadence shape (positive and
	// bounded by 24h); the wanted predicate — not the delay — keeps them
	// inert, since the loop only exists for opted-in configs.
	if got := nextAutoUpdateDelay(morning, ScheduleManual); got <= 0 || got > 24*time.Hour {
		t.Fatalf("manual delay = %v, want a positive sub-24h delay", got)
	}
}

func startOptedInSchedulerDaemon(t *testing.T, schedule string, probe *updateProbeServer) (*Daemon, *manualUpdateClock) {
	t.Helper()
	source := ""
	if probe != nil {
		source = probe.srv.URL
	}
	path := writeAutoUpdateDaemonConfig(t, schedule, source, true)
	clock := newManualUpdateClock(time.Date(2026, 5, 4, 1, 0, 0, 0, time.Local))
	d := New(Options{
		ConfigPath:       path,
		JWTPath:          filepath.Join(t.TempDir(), "daemon.jwt"),
		SkipWizard:       true,
		SkipRegistration: true,
		Version:          "0.1.0",
		SpawnerOptions:   SpawnerOptions{WorkerCommand: []string{"/bin/false"}},
		SessionShim:      SessionShimConfig{RegistryDir: t.TempDir()},
	})
	d.autoUpdateClock = clock
	d.autoUpdateRunBound = 2 * time.Second
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := d.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return d, clock
}

func TestAutoUpdateScheduler_StartStop_OptedInRunsAtApprovedTick(t *testing.T) {
	for _, schedule := range []string{string(ScheduleNightly), string(ScheduleOnRelease)} {
		t.Run(schedule, func(t *testing.T) {
			probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
			d, clock := startOptedInSchedulerDaemon(t, schedule, probe)
			if d.autoUpdateScheduler == nil {
				t.Fatal("opted-in config started no scheduler through Start")
			}
			waitForUpdateCondition(t, 5*time.Second, "scheduler tick wait", func() bool {
				return clock.waiterCount() > 0
			})
			clock.fire()
			// One tick: manifest pre-check plus Update's own re-check, then
			// the binary and bundle downloads on the way to fail-closed
			// verification.
			waitForUpdateCondition(t, 10*time.Second, "manifest re-check", func() bool {
				return probe.manifestHits.Load() >= 2
			})
			waitForUpdateCondition(t, 10*time.Second, "binary download", func() bool {
				return probe.binaryHits.Load() == 1
			})
			if got := probe.sigHits.Load(); got != 1 {
				t.Fatalf("bundle downloads = %d, want 1", got)
			}
			// The failed attempt leaves the daemon draining by contract:
			// failures never silently resume.
			if state := d.State(); state != StateDraining {
				t.Fatalf("state after failed scheduled update = %q, want draining", state)
			}
		})
	}
}

func TestAutoUpdateScheduler_StartStop_InertWithoutOptIn(t *testing.T) {
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
	cases := []struct {
		name     string
		schedule string
		source   bool
		signers  bool
	}{
		{"manual makes no calls", string(ScheduleManual), true, true},
		{"missing source makes no calls", string(ScheduleNightly), false, true},
		{"absent consent makes no calls", string(ScheduleNightly), true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := ""
			if c.source {
				source = probe.srv.URL
			}
			path := writeAutoUpdateDaemonConfig(t, c.schedule, source, c.signers)
			clock := newManualUpdateClock(time.Date(2026, 5, 4, 1, 0, 0, 0, time.Local))
			d := New(Options{
				ConfigPath:       path,
				JWTPath:          filepath.Join(t.TempDir(), "daemon.jwt"),
				SkipWizard:       true,
				SkipRegistration: true,
				Version:          "0.1.0",
				SpawnerOptions:   SpawnerOptions{WorkerCommand: []string{"/bin/false"}},
				SessionShim:      SessionShimConfig{RegistryDir: t.TempDir()},
			})
			d.autoUpdateClock = clock
			d.autoUpdateRunBound = 2 * time.Second
			if err := d.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := d.Stop(ctx); err != nil {
					t.Errorf("Stop: %v", err)
				}
			})
			if d.autoUpdateScheduler != nil {
				t.Fatalf("%s: scheduler started, want inert", c.name)
			}
			before := probe.manifestHits.Load() + probe.binaryHits.Load() + probe.sigHits.Load()
			time.Sleep(200 * time.Millisecond)
			if after := probe.manifestHits.Load() + probe.binaryHits.Load() + probe.sigHits.Load(); after != before {
				t.Fatalf("%s: probe hits moved %d -> %d, want zero scheduled calls", c.name, before, after)
			}
		})
	}
}

func TestAutoUpdateScheduler_OverlappingTicksInvokeOneAttempt(t *testing.T) {
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
	d := newRestartTestDaemon(t, nil)
	d.opts.Version = "0.1.0"
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, DrainTimeoutSeconds: 1,
		Source: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	entered := make(chan struct{})
	release := make(chan struct{})
	var entries atomic.Int32
	d.runPreparedUpdate = func(context.Context, AutoUpdateConfig, string) (*UpdateResult, error) {
		entries.Add(1)
		close(entered)
		<-release
		return &UpdateResult{Updated: false, Version: "0.1.0", Reason: "test-hold"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &autoUpdateScheduler{
		daemon: d,
		clock:  systemUpdateClock{},
		ctx:    ctx,
		cancel: cancel,
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.attemptOnce() }()
	<-entered
	// The overlapping tick must return while the first attempt is held.
	secondDone := make(chan struct{})
	go func() { defer wg.Done(); s.attemptOnce(); close(secondDone) }()
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("overlapping tick did not return while the first attempt was held")
	}
	if got := entries.Load(); got != 1 {
		t.Fatalf("update entries = %d, want exactly 1 for overlapping ticks", got)
	}
	close(release)
	wg.Wait()
}

func TestAutoUpdateScheduler_StopBoundsNeverSettlingFetch(t *testing.T) {
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/latest.json") {
			<-r.Context().Done()
			return
		}
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	t.Cleanup(hanging.Close)
	d := newRestartTestDaemon(t, nil)
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleOnRelease, DrainTimeoutSeconds: 1,
		Source: hanging.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	clock := newManualUpdateClock(time.Now())
	d.autoUpdateClock = clock
	d.autoUpdateRunBound = time.Second
	d.startAutoUpdateScheduler()
	if d.autoUpdateScheduler == nil {
		t.Fatal("opted-in config started no scheduler")
	}
	waitForUpdateCondition(t, 5*time.Second, "scheduler tick wait", func() bool {
		return clock.waiterCount() > 0
	})
	clock.fire()
	waitForUpdateCondition(t, 5*time.Second, "attempt in flight", func() bool {
		return d.autoUpdateScheduler.inFlight.Load()
	})
	start := time.Now()
	d.stopAutoUpdateScheduler()
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("Stop took %v with a never-settling fetch, want within the attempt bound", elapsed)
	}
	if d.autoUpdateScheduler != nil {
		t.Fatal("scheduler ownership not released by Stop")
	}
}

func TestAutoUpdateScheduler_UpToDateReleaseNeverDrains(t *testing.T) {
	probe := newUpdateProbeServer(t, "0.1.0", []byte("current-binary"), "not-a-bundle")
	d := newRestartTestDaemon(t, nil)
	d.opts.Version = "0.1.0"
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, DrainTimeoutSeconds: 1,
		Source: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.ctx = ctx
	s.attemptOnce()
	if state := d.State(); state != StateRunning {
		t.Fatalf("state after up-to-date tick = %q, want running (never drained)", state)
	}
	if got := probe.binaryHits.Load(); got != 0 {
		t.Fatalf("binary downloads = %d, want 0 for an unchanged release", got)
	}
	if got := probe.manifestHits.Load(); got != 1 {
		t.Fatalf("manifest checks = %d, want exactly the pre-check", got)
	}
}

func TestAutoUpdateScheduler_NewerReleaseReachesRealUpdateFailsClosed(t *testing.T) {
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
	d := newRestartTestDaemon(t, nil)
	d.opts.Version = "0.1.0"
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, DrainTimeoutSeconds: 1,
		Source: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s.ctx = ctx
	s.attemptOnce()
	// The tick entered the real Update path: manifest re-check plus binary
	// and bundle downloads, then fail-closed verification with no swap.
	if got := probe.manifestHits.Load(); got < 2 {
		t.Fatalf("manifest checks = %d, want pre-check plus Update re-check", got)
	}
	if got := probe.binaryHits.Load(); got != 1 {
		t.Fatalf("binary downloads = %d, want 1 (Update entered)", got)
	}
	if got := probe.sigHits.Load(); got != 1 {
		t.Fatalf("bundle downloads = %d, want 1", got)
	}
	// A reached-but-unverifiable update leaves the daemon draining: the
	// failure never silently resumes.
	if state := d.State(); state != StateDraining {
		t.Fatalf("state after failed scheduled update = %q, want draining", state)
	}
}

func TestAutoUpdateScheduler_PreflightRefusalBlocksBinaryFetch(t *testing.T) {
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
	d := newRestartTestDaemon(t, nil)
	d.opts.Version = "0.1.0"
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleNightly, DrainTimeoutSeconds: 1,
		Source: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	d.spawner.mu.Lock()
	d.spawner.sessions["direct"] = &spawnedSession{
		handle: SessionHandle{SessionID: "direct"}, spec: SessionSpec{SessionID: "direct"},
	}
	d.spawner.mu.Unlock()
	s := &autoUpdateScheduler{daemon: d, clock: systemUpdateClock{}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s.ctx = ctx
	s.attemptOnce()
	// The pre-check still ran (one manifest fetch), but preflight refused
	// before any binary download.
	if got := probe.manifestHits.Load(); got != 1 {
		t.Fatalf("manifest checks = %d, want exactly the pre-check", got)
	}
	if got := probe.binaryHits.Load(); got != 0 {
		t.Fatalf("binary downloads = %d, want 0 (preflight refused)", got)
	}
}

func TestUpdate_ForwardsConfiguredSourceToProductionUpdater(t *testing.T) {
	probe := newUpdateProbeServer(t, "9.9.9", []byte("new-binary"), "not-a-bundle")
	d := newRestartTestDaemon(t, nil)
	d.opts.Version = "0.1.0"
	d.config = &Config{AutoUpdate: AutoUpdateConfig{
		Channel: ChannelStable, Schedule: ScheduleManual, DrainTimeoutSeconds: 1,
		Source: probe.srv.URL,
		Signers: []UpdateSigner{
			{SAN: "releases@example.com", Issuer: "https://issuer.example"},
		},
	}}
	if _, err := d.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// Without source forwarding the updater short-circuits to up-to-date
	// with zero requests; any manifest hit proves the configured origin
	// reached the production NewUpdater call.
	if got := probe.manifestHits.Load(); got == 0 {
		t.Fatal("manual update made zero manifest requests: configured source never reached the updater")
	}
	if got := probe.binaryHits.Load(); got != 1 {
		t.Fatalf("binary downloads = %d, want 1 (full Update ran)", got)
	}
}
