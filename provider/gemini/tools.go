package gemini

import (
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// ToolPermissionFormatGemini is the ToolPermissionFormat this provider
// advertises. Gemini has no Claude-style permission grammar — tool
// gating happens via toolConfig.functionCallingConfig.mode, not a
// pattern allow/deny list. The "gemini" sentinel keeps callers that
// switch on ToolPermissionFormat from defaulting to the Claude grammar.
const ToolPermissionFormatGemini = "gemini"

// Function-calling modes for toolConfig.functionCallingConfig.mode.
const (
	functionCallingModeAuto = "AUTO"
	functionCallingModeAny  = "ANY"
	functionCallingModeNone = "NONE"
)

// toolsFromSpec builds the request body's tools array from BOTH
// Spec.AllowedTools and Spec.MCPServers. Every native allowed tool and
// every declared MCP tool/server becomes one functionDeclaration so the
// model can call it.
//
// Native tools (Bash/Read/Edit/Write derived from AllowedTools) ARE
// executed end-to-end by the session-local executor (handle.go ->
// executor.go). MCP entries are surfaced for forward-compatibility, but
// there is NO in-box MCP client: an mcp__* functionCall resolves to a
// structured "not executable" error rather than being routed to a live
// server. Capabilities.AcceptsMcpServerSpec is false to reflect this.
//
// Returns nil when the session declares no tools (no AllowedTools, no
// MCPToolNames, no MCPServers) so the request omits the tools field and
// the model behaves as a plain text generator.
//
// All functionDeclarations are collected into a single tools[] entry —
// Gemini merges declarations across array entries, but one entry keeps
// the wire payload compact and the ordering deterministic.
func toolsFromSpec(spec agent.Spec) []requestTool {
	seen := make(map[string]struct{})
	decls := make([]functionDeclaration, 0, len(spec.AllowedTools)+len(spec.MCPToolNames))

	add := func(name, desc string) {
		name = sanitizeToolName(name)
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		decls = append(decls, functionDeclaration{
			Name:        name,
			Description: desc,
			Parameters:  defaultToolParameters(),
		})
	}

	// Native allow-listed tools (Claude permission-pattern strings like
	// "Bash(git:*)" or bare "Edit"). The leading verb is the tool name.
	for _, t := range spec.AllowedTools {
		add(toolNameFromPattern(t), "Allow-listed tool: "+t)
	}

	// Explicit MCP tool names already fully-qualified
	// (e.g. "mcp__af-code-intelligence__af_code_get_repo_map").
	for _, t := range spec.MCPToolNames {
		add(t, "MCP tool (bridged): "+t)
	}

	// MCP servers: expose one catch-all declaration per server keyed by
	// the server name (we do not have the server's tool manifest at Spawn
	// time). This is forward-compat only — there is no in-box MCP client,
	// so a resulting mcp__* functionCall is NOT routed to a live server;
	// the session-local executor returns a structured "not executable"
	// error (Capabilities.AcceptsMcpServerSpec is false).
	for _, s := range spec.MCPServers {
		if s.Name == "" {
			continue
		}
		add("mcp__"+s.Name, "MCP server (declared; not executable in the native runner): "+s.Name)
	}

	if len(decls) == 0 {
		return nil
	}
	return []requestTool{{FunctionDeclarations: decls}}
}

// functionCallingMode selects the toolConfig mode. Autonomous sessions
// use AUTO (let the model decide when to call tools); the field exists
// so future policies (ANY/NONE) can be wired without a schema change.
func functionCallingMode(_ agent.Spec) string {
	return functionCallingModeAuto
}

// toolNameFromPattern extracts the tool name from a Claude
// permission-pattern string. "Bash(git:*)" → "Bash"; "Edit" → "Edit".
func toolNameFromPattern(pattern string) string {
	if i := strings.IndexByte(pattern, '('); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

// sanitizeToolName makes a tool name safe for a Gemini functionCall
// name. Gemini function names must match [a-zA-Z0-9_.-]; any other byte
// is rewritten to '_'. Empty input returns "".
func sanitizeToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// defaultToolParameters returns the permissive OpenAPI-subset schema
// used when the runner has not supplied a per-tool schema. Gemini
// requires a parameters object; an open object lets the model pass any
// args, which the session-local executor reads (command/path/old_string/
// new_string/content) when it runs the native tool.
func defaultToolParameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

// is3xModel reports whether the model id belongs to the Gemini 3.x
// family (gemini-3.5-flash, gemini-3.1-pro-preview, gemini-3.1-flash-lite,
// …). The 3.x family uses thinkingConfig.thinkingLevel; the 2.5 family
// uses thinkingConfig.thinkingBudget.
func is3xModel(model string) bool {
	return strings.HasPrefix(model, "gemini-3")
}

// thinkingConfigFor maps Spec.Effort onto the model family's thinking
// knob. Returns nil when no effort is requested.
//
//   - 3.x models  → thinkingLevel: low → "low", medium → "medium",
//     high/xhigh → "high". (3.x has no off switch; minimal is the floor.)
//   - 2.5 models  → thinkingBudget: low → 2048, medium → 8192,
//     high/xhigh → 24576 tokens. (-1 dynamic, 0 off are also valid but we
//     map effort tiers to concrete budgets.)
//
// ProviderConfig may override via the "thinkingLevel" (string) or
// "thinkingBudget" (int) keys for callers that want raw control.
func thinkingConfigFor(spec agent.Spec, model string) *thinkingConfig {
	// Explicit ProviderConfig overrides win over the effort mapping.
	if is3xModel(model) {
		if lvl, ok := stringFromProviderConfig(spec.ProviderConfig, "thinkingLevel"); ok && lvl != "" {
			return &thinkingConfig{ThinkingLevel: lvl}
		}
	} else {
		if b, ok := intFromProviderConfig(spec.ProviderConfig, "thinkingBudget"); ok {
			budget := b
			return &thinkingConfig{ThinkingBudget: &budget}
		}
	}

	if spec.Effort == "" {
		return nil
	}

	if is3xModel(model) {
		return &thinkingConfig{ThinkingLevel: thinkingLevelForEffort(spec.Effort)}
	}
	budget := thinkingBudgetForEffort(spec.Effort)
	return &thinkingConfig{ThinkingBudget: &budget}
}

// thinkingLevelForEffort maps an EffortLevel onto a 3.x thinking_level.
func thinkingLevelForEffort(e agent.EffortLevel) string {
	switch e {
	case agent.EffortLow:
		return "low"
	case agent.EffortMedium:
		return "medium"
	case agent.EffortHigh, agent.EffortXHigh:
		return "high"
	default:
		return "medium"
	}
}

// thinkingBudgetForEffort maps an EffortLevel onto a 2.5 thinkingBudget
// token count.
func thinkingBudgetForEffort(e agent.EffortLevel) int {
	switch e {
	case agent.EffortLow:
		return 2048
	case agent.EffortMedium:
		return 8192
	case agent.EffortHigh, agent.EffortXHigh:
		return 24576
	default:
		return 8192
	}
}

// stringFromProviderConfig reads a string-valued key from the opaque
// ProviderConfig map.
func stringFromProviderConfig(pc map[string]any, key string) (string, bool) {
	if len(pc) == 0 {
		return "", false
	}
	s, ok := pc[key].(string)
	return s, ok
}

// modelPricing is the per-1M-token USD pricing (input, output) for the
// Gemini models donmai exposes. Verified 2026-06-02. Unknown models fall
// through to a zero result (TotalCostUsd stays 0 but the wire path is
// still exercised).
var modelPricing = map[string]struct {
	input  float64
	output float64
}{
	"gemini-3.5-flash":       {input: 1.50, output: 9.00},
	"gemini-3.1-pro-preview": {input: 2.00, output: 12.00},
	"gemini-3.1-flash-lite":  {input: 0.25, output: 1.50},
	"gemini-2.5-pro":         {input: 1.25, output: 10.00},
	"gemini-2.5-flash":       {input: 0.30, output: 2.50},
	"gemini-2.5-flash-lite":  {input: 0.10, output: 0.40},
}

// calculateCostUSD computes the dollar cost for a token split against the
// per-model pricing table. Unknown models return 0 (the path is wired so
// adding a table entry is the only change needed for a new model).
func calculateCostUSD(inputTokens, outputTokens int64, model string) float64 {
	p, ok := modelPricing[model]
	if !ok {
		return 0
	}
	return (float64(inputTokens)/1_000_000)*p.input +
		(float64(outputTokens)/1_000_000)*p.output
}

// sortedToolNames returns the declared function names in sorted order.
// Exposed for tests asserting deterministic declaration ordering.
func sortedToolNames(tools []requestTool) []string {
	var names []string
	for _, t := range tools {
		for _, d := range t.FunctionDeclarations {
			names = append(names, d.Name)
		}
	}
	sort.Strings(names)
	return names
}
