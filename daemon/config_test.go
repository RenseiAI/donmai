package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/RenseiAI/donmai/afclient"
)

func TestLoadConfig_FileNotExist(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil config for missing file, got %+v", cfg)
	}
}

func TestLoadConfig_MaxConcurrentSessionsPresence(t *testing.T) {
	for _, tc := range []struct {
		name, capacity string
		want           int
		wantError      bool
	}{
		{name: "omitted", want: 8},
		{name: "empty", capacity: "capacity: {}\n", want: 8},
		{name: "zero", capacity: "capacity:\n  maxConcurrentSessions: 0\n", want: 0},
		{name: "positive", capacity: "capacity:\n  maxConcurrentSessions: 3\n", want: 3},
		{name: "negative", capacity: "capacity:\n  maxConcurrentSessions: -1\n", wantError: true},
		{name: "null", capacity: "capacity:\n  maxConcurrentSessions: null\n", want: 8},
		{name: "capacity_null", capacity: "capacity: null\n", want: 8},
		{name: "merged_zero", capacity: "capacity:\n  <<: {maxConcurrentSessions: 0}\n", want: 0},
		{name: "merge_overridden_zero", capacity: "capacity:\n  <<: {maxConcurrentSessions: 3}\n  maxConcurrentSessions: 0\n", want: 0},
		{name: "alias_zero", capacity: "defaults: &defaults {maxConcurrentSessions: 0}\ncapacity: *defaults\n", want: 0},
		{name: "alias_null", capacity: "defaults: &defaults {maxConcurrentSessions: null}\ncapacity: *defaults\n", want: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "daemon.yaml")
			body := "machine:\n  id: capacity-fixture\norchestrator:\n  url: https://example.test\n" + tc.capacity
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "capacity.maxConcurrentSessions must be >= 0") {
					t.Fatalf("negative value not refused: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Capacity.MaxConcurrentSessions != tc.want {
				t.Fatalf("loaded max sessions=%d want=%d", cfg.Capacity.MaxConcurrentSessions, tc.want)
			}
		})
	}
}

func TestWriteConfig_MaxConcurrentSessionsNonzeroToZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	cfg := &Config{
		Machine:      MachineConfig{ID: "isolated-fixture"},
		Orchestrator: OrchestratorConfig{URL: "https://example.test"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 3},
	}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("write nonzero config: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load nonzero config: %v", err)
	}
	updated := *loaded
	updated.Capacity.MaxConcurrentSessions = 0
	if err := WriteConfig(path, &updated); err != nil {
		t.Fatalf("write zero config: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode written YAML: %v", err)
	}
	var fields map[string]any
	if err := doc.Decode(&fields); err != nil {
		t.Fatalf("decode written mapping: %v", err)
	}
	capacity, ok := fields["capacity"].(map[string]any)
	if !ok || capacity["maxConcurrentSessions"] != 0 {
		t.Fatalf("written capacity = %v, want explicit maxConcurrentSessions: 0", fields["capacity"])
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload zero config: %v", err)
	}
	if got := reloaded.Capacity.MaxConcurrentSessions; got != 0 {
		t.Fatalf("reloaded max sessions = %d, want 0", got)
	}
}

func TestDaemonStart_UsesAuthoredZeroCapacity(t *testing.T) {
	for _, tt := range []struct {
		name     string
		capacity string
		want     int
	}{
		{name: "omitted defaults", want: 8},
		{name: "explicit zero refuses", capacity: "capacity:\n  maxConcurrentSessions: 0\n", want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "daemon.yaml")
			body := "machine:\n  id: isolated-fixture\norchestrator:\n  url: file:///" + dir + "/queue\n" + tt.capacity
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			d := New(Options{
				ConfigPath:       path,
				JWTPath:          filepath.Join(dir, "daemon.jwt"),
				SkipWizard:       true,
				SkipRegistration: true,
				HTTPHost:         "127.0.0.1",
				HTTPPort:         0,
				SpawnerOptions:   SpawnerOptions{WorkerCommand: []string{"/bin/false"}},
			})
			if err := d.Start(context.Background()); err != nil {
				t.Fatalf("foreground Start: %v", err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := d.Stop(ctx); err != nil {
					t.Errorf("foreground Stop: %v", err)
				}
			})
			if got := d.MaxConcurrentSessions(); got != tt.want {
				t.Errorf("advertised max sessions = %d, want %d", got, tt.want)
			}
			d.spawner.mu.Lock()
			spawnerLimit := d.spawner.opts.MaxConcurrentSessions
			d.spawner.mu.Unlock()
			if spawnerLimit != tt.want {
				t.Fatalf("runtime spawner limit = %d, want %d", spawnerLimit, tt.want)
			}
			if tt.want == 0 {
				if _, err := d.spawner.AcceptWork(SessionSpec{SessionID: "refused"}); err == nil || !strings.Contains(err.Error(), "at capacity (0/0 sessions)") {
					t.Fatalf("zero-capacity admission error = %v", err)
				}
			}
		})
	}
}

