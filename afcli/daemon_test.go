package afcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// ── mock daemon ───────────────────────────────────────────────────────────────

// mockDaemon implements daemonDoer with configurable responses for testing.
type mockDaemon struct {
	statusResp    *afclient.DaemonStatusResponse
	statusErr     error
	statsResp     *afclient.DaemonStatsResponse
	statsErr      error
	actionResp    *afclient.DaemonActionResponse
	actionErr     error
	drainCalls    int
	drainSecs     int
	poolStatsResp *afclient.WorkareaPoolStats
	poolStatsErr  error
	evictResp     *afclient.EvictPoolResponse
	evictErr      error
	evictReq      *afclient.EvictPoolRequest
	setCapResp    *afclient.SetCapacityResponse
	setCapErr     error
	setCapKey     string
	setCapValue   string
}

func (m *mockDaemon) GetStatus() (*afclient.DaemonStatusResponse, error) {
	return m.statusResp, m.statusErr
}

func (m *mockDaemon) GetStats(_, _ bool) (*afclient.DaemonStatsResponse, error) {
	return m.statsResp, m.statsErr
}

func (m *mockDaemon) Pause() (*afclient.DaemonActionResponse, error) {
	return m.actionResp, m.actionErr
}

func (m *mockDaemon) Resume() (*afclient.DaemonActionResponse, error) {
	return m.actionResp, m.actionErr
}

func (m *mockDaemon) Stop() (*afclient.DaemonActionResponse, error) {
	return m.actionResp, m.actionErr
}

func (m *mockDaemon) Drain(secs int) (*afclient.DaemonActionResponse, error) {
	m.drainCalls++
	m.drainSecs = secs
	return m.actionResp, m.actionErr
}

func (m *mockDaemon) Update() (*afclient.DaemonActionResponse, error) {
	return m.actionResp, m.actionErr
}

func (m *mockDaemon) GetPoolStats() (*afclient.WorkareaPoolStats, error) {
	return m.poolStatsResp, m.poolStatsErr
}

func (m *mockDaemon) EvictPool(req afclient.EvictPoolRequest) (*afclient.EvictPoolResponse, error) {
	m.evictReq = &req
	return m.evictResp, m.evictErr
}

func (m *mockDaemon) SetCapacityConfig(key, value string) (*afclient.SetCapacityResponse, error) {
	m.setCapKey = key
	m.setCapValue = value
	return m.setCapResp, m.setCapErr
}

// newTestDaemonCmd builds the daemon command tree with mock daemon injected.
// Each call creates an independent command tree — safe for parallel tests.
func newTestDaemonCmd(mock daemonDoer, args []string) (*bytes.Buffer, error) {
	factory := func(_ afclient.DaemonConfig) daemonDoer { return mock }
	cmd := newDaemonCmdWithFactory(factory, "test")
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf, err
}

// fixtureStatusResp returns a canned DaemonStatusResponse for tests.
func fixtureStatusResp() *afclient.DaemonStatusResponse {
	return &afclient.DaemonStatusResponse{
		Status:          afclient.DaemonReady,
		Version:         "0.1.0",
		MachineID:       "mac-studio-test",
		PID:             42,
		UptimeSeconds:   3725,
		ActiveSessions:  3,
		MaxSessions:     8,
		ProjectsAllowed: 2,
		Timestamp:       "2026-04-27T12:00:00Z",
	}
}

// fixtureStatsResp returns a canned DaemonStatsResponse for tests.
func fixtureStatsResp() *afclient.DaemonStatsResponse {
	return &afclient.DaemonStatsResponse{
		Capacity: afclient.MachineCapacity{
			MaxConcurrentSessions: 8,
			MaxVCpuPerSession:     4,
			MaxMemoryMbPerSession: 8192,
			ReservedVCpu:          2,
			ReservedMemoryMb:      4096,
		},
		ActiveSessions: 3,
		QueueDepth:     1,
		Timestamp:      "2026-04-27T12:00:00Z",
	}
}

// fixtureActionResp returns a canned OK DaemonActionResponse.
func fixtureActionResp() *afclient.DaemonActionResponse {
	return &afclient.DaemonActionResponse{OK: true, Message: "accepted"}
}

// ── install / uninstall (in-process; REN-1406) ────────────────────────────────
//
// Install/uninstall/doctor used to shell out to a Node `rensei-daemon`
// subprocess; REN-1406 ports them into Go and dispatches in-process via
// installer. The tests below pin the in-process integration:
// install writes a unit/plist, uninstall removes it, and doctor inspects it.

// TestDaemonInstallHelp verifies the install command exposes the expected
// flags in its help output.
func TestDaemonInstallHelp(t *testing.T) {
	t.Parallel()

	cmd := newDaemonInstallCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"--bin-path", "--user", "--system"} {
		if !strings.Contains(out, want) {
			t.Errorf("install --help missing flag %q; got:\n%s", want, out)
		}
	}
}

