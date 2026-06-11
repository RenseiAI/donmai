package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/mcp"
)

// mcpConnectTimeout bounds the Spawn-time connect + tools/list handshake
// across all declared MCP servers. Connection failures degrade the session
// (catch-all declaration + structured routing errors), never fail Spawn.
const mcpConnectTimeout = 15 * time.Second

// mcpCallTimeout bounds a single bridged tools/call so a hung server cannot
// stall the conversation driver forever.
const mcpCallTimeout = 5 * time.Minute

// mcpDialer abstracts runtime/mcp.Dial so tests inject fakes. The default
// (set in New) dials the real stdio/http transports.
type mcpDialer func(ctx context.Context, server agent.MCPServerConfig) (mcp.Client, error)

// mcpRoute binds one declared Gemini function name to a live MCP client +
// the server-local tool name.
type mcpRoute struct {
	client mcp.Client
	tool   string
}

// mcpBridge is the in-box MCP client bridge for the Gemini native runner.
//
// Gemini's REST endpoint has no MCP loader of its own (unlike Claude's
// --mcp-config or Codex's app-server), so the provider bridges: at Spawn it
// dials every Spec.MCPServers entry via runtime/mcp, discovers the server's
// tools, declares them to the model as mcp__<server>__<tool> function
// declarations, and routes the resulting mcp__* functionCalls to the live
// server (toolExecutor → call). This is what makes
// Capabilities.AcceptsMcpServerSpec=true honest end-to-end.
//
// Everything is best-effort: a server that fails to connect or list keeps
// its catch-all declaration and resolves calls to a structured error the
// model can recover from.
type mcpBridge struct {
	routes   map[string]mcpRoute              // declared function name → live route
	clients  map[string]mcp.Client            // server name → client (name-parse fallback routing)
	failures map[string]error                 // server name → connect/discover error
	decls    map[string][]functionDeclaration // server name → discovered declarations, in tools/list order
}

// newMCPBridge dials the declared servers and discovers their tool
// surfaces. Returns nil when no servers are declared. Never returns an
// error: per-server failures are recorded on the bridge and surface as
// structured tool errors at call time.
func newMCPBridge(ctx context.Context, servers []agent.MCPServerConfig, dial mcpDialer) *mcpBridge {
	if len(servers) == 0 || dial == nil {
		return nil
	}
	b := &mcpBridge{
		routes:   make(map[string]mcpRoute),
		clients:  make(map[string]mcp.Client),
		failures: make(map[string]error),
		decls:    make(map[string][]functionDeclaration),
	}
	for _, s := range servers {
		if s.Name == "" {
			continue
		}
		client, err := dial(ctx, s)
		if err != nil {
			b.failures[s.Name] = err
			continue
		}
		tools, err := client.ListTools(ctx)
		if err != nil {
			b.failures[s.Name] = err
			_ = client.Close()
			continue
		}
		b.clients[s.Name] = client
		decls := make([]functionDeclaration, 0, len(tools))
		for _, t := range tools {
			fname := sanitizeToolName("mcp__" + s.Name + "__" + t.Name)
			if fname == "" {
				continue
			}
			if _, dup := b.routes[fname]; dup {
				continue
			}
			b.routes[fname] = mcpRoute{client: client, tool: t.Name}
			desc := t.Description
			if desc == "" {
				desc = "MCP tool on server " + s.Name
			}
			decls = append(decls, functionDeclaration{
				Name:        fname,
				Description: desc,
				Parameters:  geminiParametersFromMCPSchema(t.InputSchema),
			})
		}
		b.decls[s.Name] = decls
	}
	return b
}

// amendPlan swaps each connected server's catch-all declaration (built by
// toolsFromSpec before the bridge dialed) for the real, discovered per-tool
// declarations. Failed servers keep their catch-all so the model still sees
// the declared surface and receives a structured routing error on use.
// Discovered names that already exist in the plan (e.g. asserted via
// Spec.MCPToolNames) are not re-declared — the route still serves them.
func (b *mcpBridge) amendPlan(plan *spawnPlan) {
	if b == nil || len(b.decls) == 0 || len(plan.tools) == 0 {
		return
	}
	existing := make(map[string]struct{})
	for _, d := range plan.tools[0].FunctionDeclarations {
		existing[d.Name] = struct{}{}
	}
	replaced := make(map[string][]functionDeclaration, len(b.decls))
	for serverName, decls := range b.decls {
		replaced[sanitizeToolName("mcp__"+serverName)] = decls
	}

	out := make([]functionDeclaration, 0, len(plan.tools[0].FunctionDeclarations))
	for _, d := range plan.tools[0].FunctionDeclarations {
		discovered, ok := replaced[d.Name]
		if !ok {
			out = append(out, d)
			continue
		}
		for _, rd := range discovered {
			if _, dup := existing[rd.Name]; dup {
				continue
			}
			existing[rd.Name] = struct{}{}
			out = append(out, rd)
		}
	}
	plan.tools[0].FunctionDeclarations = out
}

