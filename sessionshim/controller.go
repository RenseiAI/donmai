package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

// EventKind discriminates a ControllerEvent.
type EventKind string

// The controller event kinds.
const (
	// EventOutput carries shim-allocated sequence plus raw terminal bytes.
	EventOutput EventKind = "output"
	// EventGap declares an explicit, attributed loss of output. A consumer MUST
	// surface it rather than smoothing it over (§D5).
	EventGap EventKind = "gap"
	// EventSnapshot carries terminal state, typically right after a gap.
	EventSnapshot EventKind = "snapshot"
	// EventExit is the immutable terminal observation.
	EventExit EventKind = "exit"
	// EventError is a closed code with display-only detail.
	EventError EventKind = "error"
)

// ControllerEvent is one thing that happened on an adopted session.
type ControllerEvent struct {
	Kind EventKind

	Seq     uint64
	RelTime uint64
	Data    []byte

	Gap      shimwire.GapMsg
	Snapshot shimwire.SnapshotMsg
	Exit     shimwire.ExitMsg
	Err      shimwire.ErrorMsg
}

// ControllerOptions configure a dial.
type ControllerOptions struct {
	// ControllerID identifies this controller process in diagnostics.
	ControllerID string
	// ProposedGeneration is the generation to propose. Zero means "derive it
	// from the shim's own authoritative current generation", which is the
	// ordinary path — see NextGeneration.
	ProposedGeneration shimwire.Generation
	// NextGeneration, when set, computes the proposal from the generation the
	// shim reports in Hello. It supersedes ProposedGeneration.
	//
	// The callback shape exists so the proposal is always derived from the
	// AUTHORITATIVE value rather than from a daemon's own bookkeeping: the shim
	// owns the generation, and a daemon that proposed from a stale local counter
	// would fence itself out (§D4).
	NextGeneration func(current shimwire.Generation) shimwire.Generation
	// ResumeFrom is the first sequence this controller still needs, i.e. its
	// durable last_forwarded_seq + 1. Zero means "from the start of the stream".
	ResumeFrom uint64
	// ExpectedWorkarea, when non-empty, is compared against the shim's
	// self-reported workarea. A mismatch refuses adoption.
	ExpectedWorkarea string
	// Extensions are optional negotiated extensions offered to the shim.
	Extensions shimwire.Extensions
	// DialTimeout bounds the connect + handshake. Zero uses 5s.
	DialTimeout time.Duration
	Logger      *slog.Logger
}

func (o ControllerOptions) dialTimeout() time.Duration {
	if o.DialTimeout > 0 {
		return o.DialTimeout
	}
	return 5 * time.Second
}

func (o ControllerOptions) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Controller is the daemon side of an adopted session.
//
// It holds a socket and a generation — never an fd, an exec.Cmd, or a
// *ptyhost.Session. That is the §D1 ownership boundary made concrete: when this
// object is garbage, the session is unaffected.
type Controller struct {
	id      Identity
	conn    *net.UnixConn
	w       *shimwire.Writer
	r       *shimwire.Reader
	gen     shimwire.Generation
	hello   shimwire.Hello
	adopted shimwire.Adopted
	// resumeFrom is the exact durable cursor proposed in Welcome. Retaining it
	// lets a replacement daemon preserve last_forwarded_seq before any newly
	// replayed or live output advances its own bookkeeping.
	resumeFrom uint64
	events     chan ControllerEvent
	logger     *slog.Logger
	closeOne   sync.Once
	done       chan struct{}
	// closing is closed by Close BEFORE the connection is dropped, so a read
	// loop parked on an event send has something to select on. Without it, a
	// caller that stops consuming events and then closes would leave the loop
	// blocked on a channel nobody will ever read — the connection error that
	// would otherwise unwind it is never observed, because the loop is not in
	// Read at that moment.
	closing chan struct{}
}

// ErrAdoptionRefused reports a handshake the shim or this daemon declined.
var ErrAdoptionRefused = errors.New("sessionshim: adoption refused")

