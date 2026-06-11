package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// stdioCloseGrace is how long Close waits for the server to exit on stdin
// EOF before killing it.
const stdioCloseGrace = 3 * time.Second

// stdioClient is the Client implementation over a spawned subprocess
// speaking newline-delimited JSON-RPC on stdin/stdout (the MCP stdio
// transport).
type stdioClient struct {
	session
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	conn      *stdioConn
	closeOnce sync.Once
}

// dialStdio spawns the server process, starts the demux read loop, and
// performs the handshake. The process environment is the donmai process env
// extended with the server's Env entries.
func dialStdio(ctx context.Context, server agent.MCPServerConfig) (Client, error) {
	if server.Command == "" {
		return nil, fmt.Errorf("runtime/mcp: server %q (stdio) has empty Command", server.Name)
	}

	cmd := exec.Command(server.Command, server.Args...) //nolint:gosec // command comes from the session's MCP config, the same trust domain as the agent itself
	cmd.Env = composeServerEnv(server.Env)
	// MCP stdio servers log to stderr by convention; it is not part of the
	// protocol stream and is dropped here.
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("runtime/mcp: server %q: stdin pipe: %w", server.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("runtime/mcp: server %q: stdout pipe: %w", server.Name, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("runtime/mcp: server %q: start: %w", server.Name, err)
	}

	conn := &stdioConn{
		stdin:   stdin,
		pending: make(map[int64]chan rpcMessage),
		done:    make(chan struct{}),
	}
	go conn.readLoop(stdout)

	c := &stdioClient{
		session: session{rpc: conn},
		cmd:     cmd,
		stdin:   stdin,
		conn:    conn,
	}
	if err := c.handshake(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("runtime/mcp: server %q: %w", server.Name, err)
	}
	return c, nil
}

// Close signals EOF on stdin (stdio servers exit on it per the spec), then
// reaps the process — killing it after a grace period if it lingers.
// Idempotent; always returns nil (teardown is best-effort).
func (c *stdioClient) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		waited := make(chan struct{})
		go func() {
			_ = c.cmd.Wait()
			close(waited)
		}()
		select {
		case <-waited:
		case <-time.After(stdioCloseGrace):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-waited
		}
	})
	return nil
}

// composeServerEnv merges the server's Env entries onto the process
// environment (server entries win).
func composeServerEnv(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// stdioConn is the framing + demux layer: one writer mutex, a background
// read loop matching responses to pending calls by id.
type stdioConn struct {
	writeMu sync.Mutex
	stdin   io.Writer
	nextID  atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan rpcMessage

	// done closes when the read loop exits (server closed stdout / died);
	// pending calls unblock with an error.
	done chan struct{}
}

// readLoop drains stdout line-by-line, routing responses to their pending
// call. Server-initiated requests and notifications are ignored: this
// minimal client advertises no capabilities (no roots, no sampling) a
// conforming server could invoke.
func (c *stdioConn) readLoop(r io.Reader) {
	defer close(c.done)
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var msg rpcMessage
			if jerr := json.Unmarshal(line, &msg); jerr == nil && msg.ID != nil && msg.Method == "" {
				c.pendingMu.Lock()
				ch := c.pending[*msg.ID]
				delete(c.pending, *msg.ID)
				c.pendingMu.Unlock()
				if ch != nil {
					ch <- msg
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (c *stdioConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcMessage, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	if err := c.write(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("server closed the stream before responding to %s", method)
	}
}

func (c *stdioConn) notify(_ context.Context, method string, params any) error {
	return c.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

// write marshals one message and writes it as a single newline-terminated
// frame.
func (c *stdioConn) write(req rpcRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", req.Method, err)
	}
	body = append(body, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(body); err != nil {
		return fmt.Errorf("write %s: %w", req.Method, err)
	}
	return nil
}
