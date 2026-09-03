package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

// DefaultKeepAliveTimeout bounds one keepalive exchange end to end: dial,
// authenticated Hello, the keepalive frame, and the shim's answer. It is far
// below any orphan deadline a composing deployment resolves, because a
// keepalive that has not answered by then has already failed to extend
// anything.
const DefaultKeepAliveTimeout = 5 * time.Second

// orphanKeepaliveAnswerTimeout bounds the shim's half of one keepalive: a
// registry write and one frame. It replaces the adoption output-barrier bound
// the connection inherits, which is sized for a whole handshake and would keep
// a stalled daemon's socket alive far longer than an observation needs.
const orphanKeepaliveAnswerTimeout = 3 * time.Second

var (
	// ErrKeepAliveRefused reports a shim that answered the keepalive with a
	// refusal: it is not orphaned, its deadline has already fired, or it does
	// not implement the orphan keepalive at all. The caller MUST treat the
	// lineage as running on its ordinary orphan deadline from that moment —
	// the keepalive extended nothing (§D8, amendment 2026-09-03).
	ErrKeepAliveRefused = errors.New("sessionshim: orphan keepalive refused")

	// ErrKeepAliveUnobservable reports that the keepalive could not reach the
	// shim at all: the record is gone, the socket is not the one the record
	// binds to, the peer is not the owning uid, or the dial failed. An
	// unobservable shim falls back to its own orphan deadline immediately.
	ErrKeepAliveUnobservable = errors.New("sessionshim: shim is unobservable")
)

// KeepAliveOptions bounds one KeepAlive call.
type KeepAliveOptions struct {
	// Timeout bounds the whole exchange. Zero uses DefaultKeepAliveTimeout.
	Timeout time.Duration
	// ExpectedShimID and ExpectedProcessEpoch pin the exchange to ONE
	// incarnation. A keepalive that lands on a different incarnation than the
	// one being re-adopted would extend the wrong lineage's clock, so a
	// mismatch is refused rather than honoured. Zero values skip the check.
	ExpectedShimID       string
	ExpectedProcessEpoch uint64
}

func (o KeepAliveOptions) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return DefaultKeepAliveTimeout
}

// KeepAlive extends the orphan clock of one live, orphaned shim by exactly one
// deadline, and reports the instant the shim re-armed to.
//
// It is the daemon half of the §D8 amendment 2026-09-03 obligation: while a
// daemon retries re-adoption for a window that MAY exceed the resolved orphan
// deadline, it must keep telling the shim it is still there, or the shim reaps
// a harness the daemon can still see. The message is an ordinary v1 Heartbeat
// sent in the position a Welcome would occupy, so it needs no new wire type and
// no version bump: a shim that predates this contract answers the malformed
// code and the caller learns, from ErrKeepAliveRefused, that this lineage is
// bounded by its plain orphan deadline after all.
//
// It deliberately does NOT adopt: no generation is proposed, no authority
// moves, and the connection is closed as soon as the shim answers. A keepalive
// is an observation, not a claim — which is why it is safe to send it from a
// daemon whose adoption is still failing.
func KeepAlive(ctx context.Context, rec Record, opts KeepAliveOptions) (time.Time, error) {
	if err := rec.Validate(); err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrKeepAliveUnobservable, err)
	}
	if !peerCredSupported() {
		return time.Time{}, ErrShimUnsupported
	}
	if err := verifySocketIdentity(rec); err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrKeepAliveUnobservable, err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	var dialer net.Dialer
	raw, err := dialer.DialContext(dialCtx, "unix", rec.SocketPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: dial keepalive socket: %w", ErrKeepAliveUnobservable, err)
	}
	conn, ok := raw.(*net.UnixConn)
	if !ok {
		_ = raw.Close()
		return time.Time{}, fmt.Errorf("%w: keepalive socket is not a unix connection", ErrKeepAliveUnobservable)
	}
	defer func() { _ = conn.Close() }()
	uid, err := peerUID(conn)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: verify keepalive peer: %w", ErrKeepAliveUnobservable, err)
	}
	if uid != os.Getuid() {
		return time.Time{}, fmt.Errorf("%w: keepalive peer uid %d is not %d", ErrKeepAliveUnobservable, uid, os.Getuid())
	}
	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	w := shimwire.NewWriter(conn)
	r := shimwire.NewReader(conn)

	msg, err := r.Read()
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: read keepalive hello: %w", ErrKeepAliveUnobservable, err)
	}
	if msg.Type != shimwire.TypeHello {
		return time.Time{}, fmt.Errorf("%w: expected Hello, got %s", ErrKeepAliveRefused, msg.Type)
	}
	hello, err := shimwire.DecodeHello(msg.Body)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", ErrKeepAliveRefused, err)
	}
	if hello.OrgID != rec.OrgID || hello.SessionID != rec.SessionID {
		return time.Time{}, fmt.Errorf("%w: keepalive reached %s/%s, expected %s/%s",
			ErrKeepAliveRefused, hello.OrgID, hello.SessionID, rec.OrgID, rec.SessionID)
	}
	if opts.ExpectedShimID != "" && hello.ShimID != opts.ExpectedShimID {
		return time.Time{}, fmt.Errorf("%w: keepalive reached shim %q, expected %q",
			ErrKeepAliveRefused, hello.ShimID, opts.ExpectedShimID)
	}
	if opts.ExpectedProcessEpoch != 0 && hello.ProcessEpoch != opts.ExpectedProcessEpoch {
		return time.Time{}, fmt.Errorf("%w: keepalive reached process epoch %d, expected %d",
			ErrKeepAliveRefused, hello.ProcessEpoch, opts.ExpectedProcessEpoch)
	}
	body, err := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{Phase: shimwire.PhaseOrphaned})
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: encode keepalive: %w", ErrKeepAliveRefused, err)
	}
	if err := w.Write(shimwire.TypeHeartbeat, body); err != nil {
		return time.Time{}, fmt.Errorf("%w: write keepalive: %w", ErrKeepAliveUnobservable, err)
	}
	answer, err := r.Read()
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: read keepalive answer: %w", ErrKeepAliveUnobservable, err)
	}
	switch answer.Type {
	case shimwire.TypeHeartbeat:
		beat, err := shimwire.DecodeHeartbeat(answer.Body)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: %w", ErrKeepAliveRefused, err)
		}
		if beat.Phase != shimwire.PhaseOrphaned {
			return time.Time{}, fmt.Errorf("%w: shim answered phase %q", ErrKeepAliveRefused, beat.Phase)
		}
		if beat.OrphanDeadlineAt == 0 {
			return time.Time{}, fmt.Errorf("%w: shim answered without a re-armed deadline", ErrKeepAliveRefused)
		}
		return time.Unix(0, beat.OrphanDeadlineAt), nil
	case shimwire.TypeError:
		e, decErr := shimwire.DecodeError(answer.Body)
		if decErr != nil {
			return time.Time{}, fmt.Errorf("%w: %w", ErrKeepAliveRefused, decErr)
		}
		return time.Time{}, fmt.Errorf("%w: %s: %s", ErrKeepAliveRefused, e.Code, e.Detail)
	default:
		return time.Time{}, fmt.Errorf("%w: shim answered %s", ErrKeepAliveRefused, answer.Type)
	}
}

