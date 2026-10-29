package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

// orderedEvidenceHandle is a fake live session that refuses transcript
// descriptor access before Stop and serves a single path after it. It
// models a session whose writers stop only when Stop returns.
type orderedEvidenceHandle struct {
	events    chan agent.Event
	mu        sync.Mutex
	stopped   bool
	descs     []agent.TerminalTranscriptDescriptor
	injectErr error
	calls     int
	preStop   int
}

func newOrderedEvidenceHandle(path, assistantText string, injectErr error) *orderedEvidenceHandle {
	ch := make(chan agent.Event, 8)
	ch <- agent.InitEvent{SessionID: "evidence-session"}
	if assistantText != "" {
		ch <- agent.AssistantTextEvent{Text: assistantText}
	}
	ch <- agent.ResultEvent{Success: true, Message: "evidence turn complete"}
	close(ch)
	return &orderedEvidenceHandle{
		events:    ch,
		descs:     []agent.TerminalTranscriptDescriptor{{Path: path}},
		injectErr: injectErr,
	}
}

func (h *orderedEvidenceHandle) SessionID() string          { return "evidence-session" }
func (h *orderedEvidenceHandle) Events() <-chan agent.Event { return h.events }

func (h *orderedEvidenceHandle) Inject(context.Context, string) error {
	if h.injectErr != nil {
		return h.injectErr
	}
	return nil
}

func (h *orderedEvidenceHandle) Stop(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	return nil
}

func (h *orderedEvidenceHandle) TerminalTranscripts() ([]agent.TerminalTranscriptDescriptor, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	if !h.stopped {
		h.preStop++
		return nil, errors.New("evidence: session still running")
	}
	return h.descs, nil
}

// plainHandle implements only agent.Handle: the compatibility default
// without the optional descriptor capability.
type plainHandle struct {
	events chan agent.Event
}

func newPlainHandle(assistantText string) *plainHandle {
	ch := make(chan agent.Event, 8)
	ch <- agent.InitEvent{SessionID: "evidence-session"}
	if assistantText != "" {
		ch <- agent.AssistantTextEvent{Text: assistantText}
	}
	ch <- agent.ResultEvent{Success: true, Message: "evidence turn complete"}
	close(ch)
	return &plainHandle{events: ch}
}

func (h *plainHandle) SessionID() string                    { return "evidence-session" }
func (h *plainHandle) Events() <-chan agent.Event           { return h.events }
func (h *plainHandle) Inject(context.Context, string) error { return nil }
func (h *plainHandle) Stop(context.Context) error           { return nil }

// evidenceProvider delegates identity and manifest to the stub harness
// while serving scripted handles. A nil resumeHandle refuses resume.
type evidenceProvider struct {
	agent.HarnessProvider
	spawnHandle  agent.Handle
	resumeHandle agent.Handle
}

func (p *evidenceProvider) Spawn(context.Context, agent.Spec) (agent.Handle, error) {
	return p.spawnHandle, nil
}

func (p *evidenceProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	if p.resumeHandle != nil {
		return p.resumeHandle, nil
	}
	return nil, agent.ErrUnsupported
}

func stubBaseForEvidence(t *testing.T, caps *agent.Capabilities) agent.HarnessProvider {
	t.Helper()
	var opts []stub.Option
	if caps != nil {
		opts = append(opts, stub.WithCapabilities(*caps))
	}
	base, err := stub.New(opts...)
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	hp, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider does not satisfy HarnessProvider")
	}
	return hp
}

func runEvidenceSession(t *testing.T, provider agent.Provider, workType string, enableSteering bool) (*Result, QueuedWork) {
	t.Helper()
	h := newRunnerHarness(t)
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	if enableSteering {
		h.runner.skipSteering = false
	}
	qw := h.queuedWork("EVIDENCE")
	qw.WorkType = workType
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v (failureMode=%q error=%q)", err, res.FailureMode, res.Error)
	}
	return res, qw
}

