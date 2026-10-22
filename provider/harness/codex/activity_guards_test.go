package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/activityrelay"
)

// TestSpawnInteractive_ActivityRelayFailureFailsClosed pins B1 (relay leg):
// when the relay cannot be created, the spawn fails closed rather than
// launching with the hooks feature on and no telemetry.
func TestSpawnInteractive_ActivityRelayFailureFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named interactive sessions are unix-only")
	}
	clearInteractiveCodexAuthEnv(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	root := t.TempDir()
	boundaryRoot := filepath.Join(root, "session-boundaries")
	workdir := filepath.Join(root, "work")
	for _, dir := range []string{boundaryRoot, workdir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	mcpServers := []agent.MCPServerConfig{{
		Name: "donmai-platform",
		Type: "http",
		URL:  "https://platform.example.com/api/mcp/sess_project",
		Headers: map[string]string{
			"Authorization": "Bearer session-mcp-bearer",
		},
	}}
	spec := agent.Spec{
		SessionName: "chief-of-staff",
		Cwd:         workdir,
		MCPServers:  mcpServers,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Env: map[string]string{
			codexFakeNamedAppServerEnv:         "1",
			codexFakePTYClientCreatesThreadEnv: "1",
			"OPENAI_API_KEY":                   "fixture-key",
		},
	}
	_, err = SpawnInteractive(context.Background(), Options{
		CodexBin:                      self,
		configTempDir:                 boundaryRoot,
		HandshakeTimeout:              10 * time.Second,
		RPCTimeout:                    5 * time.Second,
		interactiveMCPInventoryRunner: inventoryRunnerFor(t, mcpServers),
		interactiveAuthSeeder: func(_ context.Context, _ string, ownedHome string, _ interactiveCodexAuthProjection) error {
			return os.WriteFile(filepath.Join(ownedHome, codexAuthFileName), []byte(`{"auth_mode":"apikey"}`), 0o600)
		},
		interactiveActivityRelayNew: func() (*activityrelay.Relay, error) {
			return nil, errors.New("relay unavailable (fixture)")
		},
	}, spec)
	if !errors.Is(err, agent.ErrSpawnFailed) || !strings.Contains(err.Error(), "install activity relay") {
		t.Fatalf("relay-failure spawn error = %v, want ErrSpawnFailed installing the relay (fail-closed)", err)
	}
	if entries, rerr := os.ReadDir(boundaryRoot); rerr != nil || len(entries) != 0 {
		t.Fatalf("relay-failure spawn leaked its private boundary: err=%v entries=%v", rerr, entries)
	}
}

// TestSpawnInteractive_ActivityAppendValidateFailureFailsClosed pins B1
// (validate leg): when the boundary fails validation during the hooks
// append (here the private home mode is widened after inventory, as the
// validator is meant to catch), the spawn fails closed rather than
// launching over a rejected boundary with the hooks feature on.
func TestSpawnInteractive_ActivityAppendValidateFailureFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named interactive sessions are unix-only")
	}
	clearInteractiveCodexAuthEnv(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	root := t.TempDir()
	boundaryRoot := filepath.Join(root, "session-boundaries")
	workdir := filepath.Join(root, "work")
	for _, dir := range []string{boundaryRoot, workdir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	mcpServers := []agent.MCPServerConfig{{
		Name: "donmai-platform",
		Type: "http",
		URL:  "https://platform.example.com/api/mcp/sess_project",
		Headers: map[string]string{
			"Authorization": "Bearer session-mcp-bearer",
		},
	}}
	baseRunner := inventoryRunnerFor(t, mcpServers)
	breakingRunner := func(ctx context.Context, binary, cwd string, env []string, configArgs, queryArgs []string) ([]byte, error) {
		body, err := baseRunner(ctx, binary, cwd, env, configArgs, queryArgs)
		if err != nil {
			return nil, err
		}
		// Widen the private home after a successful inventory readback, so
		// the subsequent hooks append fails its boundary validate().
		for _, entry := range env {
			if home, ok := strings.CutPrefix(entry, "CODEX_HOME="); ok && home != "" {
				_ = os.Chmod(home, 0o750) //nolint:gosec // fixture widens the home to trip the boundary validator
			}
		}
		return body, nil
	}
	spec := agent.Spec{
		SessionName: "chief-of-staff",
		Cwd:         workdir,
		MCPServers:  mcpServers,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Env: map[string]string{
			codexFakeNamedAppServerEnv:         "1",
			codexFakePTYClientCreatesThreadEnv: "1",
			"OPENAI_API_KEY":                   "fixture-key",
		},
	}
	_, err = SpawnInteractive(context.Background(), Options{
		CodexBin:                      self,
		configTempDir:                 boundaryRoot,
		HandshakeTimeout:              10 * time.Second,
		RPCTimeout:                    5 * time.Second,
		interactiveMCPInventoryRunner: breakingRunner,
		interactiveAuthSeeder: func(_ context.Context, _ string, ownedHome string, _ interactiveCodexAuthProjection) error {
			return os.WriteFile(filepath.Join(ownedHome, codexAuthFileName), []byte(`{"auth_mode":"apikey"}`), 0o600)
		},
	}, spec)
	if !errors.Is(err, agent.ErrSpawnFailed) || !strings.Contains(err.Error(), "install activity hooks") {
		t.Fatalf("validate-failure spawn error = %v, want ErrSpawnFailed installing the hooks (fail-closed)", err)
	}
	if entries, rerr := os.ReadDir(boundaryRoot); rerr != nil || len(entries) != 0 {
		t.Fatalf("validate-failure spawn leaked its private boundary: err=%v entries=%v", rerr, entries)
	}
}

// TestSpawnInteractive_GatewayStandaloneInheritCarriesNoHooksBlock pins C5:
// the spawn-side install gate (not just the argv gate) decides the hooks
// block. A gateway-routed standalone seat with DONMAI_CODEX_HOOKS=inherit
// keeps the hooks feature on in argv yet must carry no hooks block in its
// private config. Deleting the installActivityHooks gate (always appending)
// fails here.
func TestSpawnInteractive_GatewayStandaloneInheritCarriesNoHooksBlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	clearInteractiveCodexAuthEnv(t)
	t.Setenv(codexHooksEnv, codexHooksInherit)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "absent-codex-home"))
	workdir := t.TempDir()
	boundaryRoot := t.TempDir()
	bin := writeFakeCodexScript(t, `exit 0`)
	const baseURL = "https://gateway.example/codex/v1"
	spec := gatewayCellSpec(baseURL, "cell-key", "")
	spec.Cwd = workdir
	spec.Interactive = &agent.InteractiveSpec{Cols: 80, Rows: 24}
	var seenConfig string
	emptyInventory := func(_ context.Context, _ string, _ string, _ []string, _ []string, queryArgs []string) ([]byte, error) {
		for _, arg := range queryArgs {
			if arg == "list" {
				return []byte(`[]`), nil
			}
		}
		return nil, errors.New("unexpected inventory query")
	}
	_, err := SpawnInteractive(context.Background(), Options{
		CodexBin:                      bin,
		configTempDir:                 boundaryRoot,
		interactiveMCPInventoryRunner: emptyInventory,
		interactiveBeforePTYSpawn: func(_ context.Context, _ string, _ agent.Spec, _ interactiveLaunch, home string) error {
			raw, rerr := os.ReadFile(filepath.Join(home, codexConfigFileName))
			if rerr != nil {
				return rerr
			}
			seenConfig = string(raw)
			return errors.New("stop before PTY")
		},
	}, spec)
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("hook-aborted spawn error = %v, want ErrSpawnFailed", err)
	}
	if strings.Contains(seenConfig, "[[hooks.") {
		t.Fatalf("gateway standalone config carries a hooks block, want none:\n%s", seenConfig)
	}
}

