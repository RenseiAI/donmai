//go:build linux

package confinement

import (
	"context"
	"os"
	"testing"
	"time"
)

func newLinuxLiveConfiner(t *testing.T, backend Backend, extra ExtraRules) (*Confiner, string) {
	t.Helper()
	home, stateHome, profileDir := hostDirs(t)
	c, err := New(Options{Backend: backend, ProfileDir: profileDir, Home: home, StateHome: stateHome, ExtraRules: extra, ExecutableDigest: "sha256:test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, stateHome
}

func linuxProbeCommand(t *testing.T) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return []string{exe}
}

// linuxConfinementAvailable reports whether this host can run a confined
// seat at all: the launcher present, user namespaces creatable, and the
// Landlock stage stageable. The test skips — never passes silently — where
// any of them is missing, with the diagnostic Check carries.
func linuxConfinementAvailable(t *testing.T) bool {
	t.Helper()
	b := DefaultBackend()
	if b == nil {
		t.Skip("no confinement backend for this operating system")
	}
	if err := checkLauncher("bwrap"); err != nil {
		t.Skipf("the mount-namespace launcher is missing: %v", err)
	}
	if err := b.Check(); err != nil {
		t.Skipf("confinement unavailable here: %v", err)
	}
	if !landlockProbe() {
		t.Skip("the Landlock stage is unavailable on this kernel")
	}
	return true
}

// TestLinux_SelfTestPassesBothModes is the live backend proof: the probe
// set passes through the headless and the PTY spawn path under the
// mount-namespace backend — the confined seat cannot write outside the
// writable set or read hidden paths, and can write inside each writable
// class. It needs the launcher, user namespaces and Landlock, so it skips
// honestly where the kernel or container disallows them.
func TestLinux_SelfTestPassesBothModes(t *testing.T) {
	if !linuxConfinementAvailable(t) {
		return
	}
	c, _ := newLinuxLiveConfiner(t, DefaultBackend(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	record, err := c.SelfTest(ctx, SelfTestOptions{ProbeCommand: linuxProbeCommand(t), ScratchDir: shortTempDir(t, "dcs")})
	if err != nil {
		t.Fatalf("SelfTest: %v\n%s", err, failureIDs(record))
	}
	if !record.Passed || len(record.SessionModes) != 2 {
		t.Fatalf("record passed=%v modes=%v\n%s", record.Passed, record.SessionModes, failureIDs(record))
	}
	if record.Backend != BackendLinuxMountNamespace {
		t.Fatalf("record backend = %q, want the mount-namespace backend", record.Backend)
	}
	t.Logf("self-test passed: %d probes across %v, backend %s", len(record.Probes), record.SessionModes, record.BackendVersion)
}

// TestLinux_SelfTestRedWithoutBackend is the discriminating control: with
// the backend replaced by nothing, every probe that expects a refusal —
// each one held by the backend and nothing else — fails, in both session
// modes, while every positive control still passes.
func TestLinux_SelfTestRedWithoutBackend(t *testing.T) {
	c, _ := newLinuxLiveConfiner(t, noopBackend{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	record, err := c.SelfTest(ctx, SelfTestOptions{ProbeCommand: linuxProbeCommand(t), ScratchDir: shortTempDir(t, "dcs")})
	if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("SelfTest with no backend: err=%v, want self_test_failed", err)
	}
	if len(record.Failures()) == 0 {
		t.Fatal("SelfTest with no backend failed without a failing probe")
	}
}