func TestOnYamlChanged_MaxConcurrentSessionsZeroRetainsActiveWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	initial := &Config{
		Machine:      MachineConfig{ID: "isolated-fixture"},
		Orchestrator: OrchestratorConfig{URL: "https://example.test"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 2},
	}
	if err := WriteConfig(path, initial); err != nil {
		t.Fatalf("write initial config: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	spawner := NewWorkerSpawner(SpawnerOptions{MaxConcurrentSessions: loaded.Capacity.MaxConcurrentSessions})
	existing := &spawnedSession{}
	spawner.sessions["already-running"] = existing
	d := &Daemon{config: loaded, spawner: spawner}
	updated := *loaded
	updated.Capacity.MaxConcurrentSessions = 0
	if err := WriteConfig(path, &updated); err != nil {
		t.Fatalf("write reduced capacity: %v", err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	d.onYamlChanged(reloaded)
	if got := d.MaxConcurrentSessions(); got != 0 {
		t.Fatalf("runtime advertised max sessions = %d, want 0", got)
	}
	spawner.mu.Lock()
	limit := spawner.opts.MaxConcurrentSessions
	retained := spawner.sessions["already-running"]
	spawner.mu.Unlock()
	if limit != 0 || retained != existing {
		t.Fatalf("runtime limit = %d, retained session = %p; want 0 and %p", limit, retained, existing)
	}
	if _, err := spawner.AcceptWork(SessionSpec{SessionID: "new-work"}); err == nil || !strings.Contains(err.Error(), "at capacity (1/0 sessions)") {
		t.Fatalf("new admission error = %v, want capacity refusal", err)
	}
}

func TestLoadConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	original := &Config{
		Machine: MachineConfig{ID: "test-machine", Region: "lab"},
		Capacity: CapacityConfig{
			MaxConcurrentSessions: 4,
			MaxVCpuPerSession:     2,
			MaxMemoryMbPerSession: 4096,
			ReservedForSystem:     ReservedSystemSpec{VCpu: 2, MemoryMb: 4096},
		},
		Projects: []ProjectConfig{{
			ID: "agentfactory", Repository: "github.com/foo/bar",
		}},
		Orchestrator: OrchestratorConfig{
			URL:       "https://platform.example.com",
			AuthToken: "rsp_live_test123",
		},
		AutoUpdate: AutoUpdateConfig{
			Channel:             ChannelStable,
			Schedule:            ScheduleNightly,
			DrainTimeoutSeconds: 600,
		},
	}
	if err := WriteConfig(path, original); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Machine.ID != "test-machine" {
		t.Errorf("MachineID = %q, want test-machine", loaded.Machine.ID)
	}
	if loaded.Capacity.MaxConcurrentSessions != 4 {
		t.Errorf("MaxConcurrentSessions = %d, want 4", loaded.Capacity.MaxConcurrentSessions)
	}
	if len(loaded.Projects) != 1 || loaded.Projects[0].ID != "agentfactory" {
		t.Errorf("Projects = %+v", loaded.Projects)
	}
	if loaded.Projects[0].Repository != "github.com/foo/bar" {
		t.Errorf("Repository = %q, want github.com/foo/bar", loaded.Projects[0].Repository)
	}
}

func TestLoadConfig_EnvSubstitution(t *testing.T) {
	t.Setenv("MY_TEST_TOKEN", "rsp_live_substituted")
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	body := []byte(`apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: test
capacity:
  maxConcurrentSessions: 1
  maxVCpuPerSession: 1
  maxMemoryMbPerSession: 1024
  reservedForSystem:
    vCpu: 1
    memoryMb: 1024
orchestrator:
  url: https://platform.example.com
  authToken: ${MY_TEST_TOKEN}
autoUpdate:
  channel: stable
  schedule: nightly
  drainTimeoutSeconds: 600
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Orchestrator.AuthToken != "rsp_live_substituted" {
		t.Errorf("authToken = %q, want substituted", cfg.Orchestrator.AuthToken)
	}
}

func TestLoadConfig_EnvOverride(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_TOKEN", "rsp_live_env_override")
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	cfg := &Config{
		Machine:      MachineConfig{ID: "test"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 1, MaxVCpuPerSession: 1, MaxMemoryMbPerSession: 1024, ReservedForSystem: ReservedSystemSpec{VCpu: 1, MemoryMb: 1024}},
		Orchestrator: OrchestratorConfig{URL: "https://platform.example.com", AuthToken: "old"},
		AutoUpdate:   AutoUpdateConfig{Channel: ChannelStable, Schedule: ScheduleNightly, DrainTimeoutSeconds: 600},
	}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Orchestrator.AuthToken != "rsp_live_env_override" {
		t.Errorf("token = %q, want env override", loaded.Orchestrator.AuthToken)
	}
}

func TestLoadConfig_InvalidMissingMachineID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	body := []byte(`apiVersion: donmai.dev/v1
machine: {}
orchestrator:
  url: https://example.com
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for missing machine.id")
	}
}

func TestDeriveDefaultMachineID(t *testing.T) {
	id := DeriveDefaultMachineID()
	if id == "" {
		t.Fatal("empty id")
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			t.Errorf("unexpected char %q in machine id %q", c, id)
		}
	}
}

func TestSubstituteEnvVars_UnsetIsKept(t *testing.T) {
	if got := substituteEnvVars("${THIS_DOES_NOT_EXIST_12345}"); got != "${THIS_DOES_NOT_EXIST_12345}" {
		t.Errorf("unset var should be left as-is, got %q", got)
	}
}

func TestDefaultConfig_HasSaneDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Capacity.MaxConcurrentSessions <= 0 {
		t.Errorf("max sessions should be > 0, got %d", cfg.Capacity.MaxConcurrentSessions)
	}
	if cfg.AutoUpdate.Channel != ChannelStable {
		t.Errorf("channel = %q, want stable", cfg.AutoUpdate.Channel)
	}
	if cfg.AutoUpdate.DrainTimeoutSeconds != 600 {
		t.Errorf("drain = %d, want 600", cfg.AutoUpdate.DrainTimeoutSeconds)
	}
}

