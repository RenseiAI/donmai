package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/RenseiAI/agentfactory-tui/afclient"
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
			URL:       "https://platform.rensei.dev",
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
	if loaded.Projects[0].CloneStrategy != CloneShallow {
		t.Errorf("CloneStrategy default = %q, want shallow", loaded.Projects[0].CloneStrategy)
	}
}

func TestLoadConfig_EnvSubstitution(t *testing.T) {
	t.Setenv("MY_TEST_TOKEN", "rsp_live_substituted")
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	body := []byte(`apiVersion: rensei.dev/v1
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
  url: https://platform.rensei.dev
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
	t.Setenv("RENSEI_DAEMON_TOKEN", "rsp_live_env_override")
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	cfg := &Config{
		Machine:      MachineConfig{ID: "test"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 1, MaxVCpuPerSession: 1, MaxMemoryMbPerSession: 1024, ReservedForSystem: ReservedSystemSpec{VCpu: 1, MemoryMb: 1024}},
		Orchestrator: OrchestratorConfig{URL: "https://platform.rensei.dev", AuthToken: "old"},
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
	body := []byte(`apiVersion: rensei.dev/v1
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

// TestLoadConfig_LegacyRepoURLKey covers the REN-1419 back-compat path:
// pre-fix daemon.yaml files written by `rensei project allow` used the
// `repoUrl` key while the daemon reader expected `repository`. The
// ProjectConfig UnmarshalYAML now accepts both for one cycle, mapping the
// legacy key onto Repository so the project still counts toward
// `Projects: N allowed` in `daemon stats` / `daemon status`.
func TestLoadConfig_LegacyRepoURLKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	body := []byte(`apiVersion: rensei.dev/v1
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
    cloneStrategy: shallow
orchestrator:
  url: https://platform.rensei.dev
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
// REN-1419 itself: it writes a project allowlist entry via the same code path
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
			RepoURL:       "github.com/foo/bar",
			CloneStrategy: afclient.CloneShallow,
		}},
	}
	if err := afclient.WriteDaemonYAML(writerPath, writer); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}

	// Sanity: the on-disk file uses the canonical `repository` key, not the
	// legacy `repoUrl`. This is the line that would have caught REN-1419.
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
		Orchestrator: OrchestratorConfig{URL: "https://platform.rensei.dev"},
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
// regression test for the v0.4.1 follow-up to REN-1419 (REN-1442 + REN-1443).
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
		Orchestrator: OrchestratorConfig{
			URL:       "https://platform.rensei.dev",
			AuthToken: "rsk_live_test", //nolint:gosec // synthetic
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
		RepoURL:       "github.com/foo/bar",
		CloneStrategy: afclient.CloneShallow,
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
	if loaded.Orchestrator.URL != "https://platform.rensei.dev" {
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

// TestLoadConfig_CanonicalKeyWinsOverLegacy asserts that when both keys are
// present (pathological config) the canonical `repository` key wins, so a
// half-migrated file does not silently revert to the legacy URL.
func TestLoadConfig_CanonicalKeyWinsOverLegacy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	body := []byte(`apiVersion: rensei.dev/v1
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
    cloneStrategy: shallow
orchestrator:
  url: https://platform.rensei.dev
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
