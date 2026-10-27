package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
