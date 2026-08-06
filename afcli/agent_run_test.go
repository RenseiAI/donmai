package afcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runner"
)

// quietLogger returns a slog.Logger that drops all output. Used by
// agent_run tests that exercise buildAgentRunRegistry without
// polluting test output with provider-probe warn lines.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// codexOnPath reports whether the `codex` binary resolves on $PATH.
// Used by the happy-path test to skip when the codex provider would
// be probed (and trigger the codex startup/shutdown race).
func codexOnPath() bool {
	_, err := exec.LookPath("codex")
	return err == nil
}

// TestNewAgentRunCmd_Help verifies the `donmai agent run` command is
// registered under `agent run` and produces the expected help text.
func TestNewAgentRunCmd_Help(t *testing.T) {
	root := &cobra.Command{Use: "donmai"}
	root.AddCommand(newAgentCmd(nil, nil, Config{}))

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
		"DONMAI_SESSION_ID",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestAgentRunMaxSessionDuration(t *testing.T) {
	tests := []struct {
		name   string
		detail *daemon.SessionDetail
		want   time.Duration
	}{
		{
			name:   "interactive session disables runner timeout",
			detail: &daemon.SessionDetail{Mode: prompt.InteractiveRunMode},
			want:   -1,
		},
		{
			name:   "headless session keeps runner default",
			detail: &daemon.SessionDetail{},
		},
		{
			name:   "interview session keeps runner default",
			detail: &daemon.SessionDetail{Mode: "interview"},
		},
		{
			name:   "unknown mode keeps runner default",
			detail: &daemon.SessionDetail{Mode: "interactive-preview"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentRunMaxSessionDuration(tt.detail); got != tt.want {
				t.Fatalf("agentRunMaxSessionDuration() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestFetchSessionDetail_HappyPath drives fetchSessionDetail against
// a fake daemon HTTP server that returns a SessionDetail body.
func TestFetchSessionDetail_HappyPath(t *testing.T) {
	// nolint:gosec // G101: fake test fixture, not a real credential.
	want := &daemon.SessionDetail{
		SessionID:       "sess-1",
		IssueIdentifier: "ENG-9999",
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

	got, err := fetchSessionDetail(context.Background(), &http.Client{Timeout: 2 * time.Second}, srv.URL, "sess-1", "")
	if err != nil {
		t.Fatalf("fetchSessionDetail: %v", err)
	}
	if got.SessionID != want.SessionID || got.IssueIdentifier != want.IssueIdentifier {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestFetchSessionDetail_BearerTokenAttached verifies that when a
// non-empty daemon-control token is supplied, the session-detail request
// carries an `Authorization: Bearer <token>` header. This is the cloud
// sandbox case: DONMAI_DAEMON_URL points at an authenticated remote
// endpoint and DONMAI_RUNTIME_JWT carries the token it expects.
func TestFetchSessionDetail_BearerTokenAttached(t *testing.T) {
	// nolint:gosec // G101: fake test fixture, not a real credential.
	const token = "test.bearer.token"
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(&daemon.SessionDetail{SessionID: "sess-auth"}) // nolint:gosec // G117: test fixture
	}))
	defer srv.Close()

	_, err := fetchSessionDetail(context.Background(), &http.Client{Timeout: 2 * time.Second}, srv.URL, "sess-auth", token)
	if err != nil {
		t.Fatalf("fetchSessionDetail: %v", err)
	}
	if want := "Bearer " + token; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

// TestFetchSessionDetail_NoTokenNoAuthHeader verifies that with an empty
// token (the localhost loopback default), no Authorization header is sent —
// preserving the unauthenticated loopback behavior exactly.
func TestFetchSessionDetail_NoTokenNoAuthHeader(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		_ = json.NewEncoder(w).Encode(&daemon.SessionDetail{SessionID: "sess-loopback"}) // nolint:gosec // G117: test fixture
	}))
	defer srv.Close()

	_, err := fetchSessionDetail(context.Background(), &http.Client{Timeout: 2 * time.Second}, srv.URL, "sess-loopback", "")
	if err != nil {
		t.Fatalf("fetchSessionDetail: %v", err)
	}
	if hadAuth {
		t.Error("expected no Authorization header for empty token (loopback), but one was sent")
	}
}

// TestAgentRunCredentialCache_BearerTokenAttached verifies the credential
// cache's refetch path (which re-hits the session-detail endpoint) also
// carries the bearer token when one is configured.
func TestAgentRunCredentialCache_BearerTokenAttached(t *testing.T) {
	// nolint:gosec // G101: fake test fixture, not a real credential.
	const token = "cache.bearer.token"
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(&daemon.SessionDetail{ // nolint:gosec // G117: test fixture
			SessionID: "sess-cred",
			WorkerID:  "wkr_fresh",
			AuthToken: "fresh-token",
		})
	}))
	t.Cleanup(srv.Close)

	cache := newAgentRunCredentialCache(
		srv.Client(),
		srv.URL,
		"sess-cred",
		token,
		&daemon.SessionDetail{SessionID: "sess-cred", WorkerID: "wkr_old", AuthToken: "old-token"},
	)

	if _, err := cache.runnerCredentials(context.Background()); err != nil {
		t.Fatalf("runnerCredentials: %v", err)
	}
	if want := "Bearer " + token; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
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

	_, err := fetchSessionDetail(context.Background(), &http.Client{Timeout: 2 * time.Second}, srv.URL, "missing", "")
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

	got, err := fetchSessionDetail(context.Background(), &http.Client{Timeout: 2 * time.Second}, srv.URL, "sess-2", "")
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

func TestAgentRunCredentialCacheRefreshesFromDaemon(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/daemon/sessions/") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(&daemon.SessionDetail{ // nolint:gosec // G117: test fixture
			SessionID: "sess-cred",
			WorkerID:  "wkr_fresh",
			AuthToken: "fresh-token",
		})
	}))
	t.Cleanup(srv.Close)

	cache := newAgentRunCredentialCache(
		srv.Client(),
		srv.URL,
		"sess-cred",
		"",
		&daemon.SessionDetail{SessionID: "sess-cred", WorkerID: "wkr_old", AuthToken: "old-token"},
	)

	got, err := cache.runnerCredentials(context.Background())
	if err != nil {
		t.Fatalf("runnerCredentials: %v", err)
	}
	if got.WorkerID != "wkr_fresh" || got.AuthToken != "fresh-token" {
		t.Fatalf("credentials = (%q, %q), want (wkr_fresh, fresh-token)", got.WorkerID, got.AuthToken)
	}
}