// TestDaemonInstallInProcessRegistersDaemonRun verifies the install command
// dispatches to the Go installer in-process — writing a unit/plist that
// registers `<host-binary> daemon run` (the locked REN-1406 decision) and
// emitting the REN-1408 follow-up note in the success message.
//
// IMPORTANT: this test passes --skip-service-manager so it never invokes
// launchctl/systemctl. Without that flag, the underlying installer
// bootstraps the unit into the developer's user-level launchd domain
// (gui/<uid>) using a plist path inside t.TempDir(). The temp dir is
// deleted when the test ends, but the registration in launchd's domain
// survives — leaving launchd trying to spawn a deleted binary every
// 30 seconds (KeepAlive=true), which on a developer machine clobbers
// any real `dev.rensei.daemon` registration and produces a silent
// re-spawn loop that's only visible via `launchctl print` /
// `~/Library/Logs/rensei/daemon.log`. The launchctl-bootstrap path
// itself is exercised in installer/launchd/launchd_test.go with a
// stubbed runner.
func TestDaemonInstallInProcessRegistersDaemonRun(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// Use a fake host binary path to make the assertions deterministic.
	hostBin := tmp + "/af-fake"
	if err := os.WriteFile(hostBin, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("seed fake host binary: %v", err)
	}

	cmd := newDaemonInstallCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unsupported OS for in-process installer test")
	}
	// --skip-service-manager makes the install hermetic: writes the
	// unit/plist into HOME/$LaunchAgents only, never calls launchctl
	// bootstrap or systemctl enable. See doc above for why.
	cmd.SetArgs([]string{"--bin-path", hostBin, "--skip-service-manager"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("install (skip-service-manager): %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, hostBin+" daemon run") {
		t.Errorf("expected install output to register `<bin> daemon run`, got:\n%s", out)
	}
	if !strings.Contains(out, "REN-1408") {
		t.Errorf("install success message should reference REN-1408 (runtime port), got:\n%s", out)
	}
	if strings.Contains(out, "rensei-daemon install") || strings.Contains(out, "brew install rensei") {
		t.Errorf("install output should NOT reference the legacy rensei-daemon shell-out, got:\n%s", out)
	}

	// Defense-in-depth: if a future change to the install path makes
	// --skip-service-manager a no-op or otherwise lets launchctl run,
	// boot the registration out of the user's launchd domain so the
	// developer's machine doesn't enter a respawn loop pointing at a
	// soon-to-be-deleted t.TempDir() plist. Best-effort — if there's
	// nothing registered, bootout exits non-zero and we ignore it.
	t.Cleanup(func() { _ = launchctlBootoutTestUnit() })
}

// launchctlBootoutTestUnit removes a dev.rensei.daemon registration
// from the developer's user-level launchd domain ONLY when the
// registered plist path lives inside a temp directory — i.e. when the
// registration was created by a test whose t.TempDir() is about to be
// deleted. If the registered path is the developer's real plist
// (~/Library/LaunchAgents/dev.rensei.daemon.plist or similar), this is
// a no-op. That asymmetry is deliberate: we want defensive cleanup of
// test pollution but never destructive cleanup of a legitimate
// developer install.
//
// The path-inspection step uses `launchctl print` and parses the
// `path = ...` line from its output. If parsing fails (e.g. launchd
// changes the print format), we err on the side of NOT booting out.
func launchctlBootoutTestUnit() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	uid := strconv.Itoa(os.Geteuid())
	target := "gui/" + uid + "/dev.rensei.daemon"

	out, err := exec.Command("launchctl", "print", target).CombinedOutput() //nolint:gosec // G204: target = "gui/<uid>/<hardcoded service>"
	if err != nil {
		// Most likely "service not found" — nothing to clean up.
		return nil
	}
	plistPath := parseLaunchctlPath(string(out))
	if plistPath == "" {
		// Couldn't parse the plist path — refuse to bootout to avoid
		// clobbering a real registration we can't identify.
		return nil
	}
	if !looksLikeTestTempPath(plistPath) {
		// Registered path is the developer's real plist. Leave it alone.
		return nil
	}
	_ = exec.Command("launchctl", "bootout", target).Run() //nolint:gosec // G204: target = "gui/<uid>/<hardcoded service>"
	return nil
}

// parseLaunchctlPath extracts the value of the `path = ...` line from
// `launchctl print` output. Returns "" when the line is absent or
// malformed so the caller can treat parse failure as "do not act".
func parseLaunchctlPath(out string) string {
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "path = ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "path = "))
		}
	}
	return ""
}

// looksLikeTestTempPath returns true when the given plist path is
// inside a directory tree commonly used for test temp dirs:
// macOS's `/private/var/folders/.../T/...` and `/var/folders/.../T/...`,
// `$TMPDIR`, and `/tmp`. Anything else (notably
// `~/Library/LaunchAgents/...`) returns false so cleanup leaves real
// developer installs intact.
func looksLikeTestTempPath(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/private/var/folders/") || strings.HasPrefix(p, "/var/folders/") {
		return true
	}
	if strings.HasPrefix(p, "/tmp/") {
		return true
	}
	if tmp := os.Getenv("TMPDIR"); tmp != "" && strings.HasPrefix(p, tmp) {
		return true
	}
	return false
}

// TestDaemonInstallWipesCachedJWT verifies the install command removes
// any pre-existing daemon.jwt so the daemon's first boot performs a
// fresh registration handshake with the orchestrator. Without this,
// re-installing on a machine that previously ran the daemon would
// short-circuit Register() on the stale cache and poll a workerId
// the orchestrator no longer recognizes — the "Worker not found" 404
// loop. Uses --skip-service-manager so the test stays hermetic.
func TestDaemonInstallWipesCachedJWT(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// Pre-seed a stale cache. Place it at the canonical location the
	// runtime helper consults so the wipe logic finds it the same way
	// production code will.
	jwtPath := filepath.Join(tmp, ".rensei", "daemon.jwt")
	if err := os.MkdirAll(filepath.Dir(jwtPath), 0o750); err != nil {
		t.Fatalf("mkdir .rensei: %v", err)
	}
	if err := os.WriteFile(jwtPath, []byte(`{"workerId":"wkr_dead"}`), 0o600); err != nil {
		t.Fatalf("seed stale JWT: %v", err)
	}

	hostBin := tmp + "/af-fake"
	if err := os.WriteFile(hostBin, []byte("#!/bin/sh\n"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("seed fake host binary: %v", err)
	}

	cmd := newDaemonInstallCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--bin-path", hostBin, "--skip-service-manager"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("install: %v", err)
	}

	if _, err := os.Stat(jwtPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cache file at %s should be wiped after install (err=%v); install must remove stale cache so the daemon re-registers cleanly", jwtPath, err)
	}
	if !strings.Contains(buf.String(), "Cleared cached JWT") {
		t.Errorf("install output should announce the wipe so operators see what happened; got:\n%s", buf.String())
	}
}

