package pi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// rawEvent is one decoded JSONL line from pi's event stream. Type is the
// discriminant pi stamps on every event ("agent_start", "message_update",
// "tool_execution_start", "extension_ui_request", …); Fields is the decoded
// object so the mapper/policy can read arbitrary payload keys; Line is the
// original bytes for the agent.Event Raw passthrough and for tests.
type rawEvent struct {
	Type   string
	Fields map[string]any
	Line   []byte
}

// eventType names the discriminant field pi stamps on every JSONL event.
// Recorded as a constant so a future wire-shape correction (once verified
// against a real binary — see doc.go) touches one place.
const eventType = "type"

// rpcClient is the JSONL-over-stdio transport for one pi child. Commands are
// written LF-framed to the child's stdin; events are read LF-delimited from
// its stdout and published on Events().
//
// LF-only framing (design §1): pi requires newline-delimited JSON where the
// delimiter is U+000A ONLY. readline-style splitters that ALSO split on the
// Unicode line separators U+2028/U+2029 are non-compliant (they would sever a
// JSON string value that legitimately contains those runes). Go's
// bufio.Scanner with ScanLines splits on "\n" only, so we are compliant by
// construction — rpc_test.go's TestLFFramingOnly pins this so a future
// refactor to a splitter that also breaks on U+2028/U+2029 fails loudly.
type rpcClient struct {
	w io.Writer
	r io.Reader

	writeMu sync.Mutex

	events chan rawEvent

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

// newRPCClient wires a client over the given stdin writer / stdout reader and
// starts the read loop. The caller owns the underlying pipes' lifetimes.
func newRPCClient(w io.Writer, r io.Reader) *rpcClient {
	c := &rpcClient{
		w:      w,
		r:      r,
		events: make(chan rawEvent, 256),
		closed: make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Events returns the read-only channel of decoded pi events. It is closed
// once the read loop exits (EOF, read error, or Stop).
func (c *rpcClient) Events() <-chan rawEvent { return c.events }

// WriteCommand marshals cmd to JSON and writes it LF-framed to the child's
// stdin. Safe for concurrent callers (the write is mutex-guarded). Returns an
// error if the client is already closed or the write fails.
func (c *rpcClient) WriteCommand(cmd map[string]any) error {
	select {
	case <-c.closed:
		return fmt.Errorf("pi rpc: client closed")
	default:
	}
	b, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("pi rpc: marshal command: %w", err)
	}
	// A command must never itself contain an embedded newline (that would
	// desync the framing on the child side); json.Marshal escapes "\n"
	// inside string values as "\\n", so the only "\n" in b is impossible —
	// but assert it cheaply so a future custom encoder cannot regress the
	// invariant silently.
	for _, ch := range b {
		if ch == '\n' {
			return fmt.Errorf("pi rpc: marshaled command contains a raw newline (framing violation)")
		}
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("pi rpc: write command: %w", err)
	}
	return nil
}

// readLoop consumes LF-delimited JSONL from the child's stdout, decodes each
// line into a rawEvent, and publishes it. Exits on EOF / read error / Stop.
func (c *rpcClient) readLoop() {
	sc := bufio.NewScanner(c.r)
	// pi turns can carry large tool outputs on a single line; raise the
	// per-line cap well above bufio's 64KiB default (10 MiB).
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	sc.Split(bufio.ScanLines)

	var loopErr error
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		// Copy: Scanner reuses its buffer across Scan() calls.
		buf := make([]byte, len(line))
		copy(buf, line)

		var fields map[string]any
		if err := json.Unmarshal(buf, &fields); err != nil {
			// A malformed line is observability noise, not a fatal error;
			// surface it as a typeless event so the pump can log it without
			// tearing the session down.
			c.publish(rawEvent{Type: "", Fields: nil, Line: buf})
			continue
		}
		typ, _ := fields[eventType].(string)
		c.publish(rawEvent{Type: typ, Fields: fields, Line: buf})
	}
	if err := sc.Err(); err != nil {
		loopErr = err
	}
	c.Stop(loopErr)
}

// publish sends ev unless the client is already closing.
func (c *rpcClient) publish(ev rawEvent) {
	select {
	case c.events <- ev:
	case <-c.closed:
	}
}

// Stop closes the client exactly once, records the cause, fires onClose, and
// closes the events channel. Idempotent.
func (c *rpcClient) Stop(cause error) {
	c.closeOnce.Do(func() {
		c.closeErr = cause
		close(c.closed)
		close(c.events)
	})
}

// CloseErr returns the recorded close cause (nil if still open or closed
// cleanly at EOF).
func (c *rpcClient) CloseErr() error {
	select {
	case <-c.closed:
		return c.closeErr
	default:
		return nil
	}
}
