package afcli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/donmai/daemon"
)

// TestResolveAgentRunDaemonURL pins both halves of the resolution: the
// address, and whether this process was TOLD it or fell back to it.
func TestResolveAgentRunDaemonURL(t *testing.T) {
	tests := []struct {
		name       string
		flag       string
		env        map[string]string
		wantURL    string
		wantSource string
	}{
		{
			name:       "explicit flag wins",
			flag:       "http://127.0.0.1:18382",
			env:        map[string]string{daemon.EnvDaemonControlURL: "http://127.0.0.1:7734"},
			wantURL:    "http://127.0.0.1:18382",
			wantSource: daemonURLSourceFlag,
		},
		{
			name:       "whitespace-only flag is not an address",
			flag:       "   ",
			env:        map[string]string{daemon.EnvDaemonControlURL: "http://127.0.0.1:18382"},
			wantURL:    "http://127.0.0.1:18382",
			wantSource: daemonURLSourceEnv,
		},
		{
			name:       "the spawning daemon's address is used and named as such",
			env:        map[string]string{daemon.EnvDaemonControlURL: "http://127.0.0.1:18382"},
			wantURL:    "http://127.0.0.1:18382",
			wantSource: daemonURLSourceEnv,
		},
		{
			name:       "nothing supplied falls back to the default, labelled as a guess",
			wantURL:    DefaultAgentRunDaemonURL,
			wantSource: daemonURLSourceBuiltinDefault,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) string { return tt.env[key] }
			gotURL, gotSource := resolveAgentRunDaemonURL(tt.flag, lookup)
			if gotURL != tt.wantURL {
				t.Errorf("url = %q, want %q", gotURL, tt.wantURL)
			}
			if gotSource != tt.wantSource {
				t.Errorf("source = %q, want %q", gotSource, tt.wantSource)
			}
		})
	}
}

// TestResolveAgentRunDaemonURL_DefaultLabelWarnsAboutOtherPorts keeps the
// fallback's label honest: it is the sentence a mute worker leaves behind, so
// it has to say that no daemon told this process anything.
func TestResolveAgentRunDaemonURL_DefaultLabelWarnsAboutOtherPorts(t *testing.T) {
	_, source := resolveAgentRunDaemonURL("", func(string) string { return "" })
	for _, want := range []string{"default", daemon.EnvDaemonControlURL} {
		if !strings.Contains(source, want) {
			t.Errorf("fallback source %q does not mention %q", source, want)
		}
	}
}

// TestRunAgentRun_PreflightReachesNamedInstanceDaemon is the worker half of
// the fix: given the address its daemon stated, the worker dials THAT daemon
// — on a port that is not the built-in default — and says so when it fails.
func TestRunAgentRun_PreflightReachesNamedInstanceDaemon(t *testing.T) {
	var hits atomic.Int32
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath.Store(r.URL.Path)
		// 404 keeps the test in preflight: the point is which daemon was
		// dialled, not what the runner would do next.
		http.Error(w, "session not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	if strings.HasSuffix(srv.URL, ":7734") {
		t.Skipf("ephemeral httptest bind landed on the default port: %s", srv.URL)
	}

	t.Setenv("DONMAI_DAEMON_URL", srv.URL)
	err := runAgentRun(context.Background(), &cobra.Command{}, &agentRunOpts{sessionID: "sess-named"})
	if err == nil {
		t.Fatal("expected a preflight error from the 404 daemon")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("named-instance daemon received %d requests, want 1 — the worker dialled somewhere else", got)
	}
	if got, _ := gotPath.Load().(string); got != "/api/daemon/sessions/sess-named" {
		t.Errorf("request path = %q, want the session-detail route", got)
	}
	if !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error %q does not name the address it dialled (%s)", err.Error(), srv.URL)
	}
	if !strings.Contains(err.Error(), daemonURLSourceEnv) {
		t.Errorf("error %q does not name where that address came from", err.Error())
	}
	if strings.Contains(err.Error(), DefaultAgentRunDaemonURL) {
		t.Errorf("error %q names the built-in default it never used", err.Error())
	}
}

// TestRunAgentRun_PreflightNamesTheUnreachableAddress covers the shape the
// original silence had: a refused connection. The address and its provenance
// are the whole diagnostic, so they must survive into the error text.
func TestRunAgentRun_PreflightNamesTheUnreachableAddress(t *testing.T) {
	t.Setenv("DONMAI_DAEMON_URL", "")
	// Port 1 is closed on every reasonable host; nothing is dialled that
	// belongs to a real daemon.
	const closed = "http://127.0.0.1:1"
	err := runAgentRun(context.Background(), &cobra.Command{}, &agentRunOpts{
		sessionID: "sess-refused",
		daemonURL: closed,
	})
	if err == nil {
		t.Fatal("expected a preflight error from a closed port")
	}
	if !strings.Contains(err.Error(), closed) {
		t.Errorf("error %q does not name the address it dialled", err.Error())
	}
	if !strings.Contains(err.Error(), daemonURLSourceFlag) {
		t.Errorf("error %q does not name where that address came from", err.Error())
	}
}
