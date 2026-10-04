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

// TestShouldSteer_ContractGate asserts the publication-contract gate: only a
// work type whose completion contract requires a PR may enter commit/PR
// steering. Result-sensitive review and acceptance work still needs a verdict,
// but that verdict does not create a repository-publication obligation.
func TestShouldSteer_ContractGate(t *testing.T) {
	// A fully steer-eligible observation: succeeded, no PR, provider
	// supports injection. Only the work-type gate should decide.
	obs := streamObservation{terminalSuccess: true}
	caps := agent.Capabilities{SupportsMessageInjection: true}

	cases := []struct {
		workType string
		want     bool
	}{
		// No PR obligation → never steered.
		{WorkTypeBacklogGroomer, false},
		{WorkTypeResearch, false},
		{WorkTypeRefinement, false},
		{WorkTypeBacklogCreation, false},
		{WorkTypeQAStr, false},
		{WorkTypeAcceptance, false},
		{WorkTypeMerge, false},
		{WorkTypeCoordination, false},
		{WorkTypeInflightCoordination, false},
		{"imaginary-future-type", false},
		// Implementation contracts still require repository publication.
		{WorkTypeDevelopmentStr, true},
		{WorkTypeInflight, true},
	}
	for _, tc := range cases {
		t.Run(tc.workType, func(t *testing.T) {
			if got := shouldSteer(obs, caps, tc.workType); got != tc.want {
				t.Fatalf("shouldSteer(workType=%q) = %v; want %v", tc.workType, got, tc.want)
			}
		})
	}
}

func TestReadOnlyReviewResultSkipsVersionControlRecoveryAndPreservesTerminalResult(t *testing.T) {
	const review = "## Summary\nRead-only review complete.\n\n## Findings\nNo significant findings.\n\n## Verdict\nApprove.\n<!-- WORK_RESULT:passed -->"
	obs := streamObservation{
		terminalSuccess:   true,
		terminalEvent:     &agent.ResultEvent{Success: true},
		lastAssistantText: review,
		workResult:        "passed",
	}
	res := &Result{}
	obs.applyTo(res, agent.ProviderStub)

	if shouldSteer(obs, agent.Capabilities{SupportsMessageInjection: true, SupportsSessionResume: true}, WorkTypeQAStr) {
		t.Fatal("read-only review result entered commit/PR steering")
	}
	if shouldBackstop(res, WorkTypeQAStr) {
		t.Fatal("read-only review result entered the deterministic git backstop")
	}
	if res.WorkResult != "passed" {
		t.Fatalf("WorkResult = %q; want passed", res.WorkResult)
	}
	if res.Summary != review {
		t.Fatalf("Summary changed during finalization:\n%s", res.Summary)
	}
}

func TestAttemptSteeringReadOnlyReviewDoesNotInjectOrResume(t *testing.T) {
	r := minimalRunner(t)
	p, err := stub.New(stub.WithCapabilities(agent.Capabilities{
		SupportsMessageInjection: true,
		SupportsSessionResume:    true,
	}))
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	handle := &fakeNoSessionHandle{events: make(chan agent.Event)}
	close(handle.events)
	qw := QueuedWork{QueuedWork: queuedWorkBase("READ-ONLY-REVIEW")}
	qw.WorkType = WorkTypeQAStr

	got, err := r.attemptSteering(
		context.Background(), p, handle, agent.Spec{}, p.Capabilities(), qw,
		streamObservation{terminalSuccess: true, workResult: "passed"}, &Result{},
	)
	if err != nil {
		t.Fatalf("attemptSteering: %v", err)
	}
	if got != handle {
		t.Fatal("read-only review replaced its completed handle")
	}
	if handle.injectCalls != 0 {
		t.Fatalf("read-only review injection calls = %d; want 0", handle.injectCalls)
	}
}

