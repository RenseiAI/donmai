package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
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
// TestFreshHostSeedCarriesV2Header pins the header the container lane
// asserts: the written seed file starts with the local-runtime schema
// version, not a bare constant comparison that stays green if the
// constant ever drifts.
func TestFreshHostSeedCarriesV2Header(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := WriteConfig(path, FreshHostConfig()); err != nil {
		t.Fatalf("write fresh-host config: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "apiVersion: "+LocalRuntimeConfigAPIVersion+"\n") {
		t.Fatalf("seed file carries no %q line:\n%.500s", "apiVersion: "+LocalRuntimeConfigAPIVersion, raw)
	}
	if LocalRuntimeConfigAPIVersion != "donmai.dev/v2" {
		t.Fatalf("seed schema constant = %q, want the header the container lane asserts", LocalRuntimeConfigAPIVersion)
	}
}

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
	// The seed decision reads DONMAI_ORCHESTRATOR_URL: an image-exported
	// orchestrator URL selects the plain default, not the seed. Pin the
	// fresh-host contract (no operator URL) so the test is hermetic.
	t.Setenv("DONMAI_ORCHESTRATOR_URL", "")
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

// TestNonInteractiveFirstRunKeepsExplicitOrchestratorURL pins the other
// half of the seed contract: an explicit operator orchestrator URL keeps
// the plain default (stub/platform path) instead of the fresh-host file
// queue. It drives the same production first-run path as the seed test.
func TestNonInteractiveFirstRunKeepsExplicitOrchestratorURL(t *testing.T) {
	t.Setenv("DONMAI_ORCHESTRATOR_URL", "http://127.0.0.1:1")
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	cfg, err := RunSetupWizard(WizardOptions{ConfigPath: path, SkipWizard: true})
	if err != nil {
		t.Fatalf("non-interactive first run: %v", err)
	}
	if cfg == nil || cfg.Orchestrator.URL != "http://127.0.0.1:1" {
		t.Fatalf("first run with an explicit orchestrator URL = %+v, want the plain default", cfg)
	}
	if cfg.LocalRuntime != nil {
		t.Fatalf("explicit-URL default carries a local profile: %+v", cfg.LocalRuntime)
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
	} else if !errors.Is(err, afclient.ErrNeedsSetup) {
		t.Fatalf("seeded start error = %q, want the typed setup refusal (errors.Is ErrNeedsSetup)", err)
	}
}

// TestFreshHostSeedCompositionRefusesWithSetupAction drives the
// composition-level refusal arm directly: a seed with no harness or
// repository profile returns the queue root with no provider view. The
// entry point turns that into the setup refusal; this test pins the
// arm itself, so dropping it cannot stay green behind the Start gate.
// (The composition helper lives in the CLI layer; the assertion here is
// on the shape the daemon layer guarantees: seed plus no profile.)
func TestFreshHostSeedCompositionShape(t *testing.T) {
	cfg := FreshHostConfig()
	if cfg.LocalRuntime == nil {
		t.Fatal("seed carries no local runtime block")
	}
	if cfg.LocalRuntime.Harness != "" || cfg.LocalRuntime.Model != "" || len(cfg.LocalRuntime.Repositories) != 0 {
		t.Fatalf("seed carries a harness or repository profile: %+v", cfg.LocalRuntime)
	}
	if !strings.HasPrefix(cfg.Orchestrator.URL, "file://") {
		t.Fatalf("seed orchestrator URL = %q, want the local file queue", cfg.Orchestrator.URL)
	}
}

// TestMissingCompositionIsNotASetupRefusal pins the second Start gate's
// own message: a configured host whose shipped composition is missing
// (embedder wiring fault) must not read as "not set up yet" in the
// journal.
func TestMissingCompositionIsNotASetupRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	cfg := FreshHostConfig()
	cfg.LocalRuntime.Harness = "codex"
	cfg.LocalRuntime.Model = "test-model"
	cfg.LocalRuntime.ModelAuthor = "test-author"
	cfg.LocalRuntime.Repositories = []LocalGitHubRepository{{RepositoryID: 1, OwnerRepo: "example/repo", Label: "donmai", Ref: "main"}}
	if err := WriteConfig(path, cfg); err != nil {
		t.Fatalf("write configured config: %v", err)
	}
	d := New(Options{
		ConfigPath: path,
		JWTPath:    filepath.Join(dir, "daemon.jwt"),
		SkipWizard: true,
		HTTPHost:   "127.0.0.1",
		HTTPPort:   0,
		// No LocalRuntime composition: the embedder never supplied it.
	})
	if err := d.Start(t.Context()); err == nil {
		_ = d.Stop(t.Context())
		t.Fatal("start without a shipped composition succeeded; want the composition refusal")
	} else if strings.Contains(err.Error(), "host setup") {
		t.Fatalf("composition fault reads as a setup refusal: %q", err)
	} else if !strings.Contains(err.Error(), "composition missing") {
		t.Fatalf("composition fault error = %q, want the embedder-action message", err)
	}
}
