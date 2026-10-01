package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type invalidReloadSignalWriter struct {
	once   sync.Once
	failed chan struct{}
}

func (w *invalidReloadSignalWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("[yaml-watcher] reload failed")) {
		w.once.Do(func() { close(w.failed) })
	}
	return len(p), nil
}

func TestStartYamlWatcher_InvalidReloadKeepsLastKnownGoodState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	initial := &Config{
		APIVersion:   "v1",
		Kind:         "DaemonConfig",
		Machine:      MachineConfig{ID: "m1"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 1},
		Orchestrator: OrchestratorConfig{URL: "https://example.test", AuthToken: "stub"},
		Projects:     []ProjectConfig{{ID: "alpha", Repository: "github.com/x/alpha"}},
	}
	if err := WriteConfig(path, initial); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}
	d := &Daemon{config: initial}
	signal := &invalidReloadSignalWriter{failed: make(chan struct{})}
	priorLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(signal, nil)))
	defer slog.SetDefault(priorLogger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	applied := make(chan struct{}, 1)
	stop, err := startYamlWatcher(ctx, path, func(cfg *Config) {
		d.onYamlChanged(cfg)
		applied <- struct{}{}
	})
	if err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	defer stop()
	if err := os.WriteFile(path, []byte("projects: [\n"), 0o600); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}
	select {
	case <-signal.failed:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not report the invalid reload")
	}
	select {
	case <-applied:
		t.Fatal("invalid YAML reached the reload callback")
	default:
	}
	if len(d.config.Projects) != 1 || d.config.Projects[0].ID != "alpha" {
		t.Fatalf("last-good project config changed after invalid YAML: %+v", d.config.Projects)
	}
}

func TestOnYamlChanged_ModeOnlyReload(t *testing.T) {
	initial := &Config{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		ProjectAdmissionMode:    ProjectAdmissionModeEnumerated,
		EnabledProjectIDs:       []string{"alpha"},
	}
	d := &Daemon{
		config: initial,
		spawner: NewWorkerSpawner(SpawnerOptions{
			EnabledProjectIDs:    []string{"alpha"},
			ProjectAdmissionMode: ProjectAdmissionModeEnumerated,
		}),
	}
	d.onYamlChanged(&Config{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		ProjectAdmissionMode:    ProjectAdmissionModeAllRouted,
		EnabledProjectIDs:       []string{"alpha"},
	})
	if got := d.config.EffectiveProjectAdmissionMode(); got != ProjectAdmissionModeAllRouted {
		t.Errorf("config mode = %q, want all-routed", got)
	}
	if got := d.spawner.ProjectAdmissionMode(); got != ProjectAdmissionModeAllRouted {
		t.Errorf("spawner mode = %q, want all-routed", got)
	}
}

func TestOnYamlChanged_LaterFullRegistrationUsesReloadedProjects(t *testing.T) {
	requests := make(chan RegisterRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != RegisterEndpoint {
			http.NotFound(w, r)
			return
		}
		var request RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode registration: %v", err)
			return
		}
		requests <- request
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_reloaded", "runtimeToken": "reloaded.jwt"})
	}))
	defer srv.Close()

	old := &Config{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		EnabledProjectIDs:       []string{"alpha"},
		Projects:                []ProjectConfig{{ID: "alpha", Repository: "github.com/x/alpha"}},
	}
	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	opts.Registration.ProjectAdmissionVersion = ProjectAdmissionVersionV2
	opts.Registration.ProjectIDs = []string{"alpha"}
	opts.Registration.DaemonProjects = AllowlistEntriesFromConfig(old.EffectiveProjectConfigs())
	credentials := NewCredentialRefresher(opts)
	d := &Daemon{config: old, credentials: credentials}
	d.onYamlChanged(&Config{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		ProjectAdmissionMode:    ProjectAdmissionModeAllRouted,
		EnabledProjectIDs:       []string{"alpha", "beta"},
		Projects:                []ProjectConfig{{ID: "alpha", Repository: "github.com/x/alpha"}},
		Repositories:            []RepositoryConfig{{ID: "repo-beta", ProjectID: "beta", Source: "github.com/x/beta"}},
	})
	if _, err := credentials.Refresh(context.Background(), "worker-not-found"); err != nil {
		t.Fatalf("later full registration: %v", err)
	}
	request := <-requests
	if !slices.Equal(request.ProjectIDs, []string{"alpha", "beta"}) {
		t.Errorf("registered project IDs = %v, want alpha,beta", request.ProjectIDs)
	}
	if len(request.DaemonProjects) != 2 ||
		request.DaemonProjects[0].ID != "alpha" || request.DaemonProjects[1].ID != "beta" {
		t.Errorf("registered project entries = %+v, want merged alpha,beta", request.DaemonProjects)
	}
	if request.ProjectAdmissionMode != ProjectAdmissionModeAllRouted {
		t.Errorf("registered admission mode = %q, want all-routed", request.ProjectAdmissionMode)
	}
}

