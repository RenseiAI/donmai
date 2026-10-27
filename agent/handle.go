package agent

import "context"

// Handle is the live interface to one running session.
//
// Verbatim port of the legacy TS AgentHandle interface from
// ../donmai-libraries/packages/core/src/providers/types.ts.
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

// TerminalTranscriptDescriptor is a path-only descriptor for one
// session-owned transcript file.
//
// It carries path metadata only. Holders retain a copy without reading,
// opening, validating, or serializing the file it names.
type TerminalTranscriptDescriptor struct {
	// Path is the session-owned transcript file path.
	Path string
}

// TerminalTranscriptCapable is the optional capability a Handle
// implements when its session maintains transcript files that survive
// the session.
//
// It mirrors NoticeChannelCapable: callers type-assert, and absence of
// the interface means the session exposes no descriptors. Descriptors
// are valid only after Stop has returned; implementations refuse the
// call while writers may still be active.
type TerminalTranscriptCapable interface {
	Handle

	// TerminalTranscripts returns path-only descriptors for this
	// handle's session. Valid only after Stop has returned; before
	// then it returns an error.
	TerminalTranscripts() ([]TerminalTranscriptDescriptor, error)
}
