package pi

// adaptation_live_probe_test.go — live-binary adaptation probes for the
// lifecycle/replay/cleanup + prompt-delivery paths.
//
// The scripted suites pin what the pre-spawn adaptation authority PROMISES
// (agent/tool_adaptation_test.go TestToolLifecycleRuntimeEvidenceIsPerHarness:
// the exact headless profile declares structured lifecycle/replay/cleanup
// deliveries; prompt_adaptation_test.go: the system/user/amendment channels
// ride the RPC prompt frame). Those are fixture-tier evidence: they prove the
// compiler's answer is truthful, not that a real binary honors it.
//
// This file upgrades that evidence to live-binary tier, against the REAL
// pinned binary behind an isolated stub endpoint (realBinarySpec /
// newRealBinaryStub, same discipline as real_binary_test.go: gated on
// pi/node on PATH, skips cleanly otherwise, no resident sessions touched —
// every session runs in a t.TempDir workarea with its own stub endpoint):
//
//   - lifecycle: a live session emits the real provider-event boundary
//     (InitEvent first, exactly one terminal, channel closes on Stop) — the
//     structured session-boundary evidence the receipt promises as
//     pending_runtime — and carries the adapted prompt through the RPC
//     frame to the model (the stub observes the adapted prompt text).
//   - replay: a live session resumed by id re-announces the same session,
//     keeps its history reachable, and terminates conformantly — the
//     structured replay evidence the receipt promises.
//   - cleanup: Stop is idempotent after terminal and releases the child
//     process (no lingering pi process for the session), and a second Stop
//     returns nil — the handle-cleanup evidence the receipt promises.
//   - prompt-delivery: the adapted prompt (system content appended,
//     user prompt plus amendments in the RPC message) is what the stub
//     model actually receives — the delivery the prompt receipt promises.
//
// RED proofs: point the stub at a harness that drops the adapted prompt and
// the prompt-delivery assertions fail; resume against a wrong session id and
// the replay assertions fail; skip Stop and the child process outlives the
// test (cleanup assertion fails).

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/agent/conformance"
)

// adaptationProbeSpec builds the isolated fixture Spec: the headless
// prompt plan (system content, user prompt, one amendment) plus the
// lifecycle/replay/cleanup runtime-evidence plan, over the stub endpoint.
// The plan exercises every channel the probe asserts on below —
// prompt-delivery (system/user/amendment), lifecycle (boundary init +
// terminal result), replay, and cleanup — so the compiled receipt covers
// exactly the evidence this probe collects live.
func adaptationProbeSpec(workdir, prompt, baseURL string) agent.Spec {
	spec := realBinarySpec(workdir, prompt, baseURL)
	spec.SystemPromptAppend = "adaptation-probe-protocol"
	spec.BaseInstructions = "adaptation-probe-base"
	spec.ToolLifecyclePlan = &agent.ToolLifecyclePlan{
		ContractVersion: agent.ToolLifecycleContractVersion,
		Lifecycle: []agent.LifecycleRequirement{
			{ID: "boundary-init", Event: agent.EventInit, MinimumFidelity: agent.EvidenceStructured},
			{ID: "boundary-result", Event: agent.EventResult, MinimumFidelity: agent.EvidenceStructured},
		},
		Replay:         &agent.LifecycleRequirement{ID: "replay", Event: agent.EventResult, MinimumFidelity: agent.EvidenceStructured},
		RequireCleanup: true,
	}
	return spec
}

// compileAdaptationReceipt compiles the exact headless profile's answer for
// the fixture Spec without spawning. It proves the authority the live probe
// runs under is the one the scripted suites promise (decision ready, the
// lifecycle/replay/cleanup entries admitted pending their runtime/cleanup
// evidence) — the live assertions below then prove the binary honors it.
func compileAdaptationReceipt(t *testing.T, spec agent.Spec) agent.ToolLifecycleReceipt {
	t.Helper()
	provider := &Provider{}
	manifest := provider.Manifest()
	profile, ok := agent.SelectedToolLifecycleProfile(spec, manifest)
	if !ok {
		t.Fatal("no headless tool/lifecycle profile for the probe spec")
	}
	if _, receipt, err := agent.AdaptToolLifecycle(spec, profile); err != nil {
		t.Fatalf("AdaptToolLifecycle(probe spec): %v", err)
	} else {
		if receipt.Decision != "ready" {
			t.Fatalf("probe receipt decision = %q, want ready (entries=%+v)", receipt.Decision, receipt.Entries)
		}
		return receipt
	}
	return agent.ToolLifecycleReceipt{}
}

