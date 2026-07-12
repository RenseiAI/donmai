package ptyhost

import (
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// replyQueueDepth bounds the number of VT query replies awaiting delivery to
// the PTY master. Replies are tiny (a CPR is ~10 bytes); 64 outstanding
// replies means the child has emitted dozens of queries without reading a
// single answer — it is not consuming its own stdin.
const replyQueueDepth = 64

// replyWriter decouples VT query answers from the PTY master write so a
// child that emits terminal queries but never reads its stdin can never
// wedge the session (the responders run inside the read loop while s.mu is
// held; a blocking master write there parks the read loop against the
// kernel's bounded slave input queue and every other Session surface
// deadlocks behind it — the T10 querywedge finding).
//
// Replies are queued on a small bounded channel and written by a dedicated
// goroutine. When the queue is full — the child is not reading its own query
// answers, i.e. buggy or hostile — the reply is DROPPED (counted, logged on
// first occurrence). A real terminal whose input queue overflows loses the
// reply just the same; well-behaved TUIs read their answers promptly and
// never hit the bound.
type replyWriter struct {
	w       io.Writer
	ch      chan []byte
	done    chan struct{}
	once    sync.Once
	dropLog sync.Once
	dropped atomic.Uint64
	logger  *slog.Logger
}

func newReplyWriter(w io.Writer, logger *slog.Logger) *replyWriter {
	r := &replyWriter{
		w:      w,
		ch:     make(chan []byte, replyQueueDepth),
		done:   make(chan struct{}),
		logger: logger,
	}
	go r.pump()
	return r
}

func (r *replyWriter) pump() {
	for {
		select {
		case b := <-r.ch:
			// Errors (incl. writes against a closed master during
			// teardown) are deliberately swallowed: a lost query
			// reply is inert, exactly like the drop path above.
			_, _ = r.w.Write(b)
		case <-r.done:
			return
		}
	}
}

// Write queues one query reply. It NEVER blocks and always reports success:
// responder callers run under the session mutex inside the read loop, and no
// error they could receive would be actionable there.
func (r *replyWriter) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)
	select {
	case r.ch <- b:
	default:
		r.dropped.Add(1)
		r.dropLog.Do(func() {
			if r.logger != nil {
				r.logger.Warn("ptyhost: dropping VT query replies — child is not reading its stdin",
					"queueDepth", replyQueueDepth)
			}
		})
	}
	return len(p), nil
}

// Close stops the pump goroutine. Idempotent. Queued-but-unwritten replies
// are discarded (teardown is in progress; the child is exiting or gone).
func (r *replyWriter) Close() error {
	r.once.Do(func() { close(r.done) })
	return nil
}
