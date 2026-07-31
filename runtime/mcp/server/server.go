package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/afclient/codeintel"
)

// protocolVersion is the MCP revision this server advertises when the client's
// initialize request omits one. It matches runtime/mcp.ProtocolVersion so the
// in-repo client (the conformance oracle) and this server agree by default;
// when the client sends its own protocolVersion we echo it back (the JSON-RPC
// surface — initialize, tools/list, tools/call, ping — is identical across
// revisions).
const protocolVersion = "2025-03-26"

// JSON-RPC 2.0 error codes used by the server.
const (
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	// codeServerShuttingDown is a server-defined (-32000..-32099) code returned
	// when a tools/call is aborted by context cancellation before warm-up
	// completes.
	codeServerShuttingDown = -32000
)

// toolDef is one registered MCP tool: its advertised metadata plus the invoke
// closure that decodes arguments and drives the codeintel engine.
type toolDef struct {
	name        string
	description string
	inputSchema json.RawMessage
	// invoke runs the tool. It returns a JSON-serialisable result mirroring the
	// `donmai code` CLI output, or an error surfaced to the caller as an
	// isError tool result. A panic inside invoke is recovered by safeInvoke.
	invoke func(args json.RawMessage) (any, error)
}

// Server is a long-lived code-intelligence MCP server. It holds ONE warm
// codeintel.NativeRunner (built once at init) shared across all tools/call
// requests — the Wave-1 warm-cache design. It is safe for concurrent use.
type Server struct {
	name    string
	version string
	root    string
	runner  *codeintel.NativeRunner
	logf    func(format string, args ...any)

	tools      []*toolDef          // enabled subset, canonical order
	toolByName map[string]*toolDef // enabled subset lookup

	// warmDone closes when the initial index build finishes (success or not).
	// tools/call blocks on it so a call never races an unbuilt index.
	warmDone chan struct{}
	warmErr  error
}

// New validates the config, resolves the effective index root (optionally a
// --repo-path subtree), constructs the warm NativeRunner, registers the
// enabled tool subset, and kicks off index warm-up concurrently. It returns an
// error (and does NOT start serving) when the config is invalid — the server
// fails loud at startup rather than serving a broken root.
func New(cfg Config) (*Server, error) {
	root, err := resolveIndexRoot(cfg.Root, cfg.RepoPath)
	if err != nil {
		return nil, err
	}
	enabled, err := validateTools(cfg.Tools)
	if err != nil {
		return nil, err
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	s := &Server{
		name:       ServerName,
		version:    serverVersion,
		root:       root,
		runner:     codeintel.NewNativeRunner(root),
		logf:       logf,
		warmDone:   make(chan struct{}),
		toolByName: map[string]*toolDef{},
	}
	s.registerTools(enabled)
	go s.warmUp()
	return s, nil
}

// registerTools selects the enabled subset from the full tool set, preserving
// the caller's (validated) order.
func (s *Server) registerTools(enabled []string) {
	all := s.buildTools()
	byName := make(map[string]*toolDef, len(all))
	for _, td := range all {
		byName[td.name] = td
	}
	for _, name := range enabled {
		if td, ok := byName[name]; ok {
			s.tools = append(s.tools, td)
			s.toolByName[name] = td
		}
	}
}

// warmUp builds the index once during process init and re-warms the runner's
// in-process cache. A failure is non-fatal: the first tools/call rebuilds
// lazily via the engine's cold path. All logging goes through s.logf (stderr),
// never stdout — the protocol channel must stay pure.
func (s *Server) warmUp() {
	defer close(s.warmDone)
	start := time.Now()
	s.logf("warming code index at %s", s.root)
	if err := s.runner.Refresh(); err != nil {
		s.warmErr = err
		s.logf("warm-up index build failed (will rebuild lazily on first call): %v", err)
		return
	}
	s.logf("code index warm in %s", time.Since(start).Round(time.Millisecond))
}

// WaitReady blocks until the initial index warm-up completes. It returns the
// warm-up error so repository-bearing hosts can fail acquisition rather than
// admitting a workarea whose first call would rebuild lazily. The stdio server
// intentionally keeps its existing lazy-rebuild behavior and does not call it.
func (s *Server) WaitReady(ctx context.Context) error {
	select {
	case <-s.warmDone:
		return s.warmErr
	case <-ctx.Done():
		return fmt.Errorf("wait for code index warm-up: %w", ctx.Err())
	}
}

// ── JSON-RPC framing ─────────────────────────────────────────────────────────

// rpcRequest is one inbound newline-delimited JSON-RPC message. ID is captured
// verbatim (number/string/null) and echoed back on the response; an absent ID
// marks a notification (no response).
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcError is a JSON-RPC 2.0 error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the stdio JSON-RPC loop: it reads newline-delimited requests from
// r, dispatches each concurrently (so multiple tools/call can be in flight,
// exercising the engine's RWMutex), and writes newline-framed responses to w.
// It returns nil on stdin EOF (graceful shutdown) or ctx.Err() on cancellation.
// Writes are serialised so concurrent responses never interleave; stdout
// carries ONLY JSON-RPC.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	var writeMu sync.Mutex
	writeMsg := func(v any) {
		body, err := json.Marshal(v)
		if err != nil {
			s.logf("marshal response: %v", err)
			return
		}
		body = append(body, '\n')
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := w.Write(body); err != nil {
			s.logf("write response: %v", err)
		}
	}

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		br := bufio.NewReaderSize(r, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				cp := make([]byte, len(line))
				copy(cp, line)
				select {
				case lines <- cp:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case line := <-lines:
			wg.Add(1)
			go func(l []byte) {
				defer wg.Done()
				s.handleLine(ctx, l, writeMsg)
			}(line)
		case err := <-readErr:
			wg.Wait()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read stdin: %w", err)
		}
	}
}

