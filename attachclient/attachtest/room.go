package attachtest

import (
	"context"
	"sync"

	"github.com/RenseiAI/donmai/attachwire"
)

// room is the single-room state of the stub relay: a bounded seq-keyed ring of
// host frames, the host-leg binding (epoch CAS + jti carrier-switch), the
// trivial single-driver pen, and a broadcast channel that wakes viewer
// goroutines on every change. It is mechanism only — the real relay's policy
// lives in the platform.
type room struct {
	mu        sync.Mutex
	broadcast chan struct{}

	ringMax  int
	ring     []attachwire.Frame // seq-bearing host frames, ascending
	ringBase uint64             // seq of ring[0]; 0 when empty
	head     uint64             // highest seq appended; 0 when empty

	epoch    int64
	epochSet bool

	hostBound  bool
	hostJti    string
	hostCancel context.CancelFunc
	hostOut    chan attachwire.Frame // relay→host delivery sink of the bound leg

	ended   bool
	exitSeq uint64

	finalSnapshot   *attachwire.Frame // post-Exit Snapshot (header seq 0)
	lastSnapshotSeq uint64            // seq of the most recent seq-bearing Snapshot

	// pen — trivial single-driver policy (§ 11.1 minimum): the first driver-role
	// connection to join holds the pen forever. Never the platform arbitration.
	penHolder string
	penUser   string
	penGen    int64
	penSet    bool

	hostAckSeq int64            // highest contiguous host seq applied (degraded lane)
	batchSeen  map[string]int64 // batchId → ack (§ 14 idempotency)

	members map[string]member
}

type member struct {
	userID  string
	connID  string
	role    string
	driving bool
}

type bindResult int

const (
	bindOK bindResult = iota
	bindStale
)

func newRoom(ringMax int) *room {
	if ringMax <= 0 {
		ringMax = 256
	}
	return &room{
		broadcast: make(chan struct{}),
		ringMax:   ringMax,
		batchSeen: make(map[string]int64),
		members:   make(map[string]member),
	}
}

func (r *room) signalLocked() {
	close(r.broadcast)
	r.broadcast = make(chan struct{})
}

// waitLocked returns the current broadcast channel; the caller unlocks then
// selects on it (and ctx) to await the next change.
func (r *room) waitLocked() chan struct{} { return r.broadcast }

// bindHost applies the § 6.2 (sessionId, epoch) CAS with a jti carrier-switch
// extension (§ 15: one live logical connection per jti). Returns bindStale for a
// zombie/duplicate (a different jti with epoch ≤ current while a leg is bound).
func (r *room) bindHost(epoch int64, jti string, out chan attachwire.Frame, cancel context.CancelFunc) bindResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.hostBound {
		switch {
		case r.hostJti == jti:
			// Same logical connection re-dialing or switching carriers → take over
			// (keep generation, ring, hostAckSeq). Cancel the stale carrier.
			if r.hostCancel != nil {
				r.hostCancel()
			}
			r.hostCancel = cancel
			r.hostOut = out
			r.signalLocked()
			return bindOK
		case epoch > r.epoch:
			if r.hostCancel != nil {
				r.hostCancel()
			}
			r.startGenerationLocked(epoch)
			r.hostBound = true
			r.hostJti = jti
			r.hostCancel = cancel
			r.hostOut = out
			return bindOK
		default:
			return bindStale // epoch ≤ current, different jti → zombie
		}
	}

	// No live host leg bound.
	if r.epochSet && epoch < r.epoch {
		return bindStale
	}
	if !r.epochSet || epoch > r.epoch {
		r.startGenerationLocked(epoch)
	}
	// epoch == current & unbound → resume the same generation (keep the ring).
	r.hostBound = true
	r.hostJti = jti
	r.hostCancel = cancel
	r.hostOut = out
	r.signalLocked()
	return bindOK
}

// startGenerationLocked begins a new room generation (§ 4.1): discard the ring,
// reset the sequence base, clear ended/snapshot/ack state.
func (r *room) startGenerationLocked(epoch int64) {
	r.epoch = epoch
	r.epochSet = true
	r.ring = nil
	r.ringBase = 0
	r.head = 0
	r.ended = false
	r.exitSeq = 0
	r.finalSnapshot = nil
	r.lastSnapshotSeq = 0
	r.hostAckSeq = 0
	r.signalLocked()
}

