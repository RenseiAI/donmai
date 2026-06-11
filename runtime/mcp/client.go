package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/donmai/agent"
)

// ProtocolVersion is the MCP protocol revision this client negotiates in
// the initialize handshake. 2025-03-26 introduced the Streamable HTTP
// transport the platform's per-session MCP endpoint speaks; stdio servers
// on older revisions reply with the version they support and the
// JSON-RPC surface this client uses (initialize, tools/list, tools/call)
// is identical across revisions.
const ProtocolVersion = "2025-03-26"

// clientName/clientVersion identify donmai in the initialize handshake.
const (
	clientName    = "donmai"
	clientVersion = "dev"
)

// ToolDef is one tool advertised by an MCP server via tools/list.
type ToolDef struct {
	Name        string
	Description string
	// InputSchema is the tool's JSON-Schema argument shape, verbatim from
	// the server. May be nil when the server omits it.
	InputSchema json.RawMessage
}

// ToolResult is the outcome of one tools/call.
type ToolResult struct {
	// Content is the flattened text rendering of the result content
	// blocks (text blocks joined by newlines; structuredContent JSON when
	// the server returned no content blocks).
	Content string
	// IsError mirrors the MCP result's isError flag: the tool ran but
	// reported a domain failure the model can recover from. Transport /
	// protocol failures surface as Go errors instead.
	IsError bool
}

// Client is a live connection to one MCP server. Implementations are safe
// for concurrent use; Close is idempotent.
//
// This is the minimal in-box client surface providers without a native MCP
// loader (e.g. the Gemini direct harness) bridge through: list the server's
// tools at session start, then route the model's mcp__* function calls to
// CallTool.
type Client interface {
	// ListTools returns every tool the server advertises (paginating
	// through nextCursor).
	ListTools(ctx context.Context) ([]ToolDef, error)
	// CallTool invokes one tool by its server-local name.
	CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error)
	// Close terminates the session and releases the transport.
	Close() error
}

// Dial connects to one configured server, performs the initialize
// handshake, and returns a ready Client. The context bounds the dial +
// handshake only — the returned Client lives until Close.
func Dial(ctx context.Context, server agent.MCPServerConfig) (Client, error) {
	typ := server.Type
	if typ == "" {
		typ = "stdio"
	}
	switch typ {
	case "stdio":
		return dialStdio(ctx, server)
	case "http":
		return dialHTTP(ctx, server)
	default:
		return nil, fmt.Errorf("runtime/mcp: server %q has unknown type %q (want \"stdio\" or \"http\")", server.Name, typ)
	}
}

// rpcRequest is an outbound JSON-RPC 2.0 request (or notification when ID
// is nil).
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcMessage is an inbound JSON-RPC 2.0 message: a response (Result/Error
// + ID) or a server-initiated request/notification (Method set).
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// rpcCaller is the transport seam: stdio and http implement it; the shared
// session layer drives the MCP methods over it.
type rpcCaller interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
}

// session implements the protocol layer (handshake, tools/list, tools/call)
// over any rpcCaller transport.
type session struct {
	rpc rpcCaller
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// handshake performs initialize + notifications/initialized.
func (s *session) handshake(ctx context.Context) error {
	_, err := s.rpc.call(ctx, "initialize", initializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: clientName, Version: clientVersion},
	})
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := s.rpc.notify(ctx, "notifications/initialized", struct{}{}); err != nil {
		return fmt.Errorf("notifications/initialized: %w", err)
	}
	return nil
}

type listToolsResult struct {
	Tools      []toolEntry `json:"tools"`
	NextCursor string      `json:"nextCursor,omitempty"`
}

type toolEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ListTools pages through tools/list until the cursor is exhausted.
func (s *session) ListTools(ctx context.Context) ([]ToolDef, error) {
	var out []ToolDef
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := s.rpc.call(ctx, "tools/list", params)
		if err != nil {
			return nil, fmt.Errorf("tools/list: %w", err)
		}
		var page listToolsResult
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("tools/list: decode result: %w", err)
		}
		for _, t := range page.Tools {
			out = append(out, ToolDef(t))
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

type callToolResult struct {
	Content           []contentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallTool invokes tools/call and flattens the result content.
func (s *session) CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := s.rpc.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("tools/call %q: %w", name, err)
	}
	var res callToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return ToolResult{}, fmt.Errorf("tools/call %q: decode result: %w", name, err)
	}
	return ToolResult{Content: renderContent(res), IsError: res.IsError}, nil
}

// renderContent flattens a tools/call result into one text payload: text
// blocks joined by newlines, non-text blocks as typed placeholders, and the
// structuredContent JSON when no content blocks are present.
func renderContent(res callToolResult) string {
	if len(res.Content) == 0 {
		if len(res.StructuredContent) > 0 {
			return string(res.StructuredContent)
		}
		return ""
	}
	parts := make([]string, 0, len(res.Content))
	for _, c := range res.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
			continue
		}
		parts = append(parts, fmt.Sprintf("[%s content]", c.Type))
	}
	return joinNonEmpty(parts)
}

// joinNonEmpty joins parts with newlines, skipping empty strings.
func joinNonEmpty(parts []string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += p
	}
	return out
}
