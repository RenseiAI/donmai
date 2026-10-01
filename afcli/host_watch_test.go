package afcli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// Drive the real command in a child with pipe output and no terminal input.
// The fixture exposes only read-only daemon endpoints on an owned loopback
// server; the child context ends the actual Bubble Tea program and Wait joins
// it before this test inspects output.
func TestHostWatchPlainPipeRunsWithoutTTY(t *testing.T) {
	if os.Getenv("DONMAI_TEST_HOST_WATCH_PIPE_CHILD") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		cmd := newHostWatchCmd()
		args := []string{"--all", "--daemon-url", os.Getenv("DONMAI_TEST_HOST_WATCH_PIPE_URL")}
		if os.Getenv("DONMAI_TEST_HOST_WATCH_EXPLICIT_PLAIN") == "1" {
			args = append(args, "--plain")
		}
		cmd.SetArgs(args)
		cmd.SetOut(os.Stdout)
		cmd.SetErr(os.Stderr)
		if err := cmd.ExecuteContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("watch should run until bounded context cancellation, got %v", err)
		}
		return
	}

	var mu sync.Mutex
	requests := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/daemon/sessions":
			_, _ = w.Write([]byte(`[{"sessionId":"watch-fixture-session-1234","pid":42,"state":"running","projectName":"private-watch-project","repository":"https://github.com/example/project.git","harness":"claude-code","model":"fixture-model","workType":"development"}]`))
		case "/api/daemon/status":
			_, _ = w.Write([]byte(`{"status":"running","version":"9.9.9-fixture","maxSessions":2,"uptimeSeconds":90}`))
		case "/api/daemon/stats":
			_, _ = w.Write([]byte(`{"queueDepth":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	for _, explicit := range []bool{false, true} {
		name := "auto-plain"
		if explicit {
			name = "explicit-plain"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			output, err := os.CreateTemp(root, "watch-output-")
			if err != nil {
				t.Fatal(err)
			}
			input, err := os.Open(os.DevNull)
			if err != nil {
				_ = output.Close()
				t.Fatal(err)
			}
			env := []string{
				"HOME=" + root, "TMPDIR=" + root, "PATH=" + os.Getenv("PATH"),
				"TERM=dumb", "NO_COLOR=1", "GORACE=atexit_sleep_ms=0",
				"DONMAI_TEST_HOST_WATCH_PIPE_CHILD=1",
				"DONMAI_TEST_HOST_WATCH_PIPE_URL=" + server.URL,
			}
			if explicit {
				env = append(env, "DONMAI_TEST_HOST_WATCH_EXPLICIT_PLAIN=1")
			}
			process, err := os.StartProcess(binary, []string{binary, "-test.run=^TestHostWatchPlainPipeRunsWithoutTTY$"}, &os.ProcAttr{
				Dir: root, Env: env, Files: []*os.File{input, output, output},
			})
			_ = input.Close()
			_ = output.Close()
			if err != nil {
				t.Fatalf("start owned watch child: %v", err)
			}
			state, err := process.Wait()
			raw, readErr := os.ReadFile(output.Name())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err != nil || !state.Success() {
				t.Fatalf("owned watch child failed: state=%v err=%v output=%s", state, err, raw)
			}
			text := string(raw)
			t.Logf("piped renderer emitted ANSI control bytes: %t", strings.Contains(text, "\x1b["))
			for _, wanted := range []string{"private-watch-project", "fixture-model", "9.9.9-fixture"} {
				if !strings.Contains(text, wanted) {
					t.Errorf("piped watch lost %q: %q", wanted, text)
				}
			}
			if strings.Contains(text, "/dev/tty") {
				t.Errorf("piped watch still attempted terminal input: %q", text)
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/api/daemon/sessions", "/api/daemon/status", "/api/daemon/stats"} {
		if requests[path] == 0 {
			t.Errorf("real watch command did not read %s", path)
		}
	}
}