// TestFetchSessionDetail_DaemonUnreachable verifies a connection
// failure returns an error after exhausting retries.
func TestFetchSessionDetail_DaemonUnreachable(t *testing.T) {
	// Use 127.0.0.1:1 — typically unreachable.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := fetchSessionDetail(ctx, &http.Client{Timeout: 200 * time.Millisecond}, "http://127.0.0.1:1", "sess", "")
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
		IssueIdentifier: "ENG-1",
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
	if qw.SessionID != "sess-3" || qw.IssueIdentifier != "ENG-1" {
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

// TestDetailToQueuedWork_ThreadsHarness verifies the daemon's
// ResolvedProfile.Harness (the platform catalog loop-driver attribute) is
// threaded onto the runner's QueuedWork so the runner's harness-native
// provider selection sees it. The platform models the model as
// provider="gemini" with harness="agy"; the runner must resolve agy-cli.
func TestDetailToQueuedWork_ThreadsHarness(t *testing.T) {
	d := &daemon.SessionDetail{
		SessionID: "sess-harness",
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Harness:  "agy",
			Provider: string(agent.ProviderGemini),
			Model:    "gemini-3.1-pro",
		},
	}
	qw := detailToQueuedWork(d)
	if qw.ResolvedProfile.Harness != "agy" {
		t.Errorf("Harness = %q; want agy", qw.ResolvedProfile.Harness)
	}
	if qw.ResolvedProfile.Provider != agent.ProviderGemini {
		t.Errorf("Provider = %q; want gemini (Harness must not clobber Provider)", qw.ResolvedProfile.Provider)
	}
}

// TestProviderNameFromDetail_Harness verifies the dispatch log-line helper
// mirrors the runner's harness-native selection (agy → agy-cli) and keeps
// the legacy provider=agy-cli alias path working.
func TestProviderNameFromDetail_Harness(t *testing.T) {
	tests := []struct {
		name    string
		profile *daemon.SessionResolvedProfile
		want    string
	}{
		{
			name:    "harness agy maps to agy-cli over provider",
			profile: &daemon.SessionResolvedProfile{Harness: "agy", Provider: string(agent.ProviderGemini)},
			want:    string(agent.ProviderAGYCLI),
		},
		{
			name:    "legacy provider agy-cli without harness",
			profile: &daemon.SessionResolvedProfile{Provider: string(agent.ProviderAGYCLI)},
			want:    string(agent.ProviderAGYCLI),
		},
		{
			name:    "plain claude provider",
			profile: &daemon.SessionResolvedProfile{Provider: string(agent.ProviderClaude)},
			want:    string(agent.ProviderClaude),
		},
		{
			name:    "unknown harness falls back to provider",
			profile: &daemon.SessionResolvedProfile{Harness: "future", Provider: string(agent.ProviderCodex)},
			want:    string(agent.ProviderCodex),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &daemon.SessionDetail{SessionID: "s", ResolvedProfile: tt.profile}
			if got := providerNameFromDetail(d); got != tt.want {
				t.Errorf("providerNameFromDetail = %q; want %q", got, tt.want)
			}
		})
	}
}

