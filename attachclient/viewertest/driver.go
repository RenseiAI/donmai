package viewertest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachwire"
)

// Driver drives a connected viewer over the abstract relay wire: it sends input
// frames and requests/awaits Snapshot frames, returning decoded screens with
// bounded timeouts and clear failure messages. It never hangs — every wait is
// bounded by the supplied context (or an internal deadline).
//
// The Driver is the SOLE consumer of its Viewer's Frames() channel: it discards
// non-Snapshot frames (Output, room_state, pen_state, presence) while waiting
// for the screen the caller asked for. Do not read Frames() elsewhere while a
// Driver is in use.
type Driver struct {
	v *attachtest.Viewer

	// PollInterval is the delay between snapshot_request retries in SnapshotUntil
	// while the predicate is still unmet (the driven change may not have been
	// rendered by the host VT yet). Zero uses defaultPollInterval.
	PollInterval time.Duration
}

const defaultPollInterval = 25 * time.Millisecond

// NewDriver wraps a viewer for input+screen-assert driving.
func NewDriver(v *attachtest.Viewer) *Driver { return &Driver{v: v} }

// SendInput sends one input frame (keystrokes) to the session over the wire. The
// relay stamps the verified userId before forwarding to the host (§5).
func (d *Driver) SendInput(ctx context.Context, data []byte) error {
	if err := d.v.SendInput(ctx, data); err != nil {
		return fmt.Errorf("viewertest: sending input: %w", err)
	}
	return nil
}

// NextSnapshot waits for the next Snapshot frame to arrive on the viewer stream
// and returns the decoded screen. It does NOT request one — use it to consume
// the automatic join snapshot delivered on attach. It fails (never hangs) when
// ctx is done or the stream closes first.
func (d *Driver) NextSnapshot(ctx context.Context) (attachwire.Screen, error) {
	for {
		select {
		case <-ctx.Done():
			return attachwire.Screen{}, fmt.Errorf("viewertest: waiting for snapshot: %w", ctx.Err())
		case f, ok := <-d.v.Frames():
			if !ok {
				return attachwire.Screen{}, errors.New("viewertest: viewer stream closed before a snapshot arrived")
			}
			if f.Type != attachwire.TypeSnapshot {
				continue
			}
			return DecodeSnapshotFrame(f)
		}
	}
}

// RequestSnapshot sends a snapshot_request (reason "resync") and returns the
// resulting decoded screen. The screen reflects the host VT at the moment the
// host answered — which may PRE-date a just-sent input the host has not yet
// applied. For asserting a driven change, prefer SnapshotUntil.
func (d *Driver) RequestSnapshot(ctx context.Context) (attachwire.Screen, error) {
	if err := d.v.RequestSnapshot(ctx, attachwire.ReasonResync); err != nil {
		return attachwire.Screen{}, fmt.Errorf("viewertest: requesting snapshot: %w", err)
	}
	return d.NextSnapshot(ctx)
}

// SnapshotUntil repeatedly requests snapshots until the decoded screen satisfies
// pred or ctx is done. It is the robust way to assert a driven screen change: it
// tolerates the async gap between sending input and the host rendering it. On
// timeout it returns a descriptive error that includes a dump of the last screen
// it saw, so a failure shows what WAS on screen (distinguishing correct from
// garbled output). The last-seen screen is returned alongside the error.
func (d *Driver) SnapshotUntil(ctx context.Context, pred func(attachwire.Screen) bool) (attachwire.Screen, error) {
	interval := d.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	var last attachwire.Screen
	var haveLast bool
	for {
		if err := ctx.Err(); err != nil {
			if haveLast {
				return last, fmt.Errorf("viewertest: predicate never satisfied before timeout (%w); last screen:\n%s", err, Dump(last))
			}
			return attachwire.Screen{}, fmt.Errorf("viewertest: predicate never satisfied and no snapshot arrived before timeout: %w", err)
		}
		scr, err := d.RequestSnapshot(ctx)
		if err != nil {
			if haveLast {
				return last, fmt.Errorf("viewertest: snapshot request failed while polling (%w); last screen:\n%s", err, Dump(last))
			}
			return attachwire.Screen{}, err
		}
		last, haveLast = scr, true
		if pred(scr) {
			return scr, nil
		}
		select {
		case <-ctx.Done():
			return last, fmt.Errorf("viewertest: predicate never satisfied before timeout (%w); last screen:\n%s", ctx.Err(), Dump(last))
		case <-time.After(interval):
		}
	}
}

// SendInputAndAwait sends input then blocks in SnapshotUntil for the predicate —
// the one-call form a smoke uses to drive a key and assert the resulting screen.
func (d *Driver) SendInputAndAwait(ctx context.Context, data []byte, pred func(attachwire.Screen) bool) (attachwire.Screen, error) {
	if err := d.SendInput(ctx, data); err != nil {
		return attachwire.Screen{}, err
	}
	return d.SnapshotUntil(ctx, pred)
}
