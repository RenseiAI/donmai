package daemon

import (
	"os"
	"path/filepath"
	"testing"
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