func TestProjectContractMigration(t *testing.T) {
	tests := []struct {
		name     string
		version  int
		allowed  []string
		projects []ProjectConfig
		wantIDs  []string
	}{
		{
			name:     "legacy repositories imply admission",
			projects: []ProjectConfig{{ID: "alpha", Repository: "example.com/acme/alpha"}},
			wantIDs:  []string{"alpha"},
		},
		{
			name:     "explicit empty v2 admission stays empty",
			version:  ProjectAdmissionVersionV2,
			allowed:  []string{},
			projects: []ProjectConfig{{ID: "alpha", Repository: "example.com/acme/alpha"}},
			wantIDs:  []string{},
		},
		{
			name:    "project can be admitted without repositories",
			version: ProjectAdmissionVersionV2,
			allowed: []string{"beta"},
			wantIDs: []string{"beta"},
		},
		{
			name:     "multiple repositories share one admission identity",
			projects: []ProjectConfig{{ID: "alpha", Repository: "example.com/acme/one"}, {ID: "alpha", Repository: "example.com/acme/two"}},
			wantIDs:  []string{"alpha"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{ProjectAdmissionVersion: test.version, EnabledProjectIDs: test.allowed, Projects: test.projects}
			normalizeProjectContract(cfg)
			if diff := strings.Join(cfg.EffectiveEnabledProjectIDs(), ","); diff != strings.Join(test.wantIDs, ",") {
				t.Fatalf("EffectiveEnabledProjectIDs() = %v, want %v", cfg.EffectiveEnabledProjectIDs(), test.wantIDs)
			}
		})
	}
}

func TestWriteConfig_PreservesLegacyAdmissionVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	cfg := &Config{
		Machine:      MachineConfig{ID: "machine"},
		Orchestrator: OrchestratorConfig{URL: "file:///tmp/queue"},
		Projects: []ProjectConfig{
			{ID: "alpha", Repository: "example.com/acme/one"},
			{ID: "alpha", Repository: "example.com/acme/two"},
		},
	}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), "projectAdmissionVersion") || strings.Contains(string(raw), "enabledProjectIds") {
		t.Fatalf("legacy write prematurely emitted v2 admission fields: %s", raw)
	}
}

func TestWriteConfig_ExplicitEmptyAdmissionSurvivesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	cfg := &Config{
		Machine:                 MachineConfig{ID: "machine"},
		Orchestrator:            OrchestratorConfig{URL: "file:///tmp/queue"},
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		EnabledProjectIDs:       []string{},
		Projects:                []ProjectConfig{{ID: "alpha", Repository: "example.com/acme/alpha"}},
	}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := loaded.EffectiveEnabledProjectIDs(); len(got) != 0 {
		t.Fatalf("EffectiveEnabledProjectIDs() = %v, want empty", got)
	}
	if len(loaded.Repositories) != 1 {
		t.Fatalf("Repositories = %+v, repository resource was not preserved", loaded.Repositories)
	}
	if len(loaded.Projects) != 0 {
		t.Fatalf("Projects = %+v, disabled project leaked into legacy projection", loaded.Projects)
	}
}

func TestRepositoryNormalization_V2WinsAndRetainsDistinctLegacy(t *testing.T) {
	cfg := &Config{
		ProjectAdmissionVersion: ProjectAdmissionVersionV2,
		EnabledProjectIDs:       []string{"alpha"},
		Projects: []ProjectConfig{
			{ID: "alpha", Repository: "https://example.com/acme/api.git"},
			{ID: "alpha", Repository: "https://example.com/acme/web.git"},
		},
		Repositories: []RepositoryConfig{{
			ID:        "repo-api",
			ProjectID: "alpha",
			Source:    "https://example.com/acme/api",
			Primary:   true,
		}},
	}
	normalizeProjectContract(cfg)
	if len(cfg.Repositories) != 2 {
		t.Fatalf("Repositories = %+v, want v2 winner plus distinct legacy resource", cfg.Repositories)
	}
	var api RepositoryConfig
	for _, repository := range cfg.Repositories {
		if normalizeRepositorySource(repository.Source) == "https://example.com/acme/api" {
			api = repository
		}
	}
	if api.ID != "repo-api" || !api.Primary {
		t.Fatalf("v2 repository did not win conflict: %+v", api)
	}
}

func TestLegacyRepositoryID_IsStableAcrossEquivalentSources(t *testing.T) {
	t.Parallel()
	want := legacyRepositoryID("alpha", "https://EXAMPLE.com/acme/api.git/")
	if got := legacyRepositoryID("alpha", "https://example.com/acme/api"); got != want {
		t.Fatalf("legacyRepositoryID equivalent source = %q, want %q", got, want)
	}
	if got := legacyRepositoryID("beta", "https://example.com/acme/api"); got == want {
		t.Fatalf("legacyRepositoryID did not incorporate project identity: %q", got)
	}
}

