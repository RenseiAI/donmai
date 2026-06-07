package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runner/access"
	"gopkg.in/yaml.v3"
)

// TestLoadConfig_ModelAccessRoundTrip writes a Config carrying a ModelAccess
// block (default + per-workload, the §4.3 Mac-Studio worked example) and reads
// it back, asserting the synced narrowing block survives the WriteConfig ->
// LoadConfig atomic round-trip byte-for-byte on the load-bearing fields.
func TestLoadConfig_ModelAccessRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")

	original := &Config{
		Machine: MachineConfig{ID: "mac-studio-01", Region: "local"},
		Capacity: CapacityConfig{
			MaxConcurrentSessions: 1,
			ReservedForSystem:     ReservedSystemSpec{VCpu: 1, MemoryMb: 1024},
		},
		Orchestrator: OrchestratorConfig{URL: "https://platform.example.com"},
		AutoUpdate: AutoUpdateConfig{
			Channel: ChannelStable, Schedule: ScheduleNightly, DrainTimeoutSeconds: 600,
		},
		ModelAccess: &access.ModelAccessConfig{
			Default: access.AccessPolicy{
				Matrix: map[string]map[string]access.AccessCostCell{
					"anthropic": {
						string(agent.AuthHostSession): {Allowed: true, Host: "oauth-cli"},
						string(agent.AuthBYOK):        {Allowed: false},
					},
				},
			},
			Workloads: map[string]access.AccessPolicy{
				"kg-extraction": {
					AuthOrder: []access.AuthMode{agent.AuthBYOK, agent.AuthMetered, agent.AuthHostSession, agent.AuthLocal},
					Matrix: map[string]map[string]access.AccessCostCell{
						"anthropic": {
							string(agent.AuthBYOK):        {Allowed: true, Host: "vertex", Model: "claude-haiku"},
							string(agent.AuthHostSession): {Allowed: false},
						},
					},
				},
			},
		},
	}

	if err := WriteConfig(path, original); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.ModelAccess == nil {
		t.Fatalf("ModelAccess nil after round-trip")
	}

	def := loaded.ModelAccess.Default.Matrix["anthropic"]
	if hs := def[string(agent.AuthHostSession)]; !hs.Allowed || hs.Host != "oauth-cli" {
		t.Errorf("default host-session = %+v, want {allowed,oauth-cli}", hs)
	}
	if byok := def[string(agent.AuthBYOK)]; byok.Allowed {
		t.Errorf("default byok = %+v, want allowed:false", byok)
	}

	kg, ok := loaded.ModelAccess.Workloads["kg-extraction"]
	if !ok {
		t.Fatalf("kg-extraction workload absent after round-trip")
	}
	if len(kg.AuthOrder) != 4 || kg.AuthOrder[0] != agent.AuthBYOK {
		t.Errorf("kg authOrder = %v, want [byok metered host-session local]", kg.AuthOrder)
	}
	kgCell := kg.Matrix["anthropic"][string(agent.AuthBYOK)]
	if !kgCell.Allowed || kgCell.Host != "vertex" || kgCell.Model != "claude-haiku" {
		t.Errorf("kg byok cell = %+v, want {allowed,vertex,claude-haiku}", kgCell)
	}
}

// TestLoadConfig_ModelAccessFromYAML asserts the §4.3 daemon.yaml block parses
// from the literal YAML shape the platform syncs (key names + nesting), so a
// hand-written or platform-written daemon.yaml decodes into the typed block.
func TestLoadConfig_ModelAccessFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	body := []byte(`apiVersion: donmai.dev/v1
kind: LocalDaemon
machine:
  id: mac-studio-01
  region: local
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
modelAccess:
  default:
    matrix:
      "anthropic":
        host-session: { allowed: true,  host: "oauth-cli" }
        byok:         { allowed: false }
  workloads:
    kg-extraction:
      authOrder: [byok, metered, host-session, local]
      matrix:
        "anthropic":
          byok:         { allowed: true, host: "vertex", model: "claude-haiku" }
          host-session: { allowed: false }
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ModelAccess == nil {
		t.Fatalf("ModelAccess nil after YAML decode")
	}
	if hs := cfg.ModelAccess.Default.Matrix["anthropic"]["host-session"]; !hs.Allowed || hs.Host != "oauth-cli" {
		t.Errorf("yaml default host-session = %+v, want {allowed,oauth-cli}", hs)
	}
	kg := cfg.ModelAccess.Workloads["kg-extraction"]
	if cell := kg.Matrix["anthropic"]["byok"]; !cell.Allowed || cell.Host != "vertex" || cell.Model != "claude-haiku" {
		t.Errorf("yaml kg byok = %+v, want {allowed,vertex,claude-haiku}", cell)
	}
}

// TestLoadConfig_NilModelAccess_OmittedFromYAML asserts the additive identity
// property: a Config with NO ModelAccess block marshals WITHOUT a modelAccess
// key (omitempty) and round-trips to a nil block — a pre-P3 daemon.yaml is
// byte-unchanged and behaves exactly as today.
func TestLoadConfig_NilModelAccess_OmittedFromYAML(t *testing.T) {
	cfg := &Config{
		Machine:      MachineConfig{ID: "m1"},
		Capacity:     CapacityConfig{MaxConcurrentSessions: 1},
		Orchestrator: OrchestratorConfig{URL: "https://example.test"},
		AutoUpdate:   AutoUpdateConfig{Channel: ChannelStable, Schedule: ScheduleNightly, DrainTimeoutSeconds: 600},
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if containsKey(data, "modelAccess") {
		t.Errorf("nil ModelAccess emitted a modelAccess key; want omitempty:\n%s", data)
	}

	var back Config
	if err := yaml.Unmarshal(data, &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if back.ModelAccess != nil {
		t.Errorf("ModelAccess = %+v, want nil after nil round-trip", back.ModelAccess)
	}
}