// Dial connects to a shim described by rec and completes Hello → Welcome →
// Adopted.
//
// Verification happens BEFORE any authority is proposed, in this order: the
// socket must be the exact (device, inode) the record binds to; the peer's
// self-reported lifecycle identity, process identity, and workarea must match
// the record. Only then is a generation proposed. Proposing first and checking
// afterwards would mean a mismatched shim had already been handed control.
func Dial(ctx context.Context, rec Record, opts ControllerOptions) (*Controller, error) {
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	if !peerCredSupported() {
		return nil, ErrShimUnsupported
	}
	if err := verifySocketIdentity(rec); err != nil {
		return nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, opts.dialTimeout())
	defer cancel()
	var d net.Dialer
	raw, err := d.DialContext(dialCtx, "unix", rec.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("sessionshim: dial adoption socket: %w", err)
	}
	conn, ok := raw.(*net.UnixConn)
	if !ok {
		_ = raw.Close()
		return nil, errors.New("sessionshim: adoption socket is not a unix connection")
	}
	// The peer is authenticated in BOTH directions: the shim checks us, and we
	// check it. A one-sided check would let anything that can create a socket in
	// a readable directory impersonate a shim.
	uid, err := peerUID(conn)
	if err != nil || uid != os.Getuid() {
		_ = conn.Close()
		if err != nil {
			return nil, fmt.Errorf("sessionshim: verify shim peer: %w", err)
		}
		return nil, fmt.Errorf("%w: shim socket peer uid %d is not %d", ErrAdoptionRefused, uid, os.Getuid())
	}

	if deadline, ok := dialCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	c := &Controller{
		id:         rec.Identity(),
		conn:       conn,
		w:          shimwire.NewWriter(conn),
		r:          shimwire.NewReader(conn),
		resumeFrom: opts.ResumeFrom,
		events:     make(chan ControllerEvent, 64),
		logger:     opts.logger(),
		done:       make(chan struct{}),
		closing:    make(chan struct{}),
	}
	if err := c.handshake(rec, opts); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Clear the handshake deadline: a live session is idle for long stretches by
	// design (a human is thinking), and inheriting a dial deadline would tear
	// down exactly the sessions this whole mechanism exists to preserve.
	_ = conn.SetDeadline(time.Time{})

	go c.readLoop()
	return c, nil
}

func (c *Controller) handshake(rec Record, opts ControllerOptions) error {
	msg, err := c.r.Read()
	if err != nil {
		return fmt.Errorf("sessionshim: read hello: %w", err)
	}
	if msg.Type != shimwire.TypeError && msg.Type != shimwire.TypeHello {
		return fmt.Errorf("%w: expected Hello, got %s", ErrAdoptionRefused, msg.Type)
	}
	if msg.Type == shimwire.TypeError {
		e, decErr := shimwire.DecodeError(msg.Body)
		if decErr != nil {
			return decErr
		}
		return fmt.Errorf("%w: shim refused at hello: %s: %s", ErrAdoptionRefused, e.Code, e.Detail)
	}
	hello, err := shimwire.DecodeHello(msg.Body)
	if err != nil {
		return err
	}
	if err := verifyHello(hello, rec, opts.ExpectedWorkarea); err != nil {
		return err
	}
	if err := hello.Extensions.CheckRequired(); err != nil {
		return err
	}
	selected, err := shimwire.Negotiate(hello.Min, hello.Max, shimwire.ProtocolMin, shimwire.ProtocolMax)
	if err != nil {
		return err
	}

	proposed := opts.ProposedGeneration
	if opts.NextGeneration != nil {
		proposed = opts.NextGeneration(hello.Generation)
	}
	if proposed == 0 {
		proposed = hello.Generation + 1
	}
	if proposed <= hello.Generation {
		return fmt.Errorf("%w: proposed generation %d does not advance the shim's %d",
			shimwire.ErrStaleGeneration, proposed, hello.Generation)
	}

	welcome := shimwire.Welcome{
		Protocol:           shimwire.ProtocolName,
		Selected:           selected,
		ControllerID:       opts.ControllerID,
		ProposedGeneration: proposed,
		ResumeFrom:         opts.ResumeFrom,
		Extensions:         opts.Extensions,
	}
	if err := writeTyped(c.w, shimwire.TypeWelcome, func() ([]byte, error) { return shimwire.EncodeWelcome(welcome) }); err != nil {
		return err
	}

	reply, err := c.r.Read()
	if err != nil {
		return fmt.Errorf("sessionshim: read adopted: %w", err)
	}
	if reply.Type == shimwire.TypeError {
		e, decErr := shimwire.DecodeError(reply.Body)
		if decErr != nil {
			return decErr
		}
		return fmt.Errorf("%w: %s: %s", ErrAdoptionRefused, e.Code, e.Detail)
	}
	if reply.Type != shimwire.TypeAdopted {
		return fmt.Errorf("%w: expected Adopted, got %s", ErrAdoptionRefused, reply.Type)
	}
	adopted, err := shimwire.DecodeAdopted(reply.Body)
	if err != nil {
		return err
	}
	// Trust the COMMITTED generation, not the proposed one. They agree today, but
	// the shim is authoritative and a controller that assumed its own proposal
	// would fence itself out the moment they ever diverged.
	if adopted.Generation < proposed {
		return fmt.Errorf("%w: shim committed generation %d, below the proposed %d",
			ErrAdoptionRefused, adopted.Generation, proposed)
	}
	c.hello, c.adopted, c.gen = hello, adopted, adopted.Generation
	return nil
}

