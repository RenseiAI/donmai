package attachclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
	"github.com/coder/websocket"
)

// V2HostConfig configures one generic native WSS v2 candidate. This is a
// deliberately small carrier client for composing daemons; it owns no PTY or
// durable journal and never treats a socket write as durable success.
type V2HostConfig struct {
	AttachURL   string
	TokenSource TokenSource
	HTTPClient  *http.Client
	Logger      *slog.Logger
	DialTimeout time.Duration
	ReadLimit   int64
	Now         func() time.Time

	// DurableHighWater is the carrier journal cursor loaded before this leg is
	// admitted. It seeds gap validation and ordinary contiguous sends; zero is
	// the safe over-replay default.
	DurableHighWater uint64

	// Authority callbacks are invoked only after carrier_active. Before that,
	// Input/Resize/Kill are refused locally even if a non-conforming relay sends
	// them. SnapshotRequest is separate because exactly one mandatory resync is
	// permitted while the leg is a candidate.
	OnInput           func(context.Context, attachwire.InputPayload) error
	OnResize          func(context.Context, attachwire.ResizePayload) error
	OnKill            func(context.Context, attachwire.Kill) error
	OnSnapshotRequest func(context.Context, attachwire.SnapshotRequest) error
}

func (c *V2HostConfig) withDefaults() error {
	if c.AttachURL == "" || !strings.Contains(c.AttachURL, "/v2/") {
		return errors.New("attachclient: v2 AttachURL with /v2/ path is required")
	}
	if c.TokenSource == nil {
		return errors.New("attachclient: v2 TokenSource is required")
	}
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = defaultDialTimeout
	}
	if c.ReadLimit <= 0 {
		c.ReadLimit = defaultReadLimitBytes
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return nil
}

// V2HostCandidate is one authenticated exact v2 leg. Its methods are safe for
// concurrent activation, inbound control, and durable event callbacks.
type V2HostCandidate struct {
	cfg    V2HostConfig
	claims v2HostClaims
	conn   *websocket.Conn

	writeMu    sync.Mutex
	durableMu  sync.Mutex
	mu         sync.Mutex
	closed     bool
	err        error
	active     bool
	ackSeq     uint64
	ackVersion uint64
	// highestSent is the highest sequence-bearing raw frame written on this leg.
	highestSent         uint64
	gapTo               uint64
	gapPending          bool
	candidateSent       bool
	snapshotRequestSeen bool
	pendingSeq          uint64
	pendingRaw          []byte
	notify              chan struct{}
	closedCh            chan struct{}

	snapshotRequests chan attachwire.SnapshotRequest
	cancel           context.CancelFunc
}

// DialV2HostCandidate authenticates and subscribes one non-active candidate.
// It requires an exact v2 token and exact subprotocol echo; it never falls back
// to v1 or the degraded carrier.
func DialV2HostCandidate(ctx context.Context, cfg V2HostConfig) (*V2HostCandidate, error) {
	if err := cfg.withDefaults(); err != nil {
		return nil, err
	}
	token, err := cfg.TokenSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("attachclient: v2 token source: %w", err)
	}
	claims, err := parseV2HostClaims(token, cfg.Now())
	if err != nil {
		return nil, err
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, cfg.DialTimeout)
	conn, _, err := websocket.Dial(dialCtx, cfg.AttachURL, &websocket.DialOptions{
		HTTPClient:   cfg.HTTPClient,
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + token}},
		Subprotocols: []string{attachwirev2.SubprotocolVersion},
	})
	cancelDial()
	if err != nil {
		return nil, fmt.Errorf("attachclient: v2 wss dial: %w", err)
	}
	if conn.Subprotocol() != attachwirev2.SubprotocolVersion {
		got := conn.Subprotocol()
		_ = conn.Close(websocket.StatusProtocolError, "v2 subprotocol not negotiated")
		return nil, fmt.Errorf("attachclient: v2 subprotocol %q not echoed (got %q)", attachwirev2.SubprotocolVersion, got)
	}
	conn.SetReadLimit(cfg.ReadLimit)
	legCtx, cancel := context.WithCancel(context.Background())
	candidate := &V2HostCandidate{
		cfg: cfg, claims: claims, conn: conn, notify: make(chan struct{}),
		closedCh: make(chan struct{}), ackSeq: cfg.DurableHighWater, highestSent: cfg.DurableHighWater,
		snapshotRequests: make(chan attachwire.SnapshotRequest, 1), cancel: cancel,
	}
	subscribe, err := attachwirev2.BuildControlFrame(attachwire.Subscribe{
		SessionID: claims.SessionID, AsRole: attachwire.RoleHost,
		Epoch: int64Pointer(claims.Epoch), ResumeFrom: nil,
	})
	if err != nil {
		_ = candidate.Close()
		return nil, err
	}
	if err := candidate.writeRaw(ctx, subscribe.Encode()); err != nil {
		_ = candidate.Close()
		return nil, fmt.Errorf("attachclient: v2 subscribe: %w", err)
	}
	go candidate.readLoop(legCtx)
	return candidate, nil
}

