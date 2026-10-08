package afcli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/daemon"
)

// startBootstrapDaemon starts a real daemon + control server for the
// worker-bootstrap tests below. Sessions are seeded in-process (the HTTP
// accept route is a mutating route); the returned cleanup stops both.
//
// The daemon runs with the control gate enforced so the bootstrap read
// follows the production credential path: the per-session read
// credential for the full detail, redaction for anything else.
func startBootstrapDaemon(t *testing.T) (*daemon.Daemon, *daemon.Server, func()) {
	t.Helper()
	tmp := t.TempDir()
	cfg := daemon.DefaultConfig()
	cfg.Machine.ID = "bootstrap-test"
	cfg.Capacity.MaxConcurrentSessions = 4
	cfg.Orchestrator.URL = "http://127.0.0.1:1"
	cfg.Orchestrator.AuthToken = "local-stub-no-token"
	cfg.Projects = []daemon.ProjectConfig{{ID: "demo", Repository: "github.com/foo/bar"}}
	if err := daemon.WriteConfig(filepath.Join(tmp, "daemon.yaml"), cfg); err != nil {
		t.Fatal(err)
	}
	d := daemon.New(daemon.Options{
		ConfigPath: filepath.Join(tmp, "daemon.yaml"),
		JWTPath:    filepath.Join(tmp, "daemon.jwt"),
		HTTPHost:   "127.0.0.1",
		HTTPPort:   0,
		SkipWizard: true,
		// Enforce the control gate so the bootstrap read follows the
		// production credential path (open mode would serve every
		// read in full and prove nothing about the credential).
		RequireControlToken: true,
		ControlToken:        "bootstrap-operator-token",
		// Long-lived stub worker so the seeded session stays resident
		// through the bootstrap fetch.
		SpawnerOptions: daemon.SpawnerOptions{WorkerCommand: []string{"sleep", "10"}},
	})
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := daemon.NewServer(d)
	if _, err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = d.Stop(ctx)
	}
	return d, srv, cleanup
}

// seedBootstrapSession stores one credentialed session detail and returns
// the read credential the daemon states in the spawned worker's
// environment. It fails the test when the spawner does not state one.
func seedBootstrapSession(t *testing.T, d *daemon.Daemon, srv *daemon.Server, id string) string {
	t.Helper()
	detail := &daemon.SessionDetail{
		SessionID:             id,
		Repository:            "github.com/foo/bar",
		Ref:                   "main",
		WorkerID:              "wkr_bootstrap",
		AuthToken:             "tok-bootstrap-live",
		PlatformURL:           "https://platform.example.com",
		McpAuthToken:          "mcp-bootstrap-live",
		McpAuthTokenExpiresAt: "2030-01-01T00:00:00Z",
		IssueIdentifier:       "DEMO-1",
	}
	if _, err := d.AcceptWorkWithDetail(daemon.SessionSpec{
		SessionID: id, Repository: detail.Repository, Ref: detail.Ref,
	}, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	// The credential the daemon states in the spawn environment is the
	// worker's proof of session ownership on the detail read. Read it
	// back through the daemon's own lookup: there is no other channel.
	tok, ok := daemon.SessionReadTokenForTest(d, id)
	if !ok || tok == "" {
		t.Fatalf("daemon states no read credential for %q", id)
	}
	_ = srv
	return tok
}

// TestAgentRunBootstrapEndToEnd drives the production worker bootstrap
// against a real daemon: accept a session in-process (which mints the
// per-session read credential), fetch the detail through the production
// fetchSessionDetail entry point with exactly the stated credential, and
// prove the fetched detail carries the runtime credentials the runner
// needs. A second fetch with no credential proves the same route answers
// a credential-free caller with the credential fields cleared.
func TestAgentRunBootstrapEndToEnd(t *testing.T) {
	d, srv, cleanup := startBootstrapDaemon(t)
	defer cleanup()
	stated := seedBootstrapSession(t, d, srv, "bootstrap-e2e")

	daemonURL := "http://" + srv.Addr()
	client := &http.Client{Timeout: 5 * time.Second}

	// 1. The production fetch with the stated credential returns the full
	// detail, including the runtime credentials.
	fetched, err := fetchSessionDetail(context.Background(), client, daemonURL, "bootstrap-e2e", stated)
	if err != nil {
		t.Fatalf("credentialed bootstrap fetch: %v", err)
	}
	if fetched.SessionID != "bootstrap-e2e" {
		t.Errorf("SessionID = %q, want bootstrap-e2e", fetched.SessionID)
	}
	if fetched.AuthToken == "" {
		t.Error("credentialed bootstrap fetch returned no runtime credential; the runner cannot start from this answer")
	}
	if fetched.McpAuthToken == "" {
		t.Error("credentialed bootstrap fetch returned no session MCP credential")
	}
	if qw, err := detailToQueuedWork(fetched); err != nil {
		t.Fatalf("detailToQueuedWork on bootstrapped detail: %v", err)
	} else if qw.AuthToken == "" || qw.McpAuthToken == "" {
		t.Error("runner shape lost bootstrapped credentials")
	}

	// 2. The same route without a credential answers with the credential
	// fields cleared — no unauthenticated path to a token.
	bare, err := fetchSessionDetail(context.Background(), client, daemonURL, "bootstrap-e2e", "")
	if err != nil {
		t.Fatalf("credential-free fetch: %v", err)
	}
	if bare.SessionID != "bootstrap-e2e" {
		t.Errorf("redacted SessionID = %q, want bootstrap-e2e", bare.SessionID)
	}
	if bare.AuthToken != "" || bare.McpAuthToken != "" {
		t.Error("credential-free fetch returned live tokens")
	}

	// 3. The worker's own entry point consumes the stated credential path:
	// drive runAgentRun with the credential in the spawn environment's
	// variable and prove the fetch is not the failure. Capture the worker's
	// logs: the entry point must bootstrap from the full detail, so the
	// credential-missing warning must be absent. Dropping the
	// DONMAI_SESSION_READ_TOKEN read resolves an empty daemon token, the
	// bootstrap then fetches the redacted shape, and the warning fires —
	// this leg goes red. The session's stub profile runs the loop; what
	// matters here is that bootstrap passes preflight on the credentialed
	// read.
	t.Setenv("DONMAI_SESSION_READ_TOKEN", stated)
	logBuf := &bytes.Buffer{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, nil)))
	defer slog.SetDefault(prevLogger)
	cmd := &cobra.Command{}
	cmd.SetOut(testBootstrapDiscard{})
	err = runAgentRun(context.Background(), cmd, &agentRunOpts{
		sessionID: "bootstrap-e2e",
		daemonURL: daemonURL,
		worktree:  t.TempDir(),
		jsonOut:   false,
	})
	if err != nil && strings.Contains(err.Error(), "no runtime credential") {
		t.Fatalf("runAgentRun refused the bootstrapped fetch: %v", err)
	}
	if strings.Contains(logBuf.String(), "carries no runtime credential") {
		t.Errorf("runAgentRun bootstrapped from a credential-free detail despite the stated read credential; log: %s", logBuf.String())
	}
}