// verifyHello checks that the live peer is the shim the record described.
func verifyHello(h shimwire.Hello, rec Record, expectedWorkarea string) error {
	if h.Protocol != shimwire.ProtocolName {
		return fmt.Errorf("%w: hello names protocol %q", shimwire.ErrVersionMismatch, h.Protocol)
	}
	if h.OrgID != rec.OrgID || h.SessionID != rec.SessionID {
		return fmt.Errorf("%w: shim reports identity %s/%s, record says %s/%s",
			ErrAdoptionRefused, h.OrgID, h.SessionID, rec.OrgID, rec.SessionID)
	}
	if h.ShimID != rec.ShimID {
		return fmt.Errorf("%w: shim reports id %q, record says %q", ErrAdoptionRefused, h.ShimID, rec.ShimID)
	}
	if h.PID != rec.PID || h.ProcessStartedAt != rec.ProcessStartedAt {
		// The live peer is a DIFFERENT process than the record describes — the
		// PID-reuse case §D2 requires be caught rather than trusted.
		return fmt.Errorf("%w: shim process identity pid=%d start=%d does not match record pid=%d start=%d",
			ErrAdoptionRefused, h.PID, h.ProcessStartedAt, rec.PID, rec.ProcessStartedAt)
	}
	if !h.Phase.Known() {
		return fmt.Errorf("%w: shim reports uninterpretable phase %q", ErrAdoptionRefused, h.Phase)
	}
	if rec.WorkareaPath != "" && h.WorkareaPath != rec.WorkareaPath {
		return fmt.Errorf("%w: shim reports workarea %q, record says %q",
			ErrAdoptionRefused, h.WorkareaPath, rec.WorkareaPath)
	}
	if expectedWorkarea != "" && h.WorkareaPath != expectedWorkarea {
		return fmt.Errorf("%w: shim reports workarea %q, this daemon expects %q",
			ErrAdoptionRefused, h.WorkareaPath, expectedWorkarea)
	}
	return nil
}

// verifySocketIdentity confirms the record's socket path still names the exact
// socket file the record was written against.
//
// A path can be unlinked and recreated by anything that can write the directory;
// the (device, inode) pair cannot be reproduced that way. Without this, adoption
// would authenticate a socket that merely occupies the right NAME.
func verifySocketIdentity(rec Record) error {
	info, err := os.Stat(rec.SocketPath)
	if err != nil {
		return fmt.Errorf("sessionshim: stat shim socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: %s is not a socket", ErrRegistryUnsafe, rec.SocketPath)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	//nolint:gosec,unconvert // Dev/Ino widths are platform-dependent; both are non-negative identifiers
	dev, ino := uint64(st.Dev), uint64(st.Ino)
	if rec.SocketDevice != 0 || rec.SocketInode != 0 {
		if dev != rec.SocketDevice || ino != rec.SocketInode {
			return fmt.Errorf("%w: socket at %s is device %d inode %d, record binds device %d inode %d",
				ErrAdoptionRefused, rec.SocketPath, dev, ino, rec.SocketDevice, rec.SocketInode)
		}
	}
	return checkOwnedBySelf(info, rec.SocketPath)
}

// Identity returns the adopted session's lifecycle identity.
func (c *Controller) Identity() Identity { return c.id }

// Generation returns the committed controller generation.
func (c *Controller) Generation() shimwire.Generation { return c.gen }

// Hello returns the shim's opening self-report.
func (c *Controller) Hello() shimwire.Hello { return c.hello }

// Adoption returns the replay disposition the shim committed to.
func (c *Controller) Adoption() shimwire.Adopted { return c.adopted }

// ResumeFrom returns the first sequence this controller requested during
// adoption. When non-zero, ResumeFrom-1 is the composing carrier's exact
// durable last-forwarded sequence at the adoption boundary.
func (c *Controller) ResumeFrom() uint64 { return c.resumeFrom }

// HarnessSurvived reports whether the shim's harness is still live. It is the
// operative question after a restart: adoption succeeded AND the workload
// continues.
func (c *Controller) HarnessSurvived() bool {
	return c.hello.Phase == shimwire.PhaseRunning || c.hello.Phase == shimwire.PhaseOrphaned
}

// HarnessIdentity returns the owned harness's process identity as the shim
// reports it. Comparing this across a daemon restart is what distinguishes "the
// session survived" from "a new process took over the same session id".
func (c *Controller) HarnessIdentity() ProcessIdentity {
	return ProcessIdentity{PID: c.hello.HarnessPID, StartedAt: c.hello.HarnessStartedAt}
}

// Events is the stream of everything the shim reports. Closed when the
// connection ends.
func (c *Controller) Events() <-chan ControllerEvent { return c.events }

// Done is closed when the controller's read loop has finished.
func (c *Controller) Done() <-chan struct{} { return c.done }

// WriteInput sends attributed input bytes under this controller's generation.
func (c *Controller) WriteInput(data []byte) error {
	return c.w.Write(shimwire.TypeInput, shimwire.EncodeInput(c.gen, data))
}

// Resize sends authoritative geometry under this controller's generation.
func (c *Controller) Resize(cols, rows, pxWidth, pxHeight uint32) error {
	return writeTyped(c.w, shimwire.TypeResize, func() ([]byte, error) {
		return shimwire.EncodeResize(shimwire.ResizeMsg{
			Generation: c.gen, Cols: cols, Rows: rows, PxWidth: pxWidth, PxHeight: pxHeight,
		})
	})
}

// Stop asks the shim to terminate and reap its harness, under this controller's
// generation.
func (c *Controller) Stop(reason shimwire.StopReason) error {
	return writeTyped(c.w, shimwire.TypeStop, func() ([]byte, error) {
		return shimwire.EncodeStop(shimwire.StopMsg{Generation: c.gen, Reason: reason})
	})
}

// Heartbeat sends liveness plus the highest sequence this controller has durably
// forwarded. That acknowledgement is what a LATER adoption resumes from, so a
// controller that never heartbeats will simply be replayed more after a restart —
// never less.
func (c *Controller) Heartbeat(ackedSeq uint64) error {
	return writeTyped(c.w, shimwire.TypeHeartbeat, func() ([]byte, error) {
		return shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{Generation: c.gen, AckedSeq: ackedSeq})
	})
}

