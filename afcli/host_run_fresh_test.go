package afcli

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

// TestHostRunFreshHostRefusesWithSetupAction drives the production `host
// run` entry point on a fresh host: no config on disk, stdin /dev/null as
// the unit provides, no skip flag. The run must never open the interactive
// wizard — it seeds the fresh-host config and refuses with the `host
// setup` operator action. It fails when the wizard runs (the crash-loop
// signature: a prompt error about the orchestrator URL) or when the
// refusal does not name the fix.
func TestHostRunFreshHostRefusesWithSetupAction(t *testing.T) {
	home := t.TempDir()
	priorHome := statehome.BaseHome()
	statehome.SetBaseHome(home)
	t.Cleanup(func() { statehome.SetBaseHome(priorHome) })
	t.Setenv("HOME", home)
	t.Setenv("DONMAI_ORCHESTRATOR_URL", "")
	// Force the stub registration path: without it the run dials the
	// (empty) orchestrator URL first and the test waits on the network
	// instead of reaching the fresh-host refusal. Same opt-in the
	// skipped-wizard stub test uses.
	t.Setenv("DONMAI_DAEMON_FORCE_STUB", "1")

	devNull, err := os.Open("/dev/null")
	if err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	defer func() { _ = devNull.Close() }()
	previousStdin := os.Stdin
	os.Stdin = devNull
	t.Cleanup(func() { os.Stdin = previousStdin })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(home, "daemon.yaml")
	cmd := newDaemonRunCmd(Config{HostBinaryVersion: "test"})
	cmd.SetArgs([]string{"--config", configPath, "--port", strconv.Itoa(port), "--standalone-creds=off"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	err = cmd.Execute()
	if err == nil {
		t.Fatalf("fresh-host run succeeded without a harness profile; want the setup refusal. output=%.500s", output.String())
	}
	if !strings.Contains(err.Error(), "host setup") {
		t.Fatalf("fresh-host run error = %q, want the `host setup` operator action (output=%.500s)", err, output.String())
	}
	if code, ok := NeedsSetupExitCode(err); !ok || code != 4 {
		t.Fatalf("NeedsSetupExitCode(%v) = (%d, %v), want (4, true): the refusal must exit with the no-restart setup status", err, code, ok)
	}
	if strings.Contains(err.Error(), "first-run setup") || strings.Contains(output.String(), "Machine ID") {
		t.Fatalf("fresh-host run opened the interactive wizard: err=%q (output=%.500s)", err, output.String())
	}
	// The seed persists: the next start (and the operator's setup) sees
	// the same file the refusal was computed from.
	loaded, loadErr := daemon.LoadConfig(configPath)
	if loadErr != nil {
		t.Fatalf("seeded config does not load: %v", loadErr)
	}
	if !strings.HasPrefix(loaded.Orchestrator.URL, "file://") {
		t.Fatalf("seeded URL = %q, want the local file queue", loaded.Orchestrator.URL)
	}
}

// TestFreshHostCompositionReturnsSeedWithoutProfile pins the composition
// refusal arm on its own: a seed with no harness or repository profile
// returns the queue root with no provider view, so the entry point
// refuses with the setup action instead of an internal wiring
// complaint. Reverting the arm to the old wiring error fails this test
// while the Start-gate test stays green — the two arms are pinned
// independently.
func TestFreshHostCompositionReturnsSeedWithoutProfile(t *testing.T) {
	queue := filepath.Join(t.TempDir(), "queue")
	cfg := daemon.DefaultConfig()
	cfg.APIVersion = daemon.LocalRuntimeConfigAPIVersion
	cfg.Orchestrator.URL = "file://" + queue
	cfg.Orchestrator.AuthToken = ""
	cfg.LocalRuntime = &daemon.LocalRuntimeConfig{ExecutionSecurity: daemon.InitialLocalExecutionSecurity()}
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := daemon.WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	options, providers, err := newLocalRuntimeComposition(path, nil, nil)
	if err != nil {
		t.Fatalf("seed composition: %v", err)
	}
	if options == nil || options.QueueRoot != queue {
		t.Fatalf("seed composition queue root = %+v, want %q", options, queue)
	}
	if providers != nil {
		t.Fatalf("seed composition providers = %v, want nil (no profile yet)", providers)
	}
}

// TestFreshHostCompositionServesConfiguredProfile is the positive
// control: a file-queue config WITH a harness/model/source profile
// returns the provider view, proving the arm above keys on the missing
// profile rather than the file scheme.
func TestFreshHostCompositionServesConfiguredProfile(t *testing.T) {
	queue := filepath.Join(t.TempDir(), "queue")
	cfg := daemon.DefaultConfig()
	cfg.APIVersion = daemon.LocalRuntimeConfigAPIVersion
	cfg.Orchestrator.URL = "file://" + queue
	cfg.Orchestrator.AuthToken = ""
	cfg.LocalRuntime = &daemon.LocalRuntimeConfig{
		Harness: "codex", Model: "test-model", ModelAuthor: "test-author",
		ExecutionSecurity: daemon.InitialLocalExecutionSecurity(),
		Repositories:      []daemon.LocalGitHubRepository{{RepositoryID: 42, OwnerRepo: "example/project", Label: "donmai", Ref: "main"}},
	}
	path := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := daemon.WriteConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	options, providers, err := newLocalRuntimeComposition(path, nil, map[string]string{})
	if err != nil {
		t.Fatalf("configured composition: %v", err)
	}
	if options == nil || providers == nil {
		t.Fatalf("configured composition = (%+v, %v), want both", options, providers)
	}
}
