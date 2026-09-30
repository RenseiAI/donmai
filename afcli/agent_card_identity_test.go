package afcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/daemon"
	providerstub "github.com/RenseiAI/donmai/provider/harness/stub"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runner"
	"github.com/RenseiAI/donmai/runtime/state"
	"github.com/RenseiAI/donmai/runtime/worktree"
)

// A standalone local work source can supply an explicit card through the
// public dispatch contract. Exercise its handle/client and detail/runner hops.
func TestStandaloneAgentCardPollHandleDetailAndRun(t *testing.T) {
	var item daemon.PollWorkItem
	if err := json.Unmarshal([]byte(`{"sessionId":"card-session","projectId":"local","body":"Review local work","agentCardId":"card-review","agentCardName":"Reviewer","resolvedProfile":{"provider":"stub"}}`), &item); err != nil {
		t.Fatal(err)
	}
	projects := []daemon.ProjectConfig{{ID: "local", Repository: "https://example.invalid/local.git"}}
	// The only controller is a foreground test fixture on an ephemeral port.
	// Its actual production server owns detail serialization.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"refreshed":true,"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	cfg := daemon.DefaultConfig()
	cfg.Orchestrator.URL = srv.URL
	cfg.Projects = projects
	configPath := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := daemon.WriteConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	d := daemon.New(daemon.Options{ConfigPath: configPath, JWTPath: filepath.Join(t.TempDir(), "daemon.jwt"), HTTPHost: "127.0.0.1", HTTPPort: 0, SkipWizard: true, SkipRegistration: true, SpawnerOptions: daemon.SpawnerOptions{WorktreeParentDir: t.TempDir(), WorkerCommand: []string{"/bin/sh", "-c", "sleep 5"}}})
	if err := d.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	server := daemon.NewServer(d)
	if _, err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	daemonURL := "http://" + server.Addr()
	spec := daemon.PollItemToSessionSpec(item, projects)
	detail := daemon.PollItemToSessionDetail(item, projects, srv.URL, "local-token", "local-worker")
	if _, err := d.AcceptWorkWithDetail(spec, detail); err != nil {
		t.Fatal(err)
	}
	handles, err := afclient.NewDaemonClientFromURL(daemonURL).GetSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(handles) != 1 || handles[0].AgentCardID != "card-review" || handles[0].AgentCardName != "Reviewer" {
		t.Fatalf("standalone handle lost card annotation: %+v", handles)
	}
	fetched, err := fetchSessionDetail(t.Context(), srv.Client(), daemonURL, item.SessionID, "")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := detailToQueuedWork(fetched)
	if err != nil {
		t.Fatal(err)
	}
	if queued.AgentCardID != "card-review" || queued.AgentCardName != "Reviewer" {
		t.Fatalf("queued work lost explicit card: %+v", queued.QueuedWork)
	}
	// Field-less older dispatches have no immutable raw sidecar; exercise the
	// compatibility converter as well as the admitted-payload path above.
	legacy := *fetched
	legacy.OperationalPayload = nil
	queued, err = detailToQueuedWork(&legacy)
	if err != nil {
		t.Fatal(err)
	}
	if queued.AgentCardID != "card-review" || queued.AgentCardName != "Reviewer" {
		t.Fatal("legacy detail conversion lost explicit card")
	}
	manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{PlatformURL: srv.URL, WorkerID: "local-worker", AuthToken: "local-token", HTTPClient: srv.Client(), BaseDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	registry := runner.NewRegistry()
	provider, err := providerstub.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(provider); err != nil {
		t.Fatal(err)
	}
	run, err := runner.New(runner.Options{Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: srv.Client(), SkipBackstop: true, SkipSteering: true, SkipPostSession: true, PreserveWorktreeAlways: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := run.Run(t.Context(), queued)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.NewStore().Read(res.WorktreePath)
	if err != nil {
		t.Fatal(err)
	}
	if st.AgentCardID != "card-review" || st.AgentCardName != "Reviewer" {
		t.Fatalf("real runner state lost dispatched card: %+v", st)
	}
	// The immutable dispatched annotation must not be replaced by a detail mirror.
	fetched.AgentCardName = "Other"
	if _, err := detailToQueuedWork(fetched); err == nil {
		t.Fatal("accepted card annotation that conflicts with immutable dispatch payload")
	}
}