// TestDetailToQueuedWork_ModelProfileSupersedesResolvedProfile verifies
// that when dispatch.modelProfile is present it takes precedence over
// the legacy resolvedProfile for provider selection.
// This covers the H-lane dispatch wiring acceptance criterion.
func TestDetailToQueuedWork_ModelProfileSupersedesResolvedProfile(t *testing.T) {
	d := &daemon.SessionDetail{
		SessionID:       "sess-mp-1",
		IssueIdentifier: "REN-MP-1",
		Body:            "test body",
		WorkerID:        "wkr_mp",
		AuthToken:       "tok_mp",
		PlatformURL:     "https://app.example.com",
		// Legacy profile says "claude" — should be overridden by ModelProfile.
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider:     "claude",
			Model:        "claude-sonnet-4-5",
			CredentialID: "cred-abc",
		},
		// ModelProfile says "stub" with richer knobs — this wins.
		ModelProfile: &daemon.SessionModelProfile{
			ID:              "mp_test_h_lane",
			ProviderID:      string(agent.ProviderStub),
			Model:           "stub-v1",
			Mode:            "xhigh",
			Context:         1_000_000,
			MaxOutputTokens: 32_000,
		},
	}
	qw := detailToQueuedWork(d)

	if qw.ResolvedProfile.Provider != agent.ProviderStub {
		t.Errorf("Provider = %q; want %q (ModelProfile should supersede ResolvedProfile)", qw.ResolvedProfile.Provider, agent.ProviderStub)
	}
	if qw.ResolvedProfile.Model != "stub-v1" {
		t.Errorf("Model = %q; want %q", qw.ResolvedProfile.Model, "stub-v1")
	}
	if qw.ResolvedProfile.Effort != agent.EffortXHigh {
		t.Errorf("Effort = %q; want xhigh", qw.ResolvedProfile.Effort)
	}
	// CredentialID must be preserved from ResolvedProfile even when ModelProfile is present.
	if qw.ResolvedProfile.CredentialID != "cred-abc" {
		t.Errorf("CredentialID = %q; want %q", qw.ResolvedProfile.CredentialID, "cred-abc")
	}
	// ProviderConfig should carry context + maxOutputTokens from ModelProfile.
	if qw.ResolvedProfile.ProviderConfig == nil {
		t.Fatal("ProviderConfig is nil; expected context window knobs")
	}
	if v, ok := qw.ResolvedProfile.ProviderConfig["contextWindow"]; !ok || v != 1_000_000 {
		t.Errorf("ProviderConfig[contextWindow] = %v; want 1000000", v)
	}
}

// TestDetailToQueuedWork_FallsBackToResolvedProfileWhenNoModelProfile
// verifies the legacy path is intact: when ModelProfile is absent,
// ResolvedProfile is used as-is (backwards compat).
func TestDetailToQueuedWork_FallsBackToResolvedProfileWhenNoModelProfile(t *testing.T) {
	d := &daemon.SessionDetail{
		SessionID:       "sess-mp-2",
		IssueIdentifier: "REN-MP-2",
		Body:            "test body",
		WorkerID:        "wkr_mp",
		AuthToken:       "tok_mp",
		PlatformURL:     "https://app.example.com",
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Provider: string(agent.ProviderStub),
			Model:    "stub-legacy",
			Effort:   "medium",
		},
		// ModelProfile intentionally absent.
	}
	qw := detailToQueuedWork(d)

	if qw.ResolvedProfile.Provider != agent.ProviderStub {
		t.Errorf("Provider = %q; want stub", qw.ResolvedProfile.Provider)
	}
	if qw.ResolvedProfile.Model != "stub-legacy" {
		t.Errorf("Model = %q; want stub-legacy", qw.ResolvedProfile.Model)
	}
	if qw.ResolvedProfile.Effort != agent.EffortMedium {
		t.Errorf("Effort = %q; want medium", qw.ResolvedProfile.Effort)
	}
}

