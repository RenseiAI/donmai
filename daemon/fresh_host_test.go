package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServiceModeStdinSkipsWizardWithDevNullStdin pins the crash-loop root
// cause: the old mode-bit TTY test read /dev/null as a terminal (/dev/null
// IS a character device), so a service-mode start whose stdin is /dev/null
// ran the interactive wizard and failed on its first required answer.
// ShouldSkipWizard must report skip when stdin is /dev/null.
func TestServiceModeStdinSkipsWizardWithDevNullStdin(t *testing.T) {
	devNull, err := os.Open("/dev/null")
	if err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	defer func() { _ = devNull.Close() }()
	previous := os.Stdin
	os.Stdin = devNull
	t.Cleanup(func() { os.Stdin = previous })

	if !ServiceModeStdin() {
		t.Fatal("ServiceModeStdin() with stdin /dev/null = false, want true: the service-mode start would run the interactive wizard")
	}
	if !ShouldSkipWizard() {
		t.Fatal("ShouldSkipWizard() with stdin /dev/null = false, want true")
	}
}

// TestServiceModeStdinDetectsRegularFile pins the other non-terminal shape:
// a redirected regular file is service-mode stdin too.
func TestServiceModeStdinDetectsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte("answers\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	previous := os.Stdin
	os.Stdin = file
	t.Cleanup(func() { os.Stdin = previous })

	if !ServiceModeStdin() {
		t.Fatal("ServiceModeStdin() with a regular-file stdin = false, want true")
	}
}

// TestFreshHostConfigLoads pins the fresh-host seed: the config `host
// install` writes and the non-interactive first-run path starts from must
// load under validateConfig. A seed that does not load reopens the
// crash loop it was meant to close.
func TestFreshHostConfigLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := WriteConfig(path, FreshHostConfig()); err != nil {
		t.Fatalf("write fresh-host config: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("fresh-host seed does not load: %v", err)
	}
	if loaded.APIVersion != LocalRuntimeConfigAPIVersion {
		t.Errorf("seed apiVersion = %q, want %q", loaded.APIVersion, LocalRuntimeConfigAPIVersion)
	}
	if !strings.HasPrefix(loaded.Orchestrator.URL, "file://") {
		t.Errorf("seed orchestrator URL = %q, want the local file queue", loaded.Orchestrator.URL)
	}
	if err := ValidateLocalExecutionSecurity(loaded); err != nil {
		t.Errorf("seed carries no explicit execution policy: %v", err)
	}
	if loaded.LocalRuntime == nil || loaded.LocalRuntime.Harness != "" || len(loaded.LocalRuntime.Repositories) != 0 {
		t.Errorf("seed must carry no harness or repository profile (setup adds those): %+v", loaded.LocalRuntime)
	}
}

// TestNonInteractiveFirstRunSeedsFreshHostConfig drives the production
// first-run path: with no config on disk and the wizard skipped, the
// seeded config must persist and load. It fails when the seed is not
// written (the start falls back to an in-memory default the next restart
// cannot see) or when the written file does not load.
func TestNonInteractiveFirstRunSeedsFreshHostConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	existing, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load absent config: %v", err)
	}
	if existing != nil {
		t.Fatal("absent config loaded non-nil")
	}
	cfg, err := RunSetupWizard(WizardOptions{ConfigPath: path, SkipWizard: true})
	if err != nil {
		t.Fatalf("non-interactive first run: %v", err)
	}
	if cfg == nil || !strings.HasPrefix(cfg.Orchestrator.URL, "file://") {
		t.Fatalf("first run did not seed the file queue: %+v", cfg)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("seeded config does not load: %v", err)
	}
	if loaded.Orchestrator.URL != cfg.Orchestrator.URL {
		t.Errorf("persisted URL = %q, want %q", loaded.Orchestrator.URL, cfg.Orchestrator.URL)
	}
}

// TestFreshHostSeedRefusesWithoutProfile drives the production Start gate:
// a seeded config with no harness or repository profile and no shipped
// composition must refuse with the operator action, not an internal
// wiring complaint and not a crash-loop restart.
func TestFreshHostSeedRefusesWithoutProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	if err := WriteConfig(path, FreshHostConfig()); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	d := New(Options{
		ConfigPath: path,
		JWTPath:    filepath.Join(dir, "daemon.jwt"),
		SkipWizard: true,
		HTTPHost:   "127.0.0.1",
		HTTPPort:   0,
	})
	if err := d.Start(t.Context()); err == nil {
		_ = d.Stop(t.Context())
		t.Fatal("seeded start without a profile succeeded; want the setup refusal")
	} else if !strings.Contains(err.Error(), "host setup") {
		t.Fatalf("seeded start error = %q, want the `host setup` operator action", err)
	}
}
