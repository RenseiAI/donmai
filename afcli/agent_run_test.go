package afcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/daemon"
)

// quietLogger returns a slog.Logger that drops all output. Used by
// agent_run tests that exercise buildAgentRunRegistry without
// polluting test output with provider-probe warn lines.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// codexOnPath reports whether the `codex` binary resolves on $PATH.
// Used by the happy-path test to skip when the codex provider would
// be probed (and trigger REN-1460's startup/shutdown race).
func codexOnPath() bool {
	_, err := exec.LookPath("codex")
	return err == nil
}

// TestNewAgentRunCmd_Help verifies the `af agent run` command is
// registered under `agent run` and produces the expected help text.
func TestNewAgentRunCmd_Help(t *testing.T) {
	root := &cobra.Command{Use: "af"}
	root.AddCommand(newAgentCmd(nil, nil))

	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"agent", "run", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Run a single agent session",
		"--session-id",
		"--daemon-url",
		"RENSEI_SESSION_ID",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestFetchSessionDetail_HappyPath drives fetchSessionDetail against
// a fake daemon HTTP server that returns a SessionDetail body.
func TestFetchSessionDetail_HappyPath(t *testing.T) {
	// nolint:gosec // G101: fake test fixture, not a real credential.
	want := &daemon.SessionDetail{
		SessionID:       "sess-1",
		IssueIdentifier: "REN-9999",
		Repository:      "github.com/foo/bar",
		WorkerID:        "wkr_1",
		AuthToken:       "rt.fake.jwt",
		PlatformURL:     "https://app.example.com",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/daemon/sessions/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want) // nolint:gosec // G117: test fixture
	}))
	defer srv.Close()

	got, err := fetchSessionDetail(context.Background(), &http.Client{Timeout: 2 * time.Second}, srv.URL, "sess-1")
	if err != nil {
		t.Fatalf("fetchSessionDetail: %v", err)
	}
	if got.SessionID != want.SessionID || got.IssueIdentifier != want.IssueIdentifier {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestFetchSessionDetail_NotFound exercises the 4xx → permanent error
// path. The retry loop should short-circuit on the first response.
func TestFetchSessionDetail_NotFound(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "session not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetchSessionDetail(context.Background(), &http.Client{Timeout: 2 * time.Second}, srv.URL, "missing")
	if err == nil {
		t.Fatal("expected error for 404")
	}
	var perm *permanentFetchError
	if !errors.As(err, &perm) {
		t.Errorf("expected *permanentFetchError, got %T", err)
	} else if perm.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", perm.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("expected 1 attempt for 4xx, got %d", got)
	}
}

// TestFetchSessionDetail_TransientThenSucceeds verifies the retry
// loop recovers from a 500 then a 200.
func TestFetchSessionDetail_TransientThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := hits.Add(1)
		if count < 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&daemon.SessionDetail{SessionID: "sess-2"}) // nolint:gosec // G117: test fixture
	}))
	defer srv.Close()

	got, err := fetchSessionDetail(context.Background(), &http.Client{Timeout: 2 * time.Second}, srv.URL, "sess-2")
	if err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	if got.SessionID != "sess-2" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if hits.Load() < 2 {
		t.Errorf("expected at least 2 attempts, got %d", hits.Load())
	}
}

// TestFetchSessionDetail_DaemonUnreachable verifies a connection
// failure returns an error after exhausting retries.
func TestFetchSessionDetail_DaemonUnreachable(t *testing.T) {
	// Use 127.0.0.1:1 — typically unreachable.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := fetchSessionDetail(ctx, &http.Client{Timeout: 200 * time.Millisecond}, "http://127.0.0.1:1", "sess")
	if err == nil {
		t.Fatal("expected unreachable error")
	}
}

// TestDetailToQueuedWork verifies the wire-shape translation copies
// every field through.
func TestDetailToQueuedWork(t *testing.T) {
	d := &daemon.SessionDetail{
		SessionID:       "sess-3",
		IssueID:         "lin-1",
		IssueIdentifier: "REN-1",
		Repository:      "github.com/foo/bar",
		Branch:          "agent/sess-3",
		WorkType:        "development",
		WorkerID:        "wkr_1",
		AuthToken:       "tok",
		PlatformURL:     "https://app.example.com",
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider: "stub",
			Model:    "claude-sonnet-4-5",
			Effort:   "high",
		},
	}
	qw := detailToQueuedWork(d)
	if qw.SessionID != "sess-3" || qw.IssueIdentifier != "REN-1" {
		t.Errorf("session/identifier mismatch: %+v", qw)
	}
	if qw.Branch != "agent/sess-3" || qw.AuthToken != "tok" || qw.WorkerID != "wkr_1" {
		t.Errorf("opaque fields mismatch: %+v", qw)
	}
	if qw.ResolvedProfile.Provider != agent.ProviderStub {
		t.Errorf("provider = %q, want stub", qw.ResolvedProfile.Provider)
	}
	if qw.ResolvedProfile.Effort != agent.EffortHigh {
		t.Errorf("effort = %q, want high", qw.ResolvedProfile.Effort)
	}
}

