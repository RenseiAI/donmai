package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spawnerArgvSecretSentinel is the canary credential this test stamps on a
// work item's SessionSpec.Env. The spawner must hand it to the worker
// through its ENVIRONMENT (sessionEnv); the worker command's argv must
// carry no trace of it — any local user can read another process's command
// line via ps or /proc/<pid>/cmdline, so a credential placed there is
// readable host-wide, including by other seats running as the same user.
const spawnerArgvSecretSentinel = "sentinel-credential-must-not-ride-argv"

// TestSpawner_WorkerArgvCarriesNoSessionCredential spawns a real worker
// process through AcceptWork with a sentinel session credential and asserts
// the child's recorded argv carries no trace of it while its environment
// does. The fake worker command records /proc/self/cmdline (Linux) or
// `ps -o args` (macOS) — the same command line an operator's `pgrep -fl`
// would show — plus its full environment, then exits 0.
//
// RED proof: interpolate any SessionSpec.Env value into the worker command
// (for example by appending KEY=value entries to WorkerCommand in spawn)
// and the argv assertion fails with the sentinel quoted in the failure
// output.
func TestSpawner_WorkerArgvCarriesNoSessionCredential(t *testing.T) {
	// Not parallel: spawns a real child process with a fixed recording dir.
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv")
	envPath := filepath.Join(dir, "child-env")
	script := "#!/bin/sh\n" +
		"if [ -r /proc/self/cmdline ]; then tr '\\0' '\\n' < /proc/self/cmdline > '" + strings.ReplaceAll(argvPath, "'", `'\''`) + "'\n" +
		"else ps -o args= -p $$ > '" + strings.ReplaceAll(argvPath, "'", `'\''`) + "'\n" +
		"fi\n" +
		"env > '" + strings.ReplaceAll(envPath, "'", `'\''`) + "'\n" +
		"exit 0\n"
	recorder := filepath.Join(dir, "fake-worker.sh")
	if err := os.WriteFile(recorder, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake worker: %v", err)
	}

	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "agentfactory", Repository: "github.com/foo/bar"}},
		MaxConcurrentSessions: 1,
		WorkerCommand:         []string{recorder},
	})
	ended := sessionEnds(s)
	_, err := s.AcceptWork(SessionSpec{
		SessionID:  "argv-secret-seat",
		Repository: "github.com/foo/bar",
		Ref:        "main",
		Env:        map[string]string{"SENTINEL_API_KEY": spawnerArgvSecretSentinel},
	})
	if err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	waitSessionEnd(t, ended)
	waitForActiveCount(t, s, 0)

	argvRaw := waitForSpawnerRecord(t, argvPath)
	if strings.Contains(argvRaw, spawnerArgvSecretSentinel) {
		t.Fatalf("worker child argv carries the sentinel session credential:\n%s", argvRaw)
	}
	envRaw := waitForSpawnerRecord(t, envPath)
	if !strings.Contains(envRaw, "SENTINEL_API_KEY="+spawnerArgvSecretSentinel) {
		t.Fatalf("sentinel session credential did not reach the worker environment:\n%s", envRaw)
	}
}

// waitForSpawnerRecord polls for a non-empty recorder file up to a bounded
// timeout. The fake worker writes both records as its first act, so
// presence proves the child was exec'd.
func waitForSpawnerRecord(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 { //nolint:gosec // test fixture path
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the worker child to record at %s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