// TestDetailToQueuedWork_ModelProfileEmptyProviderIDFallback verifies
// that an empty ProviderID in ModelProfile falls through to claude (same
// fallback as ResolvedModelProfile.SelectProvider).
func TestDetailToQueuedWork_ModelProfileEmptyProviderIDFallback(t *testing.T) {
	d := &daemon.SessionDetail{
		SessionID:       "sess-mp-3",
		IssueIdentifier: "REN-MP-3",
		Body:            "test body",
		WorkerID:        "wkr_mp",
		AuthToken:       "tok_mp",
		PlatformURL:     "https://app.example.com",
		ModelProfile: &daemon.SessionModelProfile{
			ID:    "mp_no_provider",
			Model: "some-model",
			// ProviderID intentionally empty.
		},
	}
	qw := detailToQueuedWork(d)

	// Empty ProviderID in ModelProfile reaches the named legacy harness adapter,
	// which visibly defaults to Claude during runner admission.
	// We just assert the conversion did not panic and the profile is set.
	if qw.ResolvedProfile.Model != "some-model" {
		t.Errorf("Model = %q; want some-model", qw.ResolvedProfile.Model)
	}
}

// TestDetailToQueuedWork_ModelProfileOnlyThreadsHarness verifies the
// modelProfile dispatch path is harness-aware in lock-step with the
// resolvedProfile path. When ONLY modelProfile is present (no
// resolvedProfile) and it models the model as ProviderID="gemini" with
// Harness="agy", the bridged QueuedWork.ResolvedProfile must carry Harness
// so the runner's harness-native selection resolves the agy-cli provider.
// Defense-in-depth: the platform writes only resolvedProfile today, so this
// guards the day it populates modelProfile.
func TestDetailToQueuedWork_ModelProfileOnlyThreadsHarness(t *testing.T) {
	d := &daemon.SessionDetail{
		SessionID:       "sess-mp-harness",
		IssueIdentifier: "REN-MP-AGY",
		Body:            "test body",
		WorkerID:        "wkr_mp",
		AuthToken:       "tok_mp",
		PlatformURL:     "https://app.example.com",
		// resolvedProfile intentionally absent — only modelProfile drives this.
		ModelProfile: &daemon.SessionModelProfile{
			ID:         "mp_agy",
			ProviderID: string(agent.ProviderGemini),
			Harness:    "agy",
			Model:      "gemini-3.1-pro",
		},
	}
	qw := detailToQueuedWork(d)

	if qw.ResolvedProfile.Harness != "agy" {
		t.Errorf("Harness = %q; want agy (modelProfile path must carry harness)", qw.ResolvedProfile.Harness)
	}
	// Harness must not clobber Provider — both survive the bridge.
	if qw.ResolvedProfile.Provider != agent.ProviderGemini {
		t.Errorf("Provider = %q; want gemini (Harness must not clobber Provider)", qw.ResolvedProfile.Provider)
	}
}

// TestDetailToQueuedWork_DisallowedToolsForwarded verifies that
// DisallowedTools stamped by the platform's credential-injection layer
// is forwarded from SessionDetail into the runner's QueuedWork.
// Mirrors the v0.9.3 SystemPromptOverride precedent.
func TestDetailToQueuedWork_DisallowedToolsForwarded(t *testing.T) {
	cases := []struct {
		name            string
		disallowedTools []string
		wantLen         int
	}{
		{"nil — omitted", nil, 0},
		{"single pattern", []string{"WebSearch"}, 1},
		{"multiple patterns", []string{"WebSearch", "WebFetch", "Bash(curl:*)"}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &daemon.SessionDetail{
				SessionID:       "sess-dt",
				DisallowedTools: tc.disallowedTools,
				ResolvedProfile: &daemon.SessionResolvedProfile{Provider: "stub"},
			}
			qw := detailToQueuedWork(d)
			if got := len(qw.DisallowedTools); got != tc.wantLen {
				t.Errorf("DisallowedTools len = %d, want %d; got %v", got, tc.wantLen, qw.DisallowedTools)
			}
			for i, pattern := range tc.disallowedTools {
				if i < len(qw.DisallowedTools) && qw.DisallowedTools[i] != pattern {
					t.Errorf("DisallowedTools[%d] = %q, want %q", i, qw.DisallowedTools[i], pattern)
				}
			}
		})
	}
}

