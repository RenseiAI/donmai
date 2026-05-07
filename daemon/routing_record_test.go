// Package daemon routing_record_test.go covers the Wave 11 / S6a wire-up
// of RoutingTraceStore.RecordDecision into the WorkerSpawner's
// SessionEventStarted listener.
//
// The end-to-end correctness check (HTTP layer through the listener)
// lives in handle_routing_test.go; this file pins the in-process
// projection: spawner emits Started → store has a recorded decision
// keyed by session id → Explain returns it.
package daemon

import (
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/afclient"
)

// TestRecordOSSRoutingDecision_NilStoreSafe pins the contract that the
// helper is a no-op when routingTraces is nil — important because
// daemon construction in some test paths bypasses New() and leaves the
// store unpopulated.
func TestRecordOSSRoutingDecision_NilStoreSafe(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	// Must not panic.
	d.recordOSSRoutingDecision("sess-anything")
}

// TestRecordOSSRoutingDecision_NoRegistryFallsBackToStub covers the
// fallback path when ProviderRegistry is nil (test/no-orchestrator
// paths); ChosenLLM must be "stub" by contract.
func TestRecordOSSRoutingDecision_NoRegistryFallsBackToStub(t *testing.T) {
	t.Parallel()
	d := New(Options{
		ConfigPath: "/dev/null",
		HTTPHost:   "127.0.0.1",
		HTTPPort:   0,
	})

	d.recordOSSRoutingDecision("sess-stub-1")

	decision, trace, ok := d.RoutingTraces().Explain("sess-stub-1")
	if !ok {
		t.Fatal("Explain(sess-stub-1) ok=false, want true")
	}
	if decision.SessionID != "sess-stub-1" {
		t.Errorf("SessionID = %q, want sess-stub-1", decision.SessionID)
	}
	if decision.ChosenSandbox != "local" {
		t.Errorf("ChosenSandbox = %q, want local", decision.ChosenSandbox)
	}
	if decision.ChosenLLM != "stub" {
		t.Errorf("ChosenLLM = %q, want stub (no registry → fallback)", decision.ChosenLLM)
	}
	if decision.DecidedAt.IsZero() {
		t.Error("DecidedAt is zero, want now()")
	}
	if len(trace) != 1 {
		t.Fatalf("len(trace) = %d, want 1", len(trace))
	}
	step := trace[0]
	if step.Phase != "capability-filter" {
		t.Errorf("trace[0].Phase = %q, want capability-filter", step.Phase)
	}
	if step.Dimension != "sandbox" {
		t.Errorf("trace[0].Dimension = %q, want sandbox", step.Dimension)
	}
	if len(step.Remaining) != 1 || step.Remaining[0] != "local" {
		t.Errorf("trace[0].Remaining = %+v, want [local]", step.Remaining)
	}
	if step.Note == "" {
		t.Error("trace[0].Note empty, want OSS-only rationale string")
	}
}

// TestRecordOSSRoutingDecision_UsesFirstRegistryName covers the
// non-nil-registry path: ChosenLLM must come from Names()[0]
// (registry already returns sorted output, deterministic).
func TestRecordOSSRoutingDecision_UsesFirstRegistryName(t *testing.T) {
	t.Parallel()
	reg := &fakeProviderRegistry{names: []string{"claude", "codex", "stub"}}
	d := New(Options{
		ConfigPath:       "/dev/null",
		HTTPHost:         "127.0.0.1",
		HTTPPort:         0,
		ProviderRegistry: reg,
	})

	d.recordOSSRoutingDecision("sess-reg-1")

	decision, _, ok := d.RoutingTraces().Explain("sess-reg-1")
	if !ok {
		t.Fatal("Explain(sess-reg-1) ok=false, want true")
	}
	if decision.ChosenLLM != "claude" {
		t.Errorf("ChosenLLM = %q, want claude (first sorted name)", decision.ChosenLLM)
	}
}

// TestRecordOSSRoutingDecision_EmptyRegistryNamesFallsBack covers the
// edge where ProviderRegistry is wired but Names() is empty (e.g. a
// daemon that booted before any AgentRuntime registered). ChosenLLM
// must still fall back to "stub" rather than the empty string.
func TestRecordOSSRoutingDecision_EmptyRegistryNamesFallsBack(t *testing.T) {
	t.Parallel()
	reg := &fakeProviderRegistry{names: nil}
	d := New(Options{
		ConfigPath:       "/dev/null",
		HTTPHost:         "127.0.0.1",
		HTTPPort:         0,
		ProviderRegistry: reg,
	})

	d.recordOSSRoutingDecision("sess-empty-1")

	decision, _, ok := d.RoutingTraces().Explain("sess-empty-1")
	if !ok {
		t.Fatal("Explain(sess-empty-1) ok=false, want true")
	}
	if decision.ChosenLLM != "stub" {
		t.Errorf("ChosenLLM = %q, want stub (empty Names() → fallback)", decision.ChosenLLM)
	}
}