// TestEmitActivityEventReservesCoarseSlots pins C6: relay events never fill
// the slots reserved for the coarse Init/Result contract, so the blocking
// coarse sends complete even when nobody drains Events.
func TestEmitActivityEventReservesCoarseSlots(t *testing.T) {
	t.Parallel()
	ch := make(chan agent.Event, activityCoarseEventSlots+activityRelayEventSlots)
	moment := agent.HarnessLifecycleEvent(agent.HarnessLifecycleStop)
	for i := 0; i < activityCoarseEventSlots+activityRelayEventSlots; i++ {
		emitActivityEvent(ch, moment)
	}
	if got := len(ch); got != activityRelayEventSlots {
		t.Fatalf("relay fill = %d events, want %d (coarse slots reserved)", got, activityRelayEventSlots)
	}
	// The two reserved slots still fit without blocking.
	for i := 0; i < activityCoarseEventSlots; i++ {
		select {
		case ch <- agent.InitEvent{}:
		default:
			t.Fatalf("coarse slot %d blocked with %d relay events buffered", i, len(ch))
		}
	}
	if got := len(ch); got != cap(ch) {
		t.Fatalf("channel holds %d events, want full %d", got, cap(ch))
	}
}

// TestSpawnInteractive_UnnamedActivitySeatWrapsActivityHandle pins C9: the
// unnamed path carries telemetry too. Deleting its wrap (returning the bare
// conversation handle) fails the type assertion here.
func TestSpawnInteractive_UnnamedActivitySeatWrapsActivityHandle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	clearInteractiveCodexAuthEnv(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	root := t.TempDir()
	boundaryRoot := filepath.Join(root, "session-boundaries")
	workdir := filepath.Join(root, "work")
	for _, dir := range []string{boundaryRoot, workdir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	mcpServers := []agent.MCPServerConfig{{
		Name: "donmai-platform",
		Type: "http",
		URL:  "https://platform.example.com/api/mcp/sess_project",
		Headers: map[string]string{
			"Authorization": "Bearer session-mcp-bearer",
		},
	}}
	spec := agent.Spec{
		// No SessionName: the unnamed bare-TUI path.
		Cwd:         workdir,
		MCPServers:  mcpServers,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Env: map[string]string{
			codexFakePTYClientEnv: "1",
			"OPENAI_API_KEY":      "fixture-key",
		},
	}
	h, err := SpawnInteractive(context.Background(), Options{
		CodexBin:                      self,
		configTempDir:                 boundaryRoot,
		HandshakeTimeout:              10 * time.Second,
		RPCTimeout:                    5 * time.Second,
		interactiveMCPInventoryRunner: inventoryRunnerFor(t, mcpServers),
		interactiveAuthSeeder: func(_ context.Context, _ string, ownedHome string, _ interactiveCodexAuthProjection) error {
			return os.WriteFile(filepath.Join(ownedHome, codexAuthFileName), []byte(`{"auth_mode":"apikey"}`), 0o600)
		},
	}, spec)
	if err != nil {
		t.Fatalf("SpawnInteractive unnamed: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	wrapper, ok := h.(*activityHandle)
	if !ok {
		t.Fatalf("unnamed SpawnInteractive returned %T, want *activityHandle", h)
	}
	var _ agent.InteractiveCapable = wrapper
	var _ agent.InteractiveTerminalCostSource = wrapper
	var _ agent.ContextUsageSourcer = wrapper
	home, _ := wrapper.ContextUsageRollout()
	if home == "" {
		t.Fatal("unnamed activity handle reports no home")
	}
	// Drain to the terminal so the session cleans up.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case _, ok := <-wrapper.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for unnamed session end")
		}
	}
}