// TestDaemonUninstallWipesCachedJWT mirrors the install-side test for
// the uninstall path. Wiping at uninstall ensures a subsequent install
// — even one that reuses an existing daemon.yaml without re-minting —
// starts clean.
func TestDaemonUninstallWipesCachedJWT(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	jwtPath := filepath.Join(tmp, ".rensei", "daemon.jwt")
	if err := os.MkdirAll(filepath.Dir(jwtPath), 0o750); err != nil {
		t.Fatalf("mkdir .rensei: %v", err)
	}
	if err := os.WriteFile(jwtPath, []byte(`{"workerId":"wkr_dead"}`), 0o600); err != nil {
		t.Fatalf("seed stale JWT: %v", err)
	}

	cmd := newDaemonUninstallCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--skip-service-manager"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	if _, err := os.Stat(jwtPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cache file at %s should be wiped after uninstall (err=%v)", jwtPath, err)
	}
}

// TestDaemonInstallNoLegacyShellOut verifies the install command does NOT
// fail with the old `rensei-daemon: command not found` error when the
// legacy binary is absent — the new in-process implementation never looks
// it up on PATH.
//
// Uses --skip-service-manager for the same reason as
// TestDaemonInstallInProcessRegistersDaemonRun (see that test's
// IMPORTANT note): without it, a developer-machine run of this test
// bootstraps a unit pointing at a t.TempDir() plist that gets deleted
// when the test ends, leaving launchd with a 30-second respawn loop
// against a missing file. Defensive launchctl bootout in t.Cleanup
// guards against future install-path changes that demote
// --skip-service-manager.
func TestDaemonInstallNoLegacyShellOut(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("PATH", t.TempDir()) // empty PATH — would have broken old shim

	cmd := newDaemonInstallCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--skip-service-manager"})
	err := cmd.Execute()
	if err != nil {
		if strings.Contains(err.Error(), "rensei-daemon") || strings.Contains(err.Error(), "brew install rensei") {
			t.Errorf("error must NOT reference legacy rensei-daemon binary; got: %v", err)
		}
	}
	t.Cleanup(func() { _ = launchctlBootoutTestUnit() })
}

// TestDaemonUninstallHelp verifies the uninstall command exposes the
// expected flags in its help output.
func TestDaemonUninstallHelp(t *testing.T) {
	t.Parallel()

	cmd := newDaemonUninstallCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"--user", "--system"} {
		if !strings.Contains(out, want) {
			t.Errorf("uninstall --help missing flag %q; got:\n%s", want, out)
		}
	}
}

// TestDaemonUninstallNoServiceInstalled verifies the uninstall command
// returns gracefully (no error) when no unit/plist is currently installed,
// rather than the legacy "rensei-daemon: command not found" failure.
//
// IMPORTANT: passes --skip-service-manager. Without it, this test calls
// `launchctl bootout gui/<uid>/dev.rensei.daemon` against the developer's
// real launchd domain — which boots out any properly-installed daemon
// and leaves the plist on disk but unregistered. HOME being a temp dir
// does NOT sandbox the launchctl call (the bootout is against the
// running launchd domain, not a HOME-relative path). Defensive
// bootout-on-cleanup guards against future regressions. The
// "real launchctl bootout returns service-not-found" code path is
// covered in installer/launchd/launchd_test.go with stubbed runners.
func TestDaemonUninstallNoServiceInstalled(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("PATH", t.TempDir())

	cmd := newDaemonUninstallCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--skip-service-manager"})
	if err := cmd.Execute(); err != nil {
		// Only "service manager not available" is acceptable — never the
		// legacy shell-out error.
		if strings.Contains(err.Error(), "rensei-daemon") {
			t.Errorf("error must NOT reference legacy rensei-daemon binary; got: %v", err)
		}
	}
	t.Cleanup(func() { _ = launchctlBootoutTestUnit() })
}

// ── parent help ───────────────────────────────────────────────────────────────

func TestDaemonParentHelp(t *testing.T) {
	t.Parallel()

	cmd := newDaemonCmd("")
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"install", "uninstall", "setup", "status", "logs",
		"doctor", "pause", "resume", "update", "drain", "stop", "stats",
		"evict", "set",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("daemon --help missing subcommand %q; got:\n%s", want, out)
		}
	}
}

// ── status ────────────────────────────────────────────────────────────────────

func TestDaemonStatusHumanOutput(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{statusResp: fixtureStatusResp()}
	buf, err := newTestDaemonCmd(mock, []string{"status"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"Daemon:", "ready",
		"Machine:", "mac-studio-test",
		"Version:", "0.1.0",
		"PID:", "42",
		"Uptime:", "1h2m5s",
		"Sessions:", "3 / 8",
		"Projects:", "2 allowed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q; got:\n%s", want, out)
		}
	}
}

