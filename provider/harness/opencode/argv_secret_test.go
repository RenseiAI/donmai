package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// argvSecretSentinel is the canary credential this test injects on Spec.Env.
// It must reach the CLI child through its ENVIRONMENT and must never appear
// in the child's argv — any local user can read another process's command
// line via ps or /proc/<pid>/cmdline, so a credential placed there is
// readable host-wide.
const argvSecretSentinel = "sentinel-credential-must-not-ride-argv"

// fakeArgvEnvRecorderCLI writes a fake opencode CLI that records the exact
// argv the OS delivered it (one argument per line) plus its full environment
// to files under dir, then sleeps so the parent can inspect both. Reading
// /proc/self/cmdline (Linux) records the NUL-separated argv the kernel
// holds — the same bytes `ps` renders. Where /proc is absent (macOS) it
// falls back to `ps -o args= -p $$`.
func fakeArgvEnvRecorderCLI(t *testing.T, dir string) string {
	t.Helper()
	argvPath := filepath.Join(dir, "argv")
	envPath := filepath.Join(dir, "child-env")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo \"opencode 1.17.18\"; exit 0; fi\n" +
		"if [ -r /proc/self/cmdline ]; then tr '\\0' '\\n' < /proc/self/cmdline > " + argvShellQuote(argvPath) + "\n" +
		"else ps -o args= -p $$ > " + argvShellQuote(argvPath) + "\n" +
		"fi\n" +
		"env > " + argvShellQuote(envPath) + "\n" +
		"sleep 30\n"
	path := filepath.Join(dir, "fake-opencode-argv-recorder.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake opencode: %v", err)
	}
	return path
}

// argvShellQuote single-quotes a path for embedding in the fake CLI script.
func argvShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// waitForFile polls for a non-empty file up to timeout. The fake binary
// writes its records as its first act, so presence proves the child was
// exec'd with the production argv.
func waitForFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 { //nolint:gosec // test fixture path
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the harness child to record at %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSpawnCLI_SentinelCredentialRidesEnvNotArgv is the argv-secret
// regression test for the CLI lane: a real Provider.Spawn with a sentinel
// credential on Spec.Env must deliver it through the child's environment
// while the child's recorded argv carries no trace of it. The fake never
// emits session events, so the test stops the handle once both recorder
// files prove the child was exec'd with the production argv and env.
//
// RED proof: interpolate any Spec.Env value into buildOpenCodeArgs (for
// example by appending KEY=value entries to the argv) and the argv
// assertion fails with the sentinel quoted in the failure output.
func TestSpawnCLI_SentinelCredentialRidesEnvNotArgv(t *testing.T) {
	// Not parallel: spawns a real child process with a fixed recording dir.
	dir := t.TempDir()
	cli := fakeArgvEnvRecorderCLI(t, dir)
	p, err := New(Options{
		Binary:       cli,
		VersionProbe: func(context.Context, string) (string, error) { return "1.17.18", nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:    dir,
		Prompt: "prove argv carries no credential",
		Env:    map[string]string{"SENTINEL_API_KEY": argvSecretSentinel},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	argv := waitForFile(t, filepath.Join(dir, "argv"), 20*time.Second)
	envRaw := waitForFile(t, filepath.Join(dir, "child-env"), 20*time.Second)

	if strings.Contains(argv, argvSecretSentinel) {
		t.Fatalf("CLI child argv carries the sentinel credential:\n%s", argv)
	}
	if !strings.Contains(envRaw, "SENTINEL_API_KEY="+argvSecretSentinel) {
		t.Fatalf("sentinel credential did not reach the child environment:\n%s", envRaw)
	}
}

// TestBuildOpenCodeArgs_CarryNoCredentialValue pins the same invariant on
// the argv builder: no Spec.Env value may be interpolated into the `opencode
// run` argument list. Secrets travel beside argv in Spec.Env (composed onto
// cmd.Env at spawn), never inside it.
func TestBuildOpenCodeArgs_CarryNoCredentialValue(t *testing.T) {
	t.Parallel()

	argv := buildOpenCodeArgs(agent.Spec{
		Cwd:    "/tmp/work",
		Prompt: "prove argv carries no credential",
		Model:  "test-model",
		Env:    map[string]string{"SENTINEL_API_KEY": argvSecretSentinel},
	})
	for _, a := range argv {
		if strings.Contains(a, argvSecretSentinel) {
			t.Fatalf("argv builder emitted the sentinel credential: %q (argv=%q)", a, argv)
		}
	}
}
