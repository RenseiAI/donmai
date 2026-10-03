//go:build darwin

package confinement

import (
	"context"
	"os"
	"testing"
	"time"
)

func newLiveConfiner(t *testing.T, backend Backend, extra ExtraRules) (*Confiner, string) {
	t.Helper()
	home, stateHome, profileDir := hostDirs(t)
	c, err := New(Options{Backend: backend, ProfileDir: profileDir, Home: home, StateHome: stateHome, ExtraRules: extra, ExecutableDigest: "sha256:test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, stateHome
}

func probeCommand(t *testing.T) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return []string{exe}
}

func runSelfTest(t *testing.T, c *Confiner) (SelfTestRecord, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	return c.SelfTest(ctx, SelfTestOptions{ProbeCommand: probeCommand(t), ScratchDir: shortTempDir(t, "dcs")})
}

// TestSeatbelt_SelfTestPassesBothModes is the live backend proof: the probe
// set passes through the headless and the PTY spawn path.
func TestSeatbelt_SelfTestPassesBothModes(t *testing.T) {
	c, _ := newLiveConfiner(t, DefaultBackend(), nil)
	record, err := runSelfTest(t, c)
	if err != nil {
		t.Fatalf("SelfTest: %v\n%s", err, failureIDs(record))
	}
	if !record.Passed || len(record.SessionModes) != 2 {
		t.Fatalf("record passed=%v modes=%v", record.Passed, record.SessionModes)
	}
	t.Logf("self-test passed: %d probes across %v, backend %s", len(record.Probes), record.SessionModes, record.BackendVersion)
}

// probesHeldByTheBackend are the probes the boundary, and nothing else,
// holds: with the backend replaced by nothing they must each turn red.
var probesHeldByTheBackend = []string{
	"outside.home.create",
	"outside.home.write",
	"outside.state_home.write",
	"outside.shared_tmp.create",
	"outside.user_tmp.create",
	"outside.user_cache.create",
	"outside.git_common_dir.write",
	"outside.sibling_session.write",
	"outside.workarea_root.create_direct",
	"ro.write",
	"ro.create",
	"ro.rename_within",
	"ro.remove",
	"ro.chmod",
	"ro.hardlink_from_mutable",
	"ro.symlink_write_from_mutable",
	"ro.mount_over",
	"protected.write",
	"protected.rename_ancestor",
	"protected.rename_state",
	"widen.reenter_backend",
	"widen.job_launcher",
	"widen.open_application",
	"widen.service_lookup.apple_events",
	"widen.service_lookup.launch_services",
	"widen.service_lookup.mount",
	"widen.attach_process",
	"widen.socket_outside",
}

func failedIn(record SelfTestRecord) map[string]bool {
	failed := map[string]bool{}
	for _, probe := range record.Failures() {
		failed[string(probe.Mode)+"/"+probe.ID] = true
	}
	return failed
}

// TestSeatbelt_SelfTestRedWithoutBackend is the discriminating control of
// D1.5: with the backend replaced by nothing, the exact probes the backend
// holds fail, in both session modes.
func TestSeatbelt_SelfTestRedWithoutBackend(t *testing.T) {
	c, _ := newLiveConfiner(t, noopBackend{}, nil)
	record, err := runSelfTest(t, c)
	if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("SelfTest with no backend: err=%v, want self_test_failed", err)
	}
	if record.Passed || len(record.SessionModes) != 0 {
		t.Fatalf("record passed=%v modes=%v, want no attested mode", record.Passed, record.SessionModes)
	}
	failed := failedIn(record)
	for _, mode := range []string{"autonomous", "human_controlled"} {
		for _, id := range probesHeldByTheBackend {
			if !failed[mode+"/"+id] {
				t.Errorf("%s/%s passed with no backend; the probe does not discriminate", mode, id)
			}
		}
	}
	if _, err := c.Prepare(Spec{SessionMode: "autonomous"}); err == nil {
		t.Fatal("Prepare succeeded after a failed self-test")
	} else if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("Prepare after a failed self-test: %v, want self_test_failed", err)
	}
}

// TestSeatbelt_SelfTestRedWithChmod: same-identity permission bits are not
// enforcement (ADR-2026-08-22 D6.7); the permission-change probe on the
// read-only leaf and every outside probe turn red.
func TestSeatbelt_SelfTestRedWithChmod(t *testing.T) {
	c, _ := newLiveConfiner(t, chmodBackend{}, nil)
	record, err := runSelfTest(t, c)
	if reason, _ := ReasonOf(err); reason != ReasonSelfTestFailed {
		t.Fatalf("SelfTest with chmod: err=%v, want self_test_failed", err)
	}
	failed := failedIn(record)
	for _, mode := range []string{"autonomous", "human_controlled"} {
		for _, id := range []string{"ro.chmod", "ro.chmod_leaf", "protected.chmod", "outside.home.create", "widen.reenter_backend"} {
			if !failed[mode+"/"+id] {
				t.Errorf("%s/%s passed under chmod", mode, id)
			}
		}
		if failed[mode+"/ro.write"] {
			t.Errorf("%s/ro.write failed under chmod; the permission bits should have held that one", mode)
		}
	}
}
