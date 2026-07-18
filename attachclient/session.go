package attachclient

import (
	"context"

	"github.com/RenseiAI/donmai/attachwire"
)

// Session is the local, structurally-identical mirror of
// agent.InteractiveSession (donmai/agent/interactive.go). attachclient consumes
// a session purely through this surface and deliberately does NOT import agent.
//
// Equivalence: every method below matches agent.InteractiveSession exactly
// EXCEPT Subscribe's return type, which is this package's Subscription rather
// than agent.InteractiveSubscription (the two sub-interfaces are themselves
// structurally identical). Go interface satisfaction is nominal for method
// return types, so an agent.InteractiveSession value does not satisfy Session
// directly; the composing binary supplies a ~5-line adapter whose Subscribe
// wraps the returned agent.InteractiveSubscription:
//
//	type sessAdapter struct{ agent.InteractiveSession }
//	func (a sessAdapter) Subscribe(from attachwire.HostSeq) (attachclient.Subscription, error) {
//		return a.InteractiveSession.Subscribe(from)
//	}
//
// (agent.InteractiveSubscription already satisfies attachclient.Subscription
// structurally, so the returned value needs no wrapping.)
//
// Input trust posture (§ 5): the client is a stamper's downstream — it calls
// WriteInput only with relay-stamped Input (userIdLen > 0). Unstamped Input is
// dropped before it reaches WriteInput.
type Session interface {
	// WriteInput writes already-encoded terminal input bytes verbatim to the
	// PTY master (§ 5: input is never re-sanitized).
	WriteInput(p []byte) (int, error)

	// Resize applies the geometry verbatim (TIOCSWINSZ, § 8). cols == 0 ||
	// rows == 0 is a framing error rejected before this is called.
	Resize(cols, rows, pxWidth, pxHeight uint32) error

	// Snapshot serializes the current screen with the host output sequence it
	// reflects (atSeq), WITHOUT emitting a frame. The client reads atSeq to find
	// the current stream head on reconnect (§ 4.1).
	Snapshot() (screen attachwire.Screen, atSeq attachwire.HostSeq, err error)

	// EmitSnapshot answers a snapshot_request (§ 12). Pre-Exit the frame is
	// seq-bearing and rides the subscription (inStream == true); post-Exit it
	// carries header seq 0 with atSeq == the Exit seq (inStream == false) and the
	// caller transmits it directly.
	EmitSnapshot() (frame attachwire.Frame, inStream bool, err error)

	// EmitMarker appends a seq-bearing Marker frame (§ 3.1). Not used by the host
	// leg directly, but part of the seam.
	EmitMarker(label string) error

	// Subscribe returns a live feed of host-produced, seq-bearing frames starting
	// at fromSeq+1 (§ 13). fromSeq 0 serves from the oldest buffered frame.
	Subscribe(fromSeq attachwire.HostSeq) (Subscription, error)

	// Done is closed after the child exits and the PTY is drained with Exit
	// emitted (§ 12.2).
	Done() <-chan struct{}

	// Exit reports the terminal Exit payload; ok is false until Done is closed.
	Exit() (exit attachwire.ExitPayload, ok bool)
}

// Subscription mirrors agent.InteractiveSubscription: one live frame feed. The
// Frames channel closes after the Exit frame is delivered, or after Close.
type Subscription interface {
	Frames() <-chan attachwire.Frame
	Close() error
}

// TokenSource yields the current bearer JWT for the host leg. It is resolved
// before each top-level carrier attempt and may also be called concurrently by
// degraded-lane 401 recovery and the background WSS upgrade probe (§ 14/§ 15).
// Within a token's exp the same jti is legitimately re-presented on reconnect;
// at/after exp the composing binary mints a fresh token. It MUST be safe for
// concurrent use.
type TokenSource func(ctx context.Context) (string, error)

// KillFunc is the composing runner's process-group termination hook, invoked
// when the relay sends a kill Control frame (§ 7, § 12.2). The OSS attach client
// owns no PTY, so the runner wires this; after it returns the normal Exit flow
// proceeds (the Session drains and emits its final Exit frame, which the client
// forwards). reason/signal are the decoded kill fields (signal is "" when the
// relay sent null). It MUST be idempotent — on the degraded lane's at-least-once
// SSE a kill may be redelivered (§ 14) — and the client also guards against a
// second invocation.
type KillFunc func(ctx context.Context, reason, signal string) error
