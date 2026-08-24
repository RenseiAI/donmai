package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

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

func TestStartYamlWatcher_InvalidReloadKeepsLastKnownGoodState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	if err := WriteConfig(path, &Config{
		APIVersion:   "v1",
		Kind:         "DaemonConfig",
		Machine:      MachineConfig{ID: "m1"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 1},
		Orchestrator: OrchestratorConfig{URL: "https://example.test", AuthToken: "stub"},
		Projects:     []ProjectConfig{{ID: "alpha", Repository: "github.com/x/alpha"}},
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
	if err := os.WriteFile(path, []byte("projects: [\n"), 0o600); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if fires != 0 {
		t.Fatalf("invalid reload applied %d config(s), want last-known-good state preserved", fires)
	}
}

func TestOnYamlChanged_NoOpWhenAllowlistUnchanged(t *testing.T) {
	t.Parallel()

	// Set up a Daemon with an initial projects list. The callback should
	// short-circuit when the new config carries the same allowlist hash,
	// regardless of other field changes.
	d := &Daemon{
		opts: Options{},
		config: &Config{
			Projects: []ProjectConfig{
				{ID: "alpha", Repository: "github.com/x/alpha"},
			},
		},
	}
	d.onYamlChanged(&Config{
		// Capacity changed; allowlist unchanged.
		Capacity: CapacityConfig{MaxConcurrentSessions: 99},
		Projects: []ProjectConfig{
			{ID: "alpha", Repository: "github.com/x/alpha"},
		},
	})

	// Capacity should NOT have been hot-reloaded (out of scope for P3b).
	if d.config.Capacity.MaxConcurrentSessions == 99 {
		t.Error("capacity was hot-reloaded; should only reload projects[]")
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

func TestOnYamlChanged_ReRegistersMergedProjectScopes(t *testing.T) {
	t.Parallel()
	registrations := make(chan RegisterRequest, 1)
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
		registrations <- request
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "wkr_reloaded", "runtimeToken": "reloaded.jwt"})
	}))
	defer srv.Close()

	opts := testRefresherOptions(t, srv.URL, "wkr_before", "before.jwt")
	opts.Registration.JWTPath = ""
	r := NewCredentialRefresher(opts)
	d := &Daemon{
		config: &Config{
			ProjectAdmissionVersion: ProjectAdmissionVersionV2,
			EnabledProjectIDs:       []string{"alpha"},
			Repositories:            []RepositoryConfig{{ID: "repo-alpha", ProjectID: "alpha", Source: "github.com/x/alpha"}},
		},
		credentialRefresher: r,
	}

	d.onYamlChanged(&Config{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		EnabledProjectIDs:       []string{"alpha", "beta"},
		Repositories:            []RepositoryConfig{{ID: "repo-alpha", ProjectID: "alpha", Source: "github.com/x/alpha"}},
		Projects:                []ProjectConfig{{ID: "beta", Repository: "github.com/x/beta"}},
	})

	select {
	case request := <-registrations:
		if got, want := request.ProjectIDs, []string{"alpha", "beta"}; !slices.Equal(got, want) {
			t.Errorf("ProjectIDs = %v, want %v", got, want)
		}
		if got := request.DaemonProjects; len(got) != 2 || got[0].ID != "alpha" || got[1].ID != "beta" {
			t.Errorf("DaemonProjects = %+v, want merged alpha and beta", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("yaml reload did not re-register the merged project declaration")
	}
}
