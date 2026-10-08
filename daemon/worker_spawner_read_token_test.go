package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimeenv "github.com/RenseiAI/donmai/runtime/env"
)

// spawnerReadTokenSentinel is the canary credential planted in the daemon
// process's own environment. It names no live session: whatever the daemon
// inherited must not ride into a worker beside that worker's own
// explicitly stated read credential. The worker command records its real
// child environment, so the answer is what the worker process actually
// reads — not what one composition helper returns.
const spawnerReadTokenSentinel = "sentinel-inherited-read-credential"

// TestAcceptWork_BothSpawnPathsStripInheritedReadToken drives a session
// accept through both spawn paths with a stale read credential in the
// daemon's inherited environment and asserts the real worker environment
// carries the session's own stated credential but never the inherited
// sentinel: the direct child's recorded env plus the shim launch env.
//
// RED: compose the parent layer without the read-token strip and the child
// env assertion fails with the sentinel quoted in the failure output.
func TestAcceptWork_BothSpawnPathsStripInheritedReadToken(t *testing.T) {
	t.Setenv(runtimeenv.SessionReadTokenEnv, spawnerReadTokenSentinel)

	// The session's own credential is minted by the detail store, exactly
	// as a production accept does; spawning without one would prove
	// nothing about the strip-versus-state distinction.
	d, _, cleanup := mustStartDaemon(t)
	defer cleanup()
	detail := &SessionDetail{SessionID: "sess-strip-direct", AuthToken: "tok"}
	if _, err := d.AcceptWorkWithDetail(SessionSpec{
		SessionID: "sess-strip-direct", Repository: "github.com/foo/bar", Ref: "main",
	}, detail); err != nil {
		t.Fatalf("AcceptWorkWithDetail: %v", err)
	}
	stated, ok := d.sessionReadToken("sess-strip-direct")
	if !ok || stated == "" {
		t.Fatal("no read credential minted for sess-strip-direct")
	}

	t.Run("direct", func(t *testing.T) {
		dir := t.TempDir()
		envPath := filepath.Join(dir, "child-env")
		script := "#!/bin/sh\nenv > '" + strings.ReplaceAll(envPath, "'", `'\''`) + "'\nexit 0\n"
		recorder := filepath.Join(dir, "fake-worker.sh")
		if err := os.WriteFile(recorder, []byte(script), 0o700); err != nil { //nolint:gosec // test fixture
			t.Fatalf("write fake worker: %v", err)
		}
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "demo", Repository: "github.com/foo/bar"}},
			MaxConcurrentSessions: 1,
			WorkerCommand:         []string{recorder},
			SessionReadToken:      d.sessionReadToken,
		})
		ended := sessionEnds(s)
		if _, err := s.AcceptWork(SessionSpec{
			SessionID: "sess-strip-direct", Repository: "github.com/foo/bar", Ref: "main",
		}); err != nil {
			t.Fatalf("AcceptWork: %v", err)
		}
		waitSessionEnd(t, ended)
		raw := waitForSpawnerRecord(t, envPath)
		assertWorkerReadTokenEnv(t, "direct child", raw, stated)
	})

	t.Run("shim", func(t *testing.T) {
		got := make(chan []string, 1)
		s := NewWorkerSpawner(SpawnerOptions{
			Projects:              []ProjectConfig{{ID: "demo", Repository: "github.com/foo/bar"}},
			MaxConcurrentSessions: 1,
			WorkerCommand:         []string{"/bin/sh", "-c", "exit 0"},
			SessionReadToken:      d.sessionReadToken,
			ShimOwns:              func(SessionSpec) bool { return true },
			ShimSpawn: func(spec SessionSpec, _ ProjectConfig, env []string) (*SessionHandle, error) {
				select {
				case got <- append([]string(nil), env...):
				default:
				}
				return &SessionHandle{SessionID: spec.SessionID, State: SessionRunning}, nil
			},
		})
		detail := &SessionDetail{SessionID: "sess-strip-shim", AuthToken: "tok"}
		if _, err := d.AcceptWorkWithDetail(SessionSpec{
			SessionID: "sess-strip-shim", Repository: "github.com/foo/bar", Ref: "main",
		}, detail); err != nil {
			t.Fatalf("AcceptWorkWithDetail: %v", err)
		}
		if _, err := s.AcceptWork(SessionSpec{
			SessionID: "sess-strip-shim", Repository: "github.com/foo/bar", Ref: "main",
		}); err != nil {
			t.Fatalf("AcceptWork: %v", err)
		}
		var env []string
		select {
		case env = <-got:
		case <-time.After(spawnerWaitTimeout):
			t.Fatal("timed out waiting for the shim launch")
		}
		stated, ok := d.sessionReadToken("sess-strip-shim")
		if !ok || stated == "" {
			t.Fatal("no read credential minted for sess-strip-shim")
		}
		assertWorkerReadTokenEnv(t, "shim launch", strings.Join(env, "\n"), stated)
	})
}

// assertWorkerReadTokenEnv asserts a worker environment carries exactly the
// session's own stated read credential and never the inherited sentinel.
func assertWorkerReadTokenEnv(t *testing.T, where, raw, stated string) {
	t.Helper()
	if strings.Contains(raw, spawnerReadTokenSentinel) {
		t.Errorf("%s env carries the inherited read credential:\n%s", where, raw)
	}
	key := runtimeenv.SessionReadTokenEnv + "="
	found := ""
	for _, line := range strings.Split(raw, "\n") {
		if v, ok := strings.CutPrefix(line, key); ok {
			found = v
		}
	}
	if found == "" {
		t.Errorf("%s env states no %s entry; the worker cannot read its own detail", where, runtimeenv.SessionReadTokenEnv)
	} else if found != stated {
		t.Errorf("%s env %s names the wrong credential", where, runtimeenv.SessionReadTokenEnv)
	}
}
