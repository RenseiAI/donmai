package afcli

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	afcreds "github.com/RenseiAI/donmai/afcli/credentials"
	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/runtime/statehome"
)

func TestDaemonRunControlBindPreflight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantHost string
		wantPort int
		wantErr  string
	}{
		{name: "default", wantHost: "127.0.0.1", wantPort: 0},
		{name: "embedded loopback port", args: []string{"--host", "localhost:8123"}, wantHost: "localhost", wantPort: 8123},
		{name: "matching ports", args: []string{"--host", "[::1]:8123", "--port", "8123"}, wantHost: "::1", wantPort: 8123},
		{name: "reject wildcard", args: []string{"--host", "0.0.0.0"}, wantErr: "refusing non-loopback"},
		{name: "reject conflict", args: []string{"--host", "127.0.0.1:8123", "--port", "8124"}, wantErr: "conflicts with explicit port"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cmd := newDaemonRunCmd(Config{})
			if err := cmd.Flags().Parse(tt.args); err != nil {
				t.Fatalf("parse flags: %v", err)
			}
			err := cmd.PreRunE(cmd, nil)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("PreRunE() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("PreRunE(): %v", err)
			}
			if got := cmd.Flags().Lookup("host").Value.String(); got != tt.wantHost {
				t.Fatalf("host after preflight = %q, want %q", got, tt.wantHost)
			}
			if got := cmd.Flags().Lookup("port").Value.String(); got != strconv.Itoa(tt.wantPort) {
				t.Fatalf("port after preflight = %q, want %d", got, tt.wantPort)
			}
		})
	}
}

// TestFormatStartupWorkerLine covers the startup-line regression: the daemon startup log used to
// print `[daemon] worker-id worker-test-machine-stub` in stub mode, which
// misled operators into thinking the daemon had registered with the platform.
// The new helper annotates stub ids and returns "" when no id is assigned.
func TestFormatStartupWorkerLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"real platform id", "wkr_60eb0a2f35124d56", "[daemon] worker-id wkr_60eb0a2f35124d56"},
		{"stub id flagged", "worker-test-machine-stub", "[daemon] worker-id worker-test-machine-stub (stub registration, not registered with platform)"},
		{"another stub", "worker-host-stub", "[daemon] worker-id worker-host-stub (stub registration, not registered with platform)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := formatStartupWorkerLine(c.in); got != c.want {
				t.Errorf("formatStartupWorkerLine(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestResolveStandaloneCredsMode pins the auto-detect ladder for the
// --standalone-creds flag.
func TestResolveStandaloneCredsMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		flag             string
		daemonJWTPresent bool
		want             bool
	}{
		{"auto/no-jwt → on", "auto", false, true},
		{"auto/with-jwt → off", "auto", true, false},
		{"empty/no-jwt → on", "", false, true},
		{"empty/with-jwt → off", "", true, false},
		{"on overrides JWT presence", "on", true, true},
		{"off overrides JWT absence", "off", false, false},
		{"true/yes/1 normalised to on", "true", true, true},
		{"false/no/0 normalised to off", "false", false, false},
		{"unknown falls back to auto/no-jwt", "garbage", false, true},
		{"unknown falls back to auto/with-jwt", "garbage", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveStandaloneCredsMode(c.flag, c.daemonJWTPresent); got != c.want {
				t.Errorf("resolveStandaloneCredsMode(%q, jwt=%v) = %v, want %v", c.flag, c.daemonJWTPresent, got, c.want)
			}
		})
	}
}

// TestDisplayEnvLocalPath covers the nil/empty fallbacks of the
// human-readable .env.local label used in the startup log.
func TestDisplayEnvLocalPath(t *testing.T) {
	t.Parallel()
	if got := displayEnvLocalPath(nil); got != "(no .env.local)" {
		t.Errorf("displayEnvLocalPath(nil) = %q, want %q", got, "(no .env.local)")
	}
}