func TestValidateConfig_RejectsMultiplePrimaryRepositories(t *testing.T) {
	cfg := &Config{
		Machine:      MachineConfig{ID: "machine"},
		Orchestrator: OrchestratorConfig{URL: "file:///tmp/queue"},
		Repositories: []RepositoryConfig{
			{ID: "repo-a", ProjectID: "alpha", Source: "example.com/a", Primary: true},
			{ID: "repo-b", ProjectID: "alpha", Source: "example.com/b", Primary: true},
		},
	}
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "primary conflicts") {
		t.Fatalf("validateConfig() error = %v, want primary conflict", err)
	}
}

// TestLoadConfig_LegacyRepoURLKey covers the legacy `repoUrl` back-compat path:
// pre-fix daemon.yaml files written by `rensei project allow` used the
// `repoUrl` key while the daemon reader expected `repository`. The
// ProjectConfig UnmarshalYAML now accepts both for one cycle, mapping the
// legacy key onto Repository so the project still counts toward
// `Projects: N allowed` in `daemon stats` / `daemon status`.
func TestLoadConfig_LegacyRepoURLKey(t *testing.T) {
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
projects:
  - id: legacy
    repoUrl: github.com/foo/legacy
orchestrator:
  url: https://platform.example.com
autoUpdate:
  channel: stable
  schedule: nightly
  drainTimeoutSeconds: 600
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("Projects len = %d, want 1 (legacy repoUrl key was rejected)", len(cfg.Projects))
	}
	if cfg.Projects[0].Repository != "github.com/foo/legacy" {
		t.Errorf("Repository = %q, want %q (legacy repoUrl should map to Repository)",
			cfg.Projects[0].Repository, "github.com/foo/legacy")
	}
	if cfg.Projects[0].ID != "legacy" {
		t.Errorf("ID = %q, want %q", cfg.Projects[0].ID, "legacy")
	}
}

// TestProjectAllowWriter_DaemonReader_RoundTrip is the regression test for
// the original bug: it writes a project allowlist entry via the same code path
// that `rensei project allow` exercises (afclient.WriteDaemonYAML), then
// stitches the resulting YAML into a full daemon.yaml shape and parses it
// with the daemon-side reader (LoadConfig). The Repository field on the
// loaded ProjectConfig must equal the RepoURL set on the ProjectEntry — if
// the writer and reader keys diverge again, this test fails before the bug
// can ship.
func TestProjectAllowWriter_DaemonReader_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	// 1. Writer side — same call path as `rensei project allow github.com/foo/bar`.
	writerPath := filepath.Join(dir, "writer.yaml")
	writer := &afclient.DaemonYAML{
		Projects: []afclient.ProjectEntry{{
			RepoURL: "github.com/foo/bar",
		}},
	}
	if err := afclient.WriteDaemonYAML(writerPath, writer); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}

	// Sanity: the on-disk file uses the canonical `repository` key, not the
	// legacy `repoUrl`. This is the line that would have caught the original regression.
	raw, err := os.ReadFile(writerPath)
	if err != nil {
		t.Fatalf("read writer yaml: %v", err)
	}
	if !containsKey(raw, "repository") {
		t.Errorf("writer yaml missing canonical key 'repository':\n%s", raw)
	}
	if containsKey(raw, "repoUrl") {
		t.Errorf("writer yaml still emits legacy key 'repoUrl':\n%s", raw)
	}

	// 2. Reader side — synthesize a full daemon.yaml around the writer's
	// projects[] block (the project allow command only owns that subset of
	// the file in production; the rest is set up by the wizard) and feed it
	// through LoadConfig.
	fullPath := filepath.Join(dir, "daemon.yaml")
	full := &Config{
		Machine: MachineConfig{ID: "test-machine"},
		Capacity: CapacityConfig{
			MaxConcurrentSessions: 1, MaxVCpuPerSession: 1, MaxMemoryMbPerSession: 1024,
			ReservedForSystem: ReservedSystemSpec{VCpu: 1, MemoryMb: 1024},
		},
		Orchestrator: OrchestratorConfig{URL: "https://platform.example.com"},
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleNightly, DrainTimeoutSeconds: 600,
		},
		Projects: []ProjectConfig{{
			ID:         "bar",
			Repository: writer.Projects[0].RepoURL,
		}},
	}
	if err := WriteConfig(fullPath, full); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	loaded, err := LoadConfig(fullPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(loaded.Projects) != 1 {
		t.Fatalf("Projects: %d allowed, want 1", len(loaded.Projects))
	}
	if loaded.Projects[0].Repository != "github.com/foo/bar" {
		t.Errorf("Repository = %q, want %q",
			loaded.Projects[0].Repository, "github.com/foo/bar")
	}
}

