package afcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/RenseiAI/donmai/daemon"
)

func TestSessionCoAuthorDetailRouteToQueuedWork(t *testing.T) {
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

	email := "contributor" + "@" + "example.invalid"
	cases := []struct {
		name, author, email string
		valid               bool
	}{
		{"valid", "Sample Contributor", email, true},
		{"absent", "", "", true},
		{"invalid", "Sample\nContributor", email, false},
		{"incomplete", "Sample Contributor", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"sessionId": "credit-" + tc.name, "projectId": "local", "coAuthorName": tc.author, "coAuthorEmail": tc.email})
			if err != nil {
				t.Fatal(err)
			}
			var item daemon.PollWorkItem
			if err := json.Unmarshal(raw, &item); err != nil {
				t.Fatal(err)
			}
			spec := daemon.PollItemToSessionSpec(item, projects)
			detail := daemon.PollItemToSessionDetail(item, projects, srv.URL, "local-token", "local-worker")
			if _, err := d.AcceptWorkWithDetail(spec, detail); err != nil {
				t.Fatal(err)
			}
			fetched, err := fetchSessionDetail(t.Context(), srv.Client(), daemonURL, item.SessionID, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, payload := range []bool{true, false} {
				if !payload {
					fetched.OperationalPayload = nil
				}
				queued, err := detailToQueuedWork(fetched)
				if err != nil {
					t.Fatal(err)
				}
				wantName, wantEmail := "", ""
				if tc.valid {
					wantName, wantEmail = tc.author, tc.email
				}
				if queued.CoAuthorName != wantName || queued.CoAuthorEmail != wantEmail {
					t.Errorf("queued work lost validated attribution (operational payload: %v)", payload)
				}
				encoded, err := json.Marshal(queued)
				if err != nil {
					t.Fatal(err)
				}
				var decoded map[string]any
				if err := json.Unmarshal(encoded, &decoded); err != nil {
					t.Fatal(err)
				}
				if wantName != "" && (decoded["coAuthorName"] != wantName || decoded["coAuthorEmail"] != wantEmail) {
					t.Error("queued work wire lost attribution")
				}
			}
		})
	}
	// A direct legacy detail must be validated too, even without poll intake.
	queued, err := detailToQueuedWork(&daemon.SessionDetail{SessionID: "direct", CoAuthorName: "Sample", CoAuthorEmail: "invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if queued.CoAuthorName != "" || queued.CoAuthorEmail != "" {
		t.Fatal("direct detail bypassed intake validation")
	}
}
