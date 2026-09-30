package afcli

import (
	"bytes"
	"context"
	"errors"
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
