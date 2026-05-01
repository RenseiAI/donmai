package agent

import "context"

// Handle is the live interface to one running session.
//
// Verbatim port of the legacy TS AgentHandle interface from
// ../agentfactory/packages/core/src/providers/types.ts.
//
// Returned by Provider.Spawn / Provider.Resume. The provider closes
// the channel returned by Events() after emitting a terminal
// ResultEvent or an unrecoverable ErrorEvent, signalling that the
// session is done and no more events will arrive.
type Handle interface {
	// SessionID returns the provider-native session identifier
	// (Claude session UUID, Codex thread id). Empty until the
	// InitEvent fires; callers should consume events first.
	SessionID() string

	// Events returns the read-only event channel. The provider
	// closes this channel after emitting a terminal ResultEvent or
	// an unrecoverable ErrorEvent.
	Events() <-chan Event

	// Inject sends a follow-up user message into the session.
	// Returns ErrUnsupported when
	// !Provider.Capabilities().SupportsMessageInjection. Implementations
	// that support it should be safe to call concurrently with Events
	// consumption.
	Inject(ctx context.Context, text string) error

	// Stop aborts the session. Idempotent; safe to call after the
	// event channel has closed (returns nil). Implementations should
	// honor ctx cancellation for stop deadlines (typical: 5s grace
	// then SIGKILL for child-process providers).
	Stop(ctx context.Context) error
}