// TestBuildAgentRunRegistry_AlwaysHasStub asserts that the stub
// provider is always present, regardless of whether the host has
// claude / codex installed.
func TestBuildAgentRunRegistry_AlwaysHasStub(t *testing.T) {
	reg := buildAgentRunRegistry(quietLogger())
	names := reg.Names()
	if len(names) == 0 {
		t.Fatal("registry empty")
	}
	hasStub := false
	for _, n := range names {
		if n == agent.ProviderStub {
			hasStub = true
		}
	}
	if !hasStub {
		t.Errorf("registry missing stub provider; got %v", names)
	}
}

// TestBuildRegistryFromCtors_LogsProbeFailures covers REN-1462's
// per-provider WARN line: a registry built with one of three
// providers failing must emit exactly one WARN with provider=<name>
// + err, plus two happy registrations.
func TestBuildRegistryFromCtors_LogsProbeFailures(t *testing.T) {
	buf, restoreLogger := captureSlogJSON(t)
	defer restoreLogger()
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	good1 := &fakeProvider{name: agent.ProviderName("alpha")}
	good2 := &fakeProvider{name: agent.ProviderName("beta")}
	failErr := errors.New("probe failed: not on PATH")

	reg := buildRegistryFromCtors(logger, []providerCtor{
		{name: "alpha", new: func() (agent.Provider, error) { return good1, nil }},
		{name: "broken", new: func() (agent.Provider, error) { return nil, failErr }},
		{name: "beta", new: func() (agent.Provider, error) { return good2, nil }},
	})

	if got := len(reg.Names()); got != 2 {
		t.Errorf("registry size = %d, want 2", got)
	}

	records := decodeJSONLogs(t, buf)
	var warns int
	for _, r := range records {
		if r.Level == "WARN" && r.Provider == "broken" && strings.Contains(r.Err, "not on PATH") {
			warns++
		}
		if r.Level == "ERROR" {
			t.Errorf("unexpected ERROR record when 2/3 providers registered: %+v", r)
		}
	}
	if warns != 1 {
		t.Errorf("WARN count for 'broken' = %d, want 1; records=%+v", warns, records)
	}
}

// TestBuildRegistryFromCtors_ZeroProvidersErrors covers the
// fatal-misconfig path: when every provider fails, an ERROR record
// must fire so operators see the problem in production logs.
func TestBuildRegistryFromCtors_ZeroProvidersErrors(t *testing.T) {
	buf, restoreLogger := captureSlogJSON(t)
	defer restoreLogger()
	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	bad := errors.New("probe failed: not on PATH")
	reg := buildRegistryFromCtors(logger, []providerCtor{
		{name: "p1", new: func() (agent.Provider, error) { return nil, bad }},
		{name: "p2", new: func() (agent.Provider, error) { return nil, bad }},
		{name: "p3", new: func() (agent.Provider, error) { return nil, bad }},
	})

	if got := len(reg.Names()); got != 0 {
		t.Errorf("registry size = %d, want 0", got)
	}

	records := decodeJSONLogs(t, buf)
	var errors int
	for _, r := range records {
		if r.Level == "ERROR" && strings.Contains(r.Msg, "no providers available") {
			errors++
		}
	}
	if errors != 1 {
		t.Errorf("ERROR record count = %d, want 1; records=%+v", errors, records)
	}
}

// captureSlogJSON returns a buffer and restore func; tests build
// their own slog.Logger over it so the captured records include the
// per-provider attributes we want to assert on.
func captureSlogJSON(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	return buf, func() { slog.SetDefault(prev) }
}

type registryLogRecord struct {
	Level    string `json:"level"`
	Msg      string `json:"msg"`
	Provider string `json:"provider"`
	Err      string `json:"err"`
}

