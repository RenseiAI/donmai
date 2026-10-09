package freshhost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

// TestEnsureConfigSeedsLoadableConfig pins the shared seed: with no config
// on disk the seed is written and loads, so a service-mode start never
// runs the interactive wizard.
func TestEnsureConfigSeedsLoadableConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	seeded, err := EnsureConfig(path, "systemd")
	if err != nil {
		t.Fatalf("EnsureConfig: %v", err)
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

// TestEnsureConfigRefusesEmptyConfig pins the empty-file half: an empty
// file loads as nil (no config) rather than failing, which would let the
// service run the wizard. The helper refuses it loudly instead.
func TestEnsureConfigRefusesEmptyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureConfig(path, "systemd"); err == nil {
		t.Fatal("EnsureConfig(empty) succeeded; want a loud refusal")
	}
}
