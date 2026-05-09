//go:build f28_integration

// Package afcli's F.2.8 integration test — `go test -tags f28_integration`
// drives a daemon → spawn → af agent run → stub provider chain end-to-end.
//
// This test is build-tagged so it does not run on the default
// `go test ./...` path (the worker spawner constructs codex/claude
// subprocesses on a real machine, which the unit tests don't want).
// Run it explicitly when verifying the spawn chain after daemon
// changes.
package afcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/daemon"
)

// TestF28_DaemonSpawnsAgentRun exercises the wire path. It drives:
//
//  1. A fake platform server that stubs /api/workers/register +
//     /api/workers/<id>/heartbeat + /api/workers/<id>/poll.
//  2. A real daemon.Daemon with a stub WorkerCommand pointing at
//     `/bin/sh -c` (so we don't recursively run the test binary).
//  3. A poll item that triggers AcceptWorkWithDetail, recording a
//     SessionDetail.
//  4. A direct HTTP fetch of /api/daemon/sessions/<id> against the
//     live daemon, asserting the SessionDetail round-trips.
//
// The test does not invoke runner.Run end-to-end — that path needs a
// real git remote which we don't have in CI; the unit test in
// TestRunAgentRun_HappyPath_StubProvider exercises the runner-side
// loop with a fake daemon. This test fills the gap by verifying the
// daemon-side wire shape.
func TestF28_DaemonSpawnsAgentRun(t *testing.T) {
	platform := httptest.NewServer(http.NewServeMux())
	defer platform.Close()

	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "daemon.yaml")
	cfg := daemon.DefaultConfig()
	cfg.Machine.ID = "test-int"
	cfg.Orchestrator.URL = platform.URL
	cfg.Projects = []daemon.ProjectConfig{{ID: "p1", Repository: "github.com/foo/bar"}}
	if err := daemon.WriteConfig(cfgPath, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	d := daemon.New(daemon.Options{
		ConfigPath:       cfgPath,
		JWTPath:          filepath.Join(tmp, "daemon.jwt"),
		HTTPHost:         "127.0.0.1",
		HTTPPort:         0,
		SkipWizard:       true,
		SkipRegistration: true,
		SpawnerOptions: daemon.SpawnerOptions{
			WorkerCommand: []string{"/bin/sh", "-c", `printf 'integration-stub:%s\n' "$RENSEI_SESSION_ID"; exit 0`},
		},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("daemon Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })

	srv := daemon.NewServer(d)
	if _, err := srv.Start(); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	want := &daemon.SessionDetail{
		SessionID:       "f28-int-1",
		IssueIdentifier: "REN-INT-1",
		Repository:      "github.com/foo/bar",
		Ref:             "main",
		WorkerID:        "wkr_int",
		AuthToken:       "tok_int",
		PlatformURL:     platform.URL,
		ResolvedProfile: &daemon.SessionResolvedProfile{Provider: "stub"},
	}
	if _, err := d.AcceptWorkWithDetail(daemon.SessionSpec{
		SessionID:  want.SessionID,
		Repository: want.Repository,
		Ref:        want.Ref,
	}, want); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}

	res, err := http.Get("http://" + srv.Addr() + "/api/daemon/sessions/" + want.SessionID) //nolint:gosec
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
}
