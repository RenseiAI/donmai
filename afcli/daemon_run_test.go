package afcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	afcreds "github.com/RenseiAI/agentfactory-tui/afcli/credentials"
)

// TestFormatStartupWorkerLine covers REN-1445: the daemon startup log used to
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
		{"stub id flagged", "worker-test-machine-stub", "[daemon] worker-id worker-test-machine-stub (stub registration — not registered with platform)"},
		{"another stub", "worker-host-stub", "[daemon] worker-id worker-host-stub (stub registration — not registered with platform)"},
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
		"RENSEI_DAEMON_JWT=must-not-forward",
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
	if v, ok := baseEnv["RENSEI_DAEMON_JWT"]; ok {
		t.Errorf("RENSEI_DAEMON_JWT leaked through merge: %q", v)
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