// handleLine parses one request line and writes its response (if any).
func (s *Server) handleLine(ctx context.Context, line []byte, write func(any)) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		// Unparsable and no recoverable id — drop it. The in-repo client never
		// emits malformed frames; this only guards against noise on the pipe.
		s.logf("drop unparsable request: %v", err)
		return
	}
	// A request without an id is a notification: act on it, never respond.
	if len(req.ID) == 0 {
		return
	}
	result, rerr := s.dispatch(ctx, req)
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	write(resp)
}

// dispatch routes one request method to its handler.
func (s *Server) dispatch(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.Params), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return s.handleToolsList(), nil
	case "tools/call":
		return s.handleToolsCall(ctx, req.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

// ── initialize ───────────────────────────────────────────────────────────────

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfoBlock    `json:"serverInfo"`
}

type serverCapabilities struct {
	// Tools is present (as an object) to advertise the tools capability. It is
	// intentionally empty: this server does not emit tools/list_changed.
	Tools map[string]any `json:"tools"`
}

type serverInfoBlock struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (s *Server) handleInitialize(params json.RawMessage) initializeResult {
	pv := protocolVersion
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
			pv = p.ProtocolVersion
		}
	}
	return initializeResult{
		ProtocolVersion: pv,
		Capabilities:    serverCapabilities{Tools: map[string]any{}},
		ServerInfo:      serverInfoBlock{Name: s.name, Version: s.version},
	}
}

// ── tools/list ───────────────────────────────────────────────────────────────

type toolsListResult struct {
	Tools []toolListEntry `json:"tools"`
}

type toolListEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

func (s *Server) handleToolsList() toolsListResult {
	entries := make([]toolListEntry, 0, len(s.tools))
	for _, td := range s.tools {
		entries = append(entries, toolListEntry{
			Name:        td.name,
			Description: td.description,
			InputSchema: td.inputSchema,
		})
	}
	return toolsListResult{Tools: entries}
}

// ── tools/call ───────────────────────────────────────────────────────────────

// ToolResult is the frozen v0.1.0 MCP operation-result shape. It is exported
// so alternate transports can reuse the same dispatcher without re-encoding or
// drifting from the stdio contract.
type ToolResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ContentItem is one text item in a ToolResult. v0.1.0 permits text only.
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ErrUnknownTool reports a tool name outside the frozen enabled profile.
var ErrUnknownTool = errors.New("unknown or disabled tool")

func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid tools/call params: " + err.Error()}
		}
	}
	if p.Name == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call requires a tool name"}
	}
	res, rerr := s.callTool(ctx, p.Name, p.Arguments)
	if rerr != nil {
		return nil, rerr
	}
	return res, nil
}

// Call resolves a tool from the frozen enabled profile, blocks until warm-up
// completes, and invokes it under panic recovery. Recognized operation failures
// are returned as ToolResult{IsError:true}; protocol selection and cancellation
// remain Go errors for the transport to classify.
func (s *Server) Call(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	td, ok := s.toolByName[name]
	if !ok {
		return ToolResult{}, fmt.Errorf("%w: %s", ErrUnknownTool, name)
	}

	select {
	case <-s.warmDone:
	case <-ctx.Done():
		return ToolResult{}, fmt.Errorf("wait for code index warm-up: %w", ctx.Err())
	}

	out, err := safeInvoke(td, args)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	text, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return toolErrorResult("encode result: " + err.Error()), nil
	}
	return ToolResult{Content: []ContentItem{{Type: "text", Text: string(text)}}}, nil
}

// callTool preserves the stdio JSON-RPC distinction: an unknown tool is an
// invalid-params protocol error, cancellation is a server-shutdown error, and
// recognized operation failures stay normal isError results.
func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (ToolResult, *rpcError) {
	res, err := s.Call(ctx, name, args)
	if err == nil {
		return res, nil
	}
	if errors.Is(err, ErrUnknownTool) {
		return ToolResult{}, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ToolResult{}, &rpcError{Code: codeServerShuttingDown, Message: "server shutting down before warm-up completed"}
	}
	return ToolResult{}, &rpcError{Code: codeInternalError, Message: err.Error()}
}

// safeInvoke runs a tool's invoke closure, converting a panic into an error so
// a single bad tool call never brings the server process down.
func safeInvoke(td *toolDef, args json.RawMessage) (out any, err error) {
	defer func() {
		if r := recover(); r != nil {
			out = nil
			err = fmt.Errorf("tool %s panic: %v", td.name, r)
		}
	}()
	return td.invoke(args)
}

// toolErrorResult wraps a domain failure as an MCP isError content result.
func toolErrorResult(msg string) ToolResult {
	return ToolResult{
		Content: []ContentItem{{Type: "text", Text: msg}},
		IsError: true,
	}
}

// decodeArgs unmarshals a tool's arguments into v. Empty arguments leave v at
// its zero value (all tool fields are optional at the JSON layer; required
// semantics are enforced by the engine, surfaced as isError results).
func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
