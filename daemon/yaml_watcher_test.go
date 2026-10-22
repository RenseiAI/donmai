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

// TestOnYamlChanged_AdoptsRotatedRegistrationToken pins the live-rotation
// contract: when the config file carries a new registration token for the
// same orchestrator, the watcher proves it against the control plane and
// adopts the proven generation into the credential refresher before the
// re-registration runs, so the reload presents the current generation
// instead of the revoked boot-time one.
//
// Against the pre-change code this test fails: nothing adopts the reloaded
// token, so the registration request still carries the stale generation.
func TestOnYamlChanged_AdoptsRotatedRegistrationToken(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	var proveAuth []string
	var registerAuth []string
	var registerReqs []RegisterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/refresh-token") {
			// The rotation's own prove round trip: honours the candidate
			// generation without minting a new worker identity.
			mu.Lock()
			proveAuth = append(proveAuth, r.Header.Get("Authorization"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "rotated.jwt"})
			return
		}
		if r.URL.Path != RegisterEndpoint {
			http.NotFound(w, r)
			return
		}
		var request RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode registration: %v", err)
			return
		}
		mu.Lock()
		registerAuth = append(registerAuth, r.Header.Get("Authorization"))
		registerReqs = append(registerReqs, request)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_rotated", "runtimeToken": "rotated.jwt"})
	}))
	defer srv.Close()

	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	opts.Registration.RegistrationToken = "rsp_live_before"
	opts.Registration.ProjectAdmissionVersion = ProjectAdmissionVersionV2
	opts.Registration.ProjectIDs = []string{"alpha"}
	credentials := NewCredentialRefresher(opts)
	d := &Daemon{
		config: &Config{
			ProjectAdmissionVersion: ProjectAdmissionVersionV2,
			EnabledProjectIDs:       []string{"alpha"},
			Orchestrator:            OrchestratorConfig{URL: srv.URL, AuthToken: "rsp_live_before"},
		},
		credentials:           credentials,
		registrationReloadCtx: ctx,
	}
	// The reloaded file enables a second project AND carries the rotated key,
	// the shape a project enable writes.
	d.onYamlChanged(&Config{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		EnabledProjectIDs:       []string{"alpha", "beta"},
		Orchestrator:            OrchestratorConfig{URL: srv.URL, AuthToken: "rsp_live_rotated"},
	})
	if err := credentials.WaitReregister(ctx); err != nil {
		t.Fatalf("rotated reload registration: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(proveAuth) == 0 {
		t.Fatal("rotation made no prove request against the control plane")
	}
	for _, auth := range proveAuth {
		if auth != "Bearer rsp_live_rotated" {
			t.Errorf("prove request presented %q, want Bearer rsp_live_rotated", auth)
		}
	}
	// The re-registration is recorded separately from the prove round trip:
	// dropping the deferred project publish after a rotation (or publishing
	// it before the proof, under the revoked generation) must fail here
	// instead of hiding behind the prove request's bearer.
	if len(registerReqs) == 0 {
		t.Fatal("project enable with a rotated key made no registration request")
	}
	for _, auth := range registerAuth {
		if auth != "Bearer rsp_live_rotated" {
			t.Errorf("registration presented %q, want Bearer rsp_live_rotated — the reload used the revoked generation", auth)
		}
	}
	for _, request := range registerReqs {
		if !slices.Equal(request.ProjectIDs, []string{"alpha", "beta"}) {
			t.Errorf("registered project IDs = %v, want alpha,beta — the staged declaration was not published with the proven generation", request.ProjectIDs)
		}
	}
	if token, _ := credentials.Registration(); token != "rsp_live_rotated" {
		t.Errorf("refresher token = %q, want the rotated generation", token)
	}
	if d.config.Orchestrator.AuthToken != "rsp_live_rotated" {
		t.Errorf("stored config token = %q, want the rotated generation", d.config.Orchestrator.AuthToken)
	}
}