func TestWorkareaArchiveRootMissingConfig(t *testing.T) {
	t.Parallel()

	got, err := workareaArchiveRoot(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("workareaArchiveRoot(missing config): %v", err)
	}
	if got != "" {
		t.Fatalf("workareaArchiveRoot(missing config) = %q, want empty", got)
	}
}

func TestWorkareaArchiveRootPreservesConfiguredRoot(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "daemon.yaml")
	want := filepath.Join(t.TempDir(), "archives")
	config := daemon.DefaultConfig()
	config.Machine.ID = "test-machine"
	config.Orchestrator.URL = "https://orchestrator.example"
	config.Workarea.ArchiveRoot = want
	if err := daemon.WriteConfig(configPath, config); err != nil {
		t.Fatalf("write config: %v", err)
	}

	got, err := workareaArchiveRoot(configPath)
	if err != nil {
		t.Fatalf("workareaArchiveRoot(present config): %v", err)
	}
	if got != want {
		t.Fatalf("workareaArchiveRoot(present config) = %q, want %q", got, want)
	}
}

func TestWorkareaArchiveRootRejectsMalformedConfig(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(configPath, []byte("machine: [\n"), 0o600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}
	if _, err := workareaArchiveRoot(configPath); err == nil || !strings.Contains(err.Error(), "parse daemon config") {
		t.Fatalf("workareaArchiveRoot(malformed config) error = %v, want typed parse error", err)
	}
}

func TestDaemonRunRejectsMalformedConfigBeforeTerminalAuthority(t *testing.T) {
	home := t.TempDir()
	priorHome := statehome.BaseHome()
	statehome.SetBaseHome(home)
	t.Cleanup(func() { statehome.SetBaseHome(priorHome) })

	configPath := filepath.Join(t.TempDir(), "daemon.yaml")
	if err := os.WriteFile(configPath, []byte("machine: [\n"), 0o600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}

	cmd := newDaemonRunCmd(Config{HostBinaryVersion: "test"})
	cmd.SetArgs([]string{"--config", configPath, "--skip-wizard", "--standalone-creds=off"})
	var output strings.Builder
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "load workarea archive configuration") {
		t.Errorf("daemon run malformed-config error = %v, want early archive configuration error", err)
	}
	if _, statErr := os.Stat(statehome.StateDir("worktrees")); !os.IsNotExist(statErr) {
		t.Errorf("terminal authority path exists after malformed config: %v", statErr)
	}
}

