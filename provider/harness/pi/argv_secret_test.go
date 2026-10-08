package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// argvSecretSentinel is the canary credential every test in this file injects
// on Spec.Env. It must reach the harness session through the owner-only
// session credential file and must never appear in the child's argv — any
// local user can read another process's command line via ps or
// /proc/<pid>/cmdline, so a credential placed there is readable host-wide —
// nor in its exec environment, which the renamed pi process exposes the
// same way (child_env.go).
const argvSecretSentinel = "sentinel-credential-must-not-ride-argv"

// fakeArgvRecorderPi writes a fake `pi` binary that records the exact argv
// the OS delivered it (one argument per line) plus its full environment to
// files under dir, then sleeps so the parent can inspect both before the
// handshake gate gives up. It answers --version like the real probe so
// New()'s version pin labels it verified.
//
// Reading /proc/self/cmdline (Linux) records the NUL-separated argv the
// kernel holds — the same bytes `ps` renders. Where /proc is absent (macOS)
// it falls back to `ps -o args= -p $$`, which renders the same command
// line an operator's `pgrep -fl` would show.
func fakeArgvRecorderPi(t *testing.T, dir string) string {
	t.Helper()
	argvPath := filepath.Join(dir, "argv")
	envPath := filepath.Join(dir, "child-env")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo \"" + PinnedVersion + "\"; exit 0; fi\n" +
		"if [ -r /proc/self/cmdline ]; then tr '\\0' '\\n' < /proc/self/cmdline > " + shQuote(argvPath) + "\n" +
		"else ps -o args= -p $$ > " + shQuote(argvPath) + "\n" +
		"fi\n" +
		"env > " + shQuote(envPath) + "\n" +
		"sleep 30\n"
	path := filepath.Join(dir, "fake-pi-argv-recorder.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake pi: %v", err)
	}
	return path
}

// shQuote single-quotes a path for embedding in the fake binary script.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// waitForArgvRecord polls for the recorder's argv file up to timeout. The
// fake binary writes it as its first act, so its presence proves the child
// was exec'd with the production argv.
func waitForArgvRecord(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 { //nolint:gosec // test fixture path
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the harness child to record its argv at %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHeadlessChildArgv_CarriesNoCredential is the argv-secret regression
// test: a real Provider.Spawn against a fake `pi` binary with a sentinel
// credential on Spec.Env must deliver the sentinel to the session through
// the credential file (its environment section: the name is no model
// credential pi resolves, so the policy extension restores it for the
// tools pi starts) while the child's recorded argv AND exec environment
// carry no trace of it. The spawn itself is expected to fail (the fake never
// answers the policy handshake) — what matters is the child was exec'd
// first, with the production argv and env, which the recorder files prove.
//
// RED proof: move any Spec.Env value onto the spawn argv (for example by
// prepending KEY=value entries in spawnChild) and the argv assertion fails
// with the sentinel quoted in the failure output.
func TestHeadlessChildArgv_CarriesNoCredential(t *testing.T) {
	// Not parallel: spawns a real child process with a fixed recording dir.
	if os.Getenv("SHELL") == "" && os.Getenv("OS") != "" {
		t.Skip("fake pi recorder needs /bin/sh")
	}
	dir := t.TempDir()
	fake := fakeArgvRecorderPi(t, dir)

	p, err := New(Options{
		PiBin:            fake,
		HandshakeTimeout: 30 * time.Second,
		VersionProbe:     func(context.Context, string) (string, error) { return PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	spawnErr := make(chan error, 1)
	go func() {
		_, err := p.Spawn(ctx, agent.Spec{
			Cwd:    t.TempDir(),
			Prompt: "prove argv carries no credential",
			Env:    map[string]string{"SENTINEL_API_KEY": argvSecretSentinel},
		})
		spawnErr <- err
	}()

	argv := waitForArgvRecord(t, filepath.Join(dir, "argv"), 20*time.Second)
	envRaw := waitForArgvRecord(t, filepath.Join(dir, "child-env"), 20*time.Second)
	// Read the session credential file while the session is live (Stop
	// removes it).
	var delivered string
	for _, entry := range strings.Split(envRaw, "\n") {
		if path, ok := strings.CutPrefix(entry, credentialFileEnvVar+"="); ok {
			raw, err := os.ReadFile(path) //nolint:gosec // the session-owned credential file under test
			if err != nil {
				t.Fatalf("read session credential file: %v", err)
			}
			environment, err := readSessionEnvironmentMap(raw)
			if err != nil {
				t.Fatalf("decode session credential file: %v", err)
			}
			delivered = environment["SENTINEL_API_KEY"]
		}
	}
	// The child is still sleeping inside the fake; cancel so Spawn's
	// handshake gate (and its Stop) reaps it before the test ends.
	cancel()
	select {
	case err := <-spawnErr:
		if err == nil {
			t.Fatal("Spawn against a handshake-silent fake unexpectedly succeeded")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Spawn did not return after context cancellation")
	}

	if strings.Contains(argv, argvSecretSentinel) {
		t.Fatalf("harness child argv carries the sentinel credential:\n%s", argv)
	}
	for _, entry := range strings.Split(envRaw, "\n") {
		if strings.Contains(entry, argvSecretSentinel) {
			t.Fatalf("harness child exec environment carries the sentinel credential: %q", entry)
		}
	}
	if delivered != argvSecretSentinel {
		t.Fatalf("sentinel credential was not delivered through the session credential file (got %q)", delivered)
	}
}

// TestHeadlessChildArgv_ BuildersCarryNoCredentialValue pins the same
// invariant one layer down: neither argv builder may interpolate a
// credential-shaped Spec.Env value into the argument list. Builders only
// ever emit flags, paths, model pins and prompts — secrets travel beside
// argv in childEnv, never inside it.
func TestHeadlessChildArgv_BuildersCarryNoCredentialValue(t *testing.T) {
	t.Parallel()

	spec := agent.Spec{
		Cwd:    "/tmp/work",
		Prompt: "do the thing",
		Model:  "test-model",
		Env: map[string]string{
			"SENTINEL_API_KEY": argvSecretSentinel,
			PiKeyEnvVar:        argvSecretSentinel,
		},
	}
	layout := sessionLayout{root: "/tmp/session"}

	for _, argv := range [][]string{
		rpcArgs(layout, []string{"/tmp/session/extension.js"}, launchPrompt, "", spec),
		interactiveArgs(spec, layout, []string{"/tmp/session/extension.js"}),
		modelPinArgs(spec),
	} {
		for _, a := range argv {
			if strings.Contains(a, argvSecretSentinel) {
				t.Fatalf("argv builder emitted the sentinel credential: %q (argv=%q)", a, argv)
			}
		}
	}
}
