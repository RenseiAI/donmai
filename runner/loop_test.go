package runner

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/provider/stub"
	"github.com/RenseiAI/agentfactory-tui/result"
	"github.com/RenseiAI/agentfactory-tui/runtime/worktree"
)

// TestLoop_EventsMirroredToJSONL confirms every event the provider
// emits is appended to <worktree>/.agent/events.jsonl as a discrete
// JSONL row decodable via agent.UnmarshalEvent.
func TestLoop_EventsMirroredToJSONL(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-LOOP-1")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.WorktreePath == "" {
		t.Fatal("no WorktreePath on result")
	}
	jsonlPath := filepath.Join(res.WorktreePath, ".agent", "events.jsonl")
	f, err := os.Open(jsonlPath) //nolint:gosec // path is owned by the runner via worktree manager
	if err != nil {
		t.Fatalf("open events.jsonl: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	var kinds []agent.EventKind
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		ev, err := agent.UnmarshalEvent(line)
		if err != nil {
			t.Fatalf("UnmarshalEvent line %q: %v", line, err)
		}
		kinds = append(kinds, ev.Kind())
	}
	// Stub canonical sequence: init, system, assistant_text, tool_use,
	// tool_result, assistant_text, result.
	if len(kinds) < 5 {
		t.Errorf("expected >=5 events; got %d (%v)", len(kinds), kinds)
	}
	if kinds[0] != agent.EventInit {
		t.Errorf("first event = %s; want init", kinds[0])
	}
	if kinds[len(kinds)-1] != agent.EventResult {
		t.Errorf("last event = %s; want result", kinds[len(kinds)-1])
	}
}

// TestLoop_StateStoreUpdated confirms the runner writes the
// .agent/state.json snapshot during the loop.
func TestLoop_StateStoreUpdated(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-LOOP-STATE")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	statePath := filepath.Join(res.WorktreePath, ".agent", "state.json")
	body, err := os.ReadFile(statePath) //nolint:gosec // path is owned by the runner
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, qw.SessionID) {
		t.Errorf("state.json missing SessionID; got %q", bodyStr)
	}
	if !strings.Contains(bodyStr, "stub") {
		t.Errorf("state.json missing provider name; got %q", bodyStr)
	}
}

