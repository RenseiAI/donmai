package agent

import (
	"errors"

	"github.com/RenseiAI/donmai/attachwire"
)

// This file declares the interactive PTY capability seam for the
// interactive-attach-v1 protocol (donmai-architecture/protocol/
// interactive-attach-v1.md; owning ADR
// ADR-2026-07-12-interactive-pty-session-host.md).
//
// It is purely additive: a Handle becomes interactive-capable by also
// implementing InteractiveCapable. Callers type-assert; absence of the
// interface (or a nil InteractiveSession) means the session was not
// spawned interactively and every existing code path is unchanged.

// ErrRingMiss is returned by InteractiveSession.Subscribe when the
// requested resume sequence has been evicted from the host ring buffer.
// The caller recovers by taking a fresh Snapshot and subscribing from
// its sequence (the protocol's snapshot + tail repair path, spec § 13).
var ErrRingMiss = errors.New("agent: interactive ring miss — resume seq evicted")

// Marker labels the host emits at tool-approval suspend transitions so
// viewers can render a pending-approval badge inline (display-only,
// spec § 3.1 Marker). A PreToolUse suspend is a defined, attach-alive
// state: the PTY, ring, VT, recorder, and every attach stay live and
// interactive for its duration — suspension never closes the PTY or
// tears down the attach stream, and Exit ordering is unaffected.
const (
	MarkerApprovalPending  = "approval-pending"
	MarkerApprovalResolved = "approval-resolved"
)

// InteractiveSpec is the additive Spawn input that requests
// spawn-under-PTY with a live interactive session surface
// (TransportPTY). Honored only by harnesses that declare PTY transport
// capability; capability-gated like every other Spec field.
type InteractiveSpec struct {
	// Cols/Rows is the initial PTY geometry. Zero values fall back to
	// 80×24. Subsequent geometry changes arrive exclusively via
	// InteractiveSession.Resize (spec § 8: applied verbatim).
	Cols uint32 `json:"cols,omitempty"`
	Rows uint32 `json:"rows,omitempty"`

	// RecordPath is the asciinema-v2 cast destination. Empty disables
	// the parallel recording (spec § 16; the cast and the wire share
	// the process-spawn rel_time anchor).
	RecordPath string `json:"recordPath,omitempty"`

	// RingBytes bounds the host output-frame ring buffer. Zero falls
	// back to the 8 MiB default.
	RingBytes int `json:"ringBytes,omitempty"`
}

// InteractiveCapable is the optional capability a Handle implements
// when its session runs under the PTY session host. It is the seam the
// runner's interactive mode, the local attach surface, and the generic
// attach client all consume.
type InteractiveCapable interface {
	Handle

	// InteractiveSession returns the live PTY surface, or nil when the
	// session was not spawned with Spec.Interactive.
	InteractiveSession() InteractiveSession
}

// InteractiveSession is the live interactive surface of one
// PTY-hosted session. Implementations must be safe for concurrent use.
//
// Input trust posture (spec § 5): callers of WriteInput are stampers —
// the relay leg forwards only relay-stamped input, and the standalone
// local attach stamps the fixed "local" user and applies the trivial
// single-local-driver policy. Unstamped input never reaches this
// method.
type InteractiveSession interface {
	// WriteInput writes already-encoded terminal input bytes verbatim
	// to the PTY master (spec § 5: input is never re-sanitized).
	WriteInput(p []byte) (int, error)

	// Resize applies the geometry verbatim to the PTY (TIOCSWINSZ,
	// spec § 8: the host never second-guesses). cols == 0 || rows == 0
	// is rejected as a framing error.
	Resize(cols, rows, pxWidth, pxHeight uint32) error

	// Snapshot serializes the current screen (spec § 12.1) together
	// with the host output sequence it reflects (atSeq). After Exit it
	// keeps returning the final screen with atSeq == the Exit seq
	// (spec § 12.2).
	Snapshot() (screen attachwire.Screen, atSeq attachwire.HostSeq, err error)

	// Subscribe returns a live feed of host-produced, seq-bearing
	// frames starting at fromSeq+1. A fromSeq still in the ring
	// replays buffered frames then continues live; an evicted fromSeq
	// returns ErrRingMiss (recover via Snapshot). fromSeq 0 means "no
	// applied history" and is always served from the oldest buffered
	// frame.
	Subscribe(fromSeq attachwire.HostSeq) (InteractiveSubscription, error)

	// Done is closed after the child has exited AND the PTY master
	// has been drained to EOF with every pending Output frame emitted
	// (flush-before-Exit, spec § 12.2).
	Done() <-chan struct{}

	// Exit reports the terminal Exit payload. ok is false until Done
	// is closed.
	Exit() (exit attachwire.ExitPayload, ok bool)
}

// InteractiveSubscription is one live frame feed from an
// InteractiveSession.
type InteractiveSubscription interface {
	// Frames returns the read-only frame channel. It is closed after
	// the Exit frame has been delivered, or after Close.
	Frames() <-chan attachwire.Frame

	// Close releases the subscription. Idempotent.
	Close() error
}
