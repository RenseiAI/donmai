package agent

import "context"

// Provider is the contract every agent runtime implements.
//
// Verbatim port of the legacy TS AgentProvider interface from
// ../agentfactory/packages/core/src/providers/types.ts.
//
// Implementations live in github.com/RenseiAI/agentfactory-tui/provider/
// subpackages (claude, codex, stub for v0.5.0). Each implementation is
// independent and only depends on this package + its provider-native
// CLI/SDK + the standard library.
//
// Lifecycle expectations (F.1.1 §3):
//   - Provider construction (provider.New) does fail-fast probing such
//     as `which claude`. Callers see provider unavailability before any
//     worktree work.
//   - Spawn returns a Handle whose Events channel emits exactly one
//     InitEvent, then zero or more assistant/tool events, then exactly
//     one terminal ResultEvent (or ErrorEvent followed by close), then
//     closes.
//   - Resume continues a previously interrupted session, gated by
//     Capabilities().SupportsSessionResume; providers that do not
//     support resume return ErrUnsupported.
//   - Shutdown releases provider-level resources such as the long-lived
//     codex app-server child. Providers with per-session children
//     (Claude CLI) may no-op. Called once on daemon drain.
type Provider interface {
	// Name returns this provider's identifier. Stable for the lifetime
	// of the Provider instance.
	Name() ProviderName

	// Capabilities returns this provider's capability flags. Stable
	// for the lifetime of the Provider instance.
	Capabilities() Capabilities

	// Spawn starts a new agent session. The returned Handle's Events
	// channel emits exactly one InitEvent, then zero or more
	// assistant/tool events, then exactly one terminal ResultEvent,
	// then closes. ctx cancellation aborts the session.
	Spawn(ctx context.Context, spec Spec) (Handle, error)

	// Resume continues a previously interrupted session. sessionID is
	// the value captured from a prior InitEvent. Behavior is
	// capability-gated by SupportsSessionResume; providers that do
	// not support resume return ErrUnsupported.
	Resume(ctx context.Context, sessionID string, spec Spec) (Handle, error)

	// Shutdown releases provider-level resources (long-lived child
	// processes such as the codex app-server). Providers with
	// per-session children may no-op and return nil. Called by the
	// runner on fleet shutdown.
	Shutdown(ctx context.Context) error
}