func TestOnYamlChanged_ModeOnlyEditRegistersWithoutRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	requests := make(chan RegisterRequest, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != RegisterEndpoint {
			http.NotFound(w, r)
			return
		}
		var request RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode registration: %v", err)
			return
		}
		requests <- request
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_mode", "runtimeToken": "mode.jwt"})
	}))
	defer srv.Close()
	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	opts.Registration.ProjectAdmissionVersion = ProjectAdmissionVersionV2
	opts.Registration.ProjectIDs = []string{"alpha"}
	opts.Registration.ProjectAdmissionMode = ProjectAdmissionModeEnumerated
	credentials := NewCredentialRefresher(opts)
	d := &Daemon{
		config: &Config{
			ProjectAdmissionVersion: ProjectAdmissionVersionV2,
			ProjectAdmissionMode:    ProjectAdmissionModeEnumerated,
			EnabledProjectIDs:       []string{"alpha"},
		},
		credentials:           credentials,
		registrationReloadCtx: ctx,
	}
	d.onYamlChanged(&Config{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		ProjectAdmissionMode:    ProjectAdmissionModeAllRouted,
		EnabledProjectIDs:       []string{"alpha"},
	})
	if err := credentials.WaitReregister(ctx); err != nil {
		t.Fatalf("mode-only registration: %v", err)
	}
	select {
	case request := <-requests:
		if request.ProjectAdmissionMode != ProjectAdmissionModeAllRouted {
			t.Errorf("registered mode = %q, want all-routed", request.ProjectAdmissionMode)
		}
	case <-ctx.Done():
		t.Fatal("mode-only edit made no registration request")
	}
}

func TestStartYamlWatcher_FiresOnWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	initial := &Config{
		APIVersion:   "v1",
		Kind:         "DaemonConfig",
		Machine:      MachineConfig{ID: "m1"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 1},
		Orchestrator: OrchestratorConfig{URL: "https://example.test", AuthToken: "stub"},
		Projects: []ProjectConfig{
			{ID: "alpha", Repository: "github.com/x/alpha"},
		},
	}
	if err := WriteConfig(path, initial); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu      sync.Mutex
		fires   int
		lastCfg *Config
	)
	stop, err := startYamlWatcher(ctx, path, func(cfg *Config) {
		mu.Lock()
		defer mu.Unlock()
		fires++
		lastCfg = cfg
	})
	if err != nil {
		t.Fatalf("startYamlWatcher: %v", err)
	}
	defer stop()

	// Give fsnotify a beat to subscribe before we mutate.
	time.Sleep(50 * time.Millisecond)

	updated := &Config{
		APIVersion:   "v1",
		Kind:         "DaemonConfig",
		Machine:      MachineConfig{ID: "m1"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 1},
		Orchestrator: OrchestratorConfig{URL: "https://example.test", AuthToken: "stub"},
		Projects: []ProjectConfig{
			{ID: "alpha", Repository: "github.com/x/alpha"},
			{ID: "beta", Repository: "github.com/x/beta"},
		},
	}
	if err := WriteConfig(path, updated); err != nil {
		t.Fatalf("update yaml: %v", err)
	}

	// Coalesce window is 250ms; allow comfortable slack for CI jitter.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := fires
		mu.Unlock()
		if got >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if fires == 0 {
		t.Fatal("expected at least one fire after write")
	}
	if lastCfg == nil || len(lastCfg.Projects) != 2 {
		t.Fatalf("lastCfg projects = %+v, want 2 entries", lastCfg)
	}
	if lastCfg.Projects[1].ID != "beta" {
		t.Errorf("lastCfg.Projects[1].ID = %q, want beta", lastCfg.Projects[1].ID)
	}
}