func int64Pointer(value uint64) *int64 {
	if value > uint64(^uint64(0)>>1) {
		return nil
	}
	converted := int64(value) //nolint:gosec // range checked above
	return &converted
}

// WaitMandatorySnapshotRequest returns the one pre-active resync request. A
// second request before activation is a terminal protocol error.
func (c *V2HostCandidate) WaitMandatorySnapshotRequest(ctx context.Context) (attachwire.SnapshotRequest, error) {
	select {
	case request := <-c.snapshotRequests:
		if request.Reason != attachwire.ReasonResync {
			return attachwire.SnapshotRequest{}, errors.New("attachclient: v2 candidate snapshot request is not resync")
		}
		return request, nil
	case <-ctx.Done():
		return attachwire.SnapshotRequest{}, ctx.Err()
	case <-c.done():
		return attachwire.SnapshotRequest{}, c.terminalError()
	}
}

// SendCandidateSnapshot writes the exact mandatory sequence-bearing Snapshot
// while the leg is still non-active. It deliberately does not wait for host_ack:
// the strict stored receipt drives adoption publication and carrier_active later
// resolves this pending cursor.
func (c *V2HostCandidate) SendCandidateSnapshot(ctx context.Context, raw []byte) error {
	c.durableMu.Lock()
	defer c.durableMu.Unlock()
	frame, err := attachwire.DecodeFrame(raw)
	if err != nil || frame.Type != attachwire.TypeSnapshot || frame.Seq == 0 {
		return errors.New("attachclient: v2 candidate requires an exact sequence-bearing Snapshot frame")
	}
	c.mu.Lock()
	if c.closed || c.active || c.candidateSent || !c.snapshotRequestSeen {
		c.mu.Unlock()
		return errors.New("attachclient: v2 candidate Snapshot is late, duplicate, or unavailable")
	}
	if c.gapPending {
		if frame.Seq != c.gapTo+1 {
			c.mu.Unlock()
			return errors.New("attachclient: v2 candidate gap is not followed by its authoritative Snapshot")
		}
	} else if frame.Seq != c.highestSent+1 {
		c.mu.Unlock()
		return errors.New("attachclient: v2 candidate Snapshot is not contiguous with the durable cursor")
	}
	if err := c.writeRaw(ctx, raw); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("attachclient: write v2 candidate Snapshot: %w", err)
	}
	c.highestSent = frame.Seq
	c.candidateSent = true
	c.gapPending = false
	c.pendingSeq = frame.Seq
	c.pendingRaw = append([]byte(nil), raw...)
	c.mu.Unlock()
	return nil
}

// DeclareHostGap sends the exact replay-gap disposition. The following durable
// send must be the authoritative Snapshot at toSeq+1.
func (c *V2HostCandidate) DeclareHostGap(ctx context.Context, fromSeq, toSeq uint64) error {
	c.durableMu.Lock()
	defer c.durableMu.Unlock()
	if fromSeq == 0 || toSeq < fromSeq {
		return errors.New("attachclient: invalid v2 host gap")
	}
	c.mu.Lock()
	if c.closed || c.gapPending || fromSeq != c.ackSeq+1 {
		c.mu.Unlock()
		return errors.New("attachclient: v2 host gap does not begin at the contiguous durable cursor")
	}
	control, err := attachwirev2.BuildControlFrame(attachwirev2.HostGap{
		FromSeq: attachwirev2.DecimalUint64(fromSeq), ToSeq: attachwirev2.DecimalUint64(toSeq),
		Reason: attachwirev2.GapRingEvicted,
	})
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if err := c.writeRaw(ctx, control.Encode()); err != nil {
		c.mu.Unlock()
		return err
	}
	c.gapPending = true
	c.gapTo = toSeq
	c.mu.Unlock()
	return nil
}