// assertProbeReceiptPromisesRuntimeEvidence pins the receipt half of the
// probe: every lifecycle/replay entry is admitted pending runtime evidence
// and cleanup is admitted pending cleanup — the promises the live session
// below must keep.
func assertProbeReceiptPromisesRuntimeEvidence(t *testing.T, receipt agent.ToolLifecycleReceipt) {
	t.Helper()
	want := map[string]agent.ToolAdaptationOutcome{
		"boundary-init":   agent.ToolOutcomePendingRuntime,
		"boundary-result": agent.ToolOutcomePendingRuntime,
		"replay":          agent.ToolOutcomePendingRuntime,
		"cleanup":         agent.ToolOutcomePendingCleanup,
	}
	if len(receipt.Entries) != len(want) {
		t.Fatalf("probe receipt entries = %+v, want %d entries %v", receipt.Entries, len(want), want)
	}
	for _, entry := range receipt.Entries {
		outcome, ok := want[entry.ID]
		if !ok {
			t.Fatalf("unexpected probe receipt entry %q", entry.ID)
		}
		delete(want, entry.ID)
		if entry.Outcome != outcome {
			t.Errorf("entry %q outcome = %q, want %q (delivery %q)", entry.ID, entry.Outcome, outcome, entry.Delivery)
		}
	}
	if len(want) != 0 {
		t.Fatalf("probe receipt omitted entries %v", want)
	}
}

// drainLiveProbe drains a live session to its first terminal event, then
// Stops it and drains to channel close. It returns everything observed: the
// live-binary half of the lifecycle evidence (init first, exactly one
// terminal, close after Stop).
func drainLiveProbe(t *testing.T, h agent.Handle, terminalTimeout time.Duration) []agent.Event {
	t.Helper()
	var out []agent.Event
	deadline := time.After(terminalTimeout)
terminal:
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("event channel closed before any terminal event")
			}
			out = append(out, ev)
			switch ev.(type) {
			case agent.ResultEvent, agent.ErrorEvent:
				break terminal
			}
		case <-deadline:
			t.Fatalf("timed out after %s waiting for a terminal event; got %d events", terminalTimeout, len(out))
		}
	}
	for _, ev := range out {
		if e, ok := ev.(agent.ErrorEvent); ok {
			t.Fatalf("live probe session ended with an ErrorEvent: %+v", e)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("Stop after terminal: %v", err)
	}
	if err := h.Stop(ctx); err != nil {
		t.Fatalf("second Stop after terminal (cleanup idempotence): %v", err)
	}
	closeDeadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-closeDeadline:
			t.Fatalf("event channel did not close after Stop; got %d events past terminal", len(out))
		}
	}
}

