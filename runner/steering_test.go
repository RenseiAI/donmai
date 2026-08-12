package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

// TestShouldSteer_Table covers the decision matrix in steering.go.
// All version-controlled cases use WorkTypeDevelopmentStr (its
// contract requires a PR); the contract-gate behaviour is exercised
// separately in TestShouldSteer_ContractGate.
func TestShouldSteer_Table(t *testing.T) {
	cases := []struct {
		name     string
		obs      streamObservation
		caps     agent.Capabilities
		workType string
		want     bool
	}{
		{
			name:     "no capability",
			obs:      streamObservation{terminalSuccess: true},
			caps:     agent.Capabilities{},
			workType: WorkTypeDevelopmentStr,
			want:     false,
		},
		{
			name:     "unsuccessful terminal",
			obs:      streamObservation{terminalSuccess: false},
			caps:     agent.Capabilities{SupportsMessageInjection: true},
			workType: WorkTypeDevelopmentStr,
			want:     false,
		},
		{
			name:     "PR already opened",
			obs:      streamObservation{terminalSuccess: true, pullRequestURL: "https://example.test/pr/1"},
			caps:     agent.Capabilities{SupportsMessageInjection: true},
			workType: WorkTypeDevelopmentStr,
			want:     false,
		},
		{
			name:     "should steer (injection)",
			obs:      streamObservation{terminalSuccess: true},
			caps:     agent.Capabilities{SupportsMessageInjection: true},
			workType: WorkTypeDevelopmentStr,
			want:     true,
		},
		{
			name:     "should steer (resume only)",
			obs:      streamObservation{terminalSuccess: true},
			caps:     agent.Capabilities{SupportsSessionResume: true},
			workType: WorkTypeDevelopmentStr,
			want:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSteer(tc.obs, tc.caps, tc.workType); got != tc.want {
				t.Fatalf("shouldSteer = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestShouldSteer_ContractGate asserts the work-type gate: a work
// type whose completion is NOT result-sensitive (backlog-groomer,
// refinement, research) is NEVER steered toward a commit/PR — even
// with full provider capability and a successful terminal.
// Result-sensitive types (development, qa, acceptance) keep their
// existing steering flow and are UNCHANGED by this gate.
func TestShouldSteer_ContractGate(t *testing.T) {
	// A fully steer-eligible observation: succeeded, no PR, provider
	// supports injection. Only the work-type gate should decide.
	obs := streamObservation{terminalSuccess: true}
	caps := agent.Capabilities{SupportsMessageInjection: true}

	cases := []struct {
		workType string
		want     bool
	}{
		// Non-result-sensitive (no PR/branch artifact) → never steered.
		{WorkTypeBacklogGroomer, false},
		{WorkTypeResearch, false},
		{WorkTypeRefinement, false},
		{WorkTypeBacklogCreation, false},
		{"imaginary-future-type", false}, // unknown → not result-sensitive → no steering
		// Result-sensitive → behaviour preserved (still steerable).
		{WorkTypeDevelopmentStr, true},
		{WorkTypeInflight, true},
		{WorkTypeQAStr, true},
		{WorkTypeAcceptance, true},
	}
	for _, tc := range cases {
		t.Run(tc.workType, func(t *testing.T) {
			if got := shouldSteer(obs, caps, tc.workType); got != tc.want {
				t.Fatalf("shouldSteer(workType=%q) = %v; want %v", tc.workType, got, tc.want)
			}
		})
	}
}

// TestBuildSteeringPrompt_ContainsCommands ensures the steering
// prompt directs the agent to the canonical commit/push/PR workflow.
func TestBuildSteeringPrompt_ContainsCommands(t *testing.T) {
	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-T-1")}
	got := buildSteeringPrompt(qw, streamObservation{terminalSuccess: true})
	for _, want := range []string{
		"git status",
		"git add -A",
		"git commit",
		"git push",
		"gh pr create",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("steering prompt missing %q\nfull:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "REN-T-1") {
		t.Errorf("steering prompt missing identifier; got:\n%s", got)
	}
}

// TestAttemptSteering_InjectStub uses the stub provider's
// BehaviorInjectTest to confirm the runner's steering path delivers a
// message that produces an AssistantTextEvent + ResultEvent on the
// stub's channel.
func TestAttemptSteering_InjectStub(t *testing.T) {
	r := minimalRunner(t)

	p, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := r.registry.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx, cancel := withCtx(t)
	defer cancel()
	spec := agent.Spec{
		ProviderConfig: map[string]any{
			"stub.behavior": string(stub.BehaviorInjectTest),
		},
	}
	handle, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-S-1")}
	res := &Result{}
	newHandle, err := r.attemptSteering(ctx, p, handle, spec, p.Capabilities(), qw, streamObservation{terminalSuccess: true}, res)
	if err != nil {
		t.Fatalf("attemptSteering: %v", err)
	}
	if newHandle != handle {
		t.Fatal("expected the same handle back on the inject-success path")
	}
	if res.SteeringResumeFallback {
		t.Fatal("expected no resume fallback recorded on the inject-success path")
	}

	// Drain events; expect at least one AssistantTextEvent containing
	// "injected:" prefix. The stub closes the channel after the
	// terminal Result.
	var sawInject bool
	for ev := range handle.Events() {
		if at, ok := ev.(agent.AssistantTextEvent); ok {
			if strings.HasPrefix(at.Text, "injected:") {
				sawInject = true
			}
		}
	}
	if !sawInject {
		t.Fatal("expected AssistantTextEvent with 'injected:' prefix")
	}
	_ = context.Background()
}

// TestAttemptSteering_UnsupportedIsSoftFail confirms the runner treats an
// ErrUnsupported inject as NON-fatal (returns nil) — the Wave 3 contract
// for the shared injectDirective helper. The caller falls through to the
// deterministic backstop on its own (shouldBackstop is independent of the
// steering return), so a soft-fail here changes no downstream behavior.
func TestAttemptSteering_UnsupportedIsSoftFail(t *testing.T) {
	r := minimalRunner(t)

	p, err := stub.New(stub.WithCapabilities(agent.Capabilities{}))
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	_ = r.registry.Register(p)

	ctx, cancel := withCtx(t)
	defer cancel()
	spec := agent.Spec{
		ProviderConfig: map[string]any{
			"stub.injectUnsupported": true,
		},
	}
	handle, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()

	res := &Result{}
	newHandle, err := r.attemptSteering(ctx, p, handle, spec, p.Capabilities(), QueuedWork{QueuedWork: queuedWorkBase("REN-S-2")}, streamObservation{terminalSuccess: true}, res)
	if err != nil {
		t.Fatalf("expected nil (soft-fail) from attemptSteering with unsupported provider, got %v", err)
	}
	if newHandle != handle {
		t.Fatal("expected the same handle back when neither inject nor resume is supported")
	}
	if res.SteeringResumeFallback {
		t.Fatal("expected no resume fallback recorded when the provider does not support resume")
	}
}

// TestInjectDirective_SoftFails verifies the shared injectDirective helper
// returns nil on the benign provider-can't-accept-now errors
// (agent.ErrUnsupported via the stub) so both steering and the memory
// drain treat them as non-fatal.
func TestInjectDirective_SoftFails(t *testing.T) {
	r := minimalRunner(t)

	p, err := stub.New(stub.WithCapabilities(agent.Capabilities{}))
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	_ = r.registry.Register(p)

	ctx, cancel := withCtx(t)
	defer cancel()
	handle, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{"stub.injectUnsupported": true},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()

	if err := r.injectDirective(ctx, handle, "remember this"); err != nil {
		t.Fatalf("injectDirective should soft-fail on ErrUnsupported, got %v", err)
	}
}

// TestAttemptSteering_ResumeFallback_Table is the stop-and-resume steering
// fallback's acceptance table: it drives attemptSteering against three fake
// harness shapes — inject-capable, resume-only, and neither-capable — via
// the stub provider's BehaviorResumeSteer script, and asserts the fallback
// fires exactly where it should.
//
// The resume-only case additionally proves the stop-and-resume fallback's
// three load-bearing properties end to end: session continuity (the
// resumed Handle carries the SAME provider-native session id), terminal-
// event invariants (the resumed Handle's own stream still emits exactly
// one InitEvent and exactly one terminal ResultEvent before closing — the
// same Handle contract every Spawn/Resume caller relies on), and that the
// queued steer content actually arrives (echoed from the resumed Spec's
// Prompt, exactly where runner/steering.go's attemptSteeringResume places
// it).
func TestAttemptSteering_ResumeFallback_Table(t *testing.T) {
	cases := []struct {
		name               string
		caps               agent.Capabilities
		injectUnsupported  bool
		wantResumeFallback bool
		wantHandleChanged  bool
	}{
		{
			name:               "inject-capable harness steers via Inject and never falls back",
			caps:               agent.Capabilities{SupportsMessageInjection: true, SupportsSessionResume: true},
			injectUnsupported:  false,
			wantResumeFallback: false,
			wantHandleChanged:  false,
		},
		{
			name:               "resume-only harness falls back to stop-and-resume",
			caps:               agent.Capabilities{SupportsMessageInjection: false, SupportsSessionResume: true},
			injectUnsupported:  true,
			wantResumeFallback: true,
			wantHandleChanged:  true,
		},
		{
			name:               "neither-capable harness soft-fails without touching the handle",
			caps:               agent.Capabilities{},
			injectUnsupported:  true,
			wantResumeFallback: false,
			wantHandleChanged:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := minimalRunner(t)
			p, err := stub.New(stub.WithCapabilities(tc.caps))
			if err != nil {
				t.Fatalf("stub.New: %v", err)
			}
			if err := r.registry.Register(p); err != nil {
				t.Fatalf("register: %v", err)
			}

			ctx, cancel := withCtx(t)
			defer cancel()

			providerConfig := map[string]any{
				"stub.behavior": string(stub.BehaviorResumeSteer),
			}
			if tc.injectUnsupported {
				providerConfig["stub.injectUnsupported"] = true
			}
			spec := agent.Spec{ProviderConfig: providerConfig}

			handle, err := p.Spawn(ctx, spec)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}

			// Drain the first turn to its terminal event — attemptSteering
			// is only ever called post-terminal (F.1.1 §4 step 11 fires
			// after step 10's terminal wait) — and capture the session id
			// InitEvent carried for the continuity assertion below.
			var origSessionID string
			for ev := range handle.Events() {
				if ie, ok := ev.(agent.InitEvent); ok {
					origSessionID = ie.SessionID
				}
			}
			if origSessionID == "" {
				t.Fatal("expected an InitEvent with a session id")
			}

			qw := QueuedWork{QueuedWork: queuedWorkBase("REN-RF-1")}
			res := &Result{}
			newHandle, err := r.attemptSteering(ctx, p, handle, spec, tc.caps, qw, streamObservation{terminalSuccess: true}, res)
			if err != nil {
				t.Fatalf("attemptSteering: %v", err)
			}
			if res.SteeringResumeFallback != tc.wantResumeFallback {
				t.Fatalf("SteeringResumeFallback = %v, want %v", res.SteeringResumeFallback, tc.wantResumeFallback)
			}
			if handleChanged := newHandle != handle; handleChanged != tc.wantHandleChanged {
				t.Fatalf("handle changed = %v, want %v", handleChanged, tc.wantHandleChanged)
			}

			if !tc.wantResumeFallback {
				return
			}

			// Session continuity.
			if got := newHandle.SessionID(); got != origSessionID {
				t.Fatalf("resumed session id = %q, want %q (continuity)", got, origSessionID)
			}

			// Terminal-event invariants + steer-content delivery.
			var inits, terminals int
			var sawSteerContent bool
			for ev := range newHandle.Events() {
				switch e := ev.(type) {
				case agent.InitEvent:
					inits++
				case agent.ResultEvent:
					terminals++
				case agent.AssistantTextEvent:
					if strings.Contains(e.Text, "pull request") {
						sawSteerContent = true
					}
				}
			}
			if inits != 1 {
				t.Fatalf("resumed handle InitEvent count = %d, want 1", inits)
			}
			if terminals != 1 {
				t.Fatalf("resumed handle terminal ResultEvent count = %d, want 1", terminals)
			}
			if !sawSteerContent {
				t.Fatal("resumed handle did not receive the queued steer content via Spec.Prompt")
			}
		})
	}
}

// TestAttemptSteering_ResumeFallbackHardError confirms a genuine
// Provider.Resume failure (not agent.ErrUnsupported) surfaces as an error
// from attemptSteering — the caller falls through to the deterministic
// backstop instead of re-consuming events — while still handing back a
// usable (already-stopped) Handle rather than nil.
func TestAttemptSteering_ResumeFallbackHardError(t *testing.T) {
	r := minimalRunner(t)
	caps := agent.Capabilities{SupportsSessionResume: true}
	p, err := stub.New(stub.WithCapabilities(caps))
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	if err := r.registry.Register(p); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx, cancel := withCtx(t)
	defer cancel()
	spec := agent.Spec{
		ProviderConfig: map[string]any{
			"stub.behavior":          string(stub.BehaviorResumeSteer),
			"stub.injectUnsupported": true,
			"stub.resumeFailure":     true,
		},
	}
	handle, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Drain to terminal, matching the post-terminal call site.
	for range handle.Events() { //nolint:revive // draining is the whole point; nothing to do per event
	}

	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-S-5")}
	res := &Result{}
	newHandle, err := r.attemptSteering(ctx, p, handle, spec, caps, qw, streamObservation{terminalSuccess: true}, res)
	if err == nil {
		t.Fatal("expected an error when Provider.Resume hard-fails")
	}
	if newHandle != handle {
		t.Fatal("expected the original (now-stopped) handle back on a hard resume failure")
	}
	if res.SteeringResumeFallback {
		t.Fatal("expected no resume fallback recorded on a hard resume failure")
	}
}

// fakeNoSessionHandle is a minimal agent.Handle whose SessionID is always
// empty, modeling a real provider's Handle before its InitEvent has fired.
// Unlike the stub package's own handle (which assigns its session id
// eagerly at construction for caller convenience — see stub/handle.go), this
// lets the test exercise attemptSteeringResume's "no session id captured"
// guard directly.
type fakeNoSessionHandle struct {
	events chan agent.Event
}

func (h *fakeNoSessionHandle) SessionID() string          { return "" }
func (h *fakeNoSessionHandle) Events() <-chan agent.Event { return h.events }
func (h *fakeNoSessionHandle) Inject(context.Context, string) error {
	return agent.ErrUnsupported
}
func (h *fakeNoSessionHandle) Stop(context.Context) error { return nil }

// TestAttemptSteeringResume_NoSessionIDIsSoftFail confirms the fallback
// treats a missing provider-native session id as a soft no-op (nil error,
// same handle back, no fallback recorded) rather than a hard failure —
// there is nothing to resume from, and it is not the caller's fault.
func TestAttemptSteeringResume_NoSessionIDIsSoftFail(t *testing.T) {
	r := minimalRunner(t)
	p, err := stub.New(stub.WithCapabilities(agent.Capabilities{SupportsSessionResume: true}))
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}

	handle := &fakeNoSessionHandle{events: make(chan agent.Event)}
	close(handle.events)

	ctx, cancel := withCtx(t)
	defer cancel()

	res := &Result{}
	newHandle, err := r.attemptSteeringResume(ctx, p, handle, agent.Spec{}, QueuedWork{QueuedWork: queuedWorkBase("REN-S-6")}, "steer text", res)
	if err != nil {
		t.Fatalf("expected nil (soft-fail) when no session id was captured, got %v", err)
	}
	if newHandle != handle {
		t.Fatal("expected the same handle back")
	}
	if res.SteeringResumeFallback {
		t.Fatal("expected no resume fallback recorded")
	}
}
