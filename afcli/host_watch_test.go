package afcli

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/RenseiAI/donmai/afclient"
)

func TestRepoSlugFromRemote(t *testing.T) {
	tests := []struct {
		name, remote, want string
	}{
		{"https with .git", "https://github.com/RenseiAI/donmai.git", "RenseiAI/donmai"},
		{"https no .git", "https://github.com/RenseiAI/donmai", "RenseiAI/donmai"},
		{"ssh form", "git@github.com:RenseiAI/donmai.git", "RenseiAI/donmai"},
		{"ssh no .git", "git@github.com:acme/web", "acme/web"},
		{"trailing slash", "https://github.com/acme/web/", "acme/web"},
		{"empty", "", ""},
		{"garbage", "not-a-url", ""},
		{"host only", "https://github.com/", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoSlugFromRemote(tc.remote); got != tc.want {
				t.Errorf("repoSlugFromRemote(%q)=%q want %q", tc.remote, got, tc.want)
			}
		})
	}
}

func TestScopeLabel(t *testing.T) {
	tests := []struct {
		scope string
		all   bool
		want  string
	}{
		{"", false, "all projects"},
		{"", true, "all projects"},
		{"acme/web", false, "acme/web"},
		{"acme/web", true, "all projects"}, // --all overrides scope
	}
	for _, tc := range tests {
		if got := scopeLabel(tc.scope, tc.all); got != tc.want {
			t.Errorf("scopeLabel(%q,%v)=%q want %q", tc.scope, tc.all, got, tc.want)
		}
	}
}

func TestResolveHostWatchURL(t *testing.T) {
	t.Setenv(hostWatchEnvDaemonURL, "")
	if got := resolveHostWatchURL("http://flag:1"); got != "http://flag:1" {
		t.Errorf("flag should win, got %q", got)
	}
	t.Setenv(hostWatchEnvDaemonURL, "http://env:2")
	if got := resolveHostWatchURL(""); got != "http://env:2" {
		t.Errorf("env should be used when no flag, got %q", got)
	}
	if got := resolveHostWatchURL("http://flag:1"); got != "http://flag:1" {
		t.Errorf("flag should still win over env, got %q", got)
	}
}

// TestNewHostWatchCmd_Wiring asserts the command factory builds a usable
// cobra command with the expected flags (a thin smoke over the wiring).
func TestNewHostWatchCmd_Wiring(t *testing.T) {
	cmd := newHostWatchCmd()
	if cmd.Use != "watch" {
		t.Errorf("Use = %q, want watch", cmd.Use)
	}
	for _, f := range []string{"project", "all", "replay", "plain", "daemon-url"} {
		if cmd.Flags().Lookup(f) == nil {
			t.Errorf("missing flag --%s", f)
		}
	}
}

// fakeHostWatchSource confirms *afclient.DaemonClient satisfies the
// hostWatchSource interface (compile-time check) and that the interface is
// usable with a fake.
type fakeHostWatchSource struct{}

func (fakeHostWatchSource) GetSessions() ([]afclient.DaemonSessionHandle, error) { return nil, nil }
func (fakeHostWatchSource) GetStatus() (*afclient.DaemonStatusResponse, error)   { return nil, nil }
func (fakeHostWatchSource) GetStats(_, _ bool) (*afclient.DaemonStatsResponse, error) {
	return nil, nil
}

func TestHostWatchSource_Satisfied(t *testing.T) {
	var _ hostWatchSource = (*afclient.DaemonClient)(nil)
	var _ hostWatchSource = fakeHostWatchSource{}
	// Build a command with an injected fake factory to exercise that path.
	cmd := newHostWatchCmdWithSource(func(afclient.DaemonConfig) hostWatchSource {
		return fakeHostWatchSource{}
	})
	if cmd == nil {
		t.Fatal("nil command")
	}
}

// TestNewHostWatchClient_CarriesControlToken pins that every host watch
// client — an explicit --daemon-url, the env URL override, or the default
// config — carries the operator's control token on mutating requests, and
// that read-only GETs stay credential-free.
func TestNewHostWatchClient_CarriesControlToken(t *testing.T) {
	var (
		mu       sync.Mutex
		authByOp = map[string]string{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authByOp[r.Method+" "+r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	defaultCfg := afclient.DaemonConfig{Host: u.Hostname(), Port: port}

	tokenFile := filepath.Join(t.TempDir(), afclient.ControlTokenFileName)
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	absentFile := filepath.Join(t.TempDir(), "absent")

	cases := []struct {
		name      string
		flagURL   string
		envURL    string
		envToken  string
		tokenFile string
		cfg       afclient.DaemonConfig
		wantAuth  string
	}{
		{name: "flag URL with env token", flagURL: srv.URL, envToken: "env-token", tokenFile: absentFile, wantAuth: "Bearer env-token"},
		{name: "flag URL with token file", flagURL: srv.URL, tokenFile: tokenFile, wantAuth: "Bearer file-token"},
		{name: "env URL with token file", envURL: srv.URL, tokenFile: tokenFile, wantAuth: "Bearer file-token"},
		{name: "default config with token file", cfg: defaultCfg, tokenFile: tokenFile, wantAuth: "Bearer file-token"},
		{name: "flag URL without any token", flagURL: srv.URL, tokenFile: absentFile, wantAuth: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(hostWatchEnvDaemonURL, tc.envURL)
			t.Setenv(afclient.ControlTokenEnv, tc.envToken)
			t.Setenv(afclient.ControlTokenFileEnv, tc.tokenFile)
			mu.Lock()
			clear(authByOp)
			mu.Unlock()

			client := newHostWatchClient(tc.flagURL, tc.cfg)
			if _, err := client.Pause(); err != nil {
				t.Fatalf("Pause: %v", err)
			}
			if _, err := client.GetStatus(); err != nil {
				t.Fatalf("GetStatus: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if got := authByOp["POST /api/daemon/pause"]; got != tc.wantAuth {
				t.Errorf("POST Authorization = %q, want %q", got, tc.wantAuth)
			}
			if got, ok := authByOp["GET /api/daemon/status"]; !ok || got != "" {
				t.Errorf("GET Authorization = %q (seen %v), want a credential-free GET", got, ok)
			}
		})
	}
}