// A skipped first-run wizard must leave default-config construction to
// Daemon.Start. The command is real: it binds its own loopback listener,
// performs stub registration, serves healthz, and stops through the authenticated
// local control API. Every path and token belongs to this test's private home.
func TestDaemonRunSkippedWizardStartsDefaultStub(t *testing.T) {
	for _, tc := range []struct {
		name         string
		explicitFlag bool
		envSkip      bool
	}{
		{name: "explicit-flag", explicitFlag: true},
		{name: "non-tty"},
		{name: "skip-env", envSkip: true},
	} {
		quiescenceUnknown := false
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			priorHome := statehome.BaseHome()
			statehome.SetBaseHome(home)
			t.Cleanup(func() { statehome.SetBaseHome(priorHome) })
			t.Setenv("HOME", home)
			t.Setenv("DONMAI_STATE_HOME", home)
			t.Setenv("DONMAI_DAEMON_FORCE_STUB", "1")
			// A closed loopback URL, not "": an empty URL now seeds
			// the fresh-host file queue (which refuses until setup),
			// while this stub-path test needs the plain default.
			t.Setenv("DONMAI_ORCHESTRATOR_URL", "http://127.0.0.1:1")
			t.Setenv("DONMAI_DAEMON_SKIP_WIZARD", "")
			t.Setenv(afclient.ControlTokenEnv, "")
			t.Setenv(afclient.ControlTokenFileEnv, filepath.Join(home, "control-token"))
			if tc.envSkip {
				t.Setenv("DONMAI_DAEMON_SKIP_WIZARD", "1")
			}
			if !tc.explicitFlag && !tc.envSkip {
				// go test supplies /dev/null (a character device) to its test
				// process even when the go command reads a regular file. Install
				// owned regular-file stdin for this exact no-TTY command case.
				input, err := os.CreateTemp(home, "non-tty-stdin-")
				if err != nil {
					t.Fatal(err)
				}
				previousStdin := os.Stdin
				os.Stdin = input
				t.Cleanup(func() { os.Stdin = previousStdin; _ = input.Close() })
				if !daemon.ShouldSkipWizard() {
					t.Fatal("owned regular-file stdin did not select non-interactive setup")
				}
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(home, "daemon.yaml")
			cmd := newDaemonRunCmd(Config{HostBinaryVersion: "test"})
			args := []string{"--config", configPath, "--jwt-path", filepath.Join(home, "daemon.jwt"), "--port", strconv.Itoa(port), "--standalone-creds=off"}
			if tc.explicitFlag {
				args = append(args, "--skip-wizard")
			}
			cmd.SetArgs(args)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- cmd.ExecuteContext(ctx) }()
			joined := false
			t.Cleanup(func() {
				cancel()
				if joined {
					return
				}
				select {
				case <-done:
					joined = true
				case <-time.After(5 * time.Second):
					quiescenceUnknown = true
					t.Error("owned daemon command did not join after cancellation; quiescence unknown")
				}
			})

			origin := "http://127.0.0.1:" + strconv.Itoa(port)
			client := &http.Client{Timeout: 300 * time.Millisecond}
			deadline := time.NewTimer(8 * time.Second)
			defer deadline.Stop()
			for {
				resp, requestErr := client.Get(origin + "/healthz")
				if requestErr == nil {
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						break
					}
				}
				select {
				case runErr := <-done:
					joined = true
					t.Fatalf("default stub daemon exited before healthz: %v; output=%s", runErr, output.String())
				case <-deadline.C:
					t.Fatal("default stub daemon did not reach healthz")
				case <-time.After(50 * time.Millisecond):
				}
			}
			token, err := afclient.LoadControlToken(filepath.Join(home, "control-token"))
			if err != nil || token == "" {
				t.Fatalf("private control token unavailable: %v", err)
			}
			controller := afclient.NewDaemonClientFromURL(origin)
			controller.SetControlToken(token)
			if _, err := controller.Stop(); err != nil {
				t.Fatalf("stop owned stub daemon: %v", err)
			}
			select {
			case runErr := <-done:
				joined = true
				if runErr != nil {
					t.Fatalf("owned stub daemon exit: %v", runErr)
				}
			case <-time.After(12 * time.Second):
				t.Fatal("owned stub daemon did not join after control stop")
			}
		})
		if quiescenceUnknown {
			t.Fatal("owned daemon quiescence unknown; refusing another case")
		}
	}
}

func TestDaemonRunSkippedWizardStillRejectsUnconfiguredFileQueue(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "daemon.yaml")
	config := daemon.DefaultConfig()
	config.APIVersion = daemon.LocalRuntimeConfigAPIVersion
	config.Orchestrator.URL = "file://" + filepath.Join(t.TempDir(), "queue")
	if err := daemon.WriteConfig(configPath, config); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := newDaemonRunCmd(Config{HostBinaryVersion: "test"})
	cmd.SetArgs([]string{"--config", configPath, "--skip-wizard", "--standalone-creds=off"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "local runtime and its outermost policy are required") {
		t.Fatalf("incomplete file queue accepted: %v; output=%s", err, output.String())
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("refused file queue configuration was rewritten")
	}
}

