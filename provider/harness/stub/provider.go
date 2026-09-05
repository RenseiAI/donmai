package stub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
)

// defaultCapabilities is the all-on capability matrix the stub
// provider exposes by default. F.1.1 §3.3 specifies all flags true so
// the runner exercises every gating branch when wired against the
// stub. Tests can override via WithCapabilities.
func defaultCapabilities() agent.Capabilities {
	return agent.Capabilities{
		SupportsMessageInjection:            true,
		SupportsSessionResume:               true,
		SupportsToolPlugins:                 true,
		NeedsBaseInstructions:               true,
		NeedsPermissionConfig:               true,
		SupportsCodeIntelligenceEnforcement: true,
		EmitsSubagentEvents:                 true,
		SupportsReasoningEffort:             true,
		ToolPermissionFormat:                "claude",
		// Tool-use surface (002 v2): the stub honours the same wire
		// shape as Claude so the runner exercises every gating branch
		// when wired against the stub. The scripted handler does not
		// consume the values — but the contract is "the field is
		// observed, not silently dropped" which the stub satisfies by
		// virtue of being all-on for tests.
		AcceptsAllowedToolsList: true,
		AcceptsMcpServerSpec:    true,
		HumanLabel:              "Test Stub",
	}
}

// Option configures a Provider via New.
type Option func(*provider)

// WithCapabilities overrides the capability matrix. Tests use this to
// flip individual capabilities off and assert the runner gates correctly.
func WithCapabilities(caps agent.Capabilities) Option {
	return func(p *provider) { p.caps = caps }
}

// WithDefaultBehavior overrides the behavior used when neither
// Spec.Env[DONMAI_STUB_MODE] nor Spec.ProviderConfig["stub.behavior"]
// is set on a Spec.
func WithDefaultBehavior(b Behavior) Option {
	return func(p *provider) { p.defaultBehavior = b }
}

// WithStubAgentCommand overrides the program the INTERACTIVE spawn mode runs
// under the PTY. Production leaves it unset (the default is this executable
// re-invoked on StubAgentSubcommand); a test that must not re-invoke its own
// test binary points it at a fixture instead.
func WithStubAgentCommand(binary string, argv ...string) Option {
	return func(p *provider) {
		p.agentBinary = binary
		p.agentArgv = append([]string(nil), argv...)
	}
}

// WithSessionIDFunc overrides the session id generator. The default
// emits "stub-session-<8 hex bytes>". Tests use this to make session
// ids deterministic.
func WithSessionIDFunc(fn func() string) Option {
	return func(p *provider) { p.sessionIDFn = fn }
}

// provider is the concrete agent.Provider implementation. It has two
// spawn modes and holds no per-session state.
//
// Headless (Spec.Interactive == nil) is the original one: a scripted,
// in-process event sequence with no child process at all — the right shape
// for unit-testing the runner's event handling.
//
// Interactive (Spec.Interactive != nil) is the deterministic fake AGENT:
// a real PTY child running provider/harness/stub/stubagent through the shared
// ptycli driver. See interactive.go for why the difference matters.
type provider struct {
	caps            agent.Capabilities
	defaultBehavior Behavior
	sessionIDFn     func() string

	// agentBinary/agentArgv override the interactive child. Empty means the
	// default: this executable, re-invoked on StubAgentSubcommand.
	agentBinary string
	agentArgv   []string

	// deliverSeed is the PTY seed writer, injectable for tests. Nil uses
	// ptycli.DeliverSeed.
	deliverSeed func(context.Context, agent.Handle, agent.InteractiveSession, string) error
}

// New constructs a stub agent.Provider. The returned Provider has no
// external dependencies and Spawn is safe to call from any goroutine.
//
// Construction never fails — there is no probe step. The error in the
// signature is reserved for future use and currently always nil; it
// keeps New compatible with the constructor pattern used by
// provider/claude and provider/codex.
func New(opts ...Option) (agent.Provider, error) {
	p := &provider{
		caps:            defaultCapabilities(),
		defaultBehavior: BehaviorSucceedWithPR,
		sessionIDFn:     defaultSessionID,
	}
	for _, opt := range opts {
		opt(p)
	}
	// Resolve the interactive child's binary ONCE, here — the option, then the
	// environment, the same order codex uses for $CODEX_BIN and pi for
	// $PI_BIN. Reading it per Spec instead would let any caller of a shared,
	// registry-held provider change what this host executes.
	p.agentBinary, p.agentArgv = resolveStubAgentCommand(p.agentBinary, p.agentArgv)
	return p, nil
}

