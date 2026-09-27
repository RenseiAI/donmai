package afcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/daemon"
)

// The actual child detail-fetch path must be usable before the first polled
// launch. Holding OnPreSpawn keeps the admitted detail alive for observation;
// no process-start timing or larger fetch retry budget makes this assertion pass.
func TestAgentDetailFetchFollowsExplicitControlReadiness(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_FORCE_STUB", "0")
	t.Setenv("DONMAI_MACHINE_ID", "fe258277-6401-4d79-82c9-4aa8315d114c")
	var polls atomic.Int32
	polled := make(chan struct{}, 1)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/register"):
			_ = json.NewEncoder(w).Encode(map[string]any{"workerId": "bootstrap-worker", "runtimeToken": "synthetic-runtime", "heartbeatInterval": 30000, "pollInterval": 30000})
		case strings.HasSuffix(r.URL.Path, "/poll"):
			work := []any{}
			if polls.Add(1) == 1 {
				polled <- struct{}{}
				work = append(work, map[string]any{"sessionId": "bootstrap-session", "projectId": "fixture", "projectName": "fixture", "workType": "development", "promptContext": "bootstrap fixture", "requiresRepository": false})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"work": work})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"acknowledged": true})
		}
	}))
	t.Cleanup(peer.Close)
	dir := t.TempDir()
	cfg := daemon.DefaultConfig()
	cfg.ProjectAdmissionVersion = 2
	cfg.EnabledProjectIDs = []string{"fixture"}
	cfg.Orchestrator.URL = peer.URL
	cfg.Orchestrator.AuthToken = "rsp_live_bootstrap_fixture"
	configPath := filepath.Join(dir, "daemon.yaml")
	if err := daemon.WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	inSpawn := make(chan struct{})
	releaseSpawn := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSpawn) }) }
	d := daemon.New(daemon.Options{ConfigPath: configPath, JWTPath: filepath.Join(dir, "daemon.jwt"), SkipWizard: true, OrchestratorHTTPClient: peer.Client(), SessionShim: daemon.SessionShimConfig{RegistryDir: filepath.Join(dir, "shims")}, SpawnerOptions: daemon.SpawnerOptions{
		WorkerCommand: []string{"/bin/sh", "-c", ":"}, WorktreeParentDir: filepath.Join(dir, "worktrees"),
		OnPreSpawn: func(_ daemon.SessionSpec, env []string) ([]string, error) {
			close(inSpawn)
			<-releaseSpawn
			return env, nil
		},
	}})
	srv := daemon.NewServer(d)
	serverErrors, err := srv.StartBeforeDaemon()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.Stop(ctx)
		_ = srv.Shutdown(ctx)
		<-serverErrors
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-polled:
		t.Fatal("first work claimed while control HTTP still returns503")
	case <-time.After(100 * time.Millisecond):
	}
	srv.DaemonStarted()
	select {
	case <-inSpawn:
	case <-ctx.Done():
		t.Fatal("ready control server did not admit first poll")
	}
	detail, err := fetchSessionDetail(ctx, peer.Client(), "http://"+srv.Addr(), "bootstrap-session", "")
	if err != nil {
		t.Fatalf("actual bootstrap detail fetch failed: %v", err)
	}
	if detail.SessionID != "bootstrap-session" || detail.WorkerID != "bootstrap-worker" {
		t.Fatalf("wrong admitted detail: %+v", detail)
	}
	release()
}