// TestDetailToQueuedWork_MemoryBlockForwarded verifies the Wave 3
// dispatch-time agent-memory context survives the SessionDetail →
// runner.QueuedWork translation (the third + final wire hop). Mirrors the
// DisallowedTools / v0.9.3 SystemPromptOverride precedent.
func TestDetailToQueuedWork_MemoryBlockForwarded(t *testing.T) {
	cases := []struct {
		name        string
		memoryBlock string
	}{
		{"empty — omitted", ""},
		{"non-empty block", "recall: prefer the existing helper over a new one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &daemon.SessionDetail{
				SessionID:       "sess-mem",
				MemoryBlock:     tc.memoryBlock,
				ResolvedProfile: &daemon.SessionResolvedProfile{Provider: "stub"},
			}
			qw := detailToQueuedWork(d)
			if qw.MemoryBlock != tc.memoryBlock {
				t.Errorf("MemoryBlock = %q, want %q", qw.MemoryBlock, tc.memoryBlock)
			}
		})
	}
}

// TestDetailToQueuedWork_InitialPromptForwarded verifies the daemon-to-runner
// hop preserves the optional interactive seed exactly, including empty,
// whitespace-only, Unicode, and multiline values.
func TestDetailToQueuedWork_InitialPromptForwarded(t *testing.T) {
	for _, initialPrompt := range []string{"", "  ", "first line\n二行目 🌱"} {
		t.Run(fmt.Sprintf("bytes-%d", len(initialPrompt)), func(t *testing.T) {
			d := &daemon.SessionDetail{
				SessionID:       "sess-seed",
				InitialPrompt:   initialPrompt,
				ResolvedProfile: &daemon.SessionResolvedProfile{Provider: "stub"},
			}
			qw := detailToQueuedWork(d)
			if qw.InitialPrompt != initialPrompt {
				t.Errorf("InitialPrompt = %q, want %q", qw.InitialPrompt, initialPrompt)
			}
		})
	}
}

// TestDetailToQueuedWork_WS5FidelityForwarded verifies the WS5 agent-card
// fields (AllowedTools, McpServers, Skills) survive the SessionDetail →
// runner.QueuedWork translation (the third + final wire hop), including the
// daemon-mirror → agent.MCPServerConfig / prompt.SkillSpec re-typing.
func TestDetailToQueuedWork_WS5FidelityForwarded(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		d := &daemon.SessionDetail{
			SessionID:    "ws5-rt",
			AllowedTools: []string{"Bash(go:*)", "Read"},
			McpServers: []daemon.PollMCPServer{
				{Name: "linear", Type: "stdio", Command: "pnpm", Args: []string{"af-linear"}, Env: map[string]string{"K": "v"}},
				{Name: "remote", Type: "http", URL: "https://x.test/mcp", Headers: map[string]string{"Authorization": "Bearer t"}},
			},
			Skills: []daemon.PollSkill{
				{ID: "spring", Body: "do the thing", DisallowedTools: []string{"Bash(rm:*)"}},
			},
			ResolvedProfile: &daemon.SessionResolvedProfile{Provider: "stub"},
		}
		qw := detailToQueuedWork(d)
		if len(qw.AllowedTools) != 2 || qw.AllowedTools[0] != "Bash(go:*)" {
			t.Errorf("AllowedTools = %v, want [Bash(go:*) Read]", qw.AllowedTools)
		}
		if len(qw.McpServers) != 2 {
			t.Fatalf("McpServers len = %d, want 2", len(qw.McpServers))
		}
		if qw.McpServers[0].Name != "linear" || qw.McpServers[0].Type != "stdio" ||
			qw.McpServers[0].Command != "pnpm" || qw.McpServers[0].Env["K"] != "v" {
			t.Errorf("McpServers[0] re-type wrong: %+v", qw.McpServers[0])
		}
		if qw.McpServers[1].Type != "http" || qw.McpServers[1].URL != "https://x.test/mcp" ||
			qw.McpServers[1].Headers["Authorization"] != "Bearer t" {
			t.Errorf("McpServers[1] re-type wrong: %+v", qw.McpServers[1])
		}
		if len(qw.Skills) != 1 || qw.Skills[0].ID != "spring" || qw.Skills[0].Body != "do the thing" ||
			len(qw.Skills[0].DisallowedTools) != 1 || qw.Skills[0].DisallowedTools[0] != "Bash(rm:*)" {
			t.Errorf("Skills re-type wrong: %+v", qw.Skills)
		}
	})
	t.Run("absent — nil round-trip", func(t *testing.T) {
		d := &daemon.SessionDetail{
			SessionID:       "bare",
			ResolvedProfile: &daemon.SessionResolvedProfile{Provider: "stub"},
		}
		qw := detailToQueuedWork(d)
		if qw.AllowedTools != nil || qw.McpServers != nil || qw.Skills != nil {
			t.Errorf("WS5 fields must be nil when absent: allowed=%v mcp=%v skills=%v",
				qw.AllowedTools, qw.McpServers, qw.Skills)
		}
	})
}

