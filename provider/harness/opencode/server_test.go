package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestFreeLoopbackPort(t *testing.T) {
	t.Parallel()
	p, err := freeLoopbackPort()
	if err != nil {
		t.Fatalf("freeLoopbackPort: %v", err)
	}
	if p <= 0 || p > 65535 {
		t.Errorf("port = %d, out of range", p)
	}
}

// startSleepChild starts a long-running child in its own process group and
// wires the reaper, returning a serveChild whose endpoint the caller sets.
func startSleepChild(t *testing.T, endpoint string) *serveChild {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found: %v", err)
	}
	cmd := exec.Command(sh, "-c", "sleep 30") //nolint:gosec // G204: test fixture; sh resolved via LookPath
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep child: %v", err)
	}
	c := &serveChild{cmd: cmd, endpoint: endpoint, logger: slog.Default(), done: make(chan struct{})}
	go c.reap()
	return c
}

func TestServeChild_WaitHealthy_Ready(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": true})
	}))
	defer srv.Close()

	child := startSleepChild(t, srv.URL)
	defer child.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := child.waitHealthy(ctx, ""); err != nil {
		t.Fatalf("waitHealthy: want nil (server healthy), got %v", err)
	}
}

func TestServeChild_WaitHealthy_EarlyExit(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found: %v", err)
	}
	// A child that exits immediately; endpoint points at a dead port so health
	// never succeeds — waitHealthy must fail fast on the early exit.
	cmd := exec.Command(sh, "-c", "exit 3") //nolint:gosec // G204: test fixture; sh resolved via LookPath
	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	child := &serveChild{cmd: cmd, endpoint: "http://127.0.0.1:1", logger: slog.Default(), done: make(chan struct{})}
	go child.reap()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = child.waitHealthy(ctx, "")
	if err == nil {
		t.Fatal("waitHealthy: want error on early exit, got nil")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Errorf("waitHealthy err: want ErrSpawnFailed, got %v", err)
	}
}

func TestServeChild_Stop_Teardown(t *testing.T) {
	t.Parallel()
	child := startSleepChild(t, "http://127.0.0.1:1")
	if child.exited() {
		t.Fatal("child reported exited before stop")
	}
	child.stop()
	if !child.exited() {
		t.Error("child not exited after stop")
	}
	// Idempotent.
	child.stop()
}
