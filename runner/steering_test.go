package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/RenseiAI/agentfactory-tui/agent"
	"github.com/RenseiAI/agentfactory-tui/provider/stub"
)

// TestShouldSteer_Table covers the decision matrix in steering.go.
func TestShouldSteer_Table(t *testing.T) {
	cases := []struct {
		name string
		obs  streamObservation
		caps agent.Capabilities
		want bool
	}{
		{
			name: "no capability",
			obs:  streamObservation{terminalSuccess: true},
			caps: agent.Capabilities{},
			want: false,
		},
		{
			name: "unsuccessful terminal",
			obs:  streamObservation{terminalSuccess: false},
			caps: agent.Capabilities{SupportsMessageInjection: true},
			want: false,
		},
		{
			name: "PR already opened",
			obs:  streamObservation{terminalSuccess: true, pullRequestURL: "https://example.test/pr/1"},
			caps: agent.Capabilities{SupportsMessageInjection: true},
			want: false,
		},
		{
			name: "should steer (injection)",
			obs:  streamObservation{terminalSuccess: true},
			caps: agent.Capabilities{SupportsMessageInjection: true},
			want: true,
		},
		{
			name: "should steer (resume only)",
			obs:  streamObservation{terminalSuccess: true},
			caps: agent.Capabilities{SupportsSessionResume: true},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSteer(tc.obs, tc.caps); got != tc.want {
				t.Fatalf("shouldSteer = %v; want %v", got, tc.want)
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
	handle, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{
			"stub.behavior": string(stub.BehaviorInjectTest),
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	qw := QueuedWork{QueuedWork: queuedWorkBase("REN-S-1")}
	if err := r.attemptSteering(ctx, handle, qw, streamObservation{terminalSuccess: true}); err != nil {
		t.Fatalf("attemptSteering: %v", err)
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

// TestAttemptSteering_RejectsUnsupported confirms the runner returns
// a wrapped agent.ErrUnsupported when the provider does not support
// injection.
func TestAttemptSteering_RejectsUnsupported(t *testing.T) {
	r := minimalRunner(t)

	p, err := stub.New(stub.WithCapabilities(agent.Capabilities{}))
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	_ = r.registry.Register(p)

	ctx, cancel := withCtx(t)
	defer cancel()
	handle, err := p.Spawn(ctx, agent.Spec{
		ProviderConfig: map[string]any{
			"stub.injectUnsupported": true,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()

	err = r.attemptSteering(ctx, handle, QueuedWork{QueuedWork: queuedWorkBase("REN-S-2")}, streamObservation{terminalSuccess: true})
	if err == nil {
		t.Fatal("expected error from attemptSteering with unsupported provider")
	}
}
