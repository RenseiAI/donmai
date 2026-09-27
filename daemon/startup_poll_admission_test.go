package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Hold the real Start's final readiness publication, not registration or an
// invented polling mock. A non-shim daemon must not send its immediate first
// claim request until that publication has completed.
func startupPollFixture(t *testing.T) (*Daemon, context.Context, context.CancelFunc, func(), <-chan error, <-chan State) {
	t.Helper()
	t.Setenv("DONMAI_DAEMON_FORCE_STUB", "0")
	t.Setenv("DONMAI_MACHINE_ID", "f9834001-a009-4c53-b4c3-b6c65973eb89")
	var daemon *Daemon
	held := make(chan struct{})
	heartbeat := make(chan struct{})
	polls := make(chan State, 8)
	var beatOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workers/register":
			// Start already owns its lifecycle lease. Prevent only the final
			// metadata publication, while allowing the HTTP reply and all setup.
			daemon.lifecycleMu.Lock()
			close(held)
			_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "startup-worker", "runtimeToken": "synthetic-runtime", "heartbeatInterval": 30000, "pollInterval": 30000})
		case "/api/workers/startup-worker/heartbeat":
			beatOnce.Do(func() { close(heartbeat) })
			_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
		case "/api/workers/startup-worker/poll":
			polls <- daemon.State()
			_ = json.NewEncoder(w).Encode(map[string]any{"work": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "daemon.yaml")
	cfg := DefaultConfig()
	cfg.Machine.ID = "startup-test"
	cfg.Orchestrator.URL = server.URL
	cfg.Orchestrator.AuthToken = "rsp_live_startup_fixture"
	if err := WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	daemon = New(Options{ConfigPath: configPath, JWTPath: filepath.Join(dir, "daemon.jwt"), SkipWizard: true, OrchestratorHTTPClient: server.Client()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	started := make(chan error, 1)
	go func() { started <- daemon.Start(ctx) }()
	select {
	case <-held:
	case err := <-started:
		t.Fatalf("Start ended before barrier: %v", err)
	case <-ctx.Done():
		t.Fatal("registration did not hold readiness barrier")
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { daemon.lifecycleMu.Unlock() }) }
	t.Cleanup(func() { release(); _ = daemon.Stop(context.Background()) })
	select {
	case <-heartbeat:
	case <-ctx.Done():
		t.Fatal("setup did not start heartbeat behind readiness barrier")
	}

	return daemon, ctx, cancel, release, started, polls
}

func TestDaemonFirstPollFollowsStartupReadiness(t *testing.T) {
	_, ctx, _, release, started, polls := startupPollFixture(t)
	// Readiness is deterministically held for this negative observation. This
	// deadline bounds a forbidden request check; it is not a startup sleep or
	// a larger deadline for the original smoke's admission assertions.
	select {
	case state := <-polls:
		t.Fatalf("first poll claimed before final readiness publication: state=%s", state)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Start did not complete after readiness release")
	}
	select {
	case state := <-polls:
		if state != StateRunning {
			t.Fatalf("first poll state=%s, want running", state)
		}
	case <-ctx.Done():
		t.Fatal("no immediate first poll after readiness")
	}
}

func TestCanceledStartupDoesNotStartPoller(t *testing.T) {
	d, _, cancel, release, started, polls := startupPollFixture(t)
	cancel()
	release()
	select {
	case err := <-started:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error=%v, want canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled Start did not finish")
	}
	if d.poller == nil || d.poller.IsRunning() {
		t.Fatal("canceled initialization left an active poller")
	}
	select {
	case state := <-polls:
		t.Fatalf("canceled startup polled in state %s", state)
	default:
	}
}

func TestStopJoiningStartupLeavesPollerStopped(t *testing.T) {
	d, ctx, _, release, started, _ := startupPollFixture(t)
	stopped := make(chan error, 1)
	go func() { stopped <- d.Stop(ctx) }()
	release()
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Start did not finish")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Stop did not join startup")
	}
	if d.State() != StateStopped || d.poller == nil || d.poller.IsRunning() {
		t.Fatal("Stop completed with a late or active poller")
	}
}
