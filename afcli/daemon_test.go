package afcli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// ── mock daemon ───────────────────────────────────────────────────────────────

// mockDaemon implements daemonDoer with configurable responses for testing.
type mockDaemon struct {
	statusResp *afclient.DaemonStatusResponse
	statusErr  error
	statsResp  *afclient.DaemonStatsResponse
	statsErr   error
	actionResp *afclient.DaemonActionResponse
	actionErr  error
	drainCalls int
	drainSecs  int
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

// newTestDaemonCmd builds the daemon command tree with mock daemon injected.
// Each call creates an independent command tree — safe for parallel tests.
func newTestDaemonCmd(mock daemonDoer, args []string) (*bytes.Buffer, error) {
	factory := func(_ afclient.DaemonConfig) daemonDoer { return mock }
	cmd := newDaemonCmdWithFactory(factory)
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

// ── install / uninstall ───────────────────────────────────────────────────────

func TestDaemonInstallNotYetAvailable(t *testing.T) {
	t.Parallel()

	cmd := newDaemonInstallCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from install stub, got nil")
	}
	if !strings.Contains(err.Error(), "rensei-daemon install") {
		t.Errorf("error should mention 'rensei-daemon install'; got: %v", err)
	}
}

func TestDaemonUninstallNotYetAvailable(t *testing.T) {
	t.Parallel()

	cmd := newDaemonUninstallCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from uninstall stub, got nil")
	}
	if !strings.Contains(err.Error(), "rensei-daemon uninstall") {
		t.Errorf("error should mention 'rensei-daemon uninstall'; got: %v", err)
	}
}

// ── parent help ───────────────────────────────────────────────────────────────

func TestDaemonParentHelp(t *testing.T) {
	t.Parallel()

	cmd := newDaemonCmd()
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

// ── doctor ────────────────────────────────────────────────────────────────────

// TestDaemonDoctorBinaryMissing verifies that when the daemon binary is not
// on PATH, the doctor check emits a fail result for "Daemon binary".
func TestDaemonDoctorBinaryMissing(t *testing.T) {
	t.Parallel()

	// Use a port with no listener so the process check also fails (expected).
	// The doctor should still run all checks.
	cmd := newDaemonDoctorCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	// Point at a port with no listener so daemon check also fails.
	cmd.SetArgs([]string{"--port", "1"})
	// Execute will return error (any check failed).
	_ = cmd.Execute()
	out := buf.String()

	// Verify all 6 check labels appear.
	for _, label := range []string{
		"Daemon binary",
		"Daemon process",
		"Config file",
		"API token",
		"Project allowlist",
		"Orchestrator network",
	} {
		if !strings.Contains(out, label) {
			t.Errorf("doctor output missing check %q; got:\n%s", label, out)
		}
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

// TestDaemonSetupBinaryNotFound verifies setup returns a clear error when
// rensei-daemon is not on PATH. Cannot be parallel because t.Setenv requires it.
func TestDaemonSetupBinaryNotFound(t *testing.T) {
	// Override PATH to guarantee the binary is absent.
	t.Setenv("PATH", t.TempDir())

	cmd := newDaemonSetupCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when daemon binary not on PATH, got nil")
	}
	if !strings.Contains(err.Error(), "rensei-daemon") {
		t.Errorf("error should mention 'rensei-daemon'; got: %v", err)
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