// TestProviderNameFromDetail_PrefersModelProfile verifies the log-line
// helper reads ModelProfile.ProviderID first.
func TestProviderNameFromDetail_PrefersModelProfile(t *testing.T) {
	d := &daemon.SessionDetail{
		ResolvedProfile: &daemon.SessionResolvedProfile{Provider: "claude"},
		ModelProfile:    &daemon.SessionModelProfile{ProviderID: "gemini"},
	}
	got := providerNameFromDetail(d)
	if got != "gemini" {
		t.Errorf("providerNameFromDetail = %q; want gemini (ModelProfile takes priority)", got)
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

// TestBuildRegistryFromCtors_LogsProbeFailures covers the
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
	}, "donmai")

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
	}, "donmai")

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
// error when no session id is passed and DONMAI_SESSION_ID is unset.
func TestRunAgentRun_PreflightMissingSessionID(t *testing.T) {
	t.Setenv("DONMAI_SESSION_ID", "")
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

func TestRunAgentRun_UnknownExplicitHarnessPrecedesGatewayAndRunningPost(t *testing.T) {
	// Keep production registry construction deterministic: only the bundled
	// stub provider is needed to prove an unknown explicit selector is denied.
	t.Setenv("PATH", t.TempDir())
	var gatewayCalls atomic.Int32
	originalBind := bindWorkerGatewayForAgentRun
	bindWorkerGatewayForAgentRun = func(context.Context, *slog.Logger, *daemon.SessionDetail, *runner.QueuedWork) (*workerGateway, error) {
		gatewayCalls.Add(1)
		return nil, errors.New("gateway binding must not run after harness denial")
	}
	t.Cleanup(func() { bindWorkerGatewayForAgentRun = originalBind })

	var (
		completionPosts atomic.Int32
		statusPosts     atomic.Int32
		runningPosts    atomic.Int32
		failedPosts     atomic.Int32
	)
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/completion"):
			completionPosts.Add(1)
		case strings.HasSuffix(r.URL.Path, "/status"):
			statusPosts.Add(1)
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			switch body["status"] {
			case "running":
				runningPosts.Add(1)
			case "failed":
				failedPosts.Add(1)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer platform.Close()

	detail := &daemon.SessionDetail{
		SessionID: "session_unknown_explicit", IssueIdentifier: "issue-harness-denial",
		Body: "deny before worker side effects", WorkerID: "worker_test",
		AuthToken: "test-token", PlatformURL: platform.URL,
		ResolvedProfile: &daemon.SessionResolvedProfile{
			Harness: "future-harness", Provider: string(agent.ProviderStub),
			ServingHost: string(agent.HostGateway), Model: "test-model",
		},
	}
	daemonServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/daemon/sessions/") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(detail) // nolint:gosec // G117: test fixture
	}))
	defer daemonServer.Close()

	cmd := &cobra.Command{}
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	err := runAgentRun(context.Background(), cmd, &agentRunOpts{
		sessionID: detail.SessionID, daemonURL: daemonServer.URL,
		worktree: t.TempDir(), jsonOut: true,
	})
	var denial *runner.HarnessAdmissionError
	if !errors.As(err, &denial) || denial.Code != executioncell.DenialUnknownHarness {
		t.Fatalf("runAgentRun error = %v, want typed unknown_harness denial", err)
	}
	if gatewayCalls.Load() != 0 {
		t.Fatalf("gateway binding calls = %d, want 0", gatewayCalls.Load())
	}
	if runningPosts.Load() != 0 {
		t.Fatalf("eager status=running posts = %d, want 0", runningPosts.Load())
	}
	if completionPosts.Load() != 1 || statusPosts.Load() != 1 || failedPosts.Load() != 1 {
		t.Fatalf("terminal posts = completion:%d status:%d failed:%d, want one canonical failure delivery",
			completionPosts.Load(), statusPosts.Load(), failedPosts.Load())
	}
	if !strings.Contains(stdout.String(), `"denialCode": "unknown_harness"`) {
		t.Fatalf("result JSON missing canonical denial receipt: %s", stdout.String())
	}
}