// TestProjectAllowWriter_PreservesFullConfig_ThenLoadConfigSucceeds is the
// regression test for the v0.4.1 follow-up.
// Sequence:
//
//  1. Wizard / installer writes a full daemon.yaml via daemon.WriteConfig.
//  2. `rensei project allow <repo>` mutates only the projects[] array via
//     afclient.WriteDaemonYAML.
//  3. Daemon restarts and calls daemon.LoadConfig — must succeed (no
//     "machine.id is required" / "orchestrator.url is required" /
//     "projects[N].id is required" errors) and the new project must be
//     present alongside the original one.
func TestProjectAllowWriter_PreservesFullConfig_ThenLoadConfigSucceeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")

	// 1. Initial wizard-style write.
	original := &Config{
		Machine: MachineConfig{ID: "wizard-host", Region: "lab"},
		Capacity: CapacityConfig{
			MaxConcurrentSessions: 4,
			MaxVCpuPerSession:     2,
			MaxMemoryMbPerSession: 4096,
			ReservedForSystem:     ReservedSystemSpec{VCpu: 2, MemoryMb: 4096},
		},
		Orchestrator: OrchestratorConfig{ //nolint:gosec // synthetic test token
			URL:       "https://platform.example.com",
			AuthToken: "rsk_live_test",
		},
		AutoUpdate: AutoUpdateConfig{
			Channel:             ChannelStable,
			Schedule:            ScheduleNightly,
			DrainTimeoutSeconds: 600,
		},
		Projects: []ProjectConfig{{
			ID:         "existing",
			Repository: "github.com/old/proj",
		}},
	}
	if err := WriteConfig(path, original); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	// 2. CLI-side mutation: `rensei project allow github.com/foo/bar`.
	cliCfg, err := afclient.ReadDaemonYAML(path)
	if err != nil {
		t.Fatalf("ReadDaemonYAML: %v", err)
	}
	cliCfg.AddOrUpdateProject(afclient.ProjectEntry{
		RepoURL: "github.com/foo/bar",
	})
	if err := afclient.WriteDaemonYAML(path, cliCfg); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}

	// 3. Daemon reader must still load the full config.
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after CLI mutation: %v", err)
	}
	if loaded.Machine.ID != "wizard-host" {
		t.Errorf("machine.id = %q (clobbered)", loaded.Machine.ID)
	}
	if loaded.Orchestrator.URL != "https://platform.example.com" {
		t.Errorf("orchestrator.url = %q (clobbered)", loaded.Orchestrator.URL)
	}
	if loaded.Orchestrator.AuthToken == "" {
		t.Error("orchestrator.authToken clobbered")
	}
	if loaded.AutoUpdate.DrainTimeoutSeconds != 600 {
		t.Errorf("autoUpdate.drainTimeoutSeconds = %d", loaded.AutoUpdate.DrainTimeoutSeconds)
	}
	if loaded.Capacity.MaxConcurrentSessions != 4 {
		t.Errorf("capacity.maxConcurrentSessions = %d", loaded.Capacity.MaxConcurrentSessions)
	}
	if len(loaded.Projects) != 2 {
		t.Fatalf("Projects = %d, want 2", len(loaded.Projects))
	}
	// Project IDs must be non-empty (writer auto-derives).
	for i, p := range loaded.Projects {
		if p.ID == "" {
			t.Errorf("Projects[%d].ID empty", i)
		}
		if p.Repository == "" {
			t.Errorf("Projects[%d].Repository empty", i)
		}
	}
}

// containsKey reports whether the YAML byte stream contains a top-level
// mapping key with the given name (it scans the parsed node tree, not raw
// bytes, so commented-out keys do not count).
func containsKey(data []byte, name string) bool {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return false
	}
	return scanForKey(&node, name)
}

func scanForKey(n *yaml.Node, name string) bool {
	if n == nil {
		return false
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == name {
				return true
			}
		}
	}
	for _, c := range n.Content {
		if scanForKey(c, name) {
			return true
		}
	}
	return false
}

// TestLoadConfig_KitScanPaths_DefaultsToDefaultKitScanPath asserts that a
// daemon.yaml with no `kit:` block lands ScanPaths == [DefaultKitScanPath()]
// after applyDefaults runs. This is the bare minimum back-compat property:
// pre-Wave-11 daemon.yaml files (which never wrote a kit block) keep
// scanning ~/.donmai/kits without operator action.
func TestLoadConfig_KitScanPaths_DefaultsToDefaultKitScanPath(t *testing.T) {
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
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{DefaultKitScanPath()}
	if len(cfg.Kit.ScanPaths) != 1 || cfg.Kit.ScanPaths[0] != want[0] {
		t.Errorf("Kit.ScanPaths = %v, want %v", cfg.Kit.ScanPaths, want)
	}
}

// TestLoadConfig_TrustMode_DefaultsToSignedByAllowlist asserts the
// secure default trust mode — daemon.yaml with no `trust:` block lands
// `trust.mode: signed-by-allowlist` after applyDefaults, including when
// the environment asks for permissive mode.
func TestLoadConfig_TrustMode_DefaultsToSignedByAllowlist(t *testing.T) {
	t.Setenv(envKitTrustMode, "")
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
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Trust.Mode != TrustModeSignedByAllowlist {
		t.Errorf("Trust.Mode default: want %q, got %q", TrustModeSignedByAllowlist, cfg.Trust.Mode)
	}
	// The default issuer set is seeded with the official vendor signer so
	// official signed kits install out of the box without --allow-unsigned.
	if got := cfg.Trust.IssuerSet; len(got) != 1 || got[0] != vendorSignerSAN {
		t.Errorf("Trust.IssuerSet default: want [%q], got %v", vendorSignerSAN, got)
	}

	// An environment-only permissive request cannot lower the default.
	t.Setenv(envKitTrustMode, string(TrustModePermissive))
	cfg2, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig (env override): %v", err)
	}
	if cfg2.Trust.Mode != TrustModeSignedByAllowlist {
		t.Errorf("Trust.Mode with %s=permissive: want %q, got %q", envKitTrustMode, TrustModeSignedByAllowlist, cfg2.Trust.Mode)
	}
	if err := os.WriteFile(path, append(body, []byte("trust:\n  mode: permissive\n")...), 0o600); err != nil {
		t.Fatalf("write explicit mode: %v", err)
	}
	t.Setenv(envKitTrustMode, string(TrustModeAttested))
	cfg3, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig (explicit YAML mode): %v", err)
	}
	if cfg3.Trust.Mode != TrustModePermissive {
		t.Errorf("explicit YAML mode with attested environment: want %q, got %q", TrustModePermissive, cfg3.Trust.Mode)
	}
}

