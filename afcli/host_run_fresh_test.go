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