// Close drops the controller connection WITHOUT stopping the session. This is
// what a daemon shutdown does: the shim keeps the harness and starts its bounded
// orphan clock.
func (c *Controller) Close() error {
	c.closeOne.Do(func() {
		close(c.closing)
		_ = c.conn.Close()
	})
	return nil
}

func (c *Controller) readLoop() {
	defer close(c.done)
	defer close(c.events)
	for {
		msg, err := c.r.Read()
		if err != nil {
			return
		}
		ev, ok := decodeEvent(msg)
		if !ok {
			continue
		}
		if ev.Kind == EventGap {
			// A gap that is never logged is a gap nobody can audit. §D5 makes the
			// shim declare it explicitly; the least a controller can do is not be
			// the layer that swallows it.
			c.logger.Warn("sessionshim: replay gap declared by shim",
				"session", c.id.String(), "fromSeq", ev.Gap.FromSeq, "toSeq", ev.Gap.ToSeq, "reason", ev.Gap.Reason)
		}
		select {
		case c.events <- ev:
		case <-c.closing:
			return
		}
	}
}

func decodeEvent(msg shimwire.Message) (ControllerEvent, bool) {
	switch msg.Type {
	case shimwire.TypeOutput:
		seq, rel, data, err := shimwire.DecodeOutput(msg.Body)
		if err != nil {
			return ControllerEvent{}, false
		}
		// DecodeOutput aliases the reader's per-message buffer; the event escapes
		// this goroutine, so the bytes are copied.
		out := make([]byte, len(data))
		copy(out, data)
		return ControllerEvent{Kind: EventOutput, Seq: seq, RelTime: rel, Data: out}, true
	case shimwire.TypeGap:
		g, err := shimwire.DecodeGap(msg.Body)
		if err != nil {
			return ControllerEvent{}, false
		}
		return ControllerEvent{Kind: EventGap, Gap: g}, true
	case shimwire.TypeSnapshot:
		s, err := shimwire.DecodeSnapshot(msg.Body)
		if err != nil {
			return ControllerEvent{}, false
		}
		return ControllerEvent{Kind: EventSnapshot, Snapshot: s}, true
	case shimwire.TypeExit:
		e, err := shimwire.DecodeExit(msg.Body)
		if err != nil {
			return ControllerEvent{}, false
		}
		return ControllerEvent{Kind: EventExit, Exit: e}, true
	case shimwire.TypeError:
		e, err := shimwire.DecodeError(msg.Body)
		if err != nil {
			return ControllerEvent{}, false
		}
		return ControllerEvent{Kind: EventError, Err: e}, true
	default:
		return ControllerEvent{}, false
	}
}