// TestLoadConfig_TrustMode_ExplicitAllowlist asserts the
// signed-by-allowlist mode round-trips and the issuer set is preserved.
func TestLoadConfig_TrustMode_ExplicitAllowlist(t *testing.T) {
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
trust:
  mode: signed-by-allowlist
  issuerSet:
    - did:web:example.com
    - did:web:partner.example
  actor: ops@example.com
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Trust.Mode != TrustModeSignedByAllowlist {
		t.Errorf("Trust.Mode: want %q, got %q", TrustModeSignedByAllowlist, cfg.Trust.Mode)
	}
	if len(cfg.Trust.IssuerSet) != 2 {
		t.Fatalf("Trust.IssuerSet len: want 2, got %d (%v)", len(cfg.Trust.IssuerSet), cfg.Trust.IssuerSet)
	}
	if cfg.Trust.IssuerSet[0] != "did:web:example.com" {
		t.Errorf("IssuerSet[0]: got %q", cfg.Trust.IssuerSet[0])
	}
	if cfg.Trust.Actor != "ops@example.com" {
		t.Errorf("Trust.Actor: got %q", cfg.Trust.Actor)
	}
}

// TestLoadConfig_TrustMode_InvalidRejected asserts validateConfig
// rejects unknown trust mode values.
func TestLoadConfig_TrustMode_InvalidRejected(t *testing.T) {
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
trust:
  mode: bogus-mode
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatalf("LoadConfig: want validation error for unknown trust mode, got nil")
	}
}

// TestLoadConfig_UpdateSigners_RoundTrip asserts the autoUpdate signer
// allowlist + trust-root override round-trip through YAML.
func TestLoadConfig_UpdateSigners_RoundTrip(t *testing.T) {
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
  trustRootPath: /etc/donmai/trusted_root.json
  signers:
    - san: https://github.com/example/repo/.github/workflows/release.yml@refs/tags/v1.0.0
      issuer: https://token.actions.githubusercontent.com
    - san: releases@example.com
      issuer: https://issuer.example
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.AutoUpdate.Signers) != 2 {
		t.Fatalf("AutoUpdate.Signers len: want 2, got %d (%v)", len(cfg.AutoUpdate.Signers), cfg.AutoUpdate.Signers)
	}
	if got := cfg.AutoUpdate.Signers[1]; got.SAN != "releases@example.com" || got.Issuer != "https://issuer.example" {
		t.Errorf("Signers[1]: got %+v", got)
	}
	if cfg.AutoUpdate.TrustRootPath != "/etc/donmai/trusted_root.json" {
		t.Errorf("TrustRootPath: got %q", cfg.AutoUpdate.TrustRootPath)
	}
}

