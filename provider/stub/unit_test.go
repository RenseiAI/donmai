package stub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// drain consumes the events channel until close or the supplied
// deadline. It returns the kind sequence + the terminal Result/Error
// event (whichever arrived last). Used by table-driven tests to
// assert on the scripted sequence.
func drain(t *testing.T, h agent.Handle, deadline time.Duration) []agent.EventKind {
	t.Helper()
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	var kinds []agent.EventKind
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return kinds
			}
			kinds = append(kinds, ev.Kind())
		case <-timer.C:
			t.Fatalf("drain: timed out after %s, kinds=%v", deadline, kinds)
		}
	}
}

func Test_Behaviors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		behavior  Behavior
		spec      agent.Spec
		want      []agent.EventKind
		ctxCancel time.Duration // when >0, cancels ctx after the delay
	}{
		{
			name:     "succeed-with-pr canonical sequence",
			behavior: BehaviorSucceedWithPR,
			want: []agent.EventKind{
				agent.EventInit,
				agent.EventSystem,
				agent.EventAssistantText,
				agent.EventToolUse,
				agent.EventToolResult,
				agent.EventAssistantText,
				agent.EventResult,
			},
		},
		{
			name:     "fail-on-clone emits init then error",
			behavior: BehaviorFailOnClone,
			want: []agent.EventKind{
				agent.EventInit,
				agent.EventError,
			},
		},
		{
			name:      "hang-then-timeout exits on ctx cancel",
			behavior:  BehaviorHangThenTimeout,
			ctxCancel: 50 * time.Millisecond,
			want: []agent.EventKind{
				agent.EventInit,
				agent.EventSystem,
			},
		},
		{
			name:     "silent-fail emits init then closes",
			behavior: BehaviorSilentFail,
			want: []agent.EventKind{
				agent.EventInit,
				agent.EventSystem,
			},
		},
		{
			name:     "slow-tool default 3 ticks",
			behavior: BehaviorSlowTool,
			want: []agent.EventKind{
				agent.EventInit,
				agent.EventSystem,
				agent.EventToolUse,
				agent.EventToolProgress,
				agent.EventToolProgress,
				agent.EventToolProgress,
				agent.EventToolResult,
				agent.EventResult,
			},
		},
		{
			name:     "slow-tool override ticks=1",
			behavior: BehaviorSlowTool,
			spec: agent.Spec{
				ProviderConfig: map[string]any{
					"stub.progressTicks": 1,
				},
			},
			want: []agent.EventKind{
				agent.EventInit,
				agent.EventSystem,
				agent.EventToolUse,
				agent.EventToolProgress,
				agent.EventToolResult,
				agent.EventResult,
			},
		},
		{
			name:     "cost-overrun emits assistant + result",
			behavior: BehaviorCostOverrun,
			want: []agent.EventKind{
				agent.EventInit,
				agent.EventSystem,
				agent.EventAssistantText,
				agent.EventResult,
			},
		},
		{
			name:     "mid-stream-error emits then errors",
			behavior: BehaviorMidStreamError,
			want: []agent.EventKind{
				agent.EventInit,
				agent.EventSystem,
				agent.EventAssistantText,
				agent.EventError,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := New()
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			spec := tc.spec
			if spec.ProviderConfig == nil {
				spec.ProviderConfig = map[string]any{}
			}
			spec.ProviderConfig[behaviorConfigKey] = string(tc.behavior)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.ctxCancel > 0 {
				go func(d time.Duration) {
					time.Sleep(d)
					cancel()
				}(tc.ctxCancel)
			}

			h, err := p.Spawn(ctx, spec)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			got := drain(t, h, 2*time.Second)
			if len(got) != len(tc.want) {
				t.Fatalf("event-kind sequence mismatch:\n  got:  %v\n  want: %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("event[%d] kind mismatch: got %q want %q (full got %v want %v)",
						i, got[i], tc.want[i], got, tc.want)
				}
			}
		})
	}
}

func Test_DefaultBehaviorIsSucceedWithPR(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, agent.Spec{})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	kinds := drain(t, h, 2*time.Second)
	if len(kinds) == 0 || kinds[len(kinds)-1] != agent.EventResult {
		t.Fatalf("default behavior should terminate in EventResult, got %v", kinds)
	}
}

func Test_BehaviorFromEnv(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	spec := agent.Spec{Env: map[string]string{behaviorEnvKey: string(BehaviorFailOnClone)}}
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	kinds := drain(t, h, 2*time.Second)
	want := []agent.EventKind{agent.EventInit, agent.EventError}
	if len(kinds) != len(want) {
		t.Fatalf("RENSEI_STUB_MODE not honored: got %v want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kind[%d]: got %q want %q", i, kinds[i], want[i])
		}
	}
}