// TestBuildSteeringPrompt_ContainsCommands ensures the steering
// prompt directs the agent to the canonical commit/push/PR workflow.
func TestBuildSteeringPrompt_ContainsCommands(t *testing.T) {
	qw := QueuedWork{QueuedWork: queuedWorkBase("xyq-101")}
	got := buildSteeringPrompt(qw, streamObservation{terminalSuccess: true}, backstopVisibilityPrivate)
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
	if !strings.Contains(got, "xyq-101") {
		t.Errorf("steering prompt missing identifier; got:\n%s", got)
	}
}

// TestBuildSteeringPrompt_PointsBackToTaskRequirements pins the nudge's
// wording: it must ask for a PR with a real description plus the task's
// own turn-result/handoff, and must NOT script an auto-filled body or tell
// the agent to stop before posting that result. Reverting buildSteeringPrompt
// to `gh pr create --fill` / "and stop" fails this test.
func TestBuildSteeringPrompt_PointsBackToTaskRequirements(t *testing.T) {
	qw := QueuedWork{QueuedWork: queuedWorkBase("xyq-102")}
	got := buildSteeringPrompt(qw, streamObservation{terminalSuccess: true}, backstopVisibilityPrivate)
	lower := strings.ToLower(got)
	if strings.Contains(got, "--fill") {
		t.Errorf("steering prompt must not prescribe --fill\nfull:\n%s", got)
	}
	if strings.Contains(lower, "and stop") {
		t.Errorf("steering prompt must not tell the agent to stop before posting the task result\nfull:\n%s", got)
	}
	for _, want := range []string{
		"real description",
		"turn-result",
		"handoff",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("steering prompt missing %q\nfull:\n%s", want, got)
		}
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
	events      chan agent.Event
	injectCalls int
}

func (h *fakeNoSessionHandle) SessionID() string          { return "" }
func (h *fakeNoSessionHandle) Events() <-chan agent.Event { return h.events }
func (h *fakeNoSessionHandle) Inject(context.Context, string) error {
	h.injectCalls++
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

// TestSteeringPromptVisibility_Table pins the tail-recovery prompt to the
// backstop's visibility result: private keeps this run's own identifier,
// public and unknown scrub every tracker-id-shaped token (other
// identifiers and lowercase copies included) from the instructed commit
// subject. Deleting the visibility branch (always keeping the
// identifier) fails the public and unknown rows; narrowing the scrub to
// this run's own identifier fails the other-identifier and lowercase
// rows; narrowing it to the exact case fails the lowercase row.
func TestSteeringPromptVisibility_Table(t *testing.T) {
	t.Parallel()
	base := func() QueuedWork {
		qw := QueuedWork{QueuedWork: queuedWorkBase("xyq-301")}
		qw.SessionID = "sess-steer-301"
		return qw
	}
	cases := []struct {
		name      string
		mutate    func(*QueuedWork)
		vis       backstopVisibility
		wantIdent bool
		wantNoIDs bool
	}{
		{
			name:      "private keeps this run's identifier",
			mutate:    func(qw *QueuedWork) { qw.Title = "Repair the widget" },
			vis:       backstopVisibilityPrivate,
			wantIdent: true,
		},
		{
			name:      "public scrubs this run's identifier",
			mutate:    func(qw *QueuedWork) { qw.Title = "Repair the widget" },
			vis:       backstopVisibilityPublic,
			wantNoIDs: true,
		},
		{
			name:      "unknown fails safe to scrubbed",
			mutate:    func(qw *QueuedWork) { qw.Title = "Repair the widget" },
			vis:       backstopVisibilityUnknown,
			wantNoIDs: true,
		},
		{
			name: "public scrubs another identifier from the title",
			mutate: func(qw *QueuedWork) {
				qw.Title = "Follow-up to qwx-88: repair the widget"
				qw.IssueIdentifier = ""
			},
			vis:       backstopVisibilityPublic,
			wantNoIDs: true,
		},
		{
			name: "public scrubs a lowercase copy",
			mutate: func(qw *QueuedWork) {
				qw.Title = "Repair the widget"
				qw.IssueIdentifier = "xyq-301"
				qw.SessionID = "sess-steer-lower"
			},
			vis:       backstopVisibilityPublic,
			wantNoIDs: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			qw := base()
			tc.mutate(&qw)
			got := buildSteeringPrompt(qw, streamObservation{terminalSuccess: true}, tc.vis)
			for _, want := range []string{"git commit", "git push"} {
				if !strings.Contains(got, want) {
					t.Errorf("steering prompt missing %q\nfull:\n%s", want, got)
				}
			}
			if tc.wantIdent && !strings.Contains(got, "xyq-301") {
				t.Errorf("private steering prompt must keep this run's identifier\nfull:\n%s", got)
			}
			if tc.wantNoIDs && trackerIDPattern.MatchString(got) {
				t.Errorf("public/unknown steering prompt must carry no tracker-id-shaped token\nfull:\n%s", got)
			}
		})
	}
}