// errOrphanKeepaliveServed ends a connection that carried a keepalive rather
// than an adoption. It is not a fault: serveController closes the connection
// without logging it, exactly as it would after a completed exchange.
var errOrphanKeepaliveServed = errors.New("sessionshim: orphan keepalive served")

// serveOrphanKeepalive answers ONE keepalive on a connection that offered a
// Heartbeat where a Welcome belongs.
//
// The re-arm is refused unless the shim is genuinely orphaned with a live
// timer: a keepalive must never resurrect a deadline that has already fired,
// and it must never quietly become a second liveness rule for a shim that has
// a controller attached.
func (s *Shim) serveOrphanKeepalive(w *shimwire.Writer, msg shimwire.Message) error {
	if _, err := shimwire.DecodeHeartbeat(msg.Body); err != nil {
		_ = sendError(w, shimwire.CodeMalformed, "keepalive did not decode")
		return fmt.Errorf("sessionshim: decode orphan keepalive: %w", err)
	}
	deadline, rearmed := s.refreshOrphanDeadline()
	if !rearmed {
		_ = sendError(w, shimwire.CodePhaseUnknown, "keepalive outside an armed orphan deadline")
		return errOrphanKeepaliveServed
	}
	s.logger.Info("sessionshim: orphan deadline extended by a daemon keepalive",
		"session", s.id.String(), "shim", s.shimID, "deadline", deadline)
	if err := writeTyped(w, shimwire.TypeHeartbeat, func() ([]byte, error) {
		return shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{
			Phase:            shimwire.PhaseOrphaned,
			OrphanDeadlineAt: deadline.UnixNano(),
		})
	}); err != nil {
		return fmt.Errorf("sessionshim: answer orphan keepalive: %w", err)
	}
	return errOrphanKeepaliveServed
}

// refreshOrphanDeadline re-arms an ALREADY ARMED orphan timer for one further
// deadline and republishes the discovery record's deadline, reporting the
// instant it re-armed to.
//
// It reports false — and changes nothing — unless the shim is orphaned with a
// timer that has not fired yet. Those two conditions are the whole safety
// argument: a shim with a controller does not need extending, and a shim whose
// deadline already fired is on its way to a terminal observation that nothing
// may take back (§D8, §D10).
func (s *Shim) refreshOrphanDeadline() (time.Time, bool) {
	deadline := s.now().Add(s.orphan.Deadline)
	s.mu.Lock()
	if s.phase != shimwire.PhaseOrphaned || s.orphanTimer == nil {
		s.mu.Unlock()
		return time.Time{}, false
	}
	if !s.orphanTimer.Stop() {
		// The deadline already fired: onOrphanDeadline owns this shim now.
		s.mu.Unlock()
		return time.Time{}, false
	}
	s.orphanTimer.Reset(s.orphan.Deadline)
	s.mu.Unlock()
	if err := s.publishRecordWithDeadline(deadline); err != nil {
		s.logger.Warn("sessionshim: republish record on orphan keepalive",
			"session", s.id.String(), "error", err)
	}
	return deadline, true
}