// TestStandaloneCredsMergeIntoSpawnerBaseEnv simulates the integration
// point: a daemon-run startup that has built a LocalSource and is about
// to construct SpawnerOptions.BaseEnv. The merged map should contain
// .env.local-sourced values, must NOT contain any AGENT_ENV_BLOCKLIST
// entries even when they were present in the source, and the .env.local
// file MUST NOT be copied into the spawner's view of any worktree
// (LocalSource is a read-only abstraction — the file stays at gitRoot).
func TestStandaloneCredsMergeIntoSpawnerBaseEnv(t *testing.T) {
	// Don't t.Parallel — we call t.Setenv.
	root := t.TempDir()
	envLocal := filepath.Join(root, ".env.local")
	content := strings.Join([]string{
		"AF_TEST_FORWARDED=hello",
		"DONMAI_DAEMON_JWT=must-not-forward",
		"WORKER_API_KEY=rsk_must_not_forward",
		"# a comment",
		`AF_TEST_QUOTED="quoted value"`,
	}, "\n") + "\n"
	if err := os.WriteFile(envLocal, []byte(content), 0o600); err != nil {
		t.Fatalf("write .env.local: %v", err)
	}

	t.Setenv("AF_TEST_FROM_PROCESS", "process-value")

	src, err := afcreds.LoadLocalSource(root)
	if err != nil {
		t.Fatalf("LoadLocalSource: %v", err)
	}

	baseEnv := src.MergeIntoBaseEnv(nil)

	// Forwarded keys must be present.
	if v, ok := baseEnv["AF_TEST_FORWARDED"]; !ok || v != "hello" {
		t.Errorf("AF_TEST_FORWARDED: got (%q, %v), want (hello, true)", v, ok)
	}
	if v, ok := baseEnv["AF_TEST_QUOTED"]; !ok || v != "quoted value" {
		t.Errorf("AF_TEST_QUOTED: got (%q, %v), want (quoted value, true)", v, ok)
	}
	if v, ok := baseEnv["AF_TEST_FROM_PROCESS"]; !ok || v != "process-value" {
		t.Errorf("AF_TEST_FROM_PROCESS: got (%q, %v), want (process-value, true)", v, ok)
	}

	// Blocked keys must NOT be present.
	if v, ok := baseEnv["DONMAI_DAEMON_JWT"]; ok {
		t.Errorf("DONMAI_DAEMON_JWT leaked through merge: %q", v)
	}
	if v, ok := baseEnv["WORKER_API_KEY"]; ok {
		t.Errorf("WORKER_API_KEY leaked through merge: %q", v)
	}

	// .env.local is not removed by the merge — but it lives at gitRoot,
	// NOT inside any worktree path the spawner would later create. As a
	// proxy, assert that no .env.local file is materialised under a
	// fake "worktree" subdirectory the test pre-creates.
	worktree := filepath.Join(root, "worktrees", "sess-x")
	if err := os.MkdirAll(worktree, 0o750); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	// The LocalSource API has no surface that writes anywhere; this
	// is a regression guard for a future refactor that could be
	// tempted to copy values into the worktree.
	if _, err := os.Stat(filepath.Join(worktree, ".env.local")); !os.IsNotExist(err) {
		t.Errorf("LocalSource leaked .env.local into worktree path: err=%v", err)
	}
}

type fakeTerminalRuntimeCredentialSource struct {
	workerID string
	token    string
}

func (f *fakeTerminalRuntimeCredentialSource) RuntimeCredentials() (string, string) {
	return f.workerID, f.token
}