// TestSteeringPromptIdentifierOnlyTitleFallsBack pins the neutral subject:
// when the whole commit subject is an identifier, the public prompt
// instructs a neutral subject rather than an empty one.
func TestSteeringPromptIdentifierOnlyTitleFallsBack(t *testing.T) {
	t.Parallel()
	qw := QueuedWork{QueuedWork: queuedWorkBase("xyq-302")}
	qw.Title = ""
	qw.SessionID = "sess-steer-302"
	got := buildSteeringPrompt(qw, streamObservation{terminalSuccess: true}, backstopVisibilityPublic)
	if !strings.Contains(got, "recovered session work") {
		t.Errorf("identifier-only public prompt must fall back to a neutral subject\nfull:\n%s", got)
	}
	if trackerIDPattern.MatchString(got) {
		t.Errorf("identifier-only public prompt must carry no tracker-id-shaped token\nfull:\n%s", got)
	}
}

// TestAttemptSteeringReusesBackstopVisibility drives the production entry
// point: attemptSteering resolves repository visibility through the
// backstop's `gh repo view` probe, and the injected prompt carries this
// run's identifier only for PRIVATE. The stub harness records the
// injected prompt so the test observes the real steering text.
func TestAttemptSteeringReusesBackstopVisibility(t *testing.T) {
	cases := []struct {
		name       string
		visibility string
		wantIdent  bool
	}{
		{name: "private", visibility: "PRIVATE", wantIdent: true},
		{name: "public", visibility: "PUBLIC", wantIdent: false},
		{name: "gh failure fails safe", visibility: "", wantIdent: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubGhVisibilityOnly(t, tc.visibility)
			r := minimalRunner(t)
			p, err := stub.New(stub.WithCapabilities(agent.Capabilities{SupportsMessageInjection: true}))
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
			qw := QueuedWork{QueuedWork: queuedWorkBase("xyq-303")}
			qw.SessionID = "sess-steer-303"
			qw.Title = "Repair the widget"
			res := &Result{}
			res.WorktreePath = t.TempDir()
			newHandle, err := r.attemptSteering(ctx, p, handle, spec, p.Capabilities(), qw, streamObservation{terminalSuccess: true}, res)
			if err != nil {
				t.Fatalf("attemptSteering: %v", err)
			}
			if newHandle != handle {
				t.Fatal("expected the same handle back on the inject-success path")
			}
			// The stub echoes the injected prompt as an AssistantTextEvent
			// prefixed with "injected: "; read it back and assert on the
			// real steering text.
			var injected string
			for ev := range handle.Events() {
				if at, ok := ev.(agent.AssistantTextEvent); ok {
					if s, found := strings.CutPrefix(at.Text, "injected: "); found {
						injected = s
					}
				}
			}
			if injected == "" {
				t.Fatal("expected the stub to echo the injected steering prompt")
			}
			if hasIdent := strings.Contains(injected, "xyq-303"); hasIdent != tc.wantIdent {
				t.Errorf("injected prompt contains identifier = %v, want %v\nfull:\n%s", hasIdent, tc.wantIdent, injected)
			}
			if !tc.wantIdent && trackerIDPattern.MatchString(injected) {
				t.Errorf("non-private injected prompt must carry no tracker-id-shaped token\nfull:\n%s", injected)
			}
		})
	}
}
