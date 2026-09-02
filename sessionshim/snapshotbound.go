package sessionshim

import (
	"errors"
	"fmt"

	"github.com/RenseiAI/donmai/attachwire"
)

// Snapshot frames are the only host frames whose size is unbounded by
// construction.
//
// An Output frame is capped by the PTY host at 32 KiB, an Exit is a handful of
// bytes, and a Resize is fixed. A Snapshot carries the whole serialized screen
// (attachwire.Screen, §12.1): the live grid, optionally the alternate grid, and
// a scrollback tail whose length is a per-session policy (ptyhost.Spec
// Scrollback). A lineage whose harness has a long screen history therefore
// produces a Snapshot that can be many megabytes — and the resume/re-adopt path
// emits exactly that Snapshot, at exactly the moment a carrier is at its most
// fragile.
//
// Measured on production hosts twice in one day: the resume Snapshot did not
// fit, the write was refused at its source, the shim closed the controller
// connection, nothing re-adopted the shim, and a healthy seat was quarantined
// and later reaped. A single oversized frame must never sever a healthy carrier.
//
// The fix is at the producer, and it does not raise any cap. The scrollback TAIL
// is history: it is the one part of a Snapshot that can be shortened without
// changing what the session currently IS. So an oversized Snapshot is re-encoded
// keeping the newest scrollback lines that fit and dropping the oldest, which is
// the same "keep the recent, declare the loss" shape the ring already uses for
// output (§D5).
//
// # Wire compatibility
//
// This changes no message shape and no encoding. attachwire.Screen already
// carries its scrollback as a length-prefixed list of lines, so a bounded
// Snapshot is an ORDINARY, fully canonical Screen that every existing receiver —
// including one negotiated at the released selected-v2 tier — decodes exactly as
// it decodes any other. Nothing is gated on a protocol version because nothing
// new is spoken. The trim is reported through the shim's structured log rather
// than a new wire field, because a receiver-visible marker would require a new
// field in a payload whose decoder rejects trailing bytes (DecodeScreen's
// expectDone), and that WOULD be a breaking change for exactly the older
// receivers this whole mechanism exists to keep adopting.

// ErrSnapshotUnboundable reports a Snapshot whose screen does not fit the local
// wire even with its entire scrollback tail dropped.
//
// It is a sentinel because it is a genuinely different fact from "the history is
// long": everything shortenable has been shortened and the LIVE grid alone is
// still too large, which takes a geometry no terminal reports in practice
// (roughly 200k cells). Grinding the live screen down further would start
// deleting what the session currently shows, so this refuses instead and says
// so.
var ErrSnapshotUnboundable = errors.New("sessionshim: snapshot exceeds the local wire without any scrollback")

// boundSnapshotFrame returns frame unchanged when sizeOf already reports it
// within limit, and otherwise the same Snapshot frame — same sequence, same
// rel-time, same atSeq, same snapshot format — re-encoded with the oldest
// scrollback lines dropped until it fits. The second result is how many lines
// were dropped; zero means the frame was returned verbatim.
func boundSnapshotFrame(
	frame attachwire.Frame,
	limit int,
	sizeOf func(attachwire.Frame) (int, error),
) (attachwire.Frame, int, error) {
	size, err := sizeOf(frame)
	if err != nil {
		return attachwire.Frame{}, 0, err
	}
	if size <= limit {
		return frame, 0, nil
	}
	if frame.Type != attachwire.TypeSnapshot {
		return attachwire.Frame{}, 0, fmt.Errorf(
			"sessionshim: %w: a %s host frame of %d bytes has no scrollback to shorten",
			ErrSnapshotUnboundable, frame.Type, size)
	}
	envelope, err := attachwire.DecodeSnapshotEnvelope(frame.Payload)
	if err != nil {
		return attachwire.Frame{}, 0, fmt.Errorf("sessionshim: bound snapshot: %w", err)
	}
	if envelope.SnapFormat != attachwire.SnapFormatScreen {
		return attachwire.Frame{}, 0, fmt.Errorf(
			"sessionshim: %w: snapshot format %d is not a screen", ErrSnapshotUnboundable, envelope.SnapFormat)
	}
	bounded, dropped, err := boundSnapshotScreen(envelope.Snap, limit, func(screen []byte) (int, error) {
		return sizeOf(snapshotFrameWithScreen(frame, envelope, screen))
	})
	if err != nil {
		return attachwire.Frame{}, 0, err
	}
	return snapshotFrameWithScreen(frame, envelope, bounded), dropped, nil
}

// snapshotFrameWithScreen rebuilds frame around a different serialized screen,
// preserving every field a consumer correlates on: Type, Seq, RelTime, and the
// envelope's AtSeq and SnapFormat.
func snapshotFrameWithScreen(
	frame attachwire.Frame,
	envelope attachwire.SnapshotEnvelope,
	screen []byte,
) attachwire.Frame {
	envelope.Snap = screen
	frame.Payload = envelope.Encode()
	return frame
}

// boundSnapshotScreen returns screen unchanged when sizeOf already reports it
// within limit, and otherwise the same screen re-encoded with the oldest
// scrollback lines dropped until it fits.
//
// sizeOf measures the thing that actually has to fit — the whole wire message
// carrying the screen, headers and any base64 inflation included — rather than
// the screen bytes alone, so every call site gets the ceiling it is really up
// against instead of an approximation of it.
func boundSnapshotScreen(
	screen []byte,
	limit int,
	sizeOf func([]byte) (int, error),
) ([]byte, int, error) {
	size, err := sizeOf(screen)
	if err != nil {
		return nil, 0, err
	}
	if size <= limit {
		return screen, 0, nil
	}
	decoded, err := attachwire.DecodeScreen(screen)
	if err != nil {
		return nil, 0, fmt.Errorf("sessionshim: bound snapshot: decode screen: %w", err)
	}
	history := decoded.Scrollback
	total := len(history)

	// keeping re-encodes the screen with only the newest keep scrollback lines.
	// Scrollback is oldest-first, so the newest lines are its tail — the ones a
	// viewer scrolling back one page actually wants.
	keeping := func(keep int) ([]byte, int, error) {
		decoded.Scrollback = history[total-keep:]
		encoded, encErr := decoded.Encode()
		if encErr != nil {
			return nil, 0, fmt.Errorf("sessionshim: bound snapshot: re-encode screen: %w", encErr)
		}
		measured, sizeErr := sizeOf(encoded)
		return encoded, measured, sizeErr
	}

	// The floor first: if the live grid alone does not fit, no amount of
	// trimming helps and there is nothing honest left to try.
	best, size, err := keeping(0)
	if err != nil {
		return nil, 0, err
	}
	if size > limit {
		return nil, 0, fmt.Errorf(
			"sessionshim: %w: %d bytes with no scrollback, limit %d", ErrSnapshotUnboundable, size, limit)
	}
	// Encoded size is monotone in the number of retained lines, so the largest
	// tail that fits is a binary search — O(log n) re-encodes of a screen that
	// is megabytes, not O(n).
	bestKeep := 0
	for lo, hi := 1, total; lo <= hi; {
		mid := lo + (hi-lo)/2
		encoded, measured, keepErr := keeping(mid)
		if keepErr != nil {
			return nil, 0, keepErr
		}
		if measured <= limit {
			best, bestKeep = encoded, mid
			lo = mid + 1
			continue
		}
		hi = mid - 1
	}
	return best, total - bestKeep, nil
}