// call routes one mcp__* functionCall to its live server. Discovered names
// hit their exact route; other names (e.g. platform-asserted MCPToolNames)
// are parsed as mcp__<server>__<tool> and routed by server name.
func (b *mcpBridge) call(ctx context.Context, name string, args map[string]any) (mcp.ToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()

	if r, ok := b.routes[name]; ok {
		return r.client.CallTool(ctx, r.tool, args)
	}
	rest := strings.TrimPrefix(name, "mcp__")
	serverName, tool, ok := strings.Cut(rest, "__")
	if !ok || tool == "" || serverName == "" {
		return mcp.ToolResult{}, fmt.Errorf("cannot route %q (want mcp__<server>__<tool>)", name)
	}
	if client, live := b.clients[serverName]; live {
		return client.CallTool(ctx, tool, args)
	}
	if err, failed := b.failures[serverName]; failed {
		return mcp.ToolResult{}, fmt.Errorf("server %q is unavailable: %w", serverName, err)
	}
	return mcp.ToolResult{}, fmt.Errorf("no declared MCP server %q", serverName)
}

// Close releases every live server connection. nil-safe and idempotent
// (underlying clients are idempotent on Close).
func (b *mcpBridge) Close() {
	if b == nil {
		return
	}
	for _, c := range b.clients {
		_ = c.Close()
	}
}

// geminiSchemaAllowedKeys is the OpenAPI-subset key whitelist the Gemini
// v1beta Schema object accepts. Unknown keys (e.g. $schema,
// additionalProperties, oneOf) make generateContent reject the whole
// request with HTTP 400, so everything else is stripped recursively.
var geminiSchemaAllowedKeys = map[string]struct{}{
	"type": {}, "format": {}, "title": {}, "description": {}, "nullable": {},
	"default": {}, "enum": {}, "items": {}, "properties": {}, "required": {},
	"minimum": {}, "maximum": {}, "minLength": {}, "maxLength": {},
	"pattern": {}, "minItems": {}, "maxItems": {}, "anyOf": {},
	"minProperties": {}, "maxProperties": {},
}

// geminiParametersFromMCPSchema converts an MCP tool's JSON-Schema input
// into a Gemini-safe parameters object. Anything unconvertible falls back
// to the permissive default schema so a tool is never lost to a schema
// quirk (the description still guides the model's arguments).
func geminiParametersFromMCPSchema(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return defaultToolParameters()
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return defaultToolParameters()
	}
	out := sanitizeSchemaObject(m)
	if len(out) == 0 {
		return defaultToolParameters()
	}
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	if t, _ := out["type"].(string); strings.EqualFold(t, "object") {
		if _, ok := out["properties"]; !ok {
			out["properties"] = map[string]any{}
		}
	}
	return out
}

// sanitizeSchemaObject recursively strips non-whitelisted keys from one
// schema object, descending into properties / items / anyOf.
func sanitizeSchemaObject(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if _, ok := geminiSchemaAllowedKeys[k]; !ok {
			continue
		}
		switch k {
		case "properties":
			props, ok := v.(map[string]any)
			if !ok {
				continue
			}
			cleaned := make(map[string]any, len(props))
			for name, sub := range props {
				if subm, ok := sub.(map[string]any); ok {
					cleaned[name] = sanitizeSchemaObject(subm)
				}
			}
			out[k] = cleaned
		case "items":
			if subm, ok := v.(map[string]any); ok {
				out[k] = sanitizeSchemaObject(subm)
			}
		case "anyOf":
			arr, ok := v.([]any)
			if !ok {
				continue
			}
			cleaned := make([]any, 0, len(arr))
			for _, sub := range arr {
				if subm, ok := sub.(map[string]any); ok {
					cleaned = append(cleaned, sanitizeSchemaObject(subm))
				}
			}
			if len(cleaned) > 0 {
				out[k] = cleaned
			}
		default:
			out[k] = v
		}
	}
	return out
}
