package codex

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/activityrelay"
)

// drainActivityFinalFlushResult reads Events until the terminal ResultEvent,
// collecting harness-lifecycle moments. It fails on timeout.
func drainActivityFinalFlushResult(t *testing.T, h *activityHandle) (moments []string, terminal *agent.ResultEvent) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events channel closed before a terminal ResultEvent")
			}
			if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeHarnessLifecycle {
				moments = append(moments, sys.Message)
			}
			if result, ok := ev.(agent.ResultEvent); ok {
				cp := result
				terminal = &cp
				return moments, terminal
			}
		case <-deadline:
			t.Fatal("timed out waiting for the terminal ResultEvent")
		}
	}
}

// TestSpawnInteractive_ActivityStopFlushReachesTerminal pins the relay
// ownership fix: hook moments written as the child exits must reach Events
// before the terminal ResultEvent even though ptycli runs the session
// cleanup concurrently with the final flush. The fake fires the installed
// Stop telemetry command as it exits, so no per-tick sweep can deliver it,
// and the seam below holds the final relay sweep back so the cleanup always
// finishes first. Without the fix the final sweep finds the events file
// gone and the stop moment is lost; with the fix the wrapped handle removes
// the relay directory only after that final flush has read it.
func TestSpawnInteractive_ActivityStopFlushReachesTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	clearInteractiveCodexAuthEnv(t)
	finalActivitySweepDelay.Store(int64(300 * time.Millisecond))
	t.Cleanup(func() { finalActivitySweepDelay.Store(0) })

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

	var captured *activityrelay.Relay
	newRelayFn := func() (*activityrelay.Relay, error) {
		r, err := activityrelay.New()
		if err == nil {
			captured = r
		}
		return r, err
	}

	// The fake idles on a gate, then fires the installed Stop command the
	// test leaves in stop-cmd and exits at once, the way the CLI fires Stop
	// on its way out. The relay line is written before the child exits, but
	// after the last per-tick sweep the tailer could have run.
	bin := writeFakeCodexScript(t, "while [ ! -e \"$PWD/gate\" ]; do sleep 0.05; done\n/bin/sh -c \"$(cat \"$PWD/stop-cmd\")\"\n")

	h, err := SpawnInteractive(context.Background(), Options{
		CodexBin:                      bin,
		configTempDir:                 boundaryRoot,
		HandshakeTimeout:              10 * time.Second,
		RPCTimeout:                    5 * time.Second,
		interactiveMCPInventoryRunner: inventoryRunnerFor(t, mcpServers),
		interactiveAuthSeeder: func(_ context.Context, _ string, ownedHome string, _ interactiveCodexAuthProjection) error {
			return os.WriteFile(filepath.Join(ownedHome, codexAuthFileName), []byte(`{"auth_mode":"apikey"}`), 0o600)
		},
		interactiveActivityRelayNew: newRelayFn,
		interactiveBeforePTYSpawn: func(_ context.Context, _ string, spec agent.Spec, _ interactiveLaunch, _ string) error {
			if captured == nil {
				return nil
			}
			return os.WriteFile(filepath.Join(spec.Cwd, "stop-cmd"), []byte(captured.Command(activityrelay.EventStop)), 0o600)
		},
	}, agent.Spec{
		Cwd:         workdir,
		MCPServers:  mcpServers,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
		Env: map[string]string{
			"OPENAI_API_KEY": "fixture-key",
		},
	})
	if err != nil {
		t.Fatalf("SpawnInteractive: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	wrapper, ok := h.(*activityHandle)
	if !ok {
		t.Fatalf("SpawnInteractive returned %T, want *activityHandle", h)
	}
	if captured == nil {
		t.Fatal("activity relay was not created for the platform seat")
	}
	if _, err := os.Stat(filepath.Join(workdir, "stop-cmd")); err != nil {
		t.Fatalf("stop-cmd was not staged before PTY spawn: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "gate"), []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	moments, terminal := drainActivityFinalFlushResult(t, wrapper)
	if terminal == nil || !terminal.Success {
		t.Fatalf("terminal ResultEvent = %+v, want success", terminal)
	}
	found := false
	for _, m := range moments {
		if m == agent.HarnessLifecycleStop {
			found = true
		}
	}
	if !found {
		t.Fatalf("harness_lifecycle moments = %v, want stop before the terminal", moments)
	}
	select {
	case <-wrapper.ActivityFlushed():
	default:
		t.Fatal("terminal ResultEvent arrived before ActivityFlushed closed")
	}
}
