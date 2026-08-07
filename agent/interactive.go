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
	// with the host output sequence it reflects (atSeq), WITHOUT
	// emitting anything into the frame stream (read-only; local
	// rendering and tests). After Exit it keeps returning the final
	// screen with atSeq == the Exit seq (spec § 12.2).
	Snapshot() (screen attachwire.Screen, atSeq attachwire.HostSeq, err error)

	// EmitSnapshot produces a Snapshot frame in answer to a
	// snapshot_request (spec § 12).
	//
	// Before Exit the frame is seq-bearing (§ 4: Snapshot is a
	// host-produced frame): the session atomically allocates the next
	// host seq, appends the frame to the ring and every subscription
	// (ordering by construction), and reports inStream == true — the
	// caller sends nothing itself, the frame arrives on its
	// subscription. After Exit the frame carries header seq == 0 with
	// atSeq == the Exit seq (§ 12.2 out-of-namespace convention) and
	// inStream == false — the caller transmits it directly (on the
	// degraded lane it rides the outOfSeq array, § 14).
	EmitSnapshot() (frame attachwire.Frame, inStream bool, err error)

	// EmitMarker appends a seq-bearing Marker frame (display-only
	// annotation, spec § 3.1) to the stream and the recording — e.g.
	// the tool-approval suspend markers above. Returns an error after
	// Exit (Exit is the final seq-bearing frame, § 12.2).
	EmitMarker(label string) error

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

// InteractiveNotifier is the OPTIONAL capability an InteractiveSession
// implements when it can accept a RUNTIME NOTICE: a complete,
// self-contained line of text authored by the runner itself (not by a
// human at a terminal and not by the relay) and delivered into the live
// PTY as one submitted input turn.
//
// It is a third input author alongside the two the spec's §5 trust
// posture already names (relay-stamped input and the local attach). The
// runner is a stamper in exactly the same sense: it originates the bytes,
// so nothing unstamped reaches the PTY through this method either.
//
// Contract implementations must honor:
//
//   - ATOMIC. The whole notice is delivered in exactly ONE write to the
//     PTY master, so it can never interleave byte-wise with a concurrent
//     human keystroke. Callers pass a complete notice; there is no
//     chunked/short-write resume path.
//   - REFUSABLE. Whenever the implementation can see that the write would
//     be unsafe, it returns (false, nil) and writes NOTHING. A refusal is
//     not an error: the caller holds the notice and retries later. Two
//     conditions are host-observable and MUST refuse: an outstanding line
//     composition (appending to a half-typed line corrupts the human's
//     input), and the alternate screen buffer (the child is driving a
//     full-screen UI where every byte is a command).
//   - SELF-SUBMITTING. The caller supplies the trailing submit byte; the
//     implementation writes the bytes verbatim and never re-frames them.
//     The submit key a terminal sends for Return is CR, not LF — a raw-mode
//     application reads them as different keys.
//
// A session that does not implement this interface cannot accept notices;
// callers must treat that as "hold and surface", never as a silent drop.
//
// # Known limit, stated so callers do not assume more than is delivered
//
// Refusal cannot cover an INLINE modal — an application-level prompt drawn
// as ordinary text on the primary screen where Enter selects a highlighted
// option and digits pick menu entries. Nothing at the terminal layer changes
// when an application starts interpreting keys that way, so a notice CAN
// select such a prompt's default. Eliminating that needs an application-side
// inject API (the harness's own message-injection capability), not a smarter
// terminal; implementations should not pretend otherwise by guessing at
// rendered screen contents.
type InteractiveNotifier interface {
	// TryWriteNotice writes p as one atomic PTY input, or refuses.
	//
	// written == true  → the notice reached the PTY (err is nil, or a
	// non-nil err describing a partial/failed write of bytes that did
	// leave).
	// written == false, err == nil → REFUSED because the human is
	// mid-composition; nothing was written and the caller should retry.
	// written == false, err != nil → the write failed outright.
	TryWriteNotice(p []byte) (written bool, err error)
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