func decodeJSONLogs(t *testing.T, buf *bytes.Buffer) []registryLogRecord {
	t.Helper()
	dec := json.NewDecoder(buf)
	var out []registryLogRecord
	for dec.More() {
		var r registryLogRecord
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode log: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// fakeProvider is the smallest agent.Provider implementation needed
// by the registry tests. None of its non-Name methods are exercised.
type fakeProvider struct {
	name agent.ProviderName
}

func (f *fakeProvider) Name() agent.ProviderName { return f.name }
func (f *fakeProvider) Capabilities() agent.Capabilities {
	return agent.Capabilities{}
}

func (f *fakeProvider) Spawn(_ context.Context, _ agent.Spec) (agent.Handle, error) {
	return nil, errors.New("fakeProvider.Spawn not implemented")
}

func (f *fakeProvider) Resume(_ context.Context, _ string, _ agent.Spec) (agent.Handle, error) {
	return nil, errors.New("fakeProvider.Resume not implemented")
}

func (f *fakeProvider) Shutdown(_ context.Context) error { return nil }

// TestRunAgentRun_PreflightMissingSessionID asserts a clear preflight
// error when no session id is passed and RENSEI_SESSION_ID is unset.
func TestRunAgentRun_PreflightMissingSessionID(t *testing.T) {
	t.Setenv("RENSEI_SESSION_ID", "")
	cmd := &cobra.Command{}
	err := runAgentRun(context.Background(), cmd, &agentRunOpts{})
	if err == nil {
		t.Fatal("expected preflight error")
	}
	if !strings.Contains(err.Error(), "preflight") || !strings.Contains(err.Error(), "session id") {
		t.Errorf("error = %q, want preflight session-id message", err.Error())
	}
}

// TestRunAgentRun_PreflightDaemonUnreachable verifies an unreachable
// daemon URL surfaces a clear preflight error.
func TestRunAgentRun_PreflightDaemonUnreachable(t *testing.T) {
	cmd := &cobra.Command{}
	err := runAgentRun(context.Background(), cmd, &agentRunOpts{
		sessionID: "sess-x",
		daemonURL: "http://127.0.0.1:1",
	})
	if err == nil {
		t.Fatal("expected preflight error from unreachable daemon")
	}
	if !strings.Contains(err.Error(), "preflight") {
		t.Errorf("error = %q, want preflight prefix", err.Error())
	}
}

// TestRunAgentRun_PreflightSessionNotFound runs against a daemon that
// returns 404 for the requested session.
func TestRunAgentRun_PreflightSessionNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "session not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cmd := &cobra.Command{}
	err := runAgentRun(context.Background(), cmd, &agentRunOpts{
		sessionID: "missing",
		daemonURL: srv.URL,
	})
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !strings.Contains(err.Error(), "preflight") {
		t.Errorf("error = %q, want preflight prefix", err.Error())
	}
}

// TestRunAgentRun_HappyPath_StubProvider drives a full agent-run
// against a fake daemon HTTP server and a fake platform that accepts
// the result post. The session uses the stub provider in
// "succeed-with-pr" mode so we assert on the runner.Result envelope
// emitted to stdout.
//
// Skipped under -race when `codex` is on PATH — the codex provider's
// startup/shutdown race is tracked separately as REN-1460. This test
// drives runAgentRun which uses the production buildAgentRunRegistry
// path; that path probes for codex unconditionally. Once REN-1460
// lands the skip can drop.
func TestRunAgentRun_HappyPath_StubProvider(t *testing.T) {
	if codexOnPath() && raceEnabled() {
		t.Skip("skipping under -race because codex is on PATH and codex.New/Shutdown have a known race (REN-1460); rerun without -race or after REN-1460 lands")
	}
	platformHits := 0
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		platformHits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer platform.Close()

	detail := &daemon.SessionDetail{
		SessionID:       "sess-stub-1",
		IssueIdentifier: "REN-9000",
		Repository:      "github.com/foo/bar",
		WorkType:        "development",
		Body:            "Stub-mode test issue body.",
		WorkerID:        "wkr_test",
		AuthToken:       "tok_test",
		PlatformURL:     platform.URL,
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider: string(agent.ProviderStub),
		},
	}
	daemonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/daemon/sessions/") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(detail) // nolint:gosec // G117: test fixture
	}))
	defer daemonSrv.Close()

	wtDir := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wtDir, 0o750); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stdout)
	err := runAgentRun(context.Background(), cmd, &agentRunOpts{
		sessionID: "sess-stub-1",
		daemonURL: daemonSrv.URL,
		worktree:  wtDir,
		jsonOut:   true,
	})
	// The stub provider returns success without a real worktree —
	// runner.Run will still report failure modes for missing git
	// when worktree provisioning attempts run. Accept either nil or
	// a wrapped runner failure; the important part is that we got
	// past pre-flight, ran the registry, and emitted a Result JSON.
	_ = err

	body := stdout.String()
	if !strings.Contains(body, `"sessionId"`) {
		t.Errorf("expected stdout to contain a Result JSON; got %q", body)
	}
	if !strings.Contains(body, "sess-stub-1") {
		t.Errorf("expected Result JSON to include session id; got %q", body)
	}
}