func Test_UnknownBehaviorFallsBackToDefault(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	spec := agent.Spec{
		ProviderConfig: map[string]any{behaviorConfigKey: "not-a-real-behavior"},
	}
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	kinds := drain(t, h, 2*time.Second)
	if len(kinds) == 0 || kinds[len(kinds)-1] != agent.EventResult {
		t.Fatalf("unknown behavior should fall back to succeed-with-pr; got %v", kinds)
	}
}

func Test_NameAndCapabilities(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := p.Name(), agent.ProviderStub; got != want {
		t.Fatalf("Name(): got %q want %q", got, want)
	}
	caps := p.Capabilities()
	if !caps.SupportsMessageInjection {
		t.Errorf("default Caps.SupportsMessageInjection should be true")
	}
	if !caps.SupportsSessionResume {
		t.Errorf("default Caps.SupportsSessionResume should be true")
	}
	if !caps.EmitsSubagentEvents {
		t.Errorf("default Caps.EmitsSubagentEvents should be true")
	}
	if caps.HumanLabel != "Test Stub" {
		t.Errorf("Caps.HumanLabel = %q want \"Test Stub\"", caps.HumanLabel)
	}
}

func Test_WithCapabilitiesOverride(t *testing.T) {
	t.Parallel()
	p, err := New(WithCapabilities(agent.Capabilities{HumanLabel: "Override"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Capabilities().HumanLabel != "Override" {
		t.Fatalf("WithCapabilities did not override HumanLabel")
	}
	if p.Capabilities().SupportsMessageInjection {
		t.Fatalf("WithCapabilities did not zero SupportsMessageInjection")
	}
}

func Test_ResumeWithoutSessionID_ReturnsNotFound(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	_, err = p.Resume(ctx, "", agent.Spec{})
	if !errors.Is(err, agent.ErrSessionNotFound) {
		t.Fatalf("Resume(empty id): got %v want ErrSessionNotFound", err)
	}
}

func Test_ResumeUnsupportedWhenCapabilityOff(t *testing.T) {
	t.Parallel()
	p, err := New(WithCapabilities(agent.Capabilities{SupportsSessionResume: false}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	_, err = p.Resume(ctx, "stub-session-x", agent.Spec{})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("Resume(cap off): got %v want ErrUnsupported", err)
	}
}

func Test_ResumeEmitsResumeSequence(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := p.Resume(ctx, "stub-session-existing", agent.Spec{
		ProviderConfig: map[string]any{behaviorConfigKey: string(BehaviorSucceedWithPR)},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if h.SessionID() != "stub-session-existing" {
		t.Fatalf("Resume did not preserve session id: got %q", h.SessionID())
	}
	first := <-h.Events()
	init, ok := first.(agent.InitEvent)
	if !ok {
		t.Fatalf("first event is not InitEvent: %T", first)
	}
	if init.SessionID != "stub-session-existing" {
		t.Fatalf("InitEvent.SessionID: got %q want stub-session-existing", init.SessionID)
	}
	second := <-h.Events()
	sys, ok := second.(agent.SystemEvent)
	if !ok {
		t.Fatalf("second event is not SystemEvent: %T", second)
	}
	if sys.Subtype != "resumed" {
		t.Fatalf("Resume should set SystemEvent.Subtype=\"resumed\", got %q", sys.Subtype)
	}
	// drain the rest so the goroutine exits cleanly.
	drainAll(h)
}

func Test_SpawnWithCanceledContext_Fails(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.Spawn(ctx, agent.Spec{})
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn(canceled ctx): got %v want ErrSpawnFailed", err)
	}
}

func Test_Shutdown_Noop(t *testing.T) {
	t.Parallel()
	p, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: unexpected error %v", err)
	}
}

func Test_IsKnown(t *testing.T) {
	t.Parallel()
	known := []Behavior{
		BehaviorSucceedWithPR, BehaviorFailOnClone, BehaviorHangThenTimeout,
		BehaviorSilentFail, BehaviorSlowTool, BehaviorCostOverrun,
		BehaviorMidStreamError, BehaviorInjectTest,
	}
	for _, b := range known {
		if !IsKnown(b) {
			t.Errorf("IsKnown(%q) = false, want true", b)
		}
	}
	if IsKnown("not-a-behavior") {
		t.Errorf("IsKnown(unknown) = true, want false")
	}
}