// Name returns agent.ProviderStub.
func (p *provider) Name() agent.ProviderName { return agent.ProviderStub }

// Capabilities returns the configured capability matrix.
func (p *provider) Capabilities() agent.Capabilities { return p.caps }

// Spawn selects one of the two modes. With Spec.Interactive set it runs the
// deterministic fake agent under a PTY (interactive.go). Otherwise it returns
// a Handle whose Events channel emits the scripted sequence selected by the
// Spec's behavior knob. If ctx is already canceled Spawn returns a wrapped
// agent.ErrSpawnFailed.
func (p *provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	// Read the LIVE manifest rather than branching on a hardcoded true: an
	// edit that flips SupportsInteractivePTY back to false must fall through
	// to the scripted in-process mode, not keep spawning a PTY the manifest
	// no longer declares. Same guard claude.go, codex.go and pi.go use.
	if spec.Interactive != nil && p.Manifest().Caps.SupportsInteractivePTY {
		return p.spawnInteractive(ctx, spec)
	}
	b := p.resolveBehavior(spec)
	h := newHandle(p.sessionIDFn(), b, spec, false)
	go h.run(ctx)
	return h, nil
}

// Resume continues a previously spawned session. The stub does not
// persist state across runs — Resume returns a fresh Handle that
// replays the scripted sequence with a SystemEvent indicating the
// resume. The supplied sessionID is preserved on the new InitEvent so
// callers can correlate.
//
// When Capabilities.SupportsSessionResume is false Resume returns
// agent.ErrUnsupported. When Spec.ProviderConfig["stub.resumeFailure"] is
// true Resume returns a generic (non-ErrUnsupported) error regardless of
// capability — tests use this to exercise a caller's hard-failure path
// (e.g. runner/steering.go's attemptSteeringResume) distinctly from the
// soft-fail ErrUnsupported path.
func (p *provider) Resume(ctx context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	// The interactive child is a process, and a dead process has no state to
	// resume. Falling through would hand the caller a scripted in-process
	// handle for a session it asked to continue as a terminal — a silent mode
	// switch, which is worse than a refusal.
	if spec.Interactive != nil {
		return nil, fmt.Errorf(
			"provider/harness/stub: Resume: %w (the interactive spawn mode has no resumable session state; spawn a new one)",
			agent.ErrUnsupported,
		)
	}
	if !p.caps.SupportsSessionResume {
		return nil, agent.ErrUnsupported
	}
	if sessionID == "" {
		return nil, fmt.Errorf("%w: empty session id", agent.ErrSessionNotFound)
	}
	if v, ok := spec.ProviderConfig["stub.resumeFailure"]; ok {
		if b, ok := v.(bool); ok && b {
			return nil, errors.New("stub: forced resume failure")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	b := p.resolveBehavior(spec)
	h := newHandle(sessionID, b, spec, true)
	go h.run(ctx)
	return h, nil
}

// Shutdown is a no-op for the stub provider. It has no long-lived
// child process or pooled resource to release.
func (p *provider) Shutdown(_ context.Context) error { return nil }

// resolveBehavior reads Spec.ProviderConfig["stub.behavior"] (typed
// knob) or Spec.Env["DONMAI_STUB_MODE"] (legacy knob) and falls back
// to the provider's default behavior. Unknown names also fall back.
func (p *provider) resolveBehavior(spec agent.Spec) Behavior {
	// Typed ProviderConfig wins if present and a string.
	if raw, ok := spec.ProviderConfig[behaviorConfigKey]; ok {
		if s, ok := raw.(string); ok && s != "" {
			b := Behavior(s)
			if IsKnown(b) {
				return b
			}
		}
	}
	if s, ok := spec.Env[behaviorEnvKey]; ok && s != "" {
		b := Behavior(s)
		if IsKnown(b) {
			return b
		}
	}
	return p.defaultBehavior
}

// defaultSessionID returns "stub-session-<8 hex bytes>". The 8 random
// bytes give 64 bits of uniqueness which is enough to avoid collisions
// in CI without depending on time.Now (which would make tests flake
// when run in parallel).
func defaultSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read on a healthy platform never fails; the
		// fallback keeps Spawn infallible if it ever does.
		return "stub-session-fallback"
	}
	return "stub-session-" + hex.EncodeToString(b[:])
}
