package ptyhost

import (
	"sync"

	"github.com/RenseiAI/donmai/attachwire"
)

// ring is a bounded, seq-keyed buffer of host-produced frames for the current
// (single) epoch (§13). Frames are appended in strictly increasing host seq
// order (§4); the oldest is evicted once the total buffered payload exceeds the
// byte budget. It is NOT internally locked — the Session owns it under s.mu.
type ring struct {
	maxBytes int
	curBytes int
	frames   []attachwire.Frame // contiguous, ascending Seq
}

func newRing(maxBytes int) *ring { return &ring{maxBytes: maxBytes} }

// append adds a frame (which must carry the next host seq) and evicts from the
// front until within budget. A single frame larger than the whole budget is
// still retained (it is the only frame), so a fresh subscriber can always get
// at least the latest frame.
func (r *ring) append(f attachwire.Frame) {
	r.frames = append(r.frames, f)
	r.curBytes += len(f.Payload)
	for len(r.frames) > 1 && r.curBytes > r.maxBytes {
		r.curBytes -= len(r.frames[0].Payload)
		r.frames = r.frames[1:]
	}
}

// firstSeq returns the oldest buffered host seq, or 0 when empty.
func (r *ring) firstSeq() attachwire.HostSeq {
	if len(r.frames) == 0 {
		return 0
	}
	return attachwire.HostSeq(r.frames[0].Seq)
}

// lastSeq returns the newest buffered host seq, or 0 when empty.
func (r *ring) lastSeq() attachwire.HostSeq {
	if len(r.frames) == 0 {
		return 0
	}
	return attachwire.HostSeq(r.frames[len(r.frames)-1].Seq)
}

// replayFrom returns a copy of the buffered frames with Seq > afterSeq, i.e. the
// frames a subscriber that has applied up to afterSeq still needs. The bool
// reports whether the requested position is a ring hit (afterSeq is contiguous
// with what remains buffered); false means the position was evicted (ring miss,
// §13 → the caller returns agent.ErrRingMiss).
//
//   - afterSeq == 0 → replay everything buffered (from oldest); always a hit.
//   - afterSeq >= firstSeq-1 and <= lastSeq → hit; replay (afterSeq, lastSeq].
//   - otherwise (afterSeq below the retained window, or ahead of the head) → miss.
func (r *ring) replayFrom(afterSeq attachwire.HostSeq) ([]attachwire.Frame, bool) {
	if len(r.frames) == 0 {
		// Nothing buffered yet: only "from oldest" (0) is a hit — it simply goes
		// live. A specific applied position with an empty ring is a hit only when
		// it is the (nonexistent) head; treat 0 as the always-safe case.
		return nil, afterSeq == 0
	}
	first := r.firstSeq()
	last := r.lastSeq()
	if afterSeq == 0 {
		out := make([]attachwire.Frame, len(r.frames))
		copy(out, r.frames)
		return out, true
	}
	// Hit requires the NEXT wanted frame (afterSeq+1) to be in-ring, or afterSeq
	// to be exactly the head (caught up: nothing to replay, go live).
	if afterSeq < first-1 || afterSeq > last {
		return nil, false
	}
	out := make([]attachwire.Frame, 0, len(r.frames))
	for _, f := range r.frames {
		if attachwire.HostSeq(f.Seq) > afterSeq {
			out = append(out, f)
		}
	}
	return out, true
}

// subscription is one live feed of host-produced, seq-bearing frames. Replay
// frames are queued at creation; live frames arrive via enqueue (from the
// Session emit path, under s.mu). A single pump goroutine moves queued frames to
// the public channel, so a slow consumer never blocks the emit path.
//
// "Never blocks the emit path" is the property that makes the queue UNBOUNDED,
// which is fine for a consumer that is momentarily behind and is a memory leak
// for one that has stopped. The gate is what bounds it: crossing the high-water
// mark stops the PTY reader, so the producer stops producing rather than the
// queue growing or the ring evicting. See OutputFlowControl.
type subscription struct {
	sess   *Session
	gate   *outputGate
	frames chan attachwire.Frame

	mu    sync.Mutex
	cond  *sync.Cond
	queue []attachwire.Frame
	// queued is the byte cost of everything still in queue, in the same
	// currency as the gate's marks. Frames are not uniform, so a frame count
	// could not express "as much as this session will hold".
	queued int
	// saturated records whether THIS subscription is currently counted against
	// the gate, so a crossing is reported exactly once in each direction.
	saturated bool
	// abandoned records that Close already released this subscription's whole
	// charge, so a hand-off completing concurrently must not release it twice.
	abandoned bool
	ended     bool // Exit delivered upstream: close frames after draining
	stop      bool // Close called

	stopCh     chan struct{}
	closeStop  sync.Once
	closeFrame sync.Once
}