// TestLoadConfig_UpdateSigners_IncompleteRejected asserts validateConfig
// rejects signer entries that omit the san or the issuer — both must be
// pinned for binary-swap verification to be meaningful.
func TestLoadConfig_UpdateSigners_IncompleteRejected(t *testing.T) {
	cases := []struct {
		name, signerYAML string
	}{
		{"missing issuer", "    - san: releases@example.com"},
		{"missing san", "    - issuer: https://issuer.example"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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
  signers:
` + c.signerYAML + "\n")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatalf("LoadConfig: want validation error for incomplete signer, got nil")
			}
		})
	}
}

// TestLoadConfig_KitScanPaths_ExplicitOverride asserts that an explicit
// `kit.scanPaths` block round-trips through YAML in declaration order
// (no reordering, no de-duping, no default merge).
func TestLoadConfig_KitScanPaths_ExplicitOverride(t *testing.T) {
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
kit:
  scanPaths:
    - /tmp/kits-a
    - /tmp/kits-b
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{"/tmp/kits-a", "/tmp/kits-b"}
	if len(cfg.Kit.ScanPaths) != len(want) {
		t.Fatalf("Kit.ScanPaths len = %d, want %d (%v)", len(cfg.Kit.ScanPaths), len(want), cfg.Kit.ScanPaths)
	}
	for i, p := range want {
		if cfg.Kit.ScanPaths[i] != p {
			t.Errorf("Kit.ScanPaths[%d] = %q, want %q", i, cfg.Kit.ScanPaths[i], p)
		}
	}
}

// TestLoadConfig_CanonicalKeyWinsOverLegacy asserts that when both keys are
// present (pathological config) the canonical `repository` key wins, so a
// half-migrated file does not silently revert to the legacy URL.
func TestLoadConfig_CanonicalKeyWinsOverLegacy(t *testing.T) {
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
projects:
  - id: mixed
    repository: github.com/foo/canonical
    repoUrl: github.com/foo/legacy
orchestrator:
  url: https://platform.example.com
autoUpdate:
  channel: stable
  schedule: nightly
  drainTimeoutSeconds: 600
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Projects[0].Repository != "github.com/foo/canonical" {
		t.Errorf("Repository = %q, want canonical to win", cfg.Projects[0].Repository)
	}
}

func TestWriteConfig_ZeroSessionLimitRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	cfg := DefaultConfig()
	cfg.Machine.ID = "capacity-fixture"
	cfg.Orchestrator.URL = "https://example.test"
	cfg.Capacity.MaxConcurrentSessions = 3
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Capacity.MaxConcurrentSessions = 0
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Capacity.MaxConcurrentSessions != 0 {
		t.Fatalf("round trip limit = %d, want 0", loaded.Capacity.MaxConcurrentSessions)
	}
}

// TestWriteConfig_PreservesUnknownTopLevelKeys asserts the writer keeps
// top-level keys the Config struct does not declare: a fixture carrying
// an extra key survives a LoadConfig -> WriteConfig cycle verbatim in
// the re-read raw bytes, alongside the declared fields.
func TestWriteConfig_PreservesUnknownTopLevelKeys(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "daemon.yaml")
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
extraEmbeddingKey:
  nested: preserved-value
  count: 3
`)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cfg, err := LoadConfig(src)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	dst := filepath.Join(dir, "rewritten.yaml")
	if err := WriteConfig(dst, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read rewritten: %v", err)
	}
	var fields map[string]any
	if err := yaml.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode rewritten YAML: %v", err)
	}
	extra, ok := fields["extraEmbeddingKey"].(map[string]any)
	if !ok {
		t.Fatalf("rewritten YAML dropped unknown key: %s", raw)
	}
	if extra["nested"] != "preserved-value" || extra["count"] != 3 {
		t.Fatalf("unknown key value changed: %v", extra)
	}
	if cfg.Machine.ID != "test-machine" {
		t.Fatalf("Machine.ID = %q, want test-machine", cfg.Machine.ID)
	}
	// A declared field set through Config wins over a stale raw copy of
	// that same key: smuggle one in, then confirm the live value is
	// what the writer emits.
	cfg.unknownFields["machine"] = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	cfg.Machine.ID = "updated-machine"
	overridePath := filepath.Join(dir, "overridden.yaml")
	if err := WriteConfig(overridePath, cfg); err != nil {
		t.Fatalf("WriteConfig with stale raw key: %v", err)
	}
	overrideRaw, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatalf("read overridden: %v", err)
	}
	var overrideFields map[string]any
	if err := yaml.Unmarshal(overrideRaw, &overrideFields); err != nil {
		t.Fatalf("decode overridden YAML: %v", err)
	}
	machine, ok := overrideFields["machine"].(map[string]any)
	if !ok || machine["id"] != "updated-machine" {
		t.Fatalf("declared field lost to stale raw copy: %s", overrideRaw)
	}
}

// daemonSlogWarningCapture holds the log output emitted through the default
// slog logger while a test runs. warnIfRetiredCloneStrategyPresent logs via
// the package-level slog.Warn, so tests swap the default handler.
type daemonSlogWarningCapture struct {
	buf *bytes.Buffer
}

func (c *daemonSlogWarningCapture) text() string { return c.buf.String() }

func (c *daemonSlogWarningCapture) count() int {
	return bytes.Count(c.buf.Bytes(), []byte(retiredCloneStrategyWarning))
}

// captureDaemonSlogWarnings swaps the default slog logger for a text handler
// over a buffer and restores it when the test ends.
func captureDaemonSlogWarnings(t *testing.T) *daemonSlogWarningCapture {
	t.Helper()
	capture := &daemonSlogWarningCapture{buf: &bytes.Buffer{}}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(capture.buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return capture
}

// TestLoadConfig_RetiredCloneStrategyKeyLoadsWithOneWarning is the
// daemon-side retirement test for the dead per-repository clone override: a
// file that still carries the key on many entries loads successfully, the
// value is ignored (the typed schema no longer models it), the writer never
// emits it, and exactly one deprecation warning is logged per load no matter
// how many entries carry the key.
func TestLoadConfig_RetiredCloneStrategyKeyLoadsWithOneWarning(t *testing.T) {
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
projects:
  - id: one
    repository: github.com/foo/one
    cloneStrategy: shallow
  - id: two
    repository: github.com/foo/two
    cloneStrategy: full
repositories:
  - id: repo-one
    projectId: one
    source: github.com/foo/one
    cloneStrategy: shallow
orchestrator:
  url: https://platform.example.com
autoUpdate:
  channel: stable
  schedule: nightly
  drainTimeoutSeconds: 600
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	warnings := captureDaemonSlogWarnings(t)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig with retired key: %v", err)
	}
	if len(cfg.Projects) != 2 {
		t.Fatalf("Projects = %d, want 2 (retired key must not drop entries)", len(cfg.Projects))
	}
	if len(cfg.Repositories) != 2 {
		t.Fatalf("Repositories = %d, want 2 (retired key must not drop entries)", len(cfg.Repositories))
	}
	if got := warnings.count(); got != 1 {
		t.Fatalf("deprecation warnings = %d, want exactly 1", got)
	}
	if !strings.Contains(warnings.text(), "cloneStrategy") {
		t.Errorf("warning should name the retired key, got:\n%s", warnings.text())
	}
	// A load-then-write cycle drops the retired key: writers never emit it.
	out := filepath.Join(dir, "rewritten.yaml")
	if err := WriteConfig(out, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cloneStrategy") {
		t.Errorf("writer emitted retired key:\n%s", raw)
	}
	// A clean file loads with no warning.
	quiet := captureDaemonSlogWarnings(t)
	if _, err := LoadConfig(out); err != nil {
		t.Fatalf("LoadConfig after rewrite: %v", err)
	}
	if got := quiet.count(); got != 0 {
		t.Errorf("warnings on clean file = %d, want 0", got)
	}
}