// wipe resets ALL in-memory room state — ring, epoch/host binding, degraded-
// lane ack, pen, presence — to a blank room, exactly as if the relay process
// serving this room had just restarted (§13: "the ring and pen state are
// relay-local; after a relay restart every viewer resume is a ring miss").
// The room keeps its identity (same *room pointer, same RoomID/route) so a
// caller's already-issued AttachURL keeps working — only the state a real
// process restart would drop is cleared. It returns the hostCancel func of
// whatever host leg was bound at the moment of the wipe (nil if none); the
// caller invokes it AFTER unlocking (never under r.mu) to force that leg to
// observe the drop and reconnect into the now-blank room, mirroring how
// bindHost already cancels a superseded leg.
func (r *room) wipe() (hostCancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hostCancel = r.hostCancel

	r.ring = nil
	r.ringBase = 0
	r.head = 0

	r.epoch = 0
	r.epochSet = false

	r.hostBound = false
	r.hostJti = ""
	r.hostCancel = nil
	r.hostOut = nil

	r.ended = false
	r.exitSeq = 0

	r.finalSnapshot = nil
	r.lastSnapshotSeq = 0

	r.hostAckSeq = 0
	r.batchSeen = make(map[string]int64)

	r.penHolder = ""
	r.penUser = ""
	r.penGen = 0
	r.penSet = false

	r.members = make(map[string]member)

	r.signalLocked()
	return hostCancel
}

// unbindHost releases the host binding if leg (identified by out) is still the
// bound one (a superseded leg must not unbind its successor).
func (r *room) unbindHost(out chan attachwire.Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hostOut == out {
		r.hostBound = false
		r.hostOut = nil
		r.hostCancel = nil
		r.signalLocked()
	}
}

func (r *room) sendToHost(f attachwire.Frame) {
	r.mu.Lock()
	out := r.hostOut
	r.mu.Unlock()
	if out == nil {
		return
	}
	select {
	case out <- f:
	default:
		// Stub host sink full: drop. Tests run at low volume; a production relay
		// would apply backpressure here.
	}
}

// appendHostFrame appends one host-produced frame, fanning out to viewers.
func (r *room) appendHostFrame(f attachwire.Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendHostFrameLocked(f)
}

func (r *room) appendHostFrameLocked(f attachwire.Frame) {
	// Post-Exit Snapshot (out-of-namespace, header seq 0) is not a ring frame.
	if f.Type == attachwire.TypeSnapshot && f.Seq == 0 {
		ff := f
		r.finalSnapshot = &ff
		r.signalLocked()
		return
	}
	if f.Seq == 0 {
		// Any other out-of-namespace host frame is ignored by the ring.
		return
	}
	if r.ringBase == 0 {
		r.ringBase = f.Seq
	}
	r.ring = append(r.ring, f)
	r.head = f.Seq
	switch f.Type {
	case attachwire.TypeSnapshot:
		r.lastSnapshotSeq = f.Seq
	case attachwire.TypeExit:
		r.ended = true
		r.exitSeq = f.Seq
	}
	if r.ringMax > 0 && len(r.ring) > r.ringMax {
		drop := len(r.ring) - r.ringMax
		r.ring = r.ring[drop:]
		r.ringBase = r.ring[0].Seq
	}
	r.signalLocked()
}

// ringHasLocked reports whether seq is currently buffered.
func (r *room) ringHasLocked(seq uint64) bool {
	return seq > 0 && r.ringBase > 0 && seq >= r.ringBase && seq <= r.head
}

// framesFromLocked returns a copy of buffered frames with seq >= from.
func (r *room) framesFromLocked(from uint64) []attachwire.Frame {
	var out []attachwire.Frame
	for _, f := range r.ring {
		if f.Seq >= from {
			out = append(out, f)
		}
	}
	return out
}

func (r *room) ringFrameLocked(seq uint64) (attachwire.Frame, bool) {
	for _, f := range r.ring {
		if f.Seq == seq {
			return f, true
		}
	}
	return attachwire.Frame{}, false
}

// assignPenIfDriver gives the pen to connID if it is driver-capable and no pen
// is held (trivial first-driver-forever policy). Returns whether a change
// happened.
func (r *room) assignPenIfDriver(connID, userID, role string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if role != string(attachwire.RoleDriver) {
		return false
	}
	if r.penSet {
		return false
	}
	r.penHolder = connID
	r.penUser = userID
	r.penGen++
	r.penSet = true
	if m, ok := r.members[connID]; ok {
		m.driving = true
		r.members[connID] = m
	}
	r.signalLocked()
	return true
}

func (r *room) join(m member) {
	r.mu.Lock()
	r.members[m.connID] = m
	r.mu.Unlock()
}

func (r *room) leave(connID string) {
	r.mu.Lock()
	delete(r.members, connID)
	if r.penHolder == connID {
		r.penHolder = ""
		r.penUser = ""
		r.penSet = false
		r.penGen++
	}
	r.signalLocked()
	r.mu.Unlock()
}

func (r *room) penSnapshot() (holderUser, holderConn string, gen int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.penUser, r.penHolder, r.penGen
}

func (r *room) isPenHolder(connID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.penSet && r.penHolder == connID
}
