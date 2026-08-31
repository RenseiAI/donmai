package attachclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/coder/websocket"
)

// wssLeg wraps one live WSS connection. coder/websocket permits one concurrent
// reader and one concurrent writer but NOT two concurrent writers, so every
// outbound write (the subscribe control, streamed frames, post-Exit snapshot
// replies, error controls) goes through the write mutex.
type wssLeg struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (l *wssLeg) write(ctx context.Context, b []byte) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	return l.conn.Write(ctx, websocket.MessageBinary, b)
}

// runWSS dials the WSS lane once and runs the host leg until the connection
// ends. A dial/handshake failure or a mid-stream drop returns a transient error
// (RunHost backs off / counts toward the § 14 fallback). A clean end after the
// post-Exit window returns nil.
func (h *host) runWSS(ctx context.Context, tok string, cl hostClaims, exitDeadline time.Time) (attemptResult, error) {
	var res attemptResult

	dialCtx, cancel := context.WithTimeout(ctx, h.cfg.DialTimeout)
	conn, _, err := websocket.Dial(dialCtx, h.cfg.AttachURL, &websocket.DialOptions{
		HTTPClient:   h.cfg.HTTPClient,
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + tok}},
		Subprotocols: []string{attachwire.SubprotocolVersion},
	})
	cancel()
	if err != nil {
		return res, fmt.Errorf("attachclient: wss dial: %w", err)
	}
	conn.SetReadLimit(h.cfg.ReadLimitBytes)

	// § 1: the relay MUST echo the version subprotocol. coder/websocket does NOT
	// error when the server omits it, so we verify ourselves and treat a missing
	// echo as a rejected handshake (→ fallback candidate).
	if conn.Subprotocol() != attachwire.SubprotocolVersion {
		got := conn.Subprotocol()
		_ = conn.Close(websocket.StatusProtocolError, "version subprotocol not negotiated")
		return res, fmt.Errorf("attachclient: wss handshake: subprotocol %q not echoed", got)
	}

	return h.runWSSLeg(ctx, conn, cl, exitDeadline)
}

func (h *host) runWSSLeg(ctx context.Context, conn *websocket.Conn, cl hostClaims, exitDeadline time.Time) (res attemptResult, retErr error) {
	leg := &wssLeg{conn: conn}
	authorityAt := make(chan time.Time, 1)
	defer func() {
		select {
		case observedAt := <-authorityAt:
			res.authorityConfirmed = true
			res.progressedAt = observedAt
		default:
		}
	}()

	legCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.CloseNow() //nolint:errcheck // best-effort teardown

	// Send the subscribe control on open (§ 7, zeroed headers per § 2).
	subFrame, err := buildHostSubscribe(cl)
	if err != nil {
		return res, err
	}
	if err := leg.write(legCtx, subFrame.Encode()); err != nil {
		return res, fmt.Errorf("attachclient: writing subscribe: %w", err)
	}
	// Negotiated open + subscribe written → transport progress (reset ordinary
	// backoff). It is NOT authority confirmation until the relay sends a frame.
	res.progressed = true

	fromSeq, err := h.subscribeFromSeq()
	if err != nil {
		return res, fmt.Errorf("attachclient: resolving reconnect head: %w", err)
	}
	sub, err := h.cfg.Session.Subscribe(fromSeq)
	if err != nil {
		return res, fmt.Errorf("attachclient: session subscribe from %d: %w", fromSeq, err)
	}
	defer sub.Close() //nolint:errcheck
	h.markStreamed()

	// Outbound pump: Session frames → the connection.
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for f := range sub.Frames() {
			if err := leg.write(legCtx, f.Encode()); err != nil {
				return
			}
		}
	}()

	// Inbound reader: relay control → Session effects.
	readerErr := make(chan error, 1)
	go func() { readerErr <- h.wssReadLoop(legCtx, leg, authorityAt) }()

	select {
	case <-ctx.Done():
		return res, ctx.Err()
	case err := <-readerErr:
		return res, err
	case <-pumpDone:
		// The subscription ended. If Exit was delivered, hold the connection for
		// the bounded final-screen window (§ 12.2); otherwise it is a transient
		// end (Close/ring end) → reconnect.
		if _, ok := h.cfg.Session.Exit(); !ok {
			return res, errors.New("attachclient: subscription ended before Exit")
		}
		res.exitDelivered = true
		deadline := exitDeadline
		if deadline.IsZero() {
			deadline = h.now().Add(h.cfg.FinalScreenWindow)
		}
		res.windowDeadline = deadline

		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case err := <-readerErr:
			// Dropped mid-window → reconnect to serve the remaining window.
			return res, err
		case <-timer.C:
			_ = conn.Close(websocket.StatusNormalClosure, "final-screen window elapsed")
			res.windowServed = true
			return res, nil
		}
	}
}

// wssReadLoop reads and dispatches relay→host frames until the connection ends.
// A framing error (unknown type byte, bad varint, 0-dim resize, non-binary
// message) closes the leg with an error control (code framing, § 2.1/§ 3) and
// returns a transient error → reconnect. Terminal controls surface ErrEpochStale
// / *RelayStopError.
func (h *host) wssReadLoop(ctx context.Context, leg *wssLeg, authorityAt chan<- time.Time) error {
	for {
		typ, data, err := leg.conn.Read(ctx)
		if err != nil {
			return err // ws close / io error / ctx
		}
		if typ != websocket.MessageBinary {
			_ = leg.write(ctx, buildErrorControl(attachwire.CodeFraming, "binary frames only").Encode())
			_ = leg.conn.Close(websocket.StatusUnsupportedData, "binary frames only")
			return fmt.Errorf("attachclient: non-binary ws message (type %v)", typ)
		}
		frame, derr := attachwire.DecodeFrame(data)
		if derr != nil {
			// § 3: unknown frame type byte / § 2.1 bad varint → framing error →
			// close the leg with an error control, then reconnect discipline decides.
			_ = leg.write(ctx, buildErrorControl(attachwire.CodeFraming, derr.Error()).Encode())
			_ = leg.conn.Close(websocket.StatusProtocolError, "framing error")
			return fmt.Errorf("attachclient: inbound framing error: %w", derr)
		}
		back, herr := h.handleInbound(ctx, frame)
		if herr != nil {
			if attachwire.IsFramingErr(herr) {
				_ = leg.write(ctx, buildErrorControl(attachwire.CodeFraming, herr.Error()).Encode())
				_ = leg.conn.Close(websocket.StatusProtocolError, "framing error")
			}
			return herr
		}
		for _, bf := range back {
			if err := leg.write(ctx, bf.Encode()); err != nil {
				return err
			}
		}
		select {
		case authorityAt <- h.now():
		default:
		}
	}
}
