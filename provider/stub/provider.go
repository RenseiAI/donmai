package stub

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/RenseiAI/agentfactory-tui/agent"
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
		HumanLabel:                          "Test Stub",
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
// Spec.Env[RENSEI_STUB_MODE] nor Spec.ProviderConfig["stub.behavior"]
// is set on a Spec.
func WithDefaultBehavior(b Behavior) Option {
	return func(p *provider) { p.defaultBehavior = b }
}

// WithSessionIDFunc overrides the session id generator. The default
// emits "stub-session-<8 hex bytes>". Tests use this to make session
// ids deterministic.
func WithSessionIDFunc(fn func() string) Option {
	return func(p *provider) { p.sessionIDFn = fn }
}

// provider is the concrete agent.Provider implementation backed by
// scripted event sequences. It holds no per-session state — that
// lives on the returned *handle.
type provider struct {
	caps            agent.Capabilities
	defaultBehavior Behavior
	sessionIDFn     func() string
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
	return p, nil
}

// Name returns agent.ProviderStub.
func (p *provider) Name() agent.ProviderName { return agent.ProviderStub }

// Capabilities returns the configured capability matrix.
func (p *provider) Capabilities() agent.Capabilities { return p.caps }

// Spawn returns a Handle whose Events channel will emit the scripted
// sequence selected by the Spec's behavior knob. If ctx is already
// canceled Spawn returns a wrapped agent.ErrSpawnFailed.
func (p *provider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
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
// agent.ErrUnsupported.
func (p *provider) Resume(ctx context.Context, sessionID string, spec agent.Spec) (agent.Handle, error) {
	if !p.caps.SupportsSessionResume {
		return nil, agent.ErrUnsupported
	}
	if sessionID == "" {
		return nil, fmt.Errorf("%w: empty session id", agent.ErrSessionNotFound)
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
// knob) or Spec.Env["RENSEI_STUB_MODE"] (legacy knob) and falls back
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