// TestOnYamlChanged_PureTokenRotationReregisters pins that a rotation with no
// accompanying project change is proven and then presented: the watcher's
// prove round trip re-presents the identity under the new generation, and the
// re-registration presents it to the control plane. Without this, a key-only
// file edit is swallowed by the watcher's no-change early return and the
// daemon keeps the revoked key until the next project edit or restart. A
// generation the control plane refuses is never adopted and triggers no
// re-registration.
func TestOnYamlChanged_PureTokenRotationReregisters(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requests := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/refresh-token") {
			// The rotation's own prove round trip.
			_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "rotated.jwt"})
			return
		}
		if r.URL.Path != RegisterEndpoint {
			http.NotFound(w, r)
			return
		}
		requests <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_rotated", "runtimeToken": "rotated.jwt"})
	}))
	defer srv.Close()

	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	opts.Registration.RegistrationToken = "rsp_live_before"
	opts.Registration.ProjectAdmissionVersion = ProjectAdmissionVersionV2
	opts.Registration.ProjectIDs = []string{"alpha"}
	credentials := NewCredentialRefresher(opts)
	d := &Daemon{
		config: &Config{
			ProjectAdmissionVersion: ProjectAdmissionVersionV2,
			EnabledProjectIDs:       []string{"alpha"},
			Orchestrator:            OrchestratorConfig{URL: srv.URL, AuthToken: "rsp_live_before"},
		},
		credentials:           credentials,
		registrationReloadCtx: ctx,
	}
	d.onYamlChanged(&Config{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		EnabledProjectIDs:       []string{"alpha"},
		Orchestrator:            OrchestratorConfig{URL: srv.URL, AuthToken: "rsp_live_rotated"},
	})
	if err := credentials.WaitReregister(ctx); err != nil {
		t.Fatalf("pure rotation registration: %v", err)
	}
	select {
	case auth := <-requests:
		if auth != "Bearer rsp_live_rotated" {
			t.Errorf("registration presented %q, want Bearer rsp_live_rotated", auth)
		}
	case <-ctx.Done():
		t.Fatal("pure token rotation made no registration request")
	}
}

