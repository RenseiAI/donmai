package daemon

import (
	"strings"
	"testing"
	"time"
)

// TestDefaultWorkerCommand_ReturnsNilUnderGoTest verifies the test-
// binary guard. When the running executable is a Go test binary
// (`<pkg>.test`), defaultWorkerCommand returns nil so callers fall
// through to the spawner's /bin/sh stub instead of recursing into
// the test runner. (REN-1461 / F.2.8.)
func TestDefaultWorkerCommand_ReturnsNilUnderGoTest(t *testing.T) {
	got := defaultWorkerCommand()
	// Two acceptable outcomes:
	//   1. nil (preferred — test-binary detection caught it).
	//   2. resolves to `af` on PATH (the developer machine has a
	//      production-installed `af` — also fine, but in CI we
	//      expect outcome 1).
	if got == nil {
		return
	}
	if len(got) >= 1 && strings.HasSuffix(got[0], "/af") {
		t.Logf("defaultWorkerCommand resolved to PATH-installed af: %v (OK on dev machines)", got)
		return
	}
	t.Errorf("defaultWorkerCommand under go test = %v; expected nil or PATH-installed af", got)
}

// TestIsGoTestBinary_DetectsTestSuffix exercises the heuristic.
func TestIsGoTestBinary_DetectsTestSuffix(t *testing.T) {
	cases := map[string]bool{
		"/tmp/daemon.test":         true,
		"/usr/local/bin/af":        false,
		"/var/folders/x/y/z.test":  true,
		"/var/folders/x/y/z":       false,
		"/private/tmp/server.test": true,
	}
	for path, want := range cases {
		if got := isGoTestBinary(path); got != want {
			t.Errorf("isGoTestBinary(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestSpawner_DoesNotRecursivelySpawnTestBinary asserts the spawner
// honors the test-binary guard end-to-end. With no explicit
// WorkerCommand, NewWorkerSpawner + AcceptWork should NOT spawn the
// test binary. We verify by asserting the (empty WorkerCommand →
// /bin/sh stub) fallback fires and the session exits cleanly.
//
// This is the regression guard for the "tests hung for 60s" failure
// mode caught during F.2.8 development.
func TestSpawner_DoesNotRecursivelySpawnTestBinary(t *testing.T) {
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "x", Repository: "github.com/foo/bar"}},
		MaxConcurrentSessions: 1,
		// No WorkerCommand → falls through to /bin/sh stub.
	})
	if _, err := s.AcceptWork(SessionSpec{
		SessionID:  "guard-1",
		Repository: "github.com/foo/bar",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	// Guard is verified by completion under the package's existing
	// 60s timeout; if the guard fails, the test binary recurses and
	// hangs. Spin briefly until the /bin/sh stub exits.
	deadline := time.Now().Add(2 * time.Second)
	for s.ActiveCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.ActiveCount() != 0 {
		t.Fatalf("session did not drain — guard regression?")
	}
}
