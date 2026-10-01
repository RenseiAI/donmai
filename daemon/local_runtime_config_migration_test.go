package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLocalPolicyStartupMigrationPreservesAuthoredValues(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	original := []byte("# keep this comment\napiVersion: donmai.dev/v1\nkind: LocalDaemon\nmachine:\n  id: configured-host\ncapacity:\n  maxConcurrentSessions: 0\norchestrator:\n  url: file:///tmp/local-policy-queue\n  authToken: '${UNRESOLVED_FIXTURE_TOKEN}'\nextraAuthoring:\n  keep: [one, two]\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := PrepareLocalRuntimeConfig(path)
	if err != nil || !changed {
		t.Fatalf("known-version upgrade=%v,%v", changed, err)
	}
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err = yaml.Unmarshal(migrated, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["apiVersion"] != LocalRuntimeConfigAPIVersion {
		t.Fatal("migration did not record version2")
	}
	if raw["capacity"].(map[string]any)["maxConcurrentSessions"] != 0 {
		t.Fatal("migration rewrote explicit zero capacity")
	}
	if raw["orchestrator"].(map[string]any)["authToken"] != "${UNRESOLVED_FIXTURE_TOKEN}" {
		t.Fatal("migration substituted unrelated credential data")
	}
	if !strings.Contains(string(migrated), "keep this comment") || len(raw["extraAuthoring"].(map[string]any)["keep"].([]any)) != 2 {
		t.Fatal("migration lost unrelated authoring")
	}
	var cfg Config
	if err = yaml.Unmarshal(migrated, &cfg); err != nil {
		t.Fatal(err)
	}
	if err = ValidateLocalExecutionSecurity(&cfg); err != nil {
		t.Fatal(err)
	}
	if *cfg.LocalRuntime.ExecutionSecurity != *InitialLocalExecutionSecurity() {
		t.Fatal("migration did not persist the visible literal seed")
	}
	changed, err = PrepareLocalRuntimeConfig(path)
	if err != nil || changed {
		t.Fatalf("repeated upgrade=%v,%v", changed, err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(migrated, again) {
		t.Fatal("idempotent startup rewrote current policy")
	}
}

func TestLocalPolicyStartupDoesNotWriteUnknownMissingOrMalformed(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"", "apiVersion: unknown/v8\n", "apiVersion: donmai.dev/v2\n", "apiVersion: [donmai.dev/v1]\n"} {
		t.Run(strings.TrimSpace(version), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "daemon.yaml")
			raw := []byte(version + "machine: {id: host}\norchestrator: {url: 'file:///tmp/local-policy-queue'}\n")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if changed, err := PrepareLocalRuntimeConfig(path); err == nil || changed {
				t.Fatalf("unsafe schema was seeded: changed=%v err=%v", changed, err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, after) {
				t.Fatal("refused startup altered configuration")
			}
		})
	}
}

func TestLocalPolicyMigrationLeavesRemoteAndAuthoredPolicyAlone(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"remote", "authored"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "daemon.yaml")
			cfg := &Config{APIVersion: "donmai.dev/v1", Machine: MachineConfig{ID: "host"}, Orchestrator: OrchestratorConfig{URL: "https://controller.example"}}
			if mode == "authored" {
				cfg.Orchestrator.URL = "file:///tmp/local-policy-queue"
				cfg.LocalRuntime = &LocalRuntimeConfig{ExecutionSecurity: InitialLocalExecutionSecurity()}
				cfg.LocalRuntime.ExecutionSecurity.Network = "none"
			}
			raw, err := yaml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			changed, err := PrepareLocalRuntimeConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "remote" {
				if changed || !bytes.Equal(raw, after) {
					t.Fatal("remote configuration changed")
				}
				return
			}
			var actual Config
			if err = yaml.Unmarshal(after, &actual); err != nil {
				t.Fatal(err)
			}
			if !changed || actual.LocalRuntime.ExecutionSecurity.Network != "none" {
				t.Fatal("migration weakened authored policy")
			}
		})
	}
}