// TestOnYamlChanged_RefusesTokenForADifferentOrchestrator pins the adoption
// guard: a token arriving alongside a changed orchestrator URL is refused —
// the previous generation stays in place and the stored config keeps the old
// key, because a re-home is a restart-level change the watcher never applies.
func TestOnYamlChanged_RefusesTokenForADifferentOrchestrator(t *testing.T) {
	t.Parallel()

	opts := testRefresherOptions(t, "https://orchestrator.example.test", "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	opts.Registration.RegistrationToken = "rsp_live_before"
	credentials := NewCredentialRefresher(opts)
	d := &Daemon{
		config: &Config{
			Orchestrator: OrchestratorConfig{URL: "https://orchestrator.example.test", AuthToken: "rsp_live_before"},
		},
		credentials: credentials,
	}
	d.onYamlChanged(&Config{
		Orchestrator: OrchestratorConfig{URL: "https://elsewhere.example.test", AuthToken: "rsp_live_foreign"},
	})
	if token, _ := credentials.Registration(); token != "rsp_live_before" {
		t.Errorf("refresher token = %q, want the previous generation kept", token)
	}
	if d.config.Orchestrator.AuthToken != "rsp_live_before" {
		t.Errorf("stored config token = %q, want the previous generation kept", d.config.Orchestrator.AuthToken)
	}
}

// TestOnYamlChanged_RefusesForeignShapedToken pins the second adoption guard:
// a non-blank value without the platform's registration-token shape is never
// adopted mid-flight — the previous generation stays so a mis-edited file
// cannot strand the daemon on a credential the control plane will refuse.
func TestOnYamlChanged_RefusesForeignShapedToken(t *testing.T) {
	t.Parallel()

	opts := testRefresherOptions(t, "https://orchestrator.example.test", "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	opts.Registration.RegistrationToken = "rsp_live_before"
	credentials := NewCredentialRefresher(opts)
	d := &Daemon{
		config: &Config{
			Projects:     []ProjectConfig{{ID: "alpha", Repository: "github.com/x/alpha"}},
			Orchestrator: OrchestratorConfig{URL: "https://orchestrator.example.test", AuthToken: "rsp_live_before"},
		},
		credentials: credentials,
	}
	d.onYamlChanged(&Config{
		Projects:     []ProjectConfig{{ID: "alpha", Repository: "github.com/x/alpha"}, {ID: "beta", Repository: "github.com/x/beta"}},
		Orchestrator: OrchestratorConfig{URL: "https://orchestrator.example.test", AuthToken: "operator-pasted-wrong-value"},
	})
	if token, _ := credentials.Registration(); token != "rsp_live_before" {
		t.Errorf("refresher token = %q, want the previous generation kept", token)
	}
	if d.config.Orchestrator.AuthToken != "rsp_live_before" {
		t.Errorf("stored config token = %q, want the previous generation kept", d.config.Orchestrator.AuthToken)
	}
	if len(d.config.Projects) != 2 {
		t.Errorf("projects = %+v, want the project half of the reload to still apply", d.config.Projects)
	}
}

// TestOnYamlChanged_IgnoresUnchangedToken pins that an identical token is a
// no-op for the refresher: the watcher must not churn the registration when
// the file is rewritten with the same key.
func TestOnYamlChanged_IgnoresUnchangedToken(t *testing.T) {
	t.Parallel()

	opts := testRefresherOptions(t, "https://orchestrator.example.test", "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	opts.Registration.RegistrationToken = "rsp_live_same"
	credentials := NewCredentialRefresher(opts)
	d := &Daemon{
		config: &Config{
			Projects:     []ProjectConfig{{ID: "alpha", Repository: "github.com/x/alpha"}},
			Orchestrator: OrchestratorConfig{URL: "https://orchestrator.example.test", AuthToken: "rsp_live_same"},
		},
		credentials: credentials,
	}
	d.onYamlChanged(&Config{
		Projects:     []ProjectConfig{{ID: "alpha", Repository: "github.com/x/alpha"}, {ID: "beta", Repository: "github.com/x/beta"}},
		Orchestrator: OrchestratorConfig{URL: "https://orchestrator.example.test", AuthToken: "rsp_live_same"},
	})
	if token, _ := credentials.Registration(); token != "rsp_live_same" {
		t.Errorf("refresher token = %q, want the unchanged generation", token)
	}
}

// TestOnYamlChanged_RefusedTokenLeavesRegistrationUntouched pins that a
// refused token is a full no-op for the registration half of a reload: the
// refresher and the stored config keep the previous generation, and no
// re-registration runs. Re-registering against an unchanged registration
// would mint a fresh worker identity on every later write of the same
// refused file. The stub case pins that a daemon running on the stub path
// (no platform registration, so no poll loop) is never promoted to a real
// registration live: that switch would heartbeat without ever claiming work.
func TestOnYamlChanged_RefusedTokenLeavesRegistrationUntouched(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		runningToken string
		nextURL      func(srvURL string) string
		nextToken    string
	}{
		{
			name:         "foreign shape",
			runningToken: "rsp_live_before",
			nextURL:      func(srvURL string) string { return srvURL },
			nextToken:    "operator-pasted-wrong-value",
		},
		{
			name:         "different orchestrator",
			runningToken: "rsp_live_before",
			nextURL:      func(string) string { return "https://elsewhere.example.test" },
			nextToken:    "rsp_live_foreign",
		},
		{
			name:         "running on the stub path",
			runningToken: "local-stub-no-token",
			nextURL:      func(srvURL string) string { return srvURL },
			nextToken:    "rsk_live_promoted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			t.Cleanup(cancel)
			var mu sync.Mutex
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_churned", "runtimeToken": "churned.jwt"})
			}))
			t.Cleanup(srv.Close)

			opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
			opts.Registration.RegistrationToken = tc.runningToken
			credentials := NewCredentialRefresher(opts)
			d := &Daemon{
				config: &Config{
					Orchestrator: OrchestratorConfig{URL: srv.URL, AuthToken: tc.runningToken},
				},
				credentials:           credentials,
				registrationReloadCtx: ctx,
			}
			d.onYamlChanged(&Config{
				Orchestrator: OrchestratorConfig{URL: tc.nextURL(srv.URL), AuthToken: tc.nextToken},
			})
			if err := credentials.WaitReregister(ctx); err != nil {
				t.Fatalf("wait for reload registration: %v", err)
			}
			if token, _ := credentials.Registration(); token != tc.runningToken {
				t.Errorf("refresher token = %q, want the previous generation kept", token)
			}
			if d.config.Orchestrator.AuthToken != tc.runningToken {
				t.Errorf("stored config token = %q, want the previous generation kept", d.config.Orchestrator.AuthToken)
			}
			mu.Lock()
			defer mu.Unlock()
			if requests != 0 {
				t.Errorf("refused token made %d registration request(s), want none", requests)
			}
			if workerID, _ := credentials.Current(); workerID != "wkr_before" {
				t.Errorf("worker id = %q, want the running identity kept", workerID)
			}
		})
	}
}

