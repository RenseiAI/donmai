//go:build unix

package muse

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestProbeHostSessionLogin_WedgedProbeReapsProcessGroup pins the F5
// discipline through the production probe constructor: a wedged `muse
// exec` that forks a helper and then hangs must leave no orphan behind
// — the probe owns its child in a process group and the context expiry
// kills the group, never just the leader. RED proof: drop the
// configureProcessGroup/Cancel group kill and the backgrounded helper
// outlives the probe (waitGone fails after SIGKILLing the stray).
func TestProbeHostSessionLogin_WedgedProbeReapsProcessGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("wedged-probe test shells out and sleeps")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "helper.pid")
	bin := filepath.Join(dir, "fake-muse")
	script := "#!/bin/sh\n" +
		"sleep 300 </dev/null >/dev/null 2>&1 &\n" +
		"echo $! > \"" + pidFile + "\"\n" +
		"exec sleep 300\n"
	if err := os.WriteFile(bin, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture script
		t.Fatalf("write script: %v", err)
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // test fixture needs exec bit
		t.Fatalf("chmod script: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := probeHostSessionLoginWithEnv(ctx, bin, []string{"PATH=" + os.Getenv("PATH"), "NO_COLOR=1"}); err == nil {
		t.Fatal("expected refusal on a wedged probe, got nil")
	}
	// The leader is dead with the probe; the forked helper must be too.
	waitGone(t, readHelperPid(t, pidFile))
}