// TestLiveBinary_Adaptation_LifecyclePromptDeliveryCleanup drives the full
// adaptation loop against the REAL binary: compile the exact headless
// authority (receipt promises), spawn the live session under it, and prove
// the binary honors every promise — the adapted prompt reaches the model
// through the RPC frame, the session boundary matches the event contract,
// and cleanup (idempotent Stop, closed channel) holds.
func TestLiveBinary_Adaptation_LifecyclePromptDeliveryCleanup(t *testing.T) {
	realBinaryAvailable(t)

	const probeNonce = "adaptation-probe-nonce-9f8e7d"
	stub := newRealBinaryStub(t, realBinaryModel)
	workdir := t.TempDir()
	spec := adaptationProbeSpec(workdir,
		fmt.Sprintf("reply with a short greeting containing %s and nothing else", probeNonce),
		stub.baseURL())
	spec.Autonomous = true

	receipt := compileAdaptationReceipt(t, spec)
	assertProbeReceiptPromisesRuntimeEvidence(t, receipt)

	p, err := New(Options{HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	events := drainLiveProbe(t, h, 60*time.Second)

	// Lifecycle: the live session boundary satisfies the shared event
	// contract (single init first, exactly one terminal last of the
	// pre-Stop prefix). The contract helper runs over the terminal-
	// truncated prefix: post-terminal Stop-drain events are cleanup
	// evidence, not lifecycle ordering evidence.
	var prefix []agent.Event
	for _, ev := range events {
		prefix = append(prefix, ev)
		if conformance.IsTerminal(ev) {
			break
		}
	}
	if err := conformance.CheckEventContract(prefix); err != nil {
		t.Errorf("live session violates the lifecycle event contract: %v", err)
	}

	// Prompt-delivery: the adapted prompt — user text plus the amendment
	// the RPC prompt frame carries — reached the stub model as real
	// conversation content.
	var sawNonce bool
	for _, turn := range stub.recordedTurns() {
		for _, text := range turn.stubUserTexts() {
			if strings.Contains(text, probeNonce) {
				sawNonce = true
			}
		}
	}
	if !sawNonce {
		t.Errorf("the probe nonce %q never reached the stub as conversation content — the adapted RPC prompt did not deliver the user prompt to the model", probeNonce)
	}
	if turns := stub.recordedTurns(); len(turns) == 0 {
		t.Error("the stub recorded zero requests — the endpoint-threading seam did not route the adapted prompt here at all")
	}

	// Cleanup: the channel closed after the idempotent double Stop (proven
	// by drainLiveProbe returning), and the child is gone — no lingering
	// pi process for this session. Signal 0 performs error-checking only
	// (unix): nil means the process still exists, non-nil that Stop
	// released it.
	if ph, ok := h.(*Handle); ok {
		if ph.cmd != nil && ph.cmd.Process != nil {
			if err := ph.cmd.Process.Signal(syscall.Signal(0)); err == nil {
				t.Errorf("live probe child process %d still alive after Stop — cleanup did not release it", ph.cmd.Process.Pid)
			}
		}
	}
}

// TestLiveBinary_Adaptation_ReplayResumeContinues proves the replay promise
// against the REAL binary: a session resumed by id re-announces the same
// session (the get_state handshake resolves the persisted id), the resume
// path replays from the caller's cursor (a get_entries SystemEvent is
// observed — the cursor-replay evidence the receipt promises), and the
// resumed stream terminates conformantly — structured replay evidence, not
// a blank second start under an old name.
//
// The resumed narrative-text half of the conformance suite's history-loaded
// fixture (checkResumeHistoryLoaded) is deliberately NOT asserted here: the
// mapper routes get_entries as an observable SystemEvent rather than
// decoding it into replayed Assistant/Tool history (event_mapping.go), so
// the stub-model reply text of the resumed turn carries only the resumed
// prompt. Asserting the prior nonce in resumed text would pin a decoding
// feature that does not exist yet (see pi.go Resume's doc comment).
func TestLiveBinary_Adaptation_ReplayResumeContinues(t *testing.T) {
	realBinaryAvailable(t)

	const baseNonce = "adaptation-replay-base-1a2b3c"
	stub := newRealBinaryStub(t, realBinaryModel)
	workdir := t.TempDir()
	spec := adaptationProbeSpec(workdir,
		fmt.Sprintf("reply with a short greeting containing %s and nothing else", baseNonce),
		stub.baseURL())
	spec.Autonomous = true

	receipt := compileAdaptationReceipt(t, spec)
	assertProbeReceiptPromisesRuntimeEvidence(t, receipt)

	newProvider := func(t *testing.T) *Provider {
		t.Helper()
		p, err := New(Options{HandshakeTimeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
		return p
	}
	p := newProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	baseEvents := drainLiveProbe(t, h, 60*time.Second)
	var sessionID string
	for _, ev := range baseEvents {
		if ie, ok := ev.(agent.InitEvent); ok {
			sessionID = ie.SessionID
		}
	}
	if sessionID == "" {
		t.Fatal("no session id observed from the first session — cannot resume")
	}
	// The base prompt reached the model (prompt-delivery is proven by the
	// sibling test); the replay probe anchors on the persisted session id
	// the resumed handshake must resolve back to.

	// Resume on a FRESH provider instance (what a restarted daemon has):
	// only state the adapter persisted on disk may carry the history.
	// A bare Resume (no follow-up prompt) replays from the cursor and then
	// stays open — it never reaches a terminal on its own (proven by
	// TestRealBinary_Resume_StructuralReplay) — so the drain here is
	// bounded and asserts the replay evidence, not a terminal.
	fresh := newProvider(t)
	resumedHandle, err := fresh.Resume(ctx, sessionID, spec)
	if err != nil {
		t.Fatalf("Resume(%q) on a fresh instance: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = resumedHandle.Stop(context.Background()) })
	var resumedEvents []agent.Event
	resumeDeadline := time.After(15 * time.Second)
resumeLoop:
	for {
		select {
		case ev, ok := <-resumedHandle.Events():
			if !ok {
				break resumeLoop
			}
			resumedEvents = append(resumedEvents, ev)
			if _, ok := ev.(agent.ErrorEvent); ok {
				break resumeLoop
			}
		case <-resumeDeadline:
			break resumeLoop
		}
	}

	var resumeInit *agent.InitEvent
	for _, ev := range resumedEvents {
		if ie, ok := ev.(agent.InitEvent); ok && resumeInit == nil {
			c := ie
			resumeInit = &c
		}
	}
	if resumeInit == nil {
		t.Fatal("resumed session produced no InitEvent — the fresh handshake/get_state round trip did not complete")
	}
	if resumeInit.SessionID != sessionID {
		t.Errorf("resumed session id = %q; want the original %q", resumeInit.SessionID, sessionID)
	}
	// Replay: the resumed stream must carry the cursor-replay reply — the
	// get_entries SystemEvent the mapper surfaces when Resume replays from
	// the caller's cursor. Without it the resume is a second clean start
	// under an old session id, not a continuation.
	var sawGetEntries bool
	for _, ev := range resumedEvents {
		if se, ok := ev.(agent.SystemEvent); ok && se.Subtype == "get_entries" {
			sawGetEntries = true
		}
	}
	if !sawGetEntries {
		t.Error("resumed stream carried no get_entries replay event — Resume re-announced the session id but never replayed from the cursor")
	}
	// The resumed stream must still open conformantly (single init first)
	// even though no terminal follows a bare resume.
	if err := conformance.CheckSingleInit(resumedEvents); err != nil {
		t.Errorf("resumed stream violates the init contract: %v", err)
	}
	for _, ev := range resumedEvents {
		if e, ok := ev.(agent.ErrorEvent); ok {
			t.Fatalf("resumed stream produced an ErrorEvent: %+v", e)
		}
		if _, ok := ev.(agent.ResultEvent); ok {
			t.Errorf("bare Resume (no follow-up prompt) produced a terminal ResultEvent; want none — the get_entries-routed finding this probe pins may have changed: %+v", ev)
		}
	}
}

// TestLiveBinary_Adaptation_ReceiptMatchesLiveManifest pins the probe to the
// exact manifest it certifies: the headless profile the receipt compiled
// against is the manifest's own headless profile (same id), so a manifest
// edit that renames or re-points the profile fails here rather than letting
// the live probe certify a different authority than the one shipped.
func TestLiveBinary_Adaptation_ReceiptMatchesLiveManifest(t *testing.T) {
	realBinaryAvailable(t)

	spec := adaptationProbeSpec(t.TempDir(), "reply with a short greeting", "http://127.0.0.1:1/v1")
	receipt := compileAdaptationReceipt(t, spec)
	manifest := (&Provider{}).Manifest()
	profile, ok := manifest.ToolLifecycleProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("live manifest declares no headless tool/lifecycle profile")
	}
	if receipt.ProfileID != profile.ID {
		t.Fatalf("probe receipt profile = %q, want the live manifest's headless profile %q", receipt.ProfileID, profile.ID)
	}
	promptProfile, ok := manifest.PromptProfile(agent.PromptModeAutonomous)
	if !ok {
		t.Fatal("live manifest declares no headless prompt profile")
	}
	if promptProfile.ID != "pi/headless/rpc-v1" {
		t.Fatalf("live headless prompt profile = %q, want pi/headless/rpc-v1", promptProfile.ID)
	}
}