// Activate sends carrier_activate after Donmai's local publication and waits
// for the exact carrier_active acknowledgement. The returned cursor also
// resolves the pre-active Snapshot when it covers that sequence.
func (c *V2HostCandidate) Activate(ctx context.Context) (uint64, error) {
	c.mu.Lock()
	if c.closed || !c.candidateSent {
		c.mu.Unlock()
		return 0, errors.New("attachclient: carrier_activate requires the mandatory candidate Snapshot")
	}
	c.mu.Unlock()
	control, err := attachwirev2.BuildControlFrame(attachwirev2.CarrierActivate{
		PTYEpoch:     attachwirev2.DecimalUint64(c.claims.Epoch),
		CarrierEpoch: attachwirev2.DecimalUint64(c.claims.CarrierEpoch),
	})
	if err != nil {
		return 0, err
	}
	if err := c.writeRaw(ctx, control.Encode()); err != nil {
		return 0, fmt.Errorf("attachclient: carrier_activate: %w", err)
	}
	for {
		c.mu.Lock()
		if c.active {
			ack := c.ackSeq
			c.mu.Unlock()
			return ack, nil
		}
		if c.closed {
			err := c.err
			c.mu.Unlock()
			return 0, terminalV2Error(err)
		}
		notify := c.notify
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-notify:
		}
	}
}

// SendRawFrameDurable writes exact source bytes and returns nil only after the
// current exact leg reports a contiguous host_ack covering the sequence. It is
// the generic client method a composing OnSessionEventDurable callback targets.
func (c *V2HostCandidate) SendRawFrameDurable(ctx context.Context, raw []byte) error {
	c.durableMu.Lock()
	defer c.durableMu.Unlock()
	frame, err := attachwire.DecodeFrame(raw)
	if err != nil || frame.Seq == 0 || frame.Type == attachwire.TypeInput || frame.Type == attachwire.TypeControl {
		return errors.New("attachclient: v2 durable send requires a sequence-bearing host frame")
	}
	c.mu.Lock()
	if c.closed || !c.active {
		c.mu.Unlock()
		return errors.New("attachclient: v2 carrier is not active")
	}
	retryPending := c.pendingSeq != 0
	switch {
	case retryPending:
		if frame.Seq != c.pendingSeq || !bytes.Equal(raw, c.pendingRaw) {
			c.mu.Unlock()
			return errors.New("attachclient: v2 pending host frame replay changed sequence or raw bytes")
		}
	case c.gapPending:
		if frame.Type != attachwire.TypeSnapshot || frame.Seq != c.gapTo+1 {
			c.mu.Unlock()
			return errors.New("attachclient: v2 host gap must be followed by its exact authoritative Snapshot")
		}
	case frame.Seq > c.ackSeq && frame.Seq != c.highestSent+1:
		c.mu.Unlock()
		return errors.New("attachclient: v2 host frame is not contiguous with the sent stream")
	}
	ackVersion := c.ackVersion
	if err := c.writeRaw(ctx, raw); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("attachclient: write v2 durable host frame: %w", err)
	}
	if !retryPending {
		if frame.Seq > c.highestSent {
			c.highestSent = frame.Seq
		}
		c.pendingSeq = frame.Seq
		c.pendingRaw = append([]byte(nil), raw...)
		c.gapPending = false
	}
	c.mu.Unlock()
	return c.waitForAck(ctx, frame.Seq, ackVersion)
}

// OnSessionEventDurable is a named alias for SendRawFrameDurable so composing
// adapters can expose its durable boundary without restating the semantics.
func (c *V2HostCandidate) OnSessionEventDurable(ctx context.Context, raw []byte) error {
	return c.SendRawFrameDurable(ctx, raw)
}

func (c *V2HostCandidate) waitForAck(ctx context.Context, sequence, afterVersion uint64) error {
	for {
		c.mu.Lock()
		if c.ackSeq >= sequence && c.ackVersion > afterVersion {
			c.mu.Unlock()
			return nil
		}
		if c.closed {
			err := c.err
			c.mu.Unlock()
			return terminalV2Error(err)
		}
		notify := c.notify
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notify:
		}
	}
}