// TestLoadConfig_NoRetiredCloneStrategyKeyIsQuiet pins the other half: a file
// without the retired key loads with no deprecation warning.
func TestLoadConfig_NoRetiredCloneStrategyKeyIsQuiet(t *testing.T) {
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
projects:
  - id: one
    repository: github.com/foo/one
orchestrator:
  url: https://platform.example.com
autoUpdate:
  channel: stable
  schedule: nightly
  drainTimeoutSeconds: 600
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	warnings := captureDaemonSlogWarnings(t)
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := warnings.count(); got != 0 {
		t.Errorf("warnings = %d, want 0", got)
	}
}

// TestRetiredCloneStrategySettingIsUnread is the grep-style guard for the
// retired per-repository clone override: no production code may read the
// field. The typed schema no longer models it, so any reintroduction — a
// restored struct field, a read of the value, a write of the key — fails
// this test. The retired constants and the warning/decode/detect helpers
// that implement the ignore-with-warning contract are the only allowed
// production mentions.
func TestRetiredCloneStrategySettingIsUnread(t *testing.T) {
	root, err := moduleRoot(t)
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	allowedFiles := map[string]bool{
		"daemon/types.go":           true,
		"daemon/config.go":          true,
		"afclient/daemon_config.go": true,
		"afcli/project.go":          true,
	}
	var violations []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == ".agent" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !allowedFiles[rel] {
			return nil
		}
		//nolint:gosec // G122: the walk root is the repo checkout and rel is
		// confined below it (filepath.Rel from the walk path); symlinks in a
		// developer checkout are not an attack surface for this guard test.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			stripped := stripLineComment(line)
			if !mentionsRetiredCloneSetting(stripped) {
				continue
			}
			if isAllowedRetiredCloneMention(rel, stripped) {
				continue
			}
			violations = append(violations, rel+":"+itoa(i+1)+": "+strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("retired clone setting is read or written by production code:\n%s", strings.Join(violations, "\n"))
	}
}

// moduleRoot returns the repository root by walking up from the test's
// working directory to the directory holding go.mod.
func moduleRoot(t *testing.T) (string, error) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errNoGoMod
		}
		dir = parent
	}
}

var errNoGoMod = errors.New("go.mod not found")

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// stripLineComment cuts a // comment, keeping string literals intact for the
// single-quoted YAML key this guard looks for (backticks are not comment
// syntax, so quoted mentions in comments stay visible to the allowlist).
func stripLineComment(line string) string {
	inString := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			if i == 0 || line[i-1] != '\\' {
				inString = !inString
			}
		case '/':
			if !inString && i+1 < len(line) && line[i+1] == '/' {
				return line[:i]
			}
		}
	}
	return line
}

// mentionsRetiredCloneSetting reports whether the code (comments stripped)
// names the retired setting outside its own machinery.
func mentionsRetiredCloneSetting(code string) bool {
	return strings.Contains(code, "CloneStrategy") ||
		strings.Contains(code, "cloneStrategy") ||
		strings.Contains(code, "clone-strategy") ||
		strings.Contains(code, "CloneShallow") ||
		strings.Contains(code, "CloneFull") ||
		strings.Contains(code, "CloneReference")
}

// isAllowedRetiredCloneMention permits only the ignore-with-warning
// machinery: the retired type and its constants, the warning constant and
// detector, the YAML decode slots that tolerate the key, and the hidden
// no-op flag with its declared removal version.
func isAllowedRetiredCloneMention(file, code string) bool {
	trimmed := strings.TrimSpace(code)
	// The retired type declaration and its constants.
	if strings.HasPrefix(trimmed, "type CloneStrategy ") {
		return true
	}
	if strings.HasPrefix(trimmed, "CloneShallow") || strings.HasPrefix(trimmed, "CloneFull") || strings.HasPrefix(trimmed, "CloneReference") {
		return true
	}
	// The warning constant, detector, strip helper, and their comments.
	if strings.Contains(code, "retiredCloneStrategyWarning") ||
		strings.Contains(code, "warnIfRetiredCloneStrategyPresent") ||
		strings.Contains(code, "stripRetiredCloneStrategyKey") ||
		strings.Contains(code, `"cloneStrategy"`) {
		return true
	}
	// The hidden no-op flag and its declared removal version.
	if strings.Contains(code, "cloneStrategyRemovalVersion") ||
		strings.Contains(code, "clone-strategy") ||
		strings.Contains(code, "cloneStrategy ") {
		return true
	}
	// Comments naming the retired key (lowercase prose, not code).
	if strings.HasPrefix(trimmed, "//") {
		return true
	}
	_ = file
	return false
}