// TestTerminalEvidence_CapturedAfterStop proves the runner retains the
// descriptor only after the final handle's writers have stopped. The
// fake refuses access before Stop, so a capture before Stop would retain
// nothing; removing capture retains nothing.
func TestTerminalEvidence_CapturedAfterStop(t *testing.T) {
	const wantPath = "evidence/ordered-stop-7f3a9c.cast"
	handle := newOrderedEvidenceHandle(wantPath, "WORK_RESULT:passed", nil)
	if _, err := handle.TerminalTranscripts(); err == nil {
		t.Fatal("descriptor access before Stop must refuse")
	}
	base := stubBaseForEvidence(t, nil)
	provider := &evidenceProvider{HarnessProvider: base, spawnHandle: handle}

	res, _ := runEvidenceSession(t, provider, "research", false)
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (failureMode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	if len(res.terminalTranscripts) != 1 || res.terminalTranscripts[0].Path != wantPath {
		t.Fatalf("terminalTranscripts = %+v; want exactly [%q]", res.terminalTranscripts, wantPath)
	}
	handle.mu.Lock()
	preStop, calls := handle.preStop, handle.calls
	handle.mu.Unlock()
	if preStop != 1 {
		t.Fatalf("pre-stop refusal calls = %d; want 1 (the pre-Run probe)", preStop)
	}
	if calls < 2 {
		t.Fatalf("descriptor calls = %d; want at least probe + post-stop capture", calls)
	}
	// The runner retains a defensive copy: mutating the handle's slice
	// after the run leaves the retained descriptor unchanged.
	handle.descs[0].Path = "evidence/mutated-after-stop.cast"
	if res.terminalTranscripts[0].Path != wantPath {
		t.Fatalf("retained path mutated to %q; want defensive copy of %q", res.terminalTranscripts[0].Path, wantPath)
	}
}

// TestTerminalEvidence_FinalHandleOnly proves a recovery rail that
// replaces the live handle contributes only the final handle's
// descriptor. The first handle's path must not be retained.
func TestTerminalEvidence_FinalHandleOnly(t *testing.T) {
	const firstPath = "evidence/first-handle-a1b2c3.cast"
	const finalPath = "evidence/final-handle-d4e5f6.cast"
	first := newOrderedEvidenceHandle(firstPath, "WORK_RESULT:passed", agent.ErrUnsupported)
	second := newOrderedEvidenceHandle(finalPath, "WORK_RESULT:passed", agent.ErrUnsupported)
	caps := &agent.Capabilities{SupportsMessageInjection: false, SupportsSessionResume: true}
	base := stubBaseForEvidence(t, caps)
	provider := &evidenceProvider{HarnessProvider: base, spawnHandle: first, resumeHandle: second}

	res, _ := runEvidenceSession(t, provider, "development", true)
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (failureMode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	if !res.SteeringResumeFallback {
		t.Fatal("expected the steering stop-and-resume fallback to replace the handle")
	}
	if len(res.terminalTranscripts) != 1 || res.terminalTranscripts[0].Path != finalPath {
		t.Fatalf("terminalTranscripts = %+v; want exactly [%q]", res.terminalTranscripts, finalPath)
	}
	for _, p := range res.terminalTranscripts {
		if p.Path == firstPath {
			t.Fatalf("retained first handle path %q; want only the final handle", firstPath)
		}
	}
}

// TestTerminalEvidence_CompatibilityWithoutCapability proves a handle
// without the optional capability follows its existing terminal path
// unchanged and retains nothing.
func TestTerminalEvidence_CompatibilityWithoutCapability(t *testing.T) {
	handle := newPlainHandle("WORK_RESULT:passed")
	base := stubBaseForEvidence(t, nil)
	provider := &evidenceProvider{HarnessProvider: base, spawnHandle: handle}

	res, _ := runEvidenceSession(t, provider, "research", false)
	if res.Status != "completed" {
		t.Fatalf("Status = %q; want completed (failureMode=%q error=%q)", res.Status, res.FailureMode, res.Error)
	}
	if len(res.terminalTranscripts) != 0 {
		t.Fatalf("terminalTranscripts = %+v; want empty without the capability", res.terminalTranscripts)
	}
}

// TestTerminalEvidence_SentinelAbsentFromWire proves the private
// descriptor never reaches the terminal body or the marshalled result.
// The sentinel is retained privately and absent everywhere serialized.
func TestTerminalEvidence_SentinelAbsentFromWire(t *testing.T) {
	const sentinel = "evidence/sentinel-terminal-9f3a1c.cast"
	handle := newOrderedEvidenceHandle(sentinel, "WORK_RESULT:passed", nil)
	base := stubBaseForEvidence(t, nil)
	provider := &evidenceProvider{HarnessProvider: base, spawnHandle: handle}

	h := newRunnerHarness(t)
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	qw := h.queuedWork("EVIDENCE-WIRE")
	qw.WorkType = "research"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v (failureMode=%q error=%q)", err, res.FailureMode, res.Error)
	}
	if len(res.terminalTranscripts) != 1 || res.terminalTranscripts[0].Path != sentinel {
		t.Fatalf("terminalTranscripts = %+v; want exactly [%q]", res.terminalTranscripts, sentinel)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if strings.Contains(string(raw), sentinel) {
		t.Fatalf("marshalled Result carries the sentinel descriptor path")
	}
	embedded, err := json.Marshal(res.Result)
	if err != nil {
		t.Fatalf("marshal embedded result: %v", err)
	}
	if strings.Contains(string(embedded), sentinel) {
		t.Fatalf("marshalled agent.Result carries the sentinel descriptor path")
	}
	body, err := h.runner.poster.PrepareTerminalStatusBody(context.Background(), qw.SessionID, res.Result, nil)
	if err != nil {
		t.Fatalf("PrepareTerminalStatusBody: %v", err)
	}
	if strings.Contains(string(body), sentinel) {
		t.Fatalf("prepared terminal body carries the sentinel descriptor path")
	}
}

// spawnFailEvidenceProvider fails Spawn before any process exists.
type spawnFailEvidenceProvider struct {
	agent.HarnessProvider
	spawnErr error
}

func (p *spawnFailEvidenceProvider) Spawn(context.Context, agent.Spec) (agent.Handle, error) {
	return nil, p.spawnErr
}

func headlessTailLines(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

func assertBoundedRedacted(t *testing.T, tail string, secrets ...string) {
	t.Helper()
	if tail == "" {
		t.Fatal("OutputTail is empty; want bounded tail")
	}
	if len(tail) > afclient.MaxHeadlessFailureTailBytes {
		t.Fatalf("tail is %d bytes; want at most %d", len(tail), afclient.MaxHeadlessFailureTailBytes)
	}
	if n := headlessTailLines(tail); n > afclient.MaxHeadlessFailureTailLines {
		t.Fatalf("tail has %d lines; want at most %d", n, afclient.MaxHeadlessFailureTailLines)
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(tail, s) {
			t.Fatalf("tail carries unredacted secret")
		}
	}
}

// Headless regression: a 400 with provider code keeps its cause and the
// diagnostic tail is bounded/redacted; an unavailable upstream stays null.
func TestTerminalEvidence_HeadlessProviderErrorPreservesUpstreamAndBoundedTail(t *testing.T) {
	t.Parallel()
	secret := "api_key=super-secret-value-abc123"
	msg := "endpoint refused\n" + strings.Repeat("filler line\n", 80) + secret + "\n" + strings.Repeat("x", 6000) + "\nBearer hidden-token-xyz"
	upstream := &agent.UpstreamError{HTTPStatus: 400, ProviderCode: "invalid_request", ProviderMessage: "bad params"}
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "qa", skipSteering: true,
		turns: []verdictScriptTurn{{events: []agent.Event{agent.ErrorEvent{Message: msg, Upstream: upstream}}}},
	})
	if res.Status != "failed" || res.FailureMode != FailureProviderError {
		t.Fatalf("Status=%q Mode=%q; want failed/provider-error", res.Status, res.FailureMode)
	}
	if res.Upstream == nil || res.Upstream.HTTPStatus != 400 || res.Upstream.ProviderCode != "invalid_request" {
		t.Fatalf("Upstream=%+v; want 400/invalid_request", res.Upstream)
	}
	if res.ExitCode != nil {
		t.Fatalf("ExitCode=%d; want null", *res.ExitCode)
	}
	assertBoundedRedacted(t, res.OutputTail, secret, "hidden-token-xyz")
	if res.FailurePhase == "" || res.FailureDecidedBy == "" {
		t.Fatalf("phase=%q decidedBy=%q; want evidence", res.FailurePhase, res.FailureDecidedBy)
	}
	h := newRunnerHarness(t)
	body, err := h.runner.poster.PrepareTerminalStatusBody(context.Background(), "evidence-headless", res.Result, nil)
	if err != nil {
		t.Fatalf("PrepareTerminalStatusBody: %v", err)
	}
	raw := string(body)
	for _, want := range []string{`"failureMode":"provider-error"`, `"httpStatus":400`, `"providerCode":"invalid_request"`, `"outputTail"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("terminal body lacks %s: %s", want, raw)
		}
	}
	if strings.Contains(raw, secret) || strings.Contains(raw, "hidden-token-xyz") {
		t.Fatalf("terminal body carries secret")
	}
	// Unavailable upstream stays null with a bounded tail.
	secret2 := "api_key=another-hidden-value-xyz789"
	res2, _ := runScriptedSession(t, scriptedSession{
		workType: "qa", skipSteering: true,
		turns: []verdictScriptTurn{{events: []agent.Event{agent.ErrorEvent{Message: strings.Repeat("pad\n", 100) + secret2 + "\n" + strings.Repeat("z", 8000)}}}},
	})
	if res2.Upstream != nil {
		t.Fatalf("Upstream=%+v; want nil", res2.Upstream)
	}
	assertBoundedRedacted(t, res2.OutputTail, secret2)
}

// Spawn before any process: null exit, phase/decider, no invented result.
func TestTerminalEvidence_SpawnFailureHasNullExitCodeWithPhase(t *testing.T) {
	t.Parallel()
	base := stubBaseForEvidence(t, nil)
	provider := &spawnFailEvidenceProvider{HarnessProvider: base, spawnErr: errors.New("handshake failed: Bearer hidden-spawn-token dial refused\n" + strings.Repeat("detail\n", 80))}
	h := newRunnerHarness(t)
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	qw := h.queuedWork("EVIDENCE-SPAWN")
	qw.WorkType = "research"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err == nil || res == nil {
		t.Fatalf("err=%v res=%+v; want spawn failure", err, res)
	}
	if res.Status != "failed" || res.FailureMode != FailureSpawn {
		t.Fatalf("Status=%q Mode=%q; want failed/spawn-failed", res.Status, res.FailureMode)
	}
	if res.ExitCode != nil {
		t.Fatalf("ExitCode=%d; want null (never zero)", *res.ExitCode)
	}
	if res.FailurePhase != "spawn" || res.FailureDecidedBy != "runner" {
		t.Fatalf("phase=%q decidedBy=%q; want spawn/runner", res.FailurePhase, res.FailureDecidedBy)
	}
	if res.WorkResult != "" {
		t.Fatalf("WorkResult=%q; want empty", res.WorkResult)
	}
	assertBoundedRedacted(t, res.OutputTail, "hidden-spawn-token")
	if res.Upstream != nil {
		t.Fatalf("Upstream=%+v; want nil", res.Upstream)
	}
}

// Negative control: completed runs carry no failure evidence.
func TestTerminalEvidence_SuccessCarriesNoFailureEvidence(t *testing.T) {
	t.Parallel()
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "qa", skipSteering: true,
		turns: []verdictScriptTurn{{text: "done\n<!-- WORK_RESULT:passed -->"}},
	})
	if res.Status != "completed" {
		t.Fatalf("Status=%q; want completed", res.Status)
	}
	if res.FailureMode != "" || res.Upstream != nil || res.ExitCode != nil || res.OutputTail != "" || res.FailurePhase != "" || res.FailureDecidedBy != "" {
		t.Fatalf("completed carries evidence: %+v", res)
	}
}

// Unknown failure modes report no phase rather than an invented stream
// phase. Known spawn/provision/stream modes keep their explicit mapping.
func TestTerminalEvidence_FailurePhaseUnknownReportsNoPhase(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{
		FailureAgentBlocked, FailureBudgetExceeded, FailureBackstop,
		FailureKitProvision, FailureProviderResolve, FailurePromptRender,
		FailureInteractiveInput, "totally-unknown",
	} {
		if phase, decidedBy := failurePhaseForMode(mode); phase != "" || decidedBy != "" {
			t.Fatalf("failurePhaseForMode(%q) = %q/%q; want empty", mode, phase, decidedBy)
		}
	}
	for _, tc := range []struct {
		mode        string
		wantPhase   string
		wantDecided string
	}{
		{FailureSpawn, "spawn", "runner"},
		{FailureWorktreeProvision, "provision", "runner"},
		{FailureProviderError, "stream", "runner"},
		{FailureSilentExit, "stream", "runner"},
		{FailureTimeout, "stream", "runner"},
		{FailureNoProgress, "stream", "runner"},
	} {
		if phase, decidedBy := failurePhaseForMode(tc.mode); phase != tc.wantPhase || decidedBy != tc.wantDecided {
			t.Fatalf("failurePhaseForMode(%q) = %q/%q; want %q/%q", tc.mode, phase, decidedBy, tc.wantPhase, tc.wantDecided)
		}
	}
	// Production entry: a deliberately declined agent reports no invented
	// phase through the real run loop.
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "qa", skipSteering: true,
		turns: []verdictScriptTurn{{text: "need a decision\nAGENT_BLOCKED: need a product decision\nWORK_RESULT: blocked"}},
	})
	if res.Status != "failed" || res.FailureMode != FailureAgentBlocked {
		t.Fatalf("Status=%q Mode=%q; want failed/agent-blocked", res.Status, res.FailureMode)
	}
	if res.FailurePhase != "" || res.FailureDecidedBy != "" {
		t.Fatalf("phase=%q decidedBy=%q; want empty (no invented stream)", res.FailurePhase, res.FailureDecidedBy)
	}
	if res.OutputTail == "" {
		t.Fatal("OutputTail is empty; want bounded tail even with empty phase")
	}
}

// Pre-stream early returns carry bounded evidence through the real run:
// a provider-resolve refusal has a null exit and a bounded redacted tail.
func TestTerminalEvidence_PreStreamProviderResolveCarriesBoundedTail(t *testing.T) {
	t.Parallel()
	h := newRunnerHarness(t)
	qw := h.queuedWork("EVIDENCE-RESOLVE")
	qw.ResolvedProfile.Provider = agent.ProviderName("nonexistent")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err == nil || res == nil {
		t.Fatalf("err=%v res=%+v; want provider-resolve failure", err, res)
	}
	if res.Status != "failed" || res.FailureMode != FailureProviderResolve {
		t.Fatalf("Status=%q Mode=%q; want failed/provider-resolve", res.Status, res.FailureMode)
	}
	if res.ExitCode != nil {
		t.Fatalf("ExitCode=%d; want null", *res.ExitCode)
	}
	if res.OutputTail == "" {
		t.Fatal("OutputTail is empty; want bounded tail on pre-stream failure")
	}
	assertBoundedRedacted(t, res.OutputTail)
}

// Oversized diagnostic tails stay bounded through the real run: a 100 KB
// provider-error message completes with a 4096-byte redacted tail.
func TestTerminalEvidence_OversizedTailStaysBounded(t *testing.T) {
	t.Parallel()
	msg := strings.Repeat("x", 100*1024) + "\napi_key=oversized-secret-value"
	upstream := &agent.UpstreamError{HTTPStatus: 500, ProviderCode: "server_error", ProviderMessage: "boom"}
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "qa", skipSteering: true,
		turns: []verdictScriptTurn{{events: []agent.Event{agent.ErrorEvent{Message: msg, Upstream: upstream}}}},
	})
	if res.Status != "failed" || res.FailureMode != FailureProviderError {
		t.Fatalf("Status=%q Mode=%q; want failed/provider-error", res.Status, res.FailureMode)
	}
	assertBoundedRedacted(t, res.OutputTail, "oversized-secret-value")
}

// Line bound is independent of the byte bound: 200 short lines fit well
// under 4096 bytes, so only the 50-line bound can trim them.
func TestTerminalEvidence_LineBoundIndependentOfByteBound(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("pad-line\n")
	}
	msg := b.String()
	if len(msg) >= afclient.MaxHeadlessFailureTailBytes {
		t.Fatalf("fixture is %d bytes; want under %d so only the line bound applies", len(msg), afclient.MaxHeadlessFailureTailBytes)
	}
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "qa", skipSteering: true,
		turns: []verdictScriptTurn{{events: []agent.Event{agent.ErrorEvent{Message: msg}}}},
	})
	if res.Status != "failed" {
		t.Fatalf("Status=%q; want failed", res.Status)
	}
	if n := headlessTailLines(res.OutputTail); n > afclient.MaxHeadlessFailureTailLines {
		t.Fatalf("tail has %d lines; want at most %d", n, afclient.MaxHeadlessFailureTailLines)
	}
	if n := headlessTailLines(res.OutputTail); n != afclient.MaxHeadlessFailureTailLines {
		t.Fatalf("tail has %d lines; want exactly %d (last lines kept)", n, afclient.MaxHeadlessFailureTailLines)
	}
}

// Zero HTTP status with a provider code keeps the code while the status
// stays null: the endpoint named a code but no status.
func TestTerminalEvidence_ZeroStatusWithCodePreservesCode(t *testing.T) {
	t.Parallel()
	upstream := &agent.UpstreamError{HTTPStatus: 0, ProviderCode: "throttled_code", ProviderMessage: "slow down"}
	res, _ := runScriptedSession(t, scriptedSession{
		workType: "qa", skipSteering: true,
		turns: []verdictScriptTurn{{events: []agent.Event{agent.ErrorEvent{Message: "endpoint throttled", Upstream: upstream}}}},
	})
	if res.Status != "failed" || res.FailureMode != FailureProviderError {
		t.Fatalf("Status=%q Mode=%q; want failed/provider-error", res.Status, res.FailureMode)
	}
	if res.Upstream == nil || res.Upstream.HTTPStatus != 0 || res.Upstream.ProviderCode != "throttled_code" {
		t.Fatalf("Upstream=%+v; want 0/throttled_code", res.Upstream)
	}
	// The posted terminal body preserves the code while the status stays
	// absent: the endpoint named a code but no status.
	h := newRunnerHarness(t)
	body, err := h.runner.poster.PrepareTerminalStatusBody(context.Background(), "evidence-zero-status", res.Result, nil)
	if err != nil {
		t.Fatalf("PrepareTerminalStatusBody: %v", err)
	}
	raw := string(body)
	if !strings.Contains(raw, `"providerCode":"throttled_code"`) {
		t.Fatalf("terminal body lacks providerCode throttled_code: %s", raw)
	}
	if strings.Contains(raw, `"httpStatus":0`) {
		t.Fatalf("terminal body carries zero httpStatus: %s", raw)
	}
}

// providerErrorOnlyHandle emits a failed terminal with no ErrorEvent: the
// envelope Error stays empty while the stream observation carries a
// provider error, so the run-loop finalise block must fill the diagnostic
// tail from the provider error. Disabling that fallback leaves OutputTail
// empty and fails this test.
type providerErrorOnlyHandle struct {
	events chan agent.Event
}

func (h *providerErrorOnlyHandle) SessionID() string { return "evidence-provider-only" }

func (h *providerErrorOnlyHandle) Events() <-chan agent.Event { return h.events }

func (h *providerErrorOnlyHandle) Inject(context.Context, string) error {
	return agent.ErrUnsupported
}

func (h *providerErrorOnlyHandle) Stop(context.Context) error { return nil }

// A failed run whose envelope Error is empty must still carry a bounded
// tail from the stream provider error: the finalise fallback, not the
// Error-derived defer, owns this path.
func TestTerminalEvidence_ProviderErrorOnlyFillsTailWhenErrorEmpty(t *testing.T) {
	t.Parallel()
	const providerText = "model overloaded, retry later"
	ch := make(chan agent.Event, 8)
	ch <- agent.InitEvent{SessionID: "evidence-provider-only"}
	ch <- agent.SystemEvent{Subtype: agent.SystemSubtypeProviderError, Message: providerText}
	ch <- agent.ResultEvent{Success: false}
	close(ch)
	base := stubBaseForEvidence(t, nil)
	provider := &evidenceProvider{HarnessProvider: base, spawnHandle: &providerErrorOnlyHandle{events: ch}}
	h := newRunnerHarness(t)
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	qw := h.queuedWork("EVIDENCE-PROVIDER-ONLY")
	qw.WorkType = "qa"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil {
		t.Fatal("res is nil; want failed envelope")
	}
	if res.Status != "failed" {
		t.Fatalf("Status=%q; want failed", res.Status)
	}
	if res.Error != "" {
		t.Fatalf("Error=%q; want empty so the provider-error fallback owns the tail", res.Error)
	}
	if res.OutputTail == "" {
		t.Fatal("OutputTail is empty; want bounded tail from the provider error when Error is empty")
	}
	if !strings.Contains(res.OutputTail, providerText) {
		t.Fatalf("OutputTail=%q; want provider text %q", res.OutputTail, providerText)
	}
	assertBoundedRedacted(t, res.OutputTail)
	if res.FailurePhase == "" || res.FailureDecidedBy == "" {
		t.Fatalf("phase=%q decidedBy=%q; want evidence", res.FailurePhase, res.FailureDecidedBy)
	}
}
