package muse

import "sync"

// boundedBuffer accumulates the last `limit` bytes written, dropping
// the oldest data once that limit is reached. Goroutine-safe. Borrowed
// from clijsonl's stderr ring (each harness keeps its own copy per
// this repo's convention): draining serve stderr into a sink keeps the
// child from blocking on a full pipe, and Excerpt surfaces the crash
// tail for post-mortem diagnostics.
type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit, buf: make([]byte, 0, limit)}
}

// Write implements io.Writer. Always returns len(p), nil — drops
// oldest bytes when the buffer would exceed limit rather than blocking
// or erroring.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return len(p), nil
	}
	overflow := (len(b.buf) + len(p)) - b.limit
	if overflow > 0 {
		copy(b.buf, b.buf[overflow:])
		b.buf = b.buf[:len(b.buf)-overflow]
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

// String returns a snapshot of the current buffer contents. Tests use
// it to assert on raw retained bytes without redaction.
func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Excerpt returns a bounded tail of everything captured so far — safe
// to place in a returned error. A nil buffer returns "".
func (b *boundedBuffer) Excerpt() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) == 0 {
		return ""
	}
	const maxExcerpt = 2048
	if len(b.buf) <= maxExcerpt {
		return string(b.buf)
	}
	return string(b.buf[len(b.buf)-maxExcerpt:])
}
