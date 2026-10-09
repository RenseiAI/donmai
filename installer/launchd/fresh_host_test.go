package launchd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

// TestEnsureFreshHostConfigSeedsLoadableConfig pins the macOS install half
// of the fresh-host contract: with no config on disk, the seed is written
// and the written file loads, so the agent-mode start never runs the
// interactive wizard.
func TestEnsureFreshHostConfigSeedsLoadableConfig(t *testing.T) {
	home := t.TempDir()
	priorHome := statehome.BaseHome()
	statehome.SetBaseHome(home)
	t.Cleanup(func() { statehome.SetBaseHome(priorHome) })
	t.Setenv("HOME", home)

	path := filepath.Join(t.TempDir(), "daemon.yaml")
	seeded, err := EnsureFreshHostConfig(path)
	if err != nil {
		t.Fatalf("EnsureFreshHostConfig: %v", err)
	}
	if !seeded {
		t.Fatal("seeded = false on a host with no config, want true")
	}
	loaded, err := daemon.LoadConfig(path)
	if err != nil {
		t.Fatalf("seeded config does not load: %v", err)
	}
	if loaded.APIVersion != daemon.LocalRuntimeConfigAPIVersion {
		t.Errorf("seed apiVersion = %q, want %q", loaded.APIVersion, daemon.LocalRuntimeConfigAPIVersion)
	}
	if !strings.HasPrefix(loaded.Orchestrator.URL, "file://") {
		t.Errorf("seed URL = %q, want the local file queue", loaded.Orchestrator.URL)
	}
}

// TestEnsureFreshHostConfigLeavesExistingConfigAlone pins that install
// validates an operator config and leaves its bytes untouched.
func TestEnsureFreshHostConfigLeavesExistingConfigAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	before := "apiVersion: donmai.dev/v1\nkind: LocalDaemon\nmachine:\n  id: operator-host\ncapacity:\n  maxConcurrentSessions: 2\norchestrator:\n  url: http://127.0.0.1:1\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	seeded, err := EnsureFreshHostConfig(path)
	if err != nil {
		t.Fatalf("EnsureFreshHostConfig on an operator config: %v", err)
	}
	if seeded {
		t.Fatal("seeded = true over an existing config, want false")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Fatalf("install rewrote the operator config:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

// TestEnsureFreshHostConfigRefusesMalformedConfig pins the fail-loud half:
// a config the daemon cannot load fails the install with the path.
func TestEnsureFreshHostConfigRefusesMalformedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(path, []byte("machine: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureFreshHostConfig(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("EnsureFreshHostConfig(malformed) = %v, want an error naming the config", err)
	}
}

// TestEnsureFreshHostConfigRefusesEmptyConfig pins the empty-file half: an
// empty file loads as nil (no config) rather than failing, which would let
// the unit run the wizard. Install refuses it loudly instead.
func TestEnsureFreshHostConfigRefusesEmptyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureFreshHostConfig(path); err == nil {
		t.Fatal("EnsureFreshHostConfig(empty) succeeded; want a loud refusal")
	}
}