// subscriptionFrameCost is one frame's charge against the flow-control marks.
func subscriptionFrameCost(f attachwire.Frame) int {
	return outputFrameOverheadBytes + len(f.Payload)
}

func newSubscription(sess *Session, replay []attachwire.Frame, gate *outputGate) *subscription {
	s := &subscription{
		sess:   sess,
		gate:   gate,
		frames: make(chan attachwire.Frame),
		queue:  append([]attachwire.Frame(nil), replay...),
		stopCh: make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	// No other goroutine can reach s yet, so the accounting runs without the
	// lock; a replay long enough to be born saturated must still pause the
	// reader rather than wait for the next live frame to notice.
	for _, f := range s.queue {
		s.queued += subscriptionFrameCost(f)
	}
	s.gate.account(s.queued)
	if high := s.gate.highWater(); high > 0 && s.queued > high {
		s.saturated = true
		s.gate.saturate()
	}
	go s.pump()
	return s
}

// growLocked charges a newly queued frame and reports a high-water crossing.
// s.mu is held.
func (s *subscription) growLocked(cost int) {
	s.queued += cost
	s.gate.account(cost)
	if high := s.gate.highWater(); !s.saturated && high > 0 && s.queued > high {
		s.saturated = true
		s.gate.saturate()
	}
}

// shrinkLocked credits a frame that has left the queue and reports a low-water
// crossing. s.mu is held.
func (s *subscription) shrinkLocked(cost int) {
	s.queued -= cost
	s.gate.account(-cost)
	if s.saturated && s.queued <= s.gate.lowWater() {
		s.saturated = false
		s.gate.relieve()
	}
}

// abandonQueueLocked releases everything this subscription still owes the gate.
// A closed subscription is not back-pressure — nothing is waiting for those
// bytes any more — so holding the reader for them would wedge a live harness
// behind a consumer that has already gone. s.mu is held.
func (s *subscription) abandonQueueLocked() {
	s.abandoned = true
	if s.queued > 0 {
		s.shrinkLocked(s.queued)
	}
	if s.saturated {
		s.saturated = false
		s.gate.relieve()
	}
}

// Frames returns the read-only frame channel (closed after Exit is delivered or
// after Close). Implements agent.InteractiveSubscription.
func (s *subscription) Frames() <-chan attachwire.Frame { return s.frames }

// Close releases the subscription. Idempotent.
func (s *subscription) Close() error {
	s.closeStop.Do(func() {
		s.mu.Lock()
		s.stop = true
		s.abandonQueueLocked()
		s.cond.Signal()
		s.mu.Unlock()
		close(s.stopCh)
	})
	if s.sess != nil {
		s.sess.removeSub(s)
	}
	return nil
}

// enqueue appends a live frame. Called under s.sess.mu from the emit path.
func (s *subscription) enqueue(f attachwire.Frame) {
	s.mu.Lock()
	if !s.stop && !s.ended {
		s.queue = append(s.queue, f)
		s.growLocked(subscriptionFrameCost(f))
	}
	s.cond.Signal()
	s.mu.Unlock()
}

// finish marks the stream ended (Exit delivered): the pump closes the channel
// once the queue drains.
func (s *subscription) finish() {
	s.mu.Lock()
	s.ended = true
	s.cond.Signal()
	s.mu.Unlock()
}

func (s *subscription) pump() {
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.ended && !s.stop {
			s.cond.Wait()
		}
		if s.stop {
			s.mu.Unlock()
			s.closeFrames()
			return
		}
		if len(s.queue) == 0 && s.ended {
			s.mu.Unlock()
			s.closeFrames()
			return
		}
		f := s.queue[0]
		s.queue = s.queue[1:]
		cost := subscriptionFrameCost(f)
		s.mu.Unlock()

		// The charge is released when the frame is HANDED OVER, not when it
		// leaves the slice. A pump parked on the channel is holding that frame
		// on the consumer's behalf, and crediting it early lets one in-flight
		// frame's worth of budget be counted twice — enough, with a small
		// high-water mark, to un-pause a reader whose consumer has taken
		// nothing at all.
		select {
		case s.frames <- f:
			s.mu.Lock()
			if !s.abandoned {
				s.shrinkLocked(cost)
			}
			s.mu.Unlock()
		case <-s.stopCh:
			s.closeFrames()
			return
		}
	}
}

func (s *subscription) closeFrames() {
	s.closeFrame.Do(func() { close(s.frames) })
}