func (c *V2HostCandidate) readLoop(ctx context.Context) {
	for {
		kind, raw, err := c.conn.Read(ctx)
		if err != nil {
			c.fail(err)
			return
		}
		if kind != websocket.MessageBinary {
			c.fail(errors.New("attachclient: v2 carrier received a non-binary message"))
			return
		}
		frame, err := attachwire.DecodeFrame(raw)
		if err != nil {
			c.fail(fmt.Errorf("attachclient: v2 inbound frame: %w", err))
			return
		}
		if err := c.handleV2Inbound(ctx, frame); err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *V2HostCandidate) handleV2Inbound(ctx context.Context, frame attachwire.Frame) error {
	if frame.Type == attachwire.TypeControl {
		payload, err := attachwire.DecodeControlPayload(frame.Payload)
		if err != nil {
			return err
		}
		message, err := attachwirev2.DecodeControl(payload)
		if err != nil {
			return err
		}
		switch typed := message.(type) {
		case attachwire.SnapshotRequest:
			c.mu.Lock()
			active := c.active
			if !active {
				if c.snapshotRequestSeen {
					c.mu.Unlock()
					return errors.New("attachclient: v2 candidate received more than one mandatory Snapshot request")
				}
				c.snapshotRequestSeen = true
			}
			c.mu.Unlock()
			if !active {
				select {
				case c.snapshotRequests <- typed:
					return nil
				default:
					return errors.New("attachclient: v2 candidate Snapshot request queue is unavailable")
				}
			}
			if c.cfg.OnSnapshotRequest != nil {
				return c.cfg.OnSnapshotRequest(ctx, typed)
			}
			return nil
		case attachwirev2.CarrierActive:
			return c.acceptCarrierActive(typed)
		case attachwirev2.HostAck:
			return c.acceptHostAck(typed)
		case attachwire.Kill:
			if !c.isActive() {
				return errors.New("attachclient: v2 candidate received Kill before carrier_active")
			}
			if c.cfg.OnKill != nil {
				return c.cfg.OnKill(ctx, typed)
			}
			return nil
		case attachwire.ControlError:
			return &RelayStopError{Code: typed.Code, Message: typed.Message}
		default:
			return nil
		}
	}
	if !c.isActive() {
		return errors.New("attachclient: v2 candidate received an authority-bearing frame before carrier_active")
	}
	switch frame.Type {
	case attachwire.TypeInput:
		input, err := attachwire.DecodeInput(frame.Payload)
		if err != nil {
			return err
		}
		if c.cfg.OnInput != nil {
			return c.cfg.OnInput(ctx, input)
		}
	case attachwire.TypeResize:
		resize, err := attachwire.DecodeResize(frame.Payload)
		if err != nil {
			return err
		}
		if c.cfg.OnResize != nil {
			return c.cfg.OnResize(ctx, resize)
		}
	}
	return nil
}

func (c *V2HostCandidate) acceptCarrierActive(message attachwirev2.CarrierActive) error {
	if uint64(message.PTYEpoch) != c.claims.Epoch || uint64(message.CarrierEpoch) != c.claims.CarrierEpoch {
		return attachwirev2.ErrControlMismatch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ack := uint64(message.AckSeq)
	if !c.candidateSent || ack != c.highestSent || (c.active && ack < c.ackSeq) {
		return errors.New("attachclient: carrier_active cursor is outside the exact sent stream")
	}
	c.active = true
	c.ackSeq = ack
	c.ackVersion++
	if c.pendingSeq != 0 && ack >= c.pendingSeq {
		c.pendingSeq = 0
		c.pendingRaw = nil
	}
	c.signalLocked()
	return nil
}

func (c *V2HostCandidate) acceptHostAck(message attachwirev2.HostAck) error {
	if uint64(message.PTYEpoch) != c.claims.Epoch || uint64(message.CarrierEpoch) != c.claims.CarrierEpoch {
		return attachwirev2.ErrControlMismatch
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ack := uint64(message.AckSeq)
	if !c.active || ack < c.ackSeq || ack > c.highestSent {
		return errors.New("attachclient: host_ack is stale, early, or beyond the exact sent stream")
	}
	c.ackSeq = ack
	c.ackVersion++
	if c.pendingSeq != 0 && ack >= c.pendingSeq {
		c.pendingSeq = 0
		c.pendingRaw = nil
	}
	c.signalLocked()
	return nil
}

func (c *V2HostCandidate) isActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func (c *V2HostCandidate) writeRaw(ctx context.Context, raw []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(ctx, websocket.MessageBinary, raw)
}

func (c *V2HostCandidate) signalLocked() {
	close(c.notify)
	c.notify = make(chan struct{})
}

func (c *V2HostCandidate) fail(err error) {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.err = err
		close(c.closedCh)
		c.signalLocked()
	}
	c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *V2HostCandidate) done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closedCh
}

func (c *V2HostCandidate) terminalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return terminalV2Error(c.err)
}

func terminalV2Error(err error) error {
	if err == nil {
		return errors.New("attachclient: v2 carrier closed")
	}
	return err
}

// Close releases the candidate connection. It does not imply that a frame was
// durable or that the session ended.
func (c *V2HostCandidate) Close() error {
	if c == nil {
		return nil
	}
	c.fail(errors.New("attachclient: v2 carrier closed"))
	return c.conn.Close(websocket.StatusNormalClosure, "host carrier closed")
}
