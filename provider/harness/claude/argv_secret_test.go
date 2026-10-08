package claude

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

// fakeArgvEnvRecorderCLI writes a fake claude CLI that records the exact argv
// the OS delivered it (one argument per line) plus its full environment to
// files under dir, then answers with a minimal completed-turn envelope so
// Spawn's child starts cleanly. Reading /proc/self/cmdline (Linux) records
// the NUL-separated argv the kernel holds — the same bytes `ps` renders.
// Where /proc is absent (macOS) it falls back to `ps -o args= -p $$`.
func fakeArgvEnvRecorderCLI(t *testing.T, dir string) string {
	t.Helper()
	argvPath := filepath.Join(dir, "argv")
	envPath := filepath.Join(dir, "child-env")
	script := "#!/bin/sh\n" +
		"if [ -r /proc/self/cmdline ]; then tr '\\0' '\\n' < /proc/self/cmdline > " + shQuote(argvPath) + "\n" +
		"else ps -o args= -p $$ > " + shQuote(argvPath) + "\n" +
		"fi\n" +
		"env > " + shQuote(envPath) + "\n" +
		`printf '{"type":"system","subtype":"init","session_id":"sess-argv-secret"}\n'` + "\n" +
		`printf '{"type":"result","subtype":"success","is_error":false,"num_turns":1}\n'` + "\n"
	return writeFakeCLI(t, "fake-claude-argv-recorder.sh", script)
}

// TestSpawn_Headless_SentinelCredentialRidesEnvNotArgv is the argv-secret
// regression test for the headless lane: a real Provider.Spawn with a
// sentinel credential on Spec.Env must deliver it through the child's
// environment while the child's recorded argv carries no trace of it.
//
// RED proof: interpolate any Spec.Env value into buildArgs (for example by
// appending KEY=value entries to the argv) and the argv assertion fails with
// the sentinel quoted in the failure output.
func TestSpawn_Headless_SentinelCredentialRidesEnvNotArgv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cli := fakeArgvEnvRecorderCLI(t, dir)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{
		Prompt: "prove argv carries no credential",
		Cwd:    dir,
		Env:    map[string]string{"SENTINEL_API_KEY": argvSecretSentinel},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	_ = drainAllWithIdle(t, h.Events(), 5*time.Second, 30*time.Second)

	argvRaw, err := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read recorded child argv: %v", err)
	}
	if strings.Contains(string(argvRaw), argvSecretSentinel) {
		t.Fatalf("CLI child argv carries the sentinel credential:\n%s", argvRaw)
	}
	envRaw, err := os.ReadFile(filepath.Join(dir, "child-env")) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read recorded child env: %v", err)
	}
	if !strings.Contains(string(envRaw), "SENTINEL_API_KEY="+argvSecretSentinel) {
		t.Fatalf("sentinel credential did not reach the child environment:\n%s", envRaw)
	}
}

// TestInteractiveArgs_CarryNoCredentialValue pins the same invariant for the
// interactive lane's argv builder: no Spec.Env value may be interpolated
// into the REPL argument list. Secrets travel beside argv in Spec.Env (which
// ptycli layers onto the PTY child environment), never inside it.
func TestInteractiveArgs_CarryNoCredentialValue(t *testing.T) {
	t.Parallel()

	spec := agent.Spec{
		Prompt:     "prove argv carries no credential",
		Model:      "test-model",
		Autonomous: true,
		Env:        map[string]string{"SENTINEL_API_KEY": argvSecretSentinel},
	}
	for _, argv := range [][]string{
		interactiveArgs(spec),
		interactiveArgsWith(spec, "/tmp/mcp.json", `{"hooks":{}}`),
	} {
		for _, a := range argv {
			if strings.Contains(a, argvSecretSentinel) {
				t.Fatalf("interactive argv carries the sentinel credential: %q (argv=%q)", a, argv)
			}
		}
	}
	argv, stdinPrompt := buildArgs(spec, "", "")
	for _, a := range argv {
		if strings.Contains(a, argvSecretSentinel) {
			t.Fatalf("headless argv carries the sentinel credential: %q (argv=%q)", a, argv)
		}
	}
	if strings.Contains(stdinPrompt, argvSecretSentinel) {
		t.Fatal("headless stdin prompt carries the sentinel credential: the prompt must never carry secrets")
	}
}