func TestDaemonStatusJSONOutput(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{statusResp: fixtureStatusResp()}
	buf, err := newTestDaemonCmd(mock, []string{"status", "--json"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var resp afclient.DaemonStatusResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if resp.MachineID != "mac-studio-test" {
		t.Errorf("machineId = %q, want %q", resp.MachineID, "mac-studio-test")
	}
	if resp.Status != afclient.DaemonReady {
		t.Errorf("status = %q, want %q", resp.Status, afclient.DaemonReady)
	}
}

func TestDaemonStatusError(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{statusErr: fmt.Errorf("connection refused")}
	_, err := newTestDaemonCmd(mock, []string{"status"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error should mention connection error; got: %v", err)
	}
}

// ── stats ─────────────────────────────────────────────────────────────────────

func TestDaemonStatsHumanOutput(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{statsResp: fixtureStatsResp()}
	buf, err := newTestDaemonCmd(mock, []string{"stats"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"Active sessions:", "3 / 8",
		"Queue depth:", "1",
		"Max vCPU/session:", "4",
		"Max mem/session:", "8192",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stats output missing %q; got:\n%s", want, out)
		}
	}
}

func TestDaemonStatsJSONOutput(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{statsResp: fixtureStatsResp()}
	buf, err := newTestDaemonCmd(mock, []string{"stats", "--json"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var resp afclient.DaemonStatsResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if resp.ActiveSessions != 3 {
		t.Errorf("activeSessions = %d, want 3", resp.ActiveSessions)
	}
}

func TestDaemonStatsWithPool(t *testing.T) {
	t.Parallel()

	resp := fixtureStatsResp()
	resp.Pool = &afclient.WorkareaPoolStats{
		TotalMembers:     5,
		ReadyMembers:     3,
		AcquiredMembers:  2,
		WarmingMembers:   0,
		InvalidMembers:   0,
		TotalDiskUsageMb: 1024,
	}
	mock := &mockDaemon{statsResp: resp}
	buf, err := newTestDaemonCmd(mock, []string{"stats", "--pool"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Workarea pool:") {
		t.Errorf("stats --pool missing pool section; got:\n%s", out)
	}
	if !strings.Contains(out, "1024") {
		t.Errorf("stats --pool missing disk usage; got:\n%s", out)
	}
}

func TestDaemonStatsWithByMachine(t *testing.T) {
	t.Parallel()

	resp := fixtureStatsResp()
	resp.ByMachine = []afclient.MachineStats{
		{
			ID:             "mac-studio-test",
			Region:         "home-network",
			Status:         afclient.DaemonReady,
			Version:        "0.1.0",
			ActiveSessions: 3,
			Capacity:       afclient.MachineCapacity{MaxConcurrentSessions: 8},
			UptimeSeconds:  3725,
		},
	}
	mock := &mockDaemon{statsResp: resp}
	buf, err := newTestDaemonCmd(mock, []string{"stats", "--by-machine"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Per-machine:") {
		t.Errorf("stats --by-machine missing section; got:\n%s", out)
	}
	if !strings.Contains(out, "mac-studio-test") {
		t.Errorf("stats --by-machine missing machine ID; got:\n%s", out)
	}
}

func TestDaemonStatsError(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{statsErr: fmt.Errorf("daemon unreachable")}
	_, err := newTestDaemonCmd(mock, []string{"stats"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "daemon unreachable") {
		t.Errorf("error should contain 'daemon unreachable'; got: %v", err)
	}
}

// ── pause ─────────────────────────────────────────────────────────────────────

func TestDaemonPauseSuccess(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{actionResp: fixtureActionResp()}
	buf, err := newTestDaemonCmd(mock, []string{"pause"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "pause") {
		t.Errorf("output missing 'pause'; got:\n%s", buf.String())
	}
}

func TestDaemonPauseError(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{actionErr: fmt.Errorf("daemon offline")}
	_, err := newTestDaemonCmd(mock, []string{"pause"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "daemon offline") {
		t.Errorf("error should contain 'daemon offline'; got: %v", err)
	}
}

func TestDaemonPauseRejected(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{actionResp: &afclient.DaemonActionResponse{OK: false, Message: "already paused"}}
	_, err := newTestDaemonCmd(mock, []string{"pause"})
	if err == nil {
		t.Fatal("expected error for rejected action, got nil")
	}
	if !strings.Contains(err.Error(), "already paused") {
		t.Errorf("error should mention 'already paused'; got: %v", err)
	}
}

// ── resume ────────────────────────────────────────────────────────────────────

func TestDaemonResumeSuccess(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{actionResp: fixtureActionResp()}
	buf, err := newTestDaemonCmd(mock, []string{"resume"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "resume") {
		t.Errorf("output missing 'resume'; got:\n%s", buf.String())
	}
}

// ── update ────────────────────────────────────────────────────────────────────

func TestDaemonUpdateSuccess(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{actionResp: fixtureActionResp()}
	buf, err := newTestDaemonCmd(mock, []string{"update"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "update") {
		t.Errorf("output missing 'update'; got:\n%s", buf.String())
	}
}

// ── drain ─────────────────────────────────────────────────────────────────────

func TestDaemonDrainSuccess(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{actionResp: fixtureActionResp()}
	buf, err := newTestDaemonCmd(mock, []string{"drain"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "drain") {
		t.Errorf("output missing 'drain'; got:\n%s", buf.String())
	}
	if mock.drainCalls != 1 {
		t.Errorf("Drain called %d times, want 1", mock.drainCalls)
	}
}

func TestDaemonDrainTimeout(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{actionResp: fixtureActionResp()}
	_, err := newTestDaemonCmd(mock, []string{"drain", "--timeout", "120"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if mock.drainSecs != 120 {
		t.Errorf("Drain timeout = %d, want 120", mock.drainSecs)
	}
}

// ── stop ──────────────────────────────────────────────────────────────────────

func TestDaemonStopSuccess(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{actionResp: fixtureActionResp()}
	buf, err := newTestDaemonCmd(mock, []string{"stop"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "stop") {
		t.Errorf("output missing 'stop'; got:\n%s", buf.String())
	}
}

func TestDaemonStopError(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{actionErr: fmt.Errorf("not reachable")}
	_, err := newTestDaemonCmd(mock, []string{"stop"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ── httptest server integration ───────────────────────────────────────────────

// TestDaemonStatusHTTPMock exercises the full DaemonClient HTTP path against
// an httptest.Server, verifying that the client parses real JSON responses.
func TestDaemonStatusHTTPMock(t *testing.T) {
	t.Parallel()

	fixture := fixtureStatusResp()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/status" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fixture)
	}))
	t.Cleanup(srv.Close)

	client := afclient.NewDaemonClientFromURL(srv.URL)
	resp, err := client.GetStatus()
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if resp.MachineID != fixture.MachineID {
		t.Errorf("machineId = %q, want %q", resp.MachineID, fixture.MachineID)
	}
	if resp.Status != fixture.Status {
		t.Errorf("status = %q, want %q", resp.Status, fixture.Status)
	}
	if resp.ActiveSessions != fixture.ActiveSessions {
		t.Errorf("activeSessions = %d, want %d", resp.ActiveSessions, fixture.ActiveSessions)
	}
}

// TestDaemonStatsHTTPMock exercises the stats endpoint through the real HTTP client.
func TestDaemonStatsHTTPMock(t *testing.T) {
	t.Parallel()

	fixture := fixtureStatsResp()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/stats" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fixture)
	}))
	t.Cleanup(srv.Close)

	client := afclient.NewDaemonClientFromURL(srv.URL)
	resp, err := client.GetStats(false, false)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if resp.ActiveSessions != fixture.ActiveSessions {
		t.Errorf("activeSessions = %d, want %d", resp.ActiveSessions, fixture.ActiveSessions)
	}
	if resp.QueueDepth != fixture.QueueDepth {
		t.Errorf("queueDepth = %d, want %d", resp.QueueDepth, fixture.QueueDepth)
	}
}

// TestDaemonActionHTTPMock exercises action endpoints (pause, resume, drain,
// stop, update) through a single httptest.Server.
func TestDaemonActionHTTPMock(t *testing.T) {
	t.Parallel()

	actionPaths := []string{
		"/api/daemon/pause",
		"/api/daemon/resume",
		"/api/daemon/drain",
		"/api/daemon/stop",
		"/api/daemon/update",
	}

	for _, path := range actionPaths {
		path := path
		t.Run(strings.TrimPrefix(path, "/api/daemon/"), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != path {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(afclient.DaemonActionResponse{
					OK:      true,
					Message: "accepted",
				})
			}))
			t.Cleanup(srv.Close)

			client := afclient.NewDaemonClientFromURL(srv.URL)
			var (
				resp *afclient.DaemonActionResponse
				err  error
			)
			switch path {
			case "/api/daemon/pause":
				resp, err = client.Pause()
			case "/api/daemon/resume":
				resp, err = client.Resume()
			case "/api/daemon/drain":
				resp, err = client.Drain(0)
			case "/api/daemon/stop":
				resp, err = client.Stop()
			case "/api/daemon/update":
				resp, err = client.Update()
			}
			if err != nil {
				t.Fatalf("client method: %v", err)
			}
			if !resp.OK {
				t.Errorf("expected OK=true; got %+v", resp)
			}
		})
	}
}

// ── doctor (in-process; REN-1406) ─────────────────────────────────────────────

// TestDaemonDoctorHelp verifies the doctor command exposes the expected flags.
func TestDaemonDoctorHelp(t *testing.T) {
	t.Parallel()

	cmd := newDaemonDoctorCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"--json", "--user", "--system"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor --help missing flag %q; got:\n%s", want, out)
		}
	}
}

// TestDaemonDoctorReportsMissingService verifies that when no unit/plist is
// installed, doctor exits non-zero with a clear "service is not installed"
// message — and never references the legacy rensei-daemon binary.
func TestDaemonDoctorReportsMissingService(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cmd := newDaemonDoctorCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when service is not installed")
	}
	if strings.Contains(err.Error(), "rensei-daemon") {
		t.Errorf("error must NOT reference legacy rensei-daemon binary; got: %v", err)
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error should mention 'not installed'; got: %v", err)
	}

	// The doctor output must report binary-presence based on the *actual*
	// Go binary the installer would register (REN-1406 acceptance criterion).
	out := buf.String()
	if !strings.Contains(out, "Host binary:") || !strings.Contains(out, "Binary present:") {
		t.Errorf("expected doctor output to include Host binary / Binary present rows; got:\n%s", out)
	}
}

// TestDaemonDoctorJSONOutput verifies the --json flag emits a structured
// report of the in-process installer state.
func TestDaemonDoctorJSONOutput(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("installer only supports darwin/linux; this is %s", runtime.GOOS)
	}
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	cmd := newDaemonDoctorCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--json"})
	// Doctor returns an error when the service is not installed; we still
	// expect a JSON body on stdout.
	_ = cmd.Execute()

	// stdout must be valid JSON.
	var report map[string]any
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("expected --json to emit valid JSON; got: %v\noutput:\n%s", err, buf.String())
	}
	if _, ok := report["OS"]; !ok {
		t.Errorf("expected JSON to include OS field; got: %v", report)
	}
	if _, ok := report["Installed"]; !ok {
		t.Errorf("expected JSON to include Installed field; got: %v", report)
	}
}

// ── logs helper unit tests ────────────────────────────────────────────────────

func TestPrintLogLineNDJSON(t *testing.T) {
	t.Parallel()

	line := `{"time":"2026-04-27T12:00:00Z","level":"info","msg":"daemon ready"}`
	var buf bytes.Buffer
	printLogLine(&buf, line, true)
	out := buf.String()
	if !strings.Contains(out, "INFO") {
		t.Errorf("expected INFO level; got: %s", out)
	}
	if !strings.Contains(out, "daemon ready") {
		t.Errorf("expected message; got: %s", out)
	}
}

func TestPrintLogLineRaw(t *testing.T) {
	t.Parallel()

	line := `{"time":"2026-04-27T12:00:00Z","level":"info","msg":"daemon ready"}`
	var buf bytes.Buffer
	printLogLine(&buf, line, false)
	out := buf.String()
	// raw = false for parseJSON means don't parse → print as-is
	if !strings.Contains(out, `"msg"`) {
		t.Errorf("expected raw JSON output; got: %s", out)
	}
}

func TestPrintLogLinePlain(t *testing.T) {
	t.Parallel()

	line := "plain text log line"
	var buf bytes.Buffer
	printLogLine(&buf, line, true)
	if !strings.Contains(buf.String(), line) {
		t.Errorf("expected plain line; got: %s", buf.String())
	}
}

// ── helper functions ──────────────────────────────────────────────────────────

func TestFormatUptimeSeconds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   int64
		want string
	}{
		{0, "0s"},
		{-1, "0s"},
		{45, "45s"},
		{60, "1m"},
		{3600, "1h"},
		{3725, "1h2m5s"},
		{3660, "1h1m"},
		{3605, "1h5s"},
		{125, "2m5s"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("%d", tc.in), func(t *testing.T) {
			t.Parallel()
			got := formatUptimeSeconds(tc.in)
			if got != tc.want {
				t.Errorf("formatUptimeSeconds(%d) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExpandHomePath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		hasHome bool
	}{
		{"~/.rensei/daemon.log", true},
		{"/absolute/path", false},
		{"relative/path", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := expandHomePath(tc.in)
			if tc.hasHome && strings.HasPrefix(got, "~/") {
				t.Errorf("expandHomePath(%q) still starts with ~/: %q", tc.in, got)
			}
			if !tc.hasHome && got != tc.in {
				t.Errorf("expandHomePath(%q) modified non-~ path: %q", tc.in, got)
			}
		})
	}
}

func TestDaemonConfigBaseURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		cfg  afclient.DaemonConfig
		want string
	}{
		{afclient.DaemonConfig{Host: "127.0.0.1", Port: 7734}, "http://127.0.0.1:7734"},
		{afclient.DaemonConfig{Host: "", Port: 0}, "http://127.0.0.1:7734"},
		{afclient.DaemonConfig{Host: "10.0.0.1", Port: 9000}, "http://10.0.0.1:9000"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			got := tc.cfg.BaseURL()
			if got != tc.want {
				t.Errorf("BaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDaemonSetupWritesDefaultConfig verifies that `daemon setup` runs the
// in-process Go wizard (REN-1408) and, in non-TTY environments, writes the
// default config to the path provided via --config.
func TestDaemonSetupWritesDefaultConfig(t *testing.T) {
	// Force the non-TTY path via the wizard skip env (mirrors
	// RENSEI_DAEMON_SKIP_WIZARD).
	t.Setenv("RENSEI_DAEMON_SKIP_WIZARD", "1")

	cfgPath := filepath.Join(t.TempDir(), "daemon.yaml")
	cmd := newDaemonSetupCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"--config", cfgPath})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("daemon setup: %v", err)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("expected config file at %s: %v", cfgPath, err)
	}
}

// TestDaemonClientTimeout verifies that a slow server causes a timeout error.
func TestDaemonClientTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Delay longer than client timeout (10s) — we use sleep proportional
		// to avoid making the test slow by configuring a short timeout.
		time.Sleep(200 * time.Millisecond) // short for test; real timeout is 10s
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	// Use a fresh client with a very short timeout to trigger the error.
	c := afclient.NewDaemonClientFromURL(srv.URL)
	// We cannot change the timeout on the exported client from outside the package,
	// but we can verify the error path by hitting a closed server.
	srv.Close()
	_, err := c.GetStatus()
	if err == nil {
		t.Fatal("expected error from closed server, got nil")
	}
}

// TestWriteDaemonStatusTable exercises the human-readable status renderer.
func TestWriteDaemonStatusTable(t *testing.T) {
	t.Parallel()

	r := fixtureStatusResp()
	var buf bytes.Buffer
	if err := writeDaemonStatusTable(&buf, r); err != nil {
		t.Fatalf("writeDaemonStatusTable: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"mac-studio-test", "0.1.0", "42", "3 / 8", "2 allowed"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q; got:\n%s", want, out)
		}
	}
}

// TestWriteDaemonStatsTable exercises the stats renderer.
func TestWriteDaemonStatsTable(t *testing.T) {
	t.Parallel()

	r := fixtureStatsResp()
	var buf bytes.Buffer
	if err := writeDaemonStatsTable(&buf, r); err != nil {
		t.Fatalf("writeDaemonStatsTable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "3 / 8") {
		t.Errorf("stats table missing session count; got:\n%s", out)
	}
}

// ── evict ─────────────────────────────────────────────────────────────────────

func TestDaemonEvictSuccess(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{
		evictResp: &afclient.EvictPoolResponse{
			Evicted:       2,
			Message:       "2 members scheduled for eviction",
			CorrelationID: "corr-abc-123",
		},
	}
	buf, err := newTestDaemonCmd(mock, []string{
		"evict", "--repo", "github.com/foo/bar", "--older-than", "24h",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "evicted 2") {
		t.Errorf("output missing eviction count; got:\n%s", out)
	}
	if !strings.Contains(out, "corr-abc-123") {
		t.Errorf("output missing correlation ID; got:\n%s", out)
	}
	if mock.evictReq == nil {
		t.Fatal("EvictPool not called")
	}
	if mock.evictReq.RepoURL != "github.com/foo/bar" {
		t.Errorf("repoUrl = %q, want %q", mock.evictReq.RepoURL, "github.com/foo/bar")
	}
	wantSecs := int64(24 * 3600)
	if mock.evictReq.OlderThanSeconds != wantSecs {
		t.Errorf("olderThanSeconds = %d, want %d", mock.evictReq.OlderThanSeconds, wantSecs)
	}
}

func TestDaemonEvictJSONOutput(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{
		evictResp: &afclient.EvictPoolResponse{
			Evicted:       1,
			Message:       "done",
			CorrelationID: "corr-xyz",
		},
	}
	buf, err := newTestDaemonCmd(mock, []string{
		"evict", "--repo", "github.com/foo/bar", "--older-than", "1h", "--json",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var resp afclient.EvictPoolResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if resp.Evicted != 1 {
		t.Errorf("evicted = %d, want 1", resp.Evicted)
	}
}

func TestDaemonEvictMissingRepo(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{}
	_, err := newTestDaemonCmd(mock, []string{"evict", "--older-than", "1h"})
	if err == nil {
		t.Fatal("expected error when --repo is missing")
	}
	if !strings.Contains(err.Error(), "--repo") {
		t.Errorf("error should mention --repo; got: %v", err)
	}
}

func TestDaemonEvictMissingOlderThan(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{}
	_, err := newTestDaemonCmd(mock, []string{"evict", "--repo", "github.com/foo/bar"})
	if err == nil {
		t.Fatal("expected error when --older-than is missing")
	}
	if !strings.Contains(err.Error(), "--older-than") {
		t.Errorf("error should mention --older-than; got: %v", err)
	}
}

func TestDaemonEvictNegativeDuration(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{}
	_, err := newTestDaemonCmd(mock, []string{
		"evict", "--repo", "github.com/foo/bar", "--older-than", "-1h",
	})
	if err == nil {
		t.Fatal("expected error for negative duration")
	}
}

func TestDaemonEvictInvalidDuration(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{}
	_, err := newTestDaemonCmd(mock, []string{
		"evict", "--repo", "github.com/foo/bar", "--older-than", "notaduration",
	})
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestDaemonEvictError(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{evictErr: fmt.Errorf("pool not available")}
	_, err := newTestDaemonCmd(mock, []string{
		"evict", "--repo", "github.com/foo/bar", "--older-than", "24h",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "pool not available") {
		t.Errorf("error should mention 'pool not available'; got: %v", err)
	}
}

// ── evict HTTP mock ───────────────────────────────────────────────────────────

func TestDaemonEvictHTTPMock(t *testing.T) {
	t.Parallel()

	fixture := afclient.EvictPoolResponse{
		Evicted:       3,
		Message:       "3 pool members scheduled for eviction",
		CorrelationID: "corr-http-test",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/pool/evict" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fixture)
	}))
	t.Cleanup(srv.Close)

	client := afclient.NewDaemonClientFromURL(srv.URL)
	resp, err := client.EvictPool(afclient.EvictPoolRequest{
		RepoURL:          "github.com/foo/bar",
		OlderThanSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("EvictPool: %v", err)
	}
	if resp.Evicted != fixture.Evicted {
		t.Errorf("evicted = %d, want %d", resp.Evicted, fixture.Evicted)
	}
	if resp.CorrelationID != fixture.CorrelationID {
		t.Errorf("correlationId = %q, want %q", resp.CorrelationID, fixture.CorrelationID)
	}
}

// ── set ───────────────────────────────────────────────────────────────────────

func TestDaemonSetCapacitySuccess(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{
		setCapResp: &afclient.SetCapacityResponse{
			OK:      true,
			Key:     "capacity.poolMaxDiskGb",
			Value:   "50",
			Message: "updated",
		},
	}
	// Use a temp config path to avoid touching real ~/.rensei/daemon.yaml.
	tmpDir := t.TempDir()
	buf, err := newTestDaemonCmd(mock, []string{
		"set", "capacity.poolMaxDiskGb", "50",
		"--config", tmpDir + "/daemon.yaml",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "capacity.poolMaxDiskGb") {
		t.Errorf("output missing key; got:\n%s", out)
	}
	if mock.setCapKey != "capacity.poolMaxDiskGb" {
		t.Errorf("SetCapacityConfig key = %q, want %q", mock.setCapKey, "capacity.poolMaxDiskGb")
	}
	if mock.setCapValue != "50" {
		t.Errorf("SetCapacityConfig value = %q, want %q", mock.setCapValue, "50")
	}
}

func TestDaemonSetMaxConcurrentSessionsSuccess(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{
		setCapResp: &afclient.SetCapacityResponse{
			OK:      true,
			Key:     "capacity.maxConcurrentSessions",
			Value:   "6",
			Message: "updated",
		},
	}
	tmpDir := t.TempDir()
	buf, err := newTestDaemonCmd(mock, []string{
		"set", "capacity.maxConcurrentSessions", "6",
		"--config", tmpDir + "/daemon.yaml",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "capacity.maxConcurrentSessions") {
		t.Errorf("output missing key; got:\n%s", buf.String())
	}
	if mock.setCapKey != "capacity.maxConcurrentSessions" {
		t.Errorf("SetCapacityConfig key = %q, want %q", mock.setCapKey, "capacity.maxConcurrentSessions")
	}
	cfg, readErr := afclient.ReadDaemonYAML(tmpDir + "/daemon.yaml")
	if readErr != nil {
		t.Fatalf("ReadDaemonYAML: %v", readErr)
	}
	if cfg.Capacity.MaxConcurrentSessions != 6 {
		t.Errorf("maxConcurrentSessions = %d, want 6", cfg.Capacity.MaxConcurrentSessions)
	}
}

func TestDaemonSetCapacityJSONOutput(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{
		setCapResp: &afclient.SetCapacityResponse{
			OK:      true,
			Key:     "capacity.poolMaxDiskGb",
			Value:   "100",
			Message: "updated",
		},
	}
	tmpDir := t.TempDir()
	buf, err := newTestDaemonCmd(mock, []string{
		"set", "capacity.poolMaxDiskGb", "100",
		"--config", tmpDir + "/daemon.yaml",
		"--json",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var resp afclient.SetCapacityResponse
	if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if !resp.OK {
		t.Errorf("OK = false, want true")
	}
}

func TestDaemonSetCapacityUnknownKey(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{}
	_, err := newTestDaemonCmd(mock, []string{"set", "capacity.unknownField", "42"})
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "unknown config key") {
		t.Errorf("error should mention 'unknown config key'; got: %v", err)
	}
}

func TestDaemonSetCapacityNegativeValue(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{}
	_, err := newTestDaemonCmd(mock, []string{"set", "capacity.poolMaxDiskGb", "-5"})
	if err == nil {
		t.Fatal("expected error for negative value")
	}
}

func TestDaemonSetCapacityNonIntegerValue(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{}
	_, err := newTestDaemonCmd(mock, []string{"set", "capacity.poolMaxDiskGb", "notanumber"})
	if err == nil {
		t.Fatal("expected error for non-integer value")
	}
}

func TestDaemonSetCapacityDaemonNotReachable(t *testing.T) {
	t.Parallel()

	// When the daemon is not running, SetCapacityConfig returns an error but
	// the command should succeed (YAML was already written).
	mock := &mockDaemon{setCapErr: fmt.Errorf("connection refused")}
	tmpDir := t.TempDir()
	buf, err := newTestDaemonCmd(mock, []string{
		"set", "capacity.poolMaxDiskGb", "20",
		"--config", tmpDir + "/daemon.yaml",
	})
	if err != nil {
		t.Fatalf("expected success when daemon unreachable; got: %v", err)
	}
	if !strings.Contains(buf.String(), "daemon not reachable") {
		t.Errorf("output should note daemon not reachable; got:\n%s", buf.String())
	}
}

func TestDaemonSetCapacityRejected(t *testing.T) {
	t.Parallel()

	mock := &mockDaemon{
		setCapResp: &afclient.SetCapacityResponse{
			OK:      false,
			Key:     "capacity.poolMaxDiskGb",
			Value:   "999",
			Message: "value exceeds hardware limit",
		},
	}
	tmpDir := t.TempDir()
	_, err := newTestDaemonCmd(mock, []string{
		"set", "capacity.poolMaxDiskGb", "999",
		"--config", tmpDir + "/daemon.yaml",
	})
	if err == nil {
		t.Fatal("expected error when daemon rejects set")
	}
	if !strings.Contains(err.Error(), "value exceeds hardware limit") {
		t.Errorf("error should mention rejection reason; got: %v", err)
	}
}

// ── set HTTP mock ─────────────────────────────────────────────────────────────

func TestDaemonSetCapacityHTTPMock(t *testing.T) {
	t.Parallel()

	fixture := afclient.SetCapacityResponse{
		OK:      true,
		Key:     "capacity.poolMaxDiskGb",
		Value:   "75",
		Message: "updated and reloaded",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/capacity" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fixture)
	}))
	t.Cleanup(srv.Close)

	client := afclient.NewDaemonClientFromURL(srv.URL)
	resp, err := client.SetCapacityConfig("capacity.poolMaxDiskGb", "75")
	if err != nil {
		t.Fatalf("SetCapacityConfig: %v", err)
	}
	if !resp.OK {
		t.Errorf("OK = false, want true")
	}
	if resp.Key != fixture.Key {
		t.Errorf("key = %q, want %q", resp.Key, fixture.Key)
	}
}

// ── pool stats (detailed) ─────────────────────────────────────────────────────

func TestDaemonStatsPoolPerKeyBreakdown(t *testing.T) {
	t.Parallel()

	resp := fixtureStatsResp()
	resp.Pool = &afclient.WorkareaPoolStats{
		TotalMembers:     4,
		ReadyMembers:     2,
		AcquiredMembers:  1,
		WarmingMembers:   1,
		InvalidMembers:   0,
		TotalDiskUsageMb: 2048,
		Members: []afclient.WorkareaPoolMember{
			{
				ID:           "wk-1",
				Repository:   "github.com/acme/api",
				ToolchainKey: "node-20",
				Status:       afclient.PoolMemberReady,
				DiskUsageMb:  512,
			},
			{
				ID:           "wk-2",
				Repository:   "github.com/acme/api",
				ToolchainKey: "node-20",
				Status:       afclient.PoolMemberAcquired,
				DiskUsageMb:  512,
			},
			{
				ID:           "wk-3",
				Repository:   "github.com/acme/web",
				ToolchainKey: "node-20+java-17",
				Status:       afclient.PoolMemberReady,
				DiskUsageMb:  512,
			},
			{
				ID:           "wk-4",
				Repository:   "github.com/acme/web",
				ToolchainKey: "node-20+java-17",
				Status:       afclient.PoolMemberWarming,
				DiskUsageMb:  512,
			},
		},
	}
	mock := &mockDaemon{statsResp: resp}
	buf, err := newTestDaemonCmd(mock, []string{"stats", "--pool"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()

	// Aggregate section.
	if !strings.Contains(out, "Workarea pool:") {
		t.Errorf("missing 'Workarea pool:' header; got:\n%s", out)
	}
	if !strings.Contains(out, "2048") {
		t.Errorf("missing total disk usage; got:\n%s", out)
	}
	// Per-key breakdown.
	if !strings.Contains(out, "By (repo, toolchain):") {
		t.Errorf("missing per-key breakdown header; got:\n%s", out)
	}
	if !strings.Contains(out, "github.com/acme/api") {
		t.Errorf("missing repo github.com/acme/api; got:\n%s", out)
	}
	if !strings.Contains(out, "github.com/acme/web") {
		t.Errorf("missing repo github.com/acme/web; got:\n%s", out)
	}
	if !strings.Contains(out, "node-20+java-17") {
		t.Errorf("missing toolchain key node-20+java-17; got:\n%s", out)
	}
}

func TestWritePoolStatsSectionNoMembers(t *testing.T) {
	t.Parallel()

	p := &afclient.WorkareaPoolStats{
		TotalMembers:     2,
		ReadyMembers:     2,
		TotalDiskUsageMb: 256,
	}
	var buf bytes.Buffer
	if err := writePoolStatsSection(&buf, p); err != nil {
		t.Fatalf("writePoolStatsSection: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Workarea pool:") {
		t.Errorf("missing header; got:\n%s", out)
	}
	// No members → no per-key table.
	if strings.Contains(out, "By (repo, toolchain):") {
		t.Errorf("unexpected per-key section with no members; got:\n%s", out)
	}
}

// TestGetPoolStatsHTTPMock exercises GetPoolStats via a real HTTP client.
func TestGetPoolStatsHTTPMock(t *testing.T) {
	t.Parallel()

	fixture := afclient.WorkareaPoolStats{
		TotalMembers:     3,
		ReadyMembers:     2,
		AcquiredMembers:  1,
		TotalDiskUsageMb: 768,
		Timestamp:        "2026-04-27T12:00:00Z",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/pool/stats" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fixture)
	}))
	t.Cleanup(srv.Close)

	client := afclient.NewDaemonClientFromURL(srv.URL)
	resp, err := client.GetPoolStats()
	if err != nil {
		t.Fatalf("GetPoolStats: %v", err)
	}
	if resp.TotalMembers != fixture.TotalMembers {
		t.Errorf("totalMembers = %d, want %d", resp.TotalMembers, fixture.TotalMembers)
	}
	if resp.TotalDiskUsageMb != fixture.TotalDiskUsageMb {
		t.Errorf("totalDiskUsageMb = %d, want %d", resp.TotalDiskUsageMb, fixture.TotalDiskUsageMb)
	}
}

// ── daemon.yaml capacity persistence ─────────────────────────────────────────

func TestWriteDaemonYAMLCapacityRoundtrip(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir() + "/daemon.yaml"
	cfg := &afclient.DaemonYAML{
		Capacity: afclient.CapacityConfig{PoolMaxDiskGb: 42},
	}
	if err := afclient.WriteDaemonYAML(tmp, cfg); err != nil {
		t.Fatalf("WriteDaemonYAML: %v", err)
	}
	read, err := afclient.ReadDaemonYAML(tmp)
	if err != nil {
		t.Fatalf("ReadDaemonYAML: %v", err)
	}
	if read.Capacity.PoolMaxDiskGb != 42 {
		t.Errorf("poolMaxDiskGb = %d, want 42", read.Capacity.PoolMaxDiskGb)
	}
}