func TestStartYamlWatcher_EmptyPathRejected(t *testing.T) {
	t.Parallel()
	_, err := startYamlWatcher(context.Background(), "", func(*Config) {})
	if err == nil {
		t.Fatal("want error for empty path")
	}
}

func TestStartYamlWatcher_IgnoresUnrelatedFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	if err := WriteConfig(path, &Config{
		APIVersion:   "v1",
		Kind:         "DaemonConfig",
		Machine:      MachineConfig{ID: "m1"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 1},
		Orchestrator: OrchestratorConfig{URL: "https://example.test", AuthToken: "stub"},
	}); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var fires int
	var mu sync.Mutex
	stop, err := startYamlWatcher(ctx, path, func(*Config) {
		mu.Lock()
		fires++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("startYamlWatcher: %v", err)
	}
	defer stop()

	time.Sleep(50 * time.Millisecond)

	// Write a SIBLING file — should NOT trigger the callback.
	sibling := filepath.Join(dir, "other.yaml")
	if err := os.WriteFile(sibling, []byte("apiVersion: v1\nkind: Other\n"), 0o600); err != nil {
		t.Fatalf("write sibling: %v", err)
	}
	time.Sleep(400 * time.Millisecond) // past coalesce window

	mu.Lock()
	defer mu.Unlock()
	if fires != 0 {
		t.Errorf("fires = %d, want 0 for sibling write", fires)
	}
}

func TestOnYamlChanged_CapacityOnlyReload(t *testing.T) {
	t.Parallel()
	spawner := NewWorkerSpawner(SpawnerOptions{MaxConcurrentSessions: 2})
	d := &Daemon{config: &Config{Capacity: CapacityConfig{MaxConcurrentSessions: 2, MaxVCpuPerSession: 3}}, spawner: spawner}
	d.onYamlChanged(&Config{Capacity: CapacityConfig{MaxConcurrentSessions: 0, MaxVCpuPerSession: 10}})
	if got := d.MaxConcurrentSessions(); got != 0 {
		t.Fatalf("live limit = %d, want 0", got)
	}
	if got := d.config.Capacity.MaxVCpuPerSession; got != 3 {
		t.Fatalf("vCPU limit = %d, want unchanged 3", got)
	}
	_, err := spawner.AcceptWork(SessionSpec{SessionID: "refused-at-zero"})
	if err == nil || !strings.Contains(err.Error(), "at capacity (0/0 sessions)") {
		t.Fatalf("zero admission = %v, want capacity refusal", err)
	}
	d.onYamlChanged(&Config{Capacity: CapacityConfig{MaxConcurrentSessions: -1}})
	if got := d.MaxConcurrentSessions(); got != 0 {
		t.Fatalf("negative reload changed limit to %d", got)
	}
}

func TestDaemonStart_ExplicitZeroSessionLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	if err := os.WriteFile(path, []byte("machine: {id: capacity-fixture}\norchestrator: {url: https://example.test}\ncapacity: {maxConcurrentSessions: 0}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := New(Options{ConfigPath: path, JWTPath: filepath.Join(dir, "daemon.jwt"), SkipWizard: true, SkipRegistration: true, HTTPHost: "127.0.0.1", HTTPPort: 0})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := d.Stop(stopCtx); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	if got := d.MaxConcurrentSessions(); got != 0 {
		t.Fatalf("started limit = %d, want 0", got)
	}
	_, err := d.spawner.AcceptWork(SessionSpec{SessionID: "refused-at-zero"})
	if err == nil || !strings.Contains(err.Error(), "at capacity (0/0 sessions)") {
		t.Fatalf("startup zero admission = %v, want capacity refusal", err)
	}
}

func TestOnYamlChanged_UpdatesProjects(t *testing.T) {
	t.Parallel()

	d := &Daemon{
		opts: Options{},
		config: &Config{
			Projects: []ProjectConfig{
				{ID: "alpha", Repository: "github.com/x/alpha"},
			},
		},
	}
	d.onYamlChanged(&Config{
		Projects: []ProjectConfig{
			{ID: "alpha", Repository: "github.com/x/alpha"},
			{ID: "beta", Repository: "github.com/x/beta"},
		},
	})

	if len(d.config.Projects) != 2 || d.config.Projects[1].ID != "beta" {
		t.Errorf("d.config.Projects = %+v, want 2 entries with beta", d.config.Projects)
	}
}