// TestDaemon_SpawnerStartedListener_RecordsDecision exercises the live
// listener wired in Daemon.Start: a successful AcceptWork triggers the
// spawner's Started event, the listener fires, and the decision is
// recorded keyed by session id.
//
// Constructed without calling Start() — Start would require registration
// RPCs and a real config. We mirror the pattern used by
// TestHandleWorkareas_List_IncludesSpawnerLivePool: build the spawner
// directly, register the same listener Start would have, then exercise.
// This avoids the known port-7734 bind flake under -race when Start is
// invoked from multiple parallel tests.
func TestDaemon_SpawnerStartedListener_RecordsDecision(t *testing.T) {
	t.Parallel()
	d := New(Options{
		ConfigPath: "/dev/null",
		HTTPHost:   "127.0.0.1",
		HTTPPort:   0,
	})

	spawner := NewWorkerSpawner(SpawnerOptions{
		Projects: []ProjectConfig{{
			ID:         "smoke-alpha",
			Repository: "https://github.com/foo/rensei-smokes-alpha",
		}},
		MaxConcurrentSessions: 2,
		// Long-running stub so the session stays alive across the
		// assertion window — the listener runs on the Started edge,
		// but keeping the session live makes the test less timing-
		// sensitive.
		WorkerCommand: []string{"/bin/sh", "-c", "sleep 30"},
	})
	d.spawner = spawner
	t.Cleanup(func() { _ = spawner.Drain(time.Second) })

	// Wire the same listener Daemon.Start would install.
	spawner.On(func(ev SessionEvent) {
		if ev.Kind != SessionEventStarted || d.routingTraces == nil {
			return
		}
		d.recordOSSRoutingDecision(ev.Spec.SessionID)
	})

	if _, err := spawner.AcceptWork(SessionSpec{
		SessionID:  "sess-listener-1",
		Repository: "smoke-alpha",
		Ref:        "feat/x",
	}); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}

	// The listener fires synchronously from the spawn goroutine before
	// AcceptWork returns the handle (see worker_spawner.go:329 emit
	// before returning); the recording is therefore visible
	// immediately. We poll briefly anyway to keep the test resilient
	// against any future ordering tweak.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := d.RoutingTraces().Explain("sess-listener-1"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	decision, trace, ok := d.RoutingTraces().Explain("sess-listener-1")
	if !ok {
		t.Fatal("Explain(sess-listener-1) ok=false, want true (listener should have fired)")
	}
	if decision.SessionID != "sess-listener-1" {
		t.Errorf("SessionID = %q, want sess-listener-1", decision.SessionID)
	}
	if decision.ChosenSandbox != "local" {
		t.Errorf("ChosenSandbox = %q, want local", decision.ChosenSandbox)
	}
	if decision.ChosenLLM != "stub" {
		t.Errorf("ChosenLLM = %q, want stub (no registry wired)", decision.ChosenLLM)
	}
	if len(trace) != 1 {
		t.Errorf("len(trace) = %d, want 1", len(trace))
	}
}

// TestDaemon_SpawnerEndedListener_DoesNotRecord pins that the listener
// only fires on the Started edge; an Ended event must not produce a
// duplicate or stale recording.
func TestDaemon_SpawnerEndedListener_DoesNotRecord(t *testing.T) {
	t.Parallel()
	d := New(Options{
		ConfigPath: "/dev/null",
		HTTPHost:   "127.0.0.1",
		HTTPPort:   0,
	})
	// Synthesize an Ended event directly via the listener function.
	// We replicate the listener body since it's a closure inside
	// Start(); the equivalent body is exercised via recordOSSRoutingDecision
	// gating from the live test above. Here we verify that the kind
	// filter is correct by hand.
	listener := func(ev SessionEvent) {
		if ev.Kind != SessionEventStarted || d.routingTraces == nil {
			return
		}
		d.recordOSSRoutingDecision(ev.Spec.SessionID)
	}
	listener(SessionEvent{
		Kind: SessionEventEnded,
		Spec: SessionSpec{SessionID: "sess-ended-only"},
	})

	if _, _, ok := d.RoutingTraces().Explain("sess-ended-only"); ok {
		t.Error("Explain returned a recording for an Ended-only event; listener should ignore Ended")
	}
	if got := d.RoutingTraces().Len(); got != 0 {
		t.Errorf("Len = %d, want 0 (Ended events must not write to the ring)", got)
	}
}

// TestDaemon_RecordOSSRoutingDecision_ConcurrentSafe pins that
// concurrent listener invocations across many sessions are safe — the
// underlying RoutingTraceStore takes its own mutex, but the helper's
// composition (registry name read + struct build + RecordDecision) is
// re-entrant.
func TestDaemon_RecordOSSRoutingDecision_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	reg := &fakeProviderRegistry{names: []string{"claude"}}
	d := New(Options{
		ConfigPath:       "/dev/null",
		HTTPHost:         "127.0.0.1",
		HTTPPort:         0,
		ProviderRegistry: reg,
	})

	const n = 32
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			d.recordOSSRoutingDecision(sessionIDFor(idx))
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}

	// Ring size is 50 (default), so all 32 must be retained. Each
	// session id is unique, so per-session lookups must succeed.
	for i := 0; i < n; i++ {
		if _, _, ok := d.RoutingTraces().Explain(sessionIDFor(i)); !ok {
			t.Errorf("Explain(%s) ok=false; concurrent recording lost", sessionIDFor(i))
		}
	}
}

func sessionIDFor(i int) string {
	// Deterministic, unique session ids; avoid fmt to keep this test
	// allocation-light.
	const digits = "0123456789"
	return "sess-conc-" + string(digits[i/10]) + string(digits[i%10])
}

// Compile-time check that the recorded decision shape stays compatible
// with the wire types used in the explain handler.
var (
	_ = afclient.RoutingDecision{}
	_ = afclient.RoutingTraceStep{}
)