// TestOnYamlChanged_ControlPlaneRefusalMakesNoRegistrationRequest pins that a
// rotation the control plane refuses (401 on the prove round trip) is a full
// no-op for the registration half: with a live reload context set, the
// refresher and the filed pointer keep the previous generation, the lanes
// keep the running credentials, and no re-registration runs. Re-registering
// anyway would mint a fresh worker identity on every later write of the same
// refused file.
func TestOnYamlChanged_ControlPlaneRefusalMakesNoRegistrationRequest(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var mu sync.Mutex
	registerCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/refresh-token") {
			if r.Header.Get("Authorization") == "Bearer rsp_live_rejected" {
				http.Error(w, `{"error":"unknown registration"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
			return
		}
		if r.URL.Path != RegisterEndpoint {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		registerCount++
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_churned", "runtimeToken": "churned.jwt"})
	}))
	defer srv.Close()

	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	// #nosec G101 -- test fixture token
	opts.Registration.RegistrationToken = "rsp_live_before"
	credentials := NewCredentialRefresher(opts)
	lane := &recordingLane{}
	credentials.Attach(lane)
	d := &Daemon{
		config: &Config{
			ProjectAdmissionVersion: ProjectAdmissionVersionV2,
			EnabledProjectIDs:       []string{"alpha"},
			Orchestrator:            OrchestratorConfig{URL: srv.URL, AuthToken: "rsp_live_before"},
		},
		credentials:           credentials,
		registrationReloadCtx: ctx,
	}
	d.onYamlChanged(&Config{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		EnabledProjectIDs:       []string{"alpha"},
		// #nosec G101 -- test fixture token
		Orchestrator: OrchestratorConfig{URL: srv.URL, AuthToken: "rsp_live_rejected"},
	})
	if err := credentials.WaitReregister(ctx); err != nil {
		t.Fatalf("wait for reload registration: %v", err)
	}
	if token, _ := credentials.Registration(); token != "rsp_live_before" {
		t.Errorf("refresher token = %q, want the previous generation kept", token)
	}
	if d.config.Orchestrator.AuthToken != "rsp_live_before" {
		t.Errorf("stored config token = %q, want the previous generation kept", d.config.Orchestrator.AuthToken)
	}
	if id, jwt, _ := lane.current(); id != "wkr_before" || jwt != "before.jwt" {
		t.Errorf("lane = (%q, %q), want the running credentials kept", id, jwt)
	}
	if workerID, _ := credentials.Current(); workerID != "wkr_before" {
		t.Errorf("worker id = %q, want the running identity kept", workerID)
	}
	mu.Lock()
	defer mu.Unlock()
	if registerCount != 0 {
		t.Errorf("refused rotation made %d registration request(s), want none", registerCount)
	}
}

// watcherRotationHarness serves the prove round trip for back-to-back
// rotation tests: the first T1 prove blocks until release, every other
// candidate bearer is honoured, and every registration request is recorded.
type watcherRotationHarness struct {
	t            *testing.T
	srv          *httptest.Server
	entered      chan struct{}
	release      chan struct{}
	enterOnce    sync.Once
	mu           sync.Mutex
	registerAuth []string
	registerReqs []RegisterRequest
}

func newWatcherRotationHarness(t *testing.T, blockedBearer string) *watcherRotationHarness {
	t.Helper()
	h := &watcherRotationHarness{
		t:       t,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/refresh-token") {
			auth := r.Header.Get("Authorization")
			if auth == blockedBearer {
				h.enterOnce.Do(func() { close(h.entered) })
				select {
				case <-h.release:
				case <-r.Context().Done():
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"runtimeToken": "refreshed.jwt"})
			return
		}
		if r.URL.Path != RegisterEndpoint {
			http.NotFound(w, r)
			return
		}
		var request RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode registration: %v", err)
			return
		}
		h.mu.Lock()
		h.registerAuth = append(h.registerAuth, r.Header.Get("Authorization"))
		h.registerReqs = append(h.registerReqs, request)
		h.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_before", "runtimeToken": "refreshed.jwt"})
	}))
	t.Cleanup(h.srv.Close)
	t.Cleanup(func() {
		select {
		case <-h.release:
		default:
			close(h.release)
		}
	})
	return h
}

func (h *watcherRotationHarness) watchedDaemon(ctx context.Context, t *testing.T, filedToken string) *Daemon {
	t.Helper()
	opts := testRefresherOptions(t, h.srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	// #nosec G101 -- test fixture token
	opts.Registration.RegistrationToken = filedToken
	return &Daemon{
		config: &Config{
			ProjectAdmissionVersion: ProjectAdmissionVersionV2,
			EnabledProjectIDs:       []string{"alpha"},
			// #nosec G101 -- test fixture token
			Orchestrator: OrchestratorConfig{URL: h.srv.URL, AuthToken: filedToken},
		},
		credentials:           NewCredentialRefresher(opts),
		registrationReloadCtx: ctx,
	}
}

func (h *watcherRotationHarness) rotationCfg(nextToken string) *Config {
	h.t.Helper()
	return &Config{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		EnabledProjectIDs:       []string{"alpha"},
		// #nosec G101 -- test fixture tokens
		Orchestrator: OrchestratorConfig{URL: h.srv.URL, AuthToken: nextToken},
	}
}

func (h *watcherRotationHarness) filedToken(d *Daemon) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.config.Orchestrator.AuthToken
}

// TestOnYamlChanged_ConcurrentReloadsSerialize pins that whole reloads
// serialize across the prove round trip. Two rows drive it:
//
//   - back to back: a second write landing inside the first rotation's prove
//     waits for the first rotation to commit, then validates and proves
//     against the committed state, so the filed pointer and the refresher
//     converge on the newest generation. Without the serialization both
//     validate against the pre-flight generation and the older commit wins
//     the filed pointer while the refresher holds the newer one.
//   - revert in flight: a revert landing while a rotation is proving waits
//     for the in-flight rotation to commit, then validates the filed value
//     against the committed state and rotates back, so the daemon ends on
//     the generation the file names. Without the serialization the revert
//     sees no change against the pre-flight filed pointer and returns early,
//     stranding the daemon on a generation the file no longer names.
func TestOnYamlChanged_ConcurrentReloadsSerialize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		secondToken string
		wantFinal   string
	}{
		{
			name:        "back to back rotations converge on the newest generation",
			secondToken: "rsp_live_t2",
			wantFinal:   "rsp_live_t2",
		},
		{
			name:        "revert during rotation is not lost",
			secondToken: "rsp_live_t0",
			wantFinal:   "rsp_live_t0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// #nosec G101 -- test fixture tokens
			h := newWatcherRotationHarness(t, "Bearer rsp_live_t1")
			d := h.watchedDaemon(ctx, t, "rsp_live_t0")

			firstDone := make(chan struct{})
			go func() {
				defer close(firstDone)
				d.onYamlChanged(h.rotationCfg("rsp_live_t1"))
			}()
			select {
			case <-h.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first rotation did not reach the wire")
			}
			secondDone := make(chan struct{})
			go func() {
				defer close(secondDone)
				d.onYamlChanged(h.rotationCfg(tc.secondToken))
			}()
			// Give the second reload a chance to (incorrectly) validate
			// against the pre-flight generation before the first rotation
			// commits.
			time.Sleep(200 * time.Millisecond)
			close(h.release)
			select {
			case <-firstDone:
			case <-time.After(10 * time.Second):
				t.Fatal("first reload did not return")
			}
			select {
			case <-secondDone:
			case <-time.After(10 * time.Second):
				t.Fatal("second reload did not return")
			}
			if err := d.credentials.WaitReregister(ctx); err != nil {
				t.Fatalf("reload registrations: %v", err)
			}
			if token, _ := d.credentials.Registration(); token != tc.wantFinal {
				t.Errorf("refresher holds %q, want %q", token, tc.wantFinal)
			}
			if filed := h.filedToken(d); filed != tc.wantFinal {
				t.Errorf("filed token = %q, want %q — the reloads did not serialize", filed, tc.wantFinal)
			}
		})
	}
}

// TestOnYamlChanged_NoRegistrationBeforeProof pins that the staged project
// declaration is published only after the rotation proves: while the prove
// round trip is in flight, no registration request reaches the wire, and the
// one that follows presents the proven generation with the staged project
// IDs. Publishing before the proof would present the revoked generation to
// the control plane.
func TestOnYamlChanged_NoRegistrationBeforeProof(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// #nosec G101 -- test fixture tokens
	h := newWatcherRotationHarness(t, "Bearer rsp_live_rotated")
	d := h.watchedDaemon(ctx, t, "rsp_live_t0")

	// A project enable riding with the rotated key, the shape a project
	// enable writes.
	next := h.rotationCfg("rsp_live_rotated")
	next.EnabledProjectIDs = []string{"alpha", "beta"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.onYamlChanged(next)
	}()
	select {
	case <-h.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not reach the wire")
	}
	// Give an early (pre-proof) publish a chance to (incorrectly) reach the
	// wire while the prove is still blocked.
	time.Sleep(200 * time.Millisecond)
	h.mu.Lock()
	early := len(h.registerAuth)
	h.mu.Unlock()
	if early != 0 {
		t.Fatalf("registration published before the proof (%d request(s)) — the reload presented the revoked generation", early)
	}
	close(h.release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reload did not return")
	}
	if err := d.credentials.WaitReregister(ctx); err != nil {
		t.Fatalf("reload registrations: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.registerReqs) == 0 {
		t.Fatal("proven rotation with a project enable made no registration request")
	}
	for _, auth := range h.registerAuth {
		if auth != "Bearer rsp_live_rotated" {
			t.Errorf("registration presented %q, want Bearer rsp_live_rotated", auth)
		}
	}
	for _, request := range h.registerReqs {
		if !slices.Equal(request.ProjectIDs, []string{"alpha", "beta"}) {
			t.Errorf("registered project IDs = %v, want alpha,beta", request.ProjectIDs)
		}
	}
}