func TestTerminalReceiverAuthorizationResolvesFreshRuntimeToken(t *testing.T) {
	t.Parallel()

	source := &fakeTerminalRuntimeCredentialSource{}
	resolve := terminalReceiverAuthorization(source)
	if _, err := resolve(context.Background(), "rcv_00000000000000000000000000000000"); err == nil {
		t.Fatal("authorization before daemon credentials = nil error, want unavailable error")
	}

	source.workerID = "wkr_test"
	source.token = "first-token"
	got, err := resolve(context.Background(), "rcv_00000000000000000000000000000000")
	if err != nil {
		t.Fatalf("resolve first token: %v", err)
	}
	if got != "Bearer first-token" {
		t.Fatalf("first authorization = %q, want %q", got, "Bearer first-token")
	}

	source.token = "rotated-token"
	got, err = resolve(context.Background(), "rcv_00000000000000000000000000000000")
	if err != nil {
		t.Fatalf("resolve rotated token: %v", err)
	}
	if got != "Bearer rotated-token" {
		t.Fatalf("rotated authorization = %q, want %q", got, "Bearer rotated-token")
	}

	source.token = ""
	got, err = resolve(context.Background(), "rcv_00000000000000000000000000000000")
	if err != nil {
		t.Fatalf("resolve unauthenticated configured daemon: %v", err)
	}
	if got != "" {
		t.Fatalf("unauthenticated authorization = %q, want empty", got)
	}
}

// unmintableControlTokenPath returns a token path whose parent is a regular
// file, so minting fails on every platform and for every user (root too).
func unmintableControlTokenPath(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	return filepath.Join(blocker, afclient.ControlTokenFileName)
}

// TestApplyDaemonControlAuth pins the entry-point half of the fail-closed
// control gate: the daemon always requires the token, and when it cannot be
// minted or its path does not resolve, it carries no token (so the gate
// refuses) and says so loudly instead of leaving the routes open.
func TestApplyDaemonControlAuth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		path      func(t *testing.T) string
		wantToken bool
		wantLog   string
	}{
		{
			name:      "minted",
			path:      func(t *testing.T) string { return filepath.Join(t.TempDir(), "state", afclient.ControlTokenFileName) },
			wantToken: true,
		},
		{name: "mint fails", path: unmintableControlTokenPath, wantLog: "mutating control routes are DISABLED (fail closed)"},
		{name: "path unresolved", path: func(*testing.T) string { return "" }, wantLog: "control token path unresolved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := tc.path(t)
			var errOut bytes.Buffer
			var opts daemon.Options
			applyDaemonControlAuth(&opts, path, &errOut)

			if !opts.RequireControlToken {
				t.Fatal("RequireControlToken = false; the daemon entry point must always require the control token")
			}
			if strings.Contains(errOut.String(), "stay open") {
				t.Errorf("errOut still advertises open routes: %q", errOut.String())
			}
			if !tc.wantToken {
				if opts.ControlToken != "" {
					t.Errorf("ControlToken = %q, want empty on failure", opts.ControlToken)
				}
				if !strings.Contains(errOut.String(), tc.wantLog) {
					t.Errorf("errOut = %q, want it to contain %q", errOut.String(), tc.wantLog)
				}
				return
			}
			if opts.ControlToken == "" {
				t.Fatal("ControlToken empty after a successful mint")
			}
			if errOut.Len() != 0 {
				t.Errorf("errOut = %q, want silence on success", errOut.String())
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat minted token: %v", err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("token file mode = %o, want 600", perm)
			}
		})
	}
}