// TestAgentRunBootstrapReadTokenScoped proves the stated credential is
// scoped to its own session: presenting session A's credential for
// session B's detail returns the redacted shape.
func TestAgentRunBootstrapReadTokenScoped(t *testing.T) {
	d, srv, cleanup := startBootstrapDaemon(t)
	defer cleanup()
	tokA := seedBootstrapSession(t, d, srv, "scoped-a")
	seedBootstrapSession(t, d, srv, "scoped-b")

	daemonURL := "http://" + srv.Addr()
	client := &http.Client{Timeout: 5 * time.Second}
	other, err := fetchSessionDetail(context.Background(), client, daemonURL, "scoped-b", tokA)
	if err != nil {
		t.Fatalf("cross-session fetch: %v", err)
	}
	if other.AuthToken != "" || other.McpAuthToken != "" {
		t.Error("session A's credential read session B's tokens")
	}
	own, err := fetchSessionDetail(context.Background(), client, daemonURL, "scoped-a", tokA)
	if err != nil {
		t.Fatalf("own-session fetch: %v", err)
	}
	if own.AuthToken == "" {
		t.Error("session A's credential cannot read session A's detail")
	}
}

// TestAgentRunBootstrapFakeDaemonRedaction pins the worker-side contract
// against a fake daemon: whatever the daemon serves, the worker's fetch
// path surfaces it unchanged, so a redacted answer stays visibly
// credential-free rather than failing closed inside the fetch.
func TestAgentRunBootstrapFakeDaemonRedaction(t *testing.T) {
	redacted := &daemon.SessionDetail{SessionID: "redacted-1", IssueIdentifier: "DEMO-1"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/daemon/sessions/") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(redacted) //nolint:gosec // G117: test fixture
	}))
	defer srv.Close()

	got, err := fetchSessionDetail(context.Background(), srv.Client(), srv.URL, "redacted-1", "")
	if err != nil {
		t.Fatalf("fetchSessionDetail: %v", err)
	}
	if got.AuthToken != "" || got.McpAuthToken != "" {
		t.Errorf("fake-daemon redacted shape gained credentials: %+v", got)
	}
	if got.IssueIdentifier != "DEMO-1" {
		t.Errorf("IssueIdentifier = %q, want DEMO-1", got.IssueIdentifier)
	}
}

// testBootstrapDiscard is an io.Writer that drops bootstrap-runner output.
type testBootstrapDiscard struct{}

func (testBootstrapDiscard) Write(p []byte) (int, error) { return len(p), nil }