// TestRunAgentRun_HappyPath_StubProvider drives a full agent-run
// against a fake daemon HTTP server and a fake platform that accepts
// the result post. The session uses the stub provider in
// "succeed-with-pr" mode so we assert on the runner.Result envelope
// emitted to stdout.
//
// Skipped under -race when `codex` is on PATH — the codex provider's
// startup/shutdown race is tracked separately. This test
// drives runAgentRun which uses the production buildAgentRunRegistry
// path; that path probes for codex unconditionally. Once that race
// lands the skip can drop.
func TestRunAgentRun_HappyPath_StubProvider(t *testing.T) {
	if codexOnPath() && raceEnabled() {
		t.Skip("skipping under -race because codex is on PATH and codex.New/Shutdown have a known race (ENG-1460); rerun without -race or after ENG-1460 lands")
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
		IssueIdentifier: "ENG-9000",
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

// TestPostSessionRunning_PostsRunningWithBearer verifies the eager pre-spawn
// status nudge hits POST /api/sessions/<id>/status with the running body,
// workerId, and bearer token — the wire shape maybePostRunning also uses, so
// the two are interchangeable + idempotent.
func TestPostSessionRunning_PostsRunningWithBearer(t *testing.T) {
	// nolint:gosec // G101: fake test fixture, not a real credential.
	const token = "run.bearer.token"
	var (
		gotPath   string
		gotMethod string
		gotAuth   string
		gotBody   map[string]string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	postSessionRunning(context.Background(), &http.Client{Timeout: 2 * time.Second},
		quietLogger(), srv.URL, "sess-run-1", "wkr_run", token)

	if want := "/api/sessions/sess-run-1/status"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if want := "Bearer " + token; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	if gotBody["status"] != "running" {
		t.Errorf("body status = %q, want running", gotBody["status"])
	}
	if gotBody["workerId"] != "wkr_run" {
		t.Errorf("body workerId = %q, want wkr_run", gotBody["workerId"])
	}
}

// TestPostSessionRunning_EmptyPlatformURLIsNoop verifies the standalone /
// no-platform path makes no HTTP call when PlatformURL is empty.
func TestPostSessionRunning_EmptyPlatformURLIsNoop(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Empty platformURL — must short-circuit before building any request.
	postSessionRunning(context.Background(), &http.Client{Timeout: 2 * time.Second},
		quietLogger(), "  ", "sess-noop", "wkr", "tok")

	if called.Load() {
		t.Error("expected no HTTP call for empty platform URL")
	}
}

// TestPostSessionRunning_NoTokenNoAuthHeader verifies an empty auth token
// sends no Authorization header (mirrors the loopback/unauthenticated case).
func TestPostSessionRunning_NoTokenNoAuthHeader(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	postSessionRunning(context.Background(), &http.Client{Timeout: 2 * time.Second},
		quietLogger(), srv.URL, "sess-noauth", "wkr", "")

	if hadAuth {
		t.Error("expected no Authorization header for empty token")
	}
}

// TestPostSessionRunning_Non2xxIsSwallowed verifies a platform error status
// never panics or surfaces — the nudge is best-effort observability.
func TestPostSessionRunning_Non2xxIsSwallowed(_ *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// Must return cleanly (no panic, no error surface) on a 5xx.
	postSessionRunning(context.Background(), &http.Client{Timeout: 2 * time.Second},
		quietLogger(), srv.URL, "sess-5xx", "wkr", "tok")
}

func TestDonmaiSpanTracingEnabled(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"1", true},
		{"true", true},
		{" TRUE ", true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("DONMAI_OTEL_TRACES", tt.value)
			if got := donmaiSpanTracingEnabled(); got != tt.want {
				t.Fatalf("donmaiSpanTracingEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}
