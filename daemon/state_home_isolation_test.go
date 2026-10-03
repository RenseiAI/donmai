package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/testisolation"
)

// TestStateHomeIsolationKeepsTestsOffTheRealHome is the guard for the
// TestMain isolation: it fails when the package's tests resolve any daemon
// state path — or write through the production write paths — outside the
// isolated temp home. Removing the TestMain isolation makes every
// assertion below fail against the developer's real home.
func TestStateHomeIsolationKeepsTestsOffTheRealHome(t *testing.T) {
	realHome := testisolation.RealHome()
	if realHome == "" {
		t.Skip("no resolvable real home to guard")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve test home: %v", err)
	}
	if home == realHome {
		t.Fatalf("test HOME %q is the real home %q: TestMain isolation is missing", home, realHome)
	}
	for name, p := range map[string]string{
		"daemon.yaml":   DefaultConfigPath(),
		"daemon.jwt":    DefaultJWTPath(),
		"session-shims": defaultShimRegistryDir(),
	} {
		if !strings.HasPrefix(p, home+string(os.PathSeparator)) {
			t.Errorf("state path %s = %q, want it under isolated HOME %q", name, p, home)
		}
		if p == filepath.Join(realHome, ".donmai", name) {
			t.Errorf("state path %s resolves to the real home: %q", name, p)
		}
	}

	// Exercise the production write paths and prove they land in the
	// temp home: a stub registration must cache its JWT at the default
	// path, and a restart preflight with no sessions must publish its
	// not-required audit into the default registry.
	d := New(Options{SkipRegistration: true})
	d.config = DefaultConfig()
	d.config.Machine.ID = "isolation-guard"
	if err := WriteConfig(DefaultConfigPath(), d.config); err != nil {
		t.Fatalf("write default config: %v", err)
	}
	if _, err := Register(context.Background(), RegistrationOptions{
		OrchestratorURL:   "http://127.0.0.1:1",
		RegistrationToken: "local-stub-no-token",
		Hostname:          "isolation-guard",
		Version:           Version,
		MaxAgents:         1,
	}); err != nil {
		t.Fatalf("stub registration: %v", err)
	}
	if _, err := os.Stat(DefaultJWTPath()); err != nil {
		t.Errorf("stub registration did not cache a JWT at the default path: %v", err)
	}

	d2 := New(Options{SkipRegistration: true})
	d2.spawner = NewWorkerSpawner(SpawnerOptions{})
	d2.setState(StateRunning)
	if _, err := d2.PrepareRestart(context.Background()); err != nil {
		t.Fatalf("restart preflight: %v", err)
	}
	if _, err := os.Stat(filepath.Join(defaultShimRegistryDir(), restartPreparationStateName)); err != nil {
		t.Errorf("restart preflight did not publish its audit into the default registry: %v", err)
	}
}