// TestApplyDaemonControlAuth_StatesTheResolvedTokenPath pins the token path
// the daemon states to its seats (Options.ControlTokenPath): the file the
// token was really minted into or read from, symbolic links and ".." walked
// the way the kernel walks them. A ".." after a symbolic link names a
// different file when cleaned lexically, so stating the override as spelled
// would aim the seat's deny at a file that does not hold the token.
func TestApplyDaemonControlAuth_StatesTheResolvedTokenPath(t *testing.T) {
	t.Parallel()

	resolvedDir := func(t *testing.T, dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	cases := []struct {
		name string
		// setup returns the path the operator configured and the path the
		// daemon must state.
		setup func(t *testing.T) (configured, want string)
	}{
		{
			name: "plain path",
			setup: func(t *testing.T) (string, string) {
				dir := filepath.Join(t.TempDir(), "state")
				return filepath.Join(dir, afclient.ControlTokenFileName), filepath.Join(resolvedDir(t, dir), afclient.ControlTokenFileName)
			},
		},
		{
			name: "dot-dot after a symbolic link",
			setup: func(t *testing.T) (string, string) {
				base := t.TempDir()
				target := resolvedDir(t, filepath.Join(base, "real", "sub"))
				tokens := resolvedDir(t, filepath.Join(base, "real", "tokens"))
				if err := os.Symlink(target, filepath.Join(base, "link")); err != nil {
					t.Fatal(err)
				}
				configured := filepath.Join(base, "link") + "/../tokens/" + afclient.ControlTokenFileName
				return configured, filepath.Join(tokens, afclient.ControlTokenFileName)
			},
		},
		{
			name: "symbolic link to the token file",
			setup: func(t *testing.T) (string, string) {
				base := t.TempDir()
				token := filepath.Join(resolvedDir(t, filepath.Join(base, "real")), afclient.ControlTokenFileName)
				if err := os.WriteFile(token, []byte("sentinel-token\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(base, "token-link")
				if err := os.Symlink(token, link); err != nil {
					t.Fatal(err)
				}
				return link, token
			},
		},
		{
			name: "mint fails: stated as given",
			setup: func(t *testing.T) (string, string) {
				path := unmintableControlTokenPath(t)
				return path, path
			},
		},
		{name: "unresolved", setup: func(*testing.T) (string, string) { return "", "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			configured, want := tc.setup(t)
			var errOut bytes.Buffer
			var opts daemon.Options
			applyDaemonControlAuth(&opts, configured, &errOut)
			if opts.ControlTokenPath != want {
				t.Fatalf("stated ControlTokenPath = %q, want %q", opts.ControlTokenPath, want)
			}
			if opts.ControlToken == "" {
				return
			}
			// The stated file is the one holding the live token.
			raw, err := os.ReadFile(want) //nolint:gosec // G304: the test's own temp token.
			if err != nil || strings.TrimSpace(string(raw)) != opts.ControlToken {
				t.Fatalf("stated path %q does not hold the daemon's token (err %v)", want, err)
			}
		})
	}
}

// TestDaemonRunControlAuth_FailsClosedEndToEnd drives the entry-point wiring
// against a live daemon whose token cannot be minted: the operator client's
// mutating call is refused with ErrUnavailable and a non-sensitive reason,
// while a read-only call still succeeds.
func TestDaemonRunControlAuth_FailsClosedEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "daemon.yaml")
	cfg := daemon.DefaultConfig()
	cfg.Machine.ID = "test-control-auth"
	cfg.Orchestrator.URL = "file:///tmp/queue"
	cfg.Projects = []daemon.ProjectConfig{{ID: "p1", Repository: "github.com/foo/bar"}}
	if err := daemon.WriteConfig(cfgPath, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	opts := daemon.Options{
		ConfigPath:       cfgPath,
		JWTPath:          filepath.Join(tmp, "daemon.jwt"),
		HTTPHost:         "127.0.0.1",
		HTTPPort:         0,
		SkipWizard:       true,
		SkipRegistration: true,
	}
	var errOut bytes.Buffer
	applyDaemonControlAuth(&opts, unmintableControlTokenPath(t), &errOut)

	d := daemon.New(opts)
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

	client := afclient.NewDaemonClientFromURL("http://" + srv.Addr())
	client.SetControlToken("operator-guess")
	_, err := client.Pause()
	if !errors.Is(err, afclient.ErrUnavailable) {
		t.Fatalf("Pause on a daemon without a control token: err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "control token unavailable") {
		t.Errorf("Pause error = %q, want the control-token reason", err)
	}
	if strings.Contains(err.Error(), afclient.ControlTokenFileName) || strings.Contains(err.Error(), "operator-guess") {
		t.Errorf("Pause error leaks a path or credential: %q", err)
	}
	if _, err := client.GetStatus(); err != nil {
		t.Fatalf("GetStatus on a daemon without a control token: %v, want read-only routes to keep working", err)
	}
}