// TestLoop_ProviderError_ClassifiesFailure routes the stub's
// BehaviorMidStreamError through the runner and asserts the failure
// mode is FailureProviderError with the provider's error message
// surfaced on Result.Error.
func TestLoop_ProviderError_ClassifiesFailure(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-LOOP-ERR")
	qw.ResolvedProfile.ProviderConfig = map[string]any{
		"stub.behavior": string(stub.BehaviorMidStreamError),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, _ := h.runner.Run(ctx, qw)
	if res.FailureMode != FailureProviderError {
		t.Errorf("FailureMode = %q; want %q (Error=%q)", res.FailureMode, FailureProviderError, res.Error)
	}
	if !strings.Contains(res.Error, "crashed") {
		t.Errorf("expected provider crash text in Error; got %q", res.Error)
	}
}

// TestLoop_SilentExit_Classifies routes BehaviorSilentFail (no terminal
// ResultEvent) through the runner and asserts FailureSilentExit.
func TestLoop_SilentExit_Classifies(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-LOOP-SIL")
	qw.ResolvedProfile.ProviderConfig = map[string]any{
		"stub.behavior": string(stub.BehaviorSilentFail),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, _ := h.runner.Run(ctx, qw)
	if res.FailureMode != FailureSilentExit {
		t.Errorf("FailureMode = %q; want %q", res.FailureMode, FailureSilentExit)
	}
}

// TestLoop_TimeoutCancelsStream confirms ctx cancellation propagates
// to the provider and surfaces FailureTimeout on the result.
func TestLoop_TimeoutCancelsStream(t *testing.T) {
	h := newRunnerHarness(t)
	qw := h.queuedWork("REN-LOOP-TO")
	qw.ResolvedProfile.ProviderConfig = map[string]any{
		"stub.behavior": string(stub.BehaviorHangThenTimeout),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, _ := h.runner.Run(ctx, qw)
	// Both timeout and silent-exit are valid — depends on whether the
	// provider got the cancel before or after closing the channel.
	if res.FailureMode != FailureTimeout && res.FailureMode != FailureSilentExit {
		t.Errorf("FailureMode = %q; want timeout or silent-exit", res.FailureMode)
	}
}

// TestLoop_HeartbeatLostOwnership simulates a platform that always
// rejects /lock-refresh; after 3 strikes the pulser closes its
// LostOwnership channel and the runner cancels the stream with
// FailureLostOwnership.
//
// Uses BehaviorHangThenTimeout so the provider does not race the
// heartbeat to a terminal Result.
func TestLoop_HeartbeatLostOwnership(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	var refreshes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/lock-refresh") {
			refreshes.Add(1)
			http.Error(w, "lost", http.StatusGone)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatal(err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: srv.URL,
		WorkerID:    "w1",
		AuthToken:   "tok",
		HTTPClient:  srv.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	p, _ := stub.New()
	_ = reg.Register(p)
	r, err := New(Options{
		Registry:          reg,
		WorktreeManager:   wtm,
		Poster:            poster,
		HTTPClient:        srv.Client(),
		HeartbeatInterval: 50 * time.Millisecond,
		SkipBackstop:      true,
		SkipSteering:      true,
	})
	if err != nil {
		t.Fatal(err)
	}

	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-LOOP-LOST"),
		WorkerID:    "w1",
		AuthToken:   "tok",
		PlatformURL: srv.URL,
		ResolvedProfile: ResolvedProfile{
			Provider: agent.ProviderStub,
			ProviderConfig: map[string]any{
				"stub.behavior": string(stub.BehaviorHangThenTimeout),
			},
		},
	}
	qw.Repository = bareRepo

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, _ := r.Run(ctx, qw)

	// Either lost-ownership or timeout depending on the cancellation
	// race; lost-ownership is the expected outcome but we tolerate
	// timeout to keep the test stable on slow CI.
	if res.FailureMode != FailureLostOwnership && res.FailureMode != FailureTimeout {
		t.Errorf("FailureMode = %q; want lost-ownership or timeout", res.FailureMode)
	}
}

// TestObserveEvent_ScansWorkResultMarker confirms the loop's
// AssistantText scanner reads the WORK_RESULT:passed/failed marker.
func TestObserveEvent_ScansWorkResultMarker(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"WORK_RESULT:passed", "passed"},
		{"<!-- WORK_RESULT:failed -->", "failed"},
		{"some text WORK_RESULT: passed and more", "passed"},
		{"no marker here", ""},
	}
	for _, tc := range cases {
		got := scanWorkResult(tc.text)
		if got != tc.want {
			t.Errorf("scanWorkResult(%q) = %q; want %q", tc.text, got, tc.want)
		}
	}
}

// TestScanPRURL_ExtractsURL confirms the regex captures a github PR
// URL out of arbitrary tool output.
func TestScanPRURL_ExtractsURL(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"opened https://github.com/RenseiAI/agentfactory-tui/pull/123", "https://github.com/RenseiAI/agentfactory-tui/pull/123"},
		{"https://github.com/foo-bar/baz_qux/pull/9", "https://github.com/foo-bar/baz_qux/pull/9"},
		{"no url", ""},
	}
	for _, tc := range cases {
		got := scanPRURL(tc.text)
		if got != tc.want {
			t.Errorf("scanPRURL(%q) = %q; want %q", tc.text, got, tc.want)
		}
	}
}

// TestEnvToMap_RoundTrip confirms the KEY=VALUE → map conversion the
// loop uses to thread env through the composer.
func TestEnvToMap_RoundTrip(t *testing.T) {
	in := []string{"FOO=bar", "BAZ=", "KEY=val=with=eq"}
	got := envToMap(in)
	if got["FOO"] != "bar" {
		t.Errorf("FOO = %q; want bar", got["FOO"])
	}
	if v, ok := got["BAZ"]; !ok || v != "" {
		t.Errorf("BAZ = %q (ok=%v); want empty present", v, ok)
	}
	if got["KEY"] != "val=with=eq" {
		t.Errorf("KEY = %q; want val=with=eq", got["KEY"])
	}
}

// TestBuildSessionEnv_PopulatesStandardKeys confirms LINEAR_* +
// AGENTFACTORY_* keys land on the per-session env.
func TestBuildSessionEnv_PopulatesStandardKeys(t *testing.T) {
	qw := QueuedWork{
		QueuedWork:  queuedWorkBase("REN-ENV-1"),
		WorkerID:    "w1",
		AuthToken:   "tok",
		PlatformURL: "https://example.test",
	}
	envOut := buildSessionEnv(qw)
	for _, key := range []string{
		"AGENTFACTORY_SESSION_ID",
		"LINEAR_SESSION_ID",
		"LINEAR_ISSUE_ID",
		"LINEAR_ISSUE_IDENTIFIER",
		"LINEAR_WORK_TYPE",
		"AGENTFACTORY_PROJECT",
		"AGENTFACTORY_ORG_ID",
		"AGENTFACTORY_API_URL",
		"WORKER_AUTH_TOKEN",
	} {
		if envOut[key] == "" {
			t.Errorf("env missing %q", key)
		}
	}
}
