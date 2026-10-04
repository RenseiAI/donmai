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

// nativeToolNames is the native tool surface this loop offers when no allow
// list bounds it, in declaration order.
var nativeToolNames = []string{"Bash", "Edit", "Write", "Read", "Grep", "Glob", "Task"}

// toolsFromSpec builds the request body's tools array from BOTH
// Spec.AllowedTools and Spec.MCPServers. Every native allowed tool and
// every declared MCP tool/server becomes one functionDeclaration so the
// model can call it.
//
// Native tools (Bash/Read/Edit/Write derived from AllowedTools) are
// executed end-to-end by the session-local executor (handle.go →
// executor.go). MCP servers initially get a catch-all per-server
// declaration; once Spawn's MCP bridge has dialed the server and listed
// its tools, mcpBridge.amendPlan (mcp.go) swaps the catch-all for the
// real mcp__<server>__<tool> declarations, and the executor routes the
// resulting mcp__* functionCalls to the live server. A server that fails
// to connect keeps its catch-all and resolves calls to a structured
// error.
//
// Returns nil when an allow gate admits no tools — an explicitly empty
// AllowedTools, as a one-shot completion sends — and the session declares no
// MCPToolNames or MCPServers, so the request omits the tools field and the
// model behaves as a plain text generator.
//
// All functionDeclarations are collected into a single tools[] entry —
// Gemini merges declarations across array entries, but one entry keeps
// the wire payload compact and the ordering deterministic.
func toolsFromSpec(spec agent.Spec) []requestTool {
	seen := make(map[string]struct{})
	// No capacity hint: summed len()s as an allocation size is the exact
	// shape a static scanner (go/allocation-size-overflow) flags as a
	// potential overflow; append grows the slice on demand just fine.
	decls := make([]functionDeclaration, 0)

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
	// Without an allow gate (no configured list below an allow-gated
	// toolApproval level) the session is offered the full native surface:
	// removing the runner's hidden allow list removed a gate, never the
	// tools behind it.
	if allowListGated(spec) {
		for _, t := range spec.AllowedTools {
			add(toolNameFromPattern(t), "Allow-listed tool: "+t)
		}
	} else {
		for _, name := range nativeToolNames {
			add(name, "Native tool: "+name)
		}
	}

	// Explicit MCP tool names already fully-qualified
	// (e.g. "mcp__af-code-intelligence__af_code_get_repo_map").
	for _, t := range spec.MCPToolNames {
		add(t, "MCP tool (bridged): "+t)
	}

	// MCP servers: expose one catch-all declaration per server keyed by
	// the server name (the server's tool manifest is not known at plan
	// build time). Spawn's MCP bridge replaces it with the discovered
	// per-tool declarations via amendPlan; when the server cannot be
	// reached the catch-all stays and the executor resolves calls to a
	// structured error.
	for _, s := range spec.MCPServers {
		if s.Name == "" {
			continue
		}
		add("mcp__"+s.Name, "MCP server (declared): "+s.Name)
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

// geminiMajorVersion parses the Gemini family major version from a model
// id of the form "gemini-<major>[.<minor>]-…" (gemini-3.5-flash → 3,
// gemini-4.1-flash → 4, gemini-2.5-pro → 2). It returns false when the id
// carries no explicit numeric major (gemini-flash-latest, an empty id,
// or any shape that does not open with "gemini-<digit>").
func geminiMajorVersion(model string) (int, bool) {
	rest, ok := strings.CutPrefix(model, "gemini-")
	if !ok {
		return 0, false
	}
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	major := 0
	for i := 0; i < end; i++ {
		major = major*10 + int(rest[i]-'0')
	}
	return major, true
}

// usesThinkingLevel reports whether the model id routes reasoning effort
// to thinkingConfig.thinkingLevel (the level knob) rather than
// thinkingConfig.thinkingBudget (the token-budget knob). Major version 3
// and later use the level knob; 2.x (and 1.x) keep the budget knob; ids
// without a parseable major version default to the level knob — the
// current and forward-compatible surface (e.g. gemini-flash-latest).
func usesThinkingLevel(model string) bool {
	major, ok := geminiMajorVersion(model)
	if !ok {
		return true
	}
	return major >= 3
}

// thinkingConfigFor maps Spec.Effort onto the model family's thinking
// knob. Returns nil when no effort is requested.
//
//   - Level-knob models (major >= 3, or an unversioned id) →
//     thinkingLevel: low → "low", medium → "medium", high/xhigh/max →
//     "high" (the family's highest level). (The level knob has no off
//     switch; minimal is the floor.)
//   - Budget-knob models (2.x and earlier) → thinkingBudget: low → 2048,
//     medium → 8192, high/xhigh/max → 24576 tokens. (-1 dynamic, 0 off
//     are also valid but we map effort tiers to concrete budgets.)
//
// ProviderConfig may override via the "thinkingLevel" (string) or
// "thinkingBudget" (int) keys for callers that want raw control.
func thinkingConfigFor(spec agent.Spec, model string) *thinkingConfig {
	// Explicit ProviderConfig overrides win over the effort mapping.
	if usesThinkingLevel(model) {
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

	if usesThinkingLevel(model) {
		return &thinkingConfig{ThinkingLevel: thinkingLevelForEffort(spec.Effort)}
	}
	budget := thinkingBudgetForEffort(spec.Effort)
	return &thinkingConfig{ThinkingBudget: &budget}
}

// thinkingLevelForEffort maps an EffortLevel onto a 3.x thinking_level.
//
// Casing note: the Gemini REST API (generativelanguage.googleapis.com)
// accepts lowercase values for thinkingConfig.thinkingLevel. The canonical
// examples in the Google AI for Developers documentation
// (https://ai.google.dev/gemini-api/docs/thinking, re-verified 2026-06-10:
// accepted values are minimal | low | medium | high, REST example
// "thinkingLevel": "low") use lowercase, and the API rejects uppercase
// variants. Lowercase is intentional here — do NOT change to uppercase.
// Pinned by thinking_level_test.go: the enum/wire fixture tests run
// always; TestLive_ThinkingLevelCasing (GEMINI_LIVE=1) probes the real
// API in both casings.
func thinkingLevelForEffort(e agent.EffortLevel) string {
	switch e {
	case agent.EffortLow:
		return "low"
	case agent.EffortMedium:
		return "medium"
	case agent.EffortHigh, agent.EffortXHigh, agent.EffortMax:
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
	case agent.EffortHigh, agent.EffortXHigh, agent.EffortMax:
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

// modelPrice is the per-1M-token USD pricing (input, cached input,
// output) for one Gemini model.
type modelPrice struct {
	input  float64
	cached float64
	output float64
}

// ProviderConfig keys carrying per-model per-1M-token USD prices supplied
// by the dispatcher. They win over the fallback table below (see
// resolveModelPricing).
const (
	providerConfigInputPricePer1M  = "inputPricePer1M"
	providerConfigCachedPricePer1M = "cachedPricePer1M"
	providerConfigOutputPricePer1M = "outputPricePer1M"
)

// modelPricing is the per-1M-token USD pricing fallback for the Gemini
// models donmai exposes. Verified 2026-06-02; the 3.6/3.7/3.8 flash rows
// carry the current flash rate. A zero cached rate means no cached-input
// rate is published for that model — resolution then prices cached tokens
// at the input rate, preserving the pre-split totals. Unknown models fall
// through to a zero result (TotalCostUsd stays 0 but the wire path is
// still exercised).
var modelPricing = map[string]modelPrice{
	"gemini-3.8-flash":       {input: 0.75, cached: 0.075, output: 3.75},
	"gemini-3.7-flash":       {input: 0.75, cached: 0.075, output: 3.75},
	"gemini-3.6-flash":       {input: 0.75, cached: 0.075, output: 3.75},
	"gemini-3.5-flash":       {input: 1.50, output: 9.00},
	"gemini-3.1-pro-preview": {input: 2.00, output: 12.00},
	"gemini-3.1-flash-lite":  {input: 0.25, output: 1.50},
	"gemini-2.5-pro":         {input: 1.25, output: 10.00},
	"gemini-2.5-flash":       {input: 0.30, output: 2.50},
	"gemini-2.5-flash-lite":  {input: 0.10, output: 0.40},
}

// floatFromProviderConfig reads a float-valued key from the opaque
// ProviderConfig map. JSON decoding yields float64 for numbers, so int
// and int64 are accepted as well.
func floatFromProviderConfig(pc map[string]any, key string) (float64, bool) {
	if len(pc) == 0 {
		return 0, false
	}
	switch v := pc[key].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

// resolveModelPricing returns the per-1M-token USD pricing for a model.
// Per-model prices supplied by the dispatcher via Spec.ProviderConfig
// (inputPricePer1M / cachedPricePer1M / outputPricePer1M) win over the
// fallback table: each supplied positive side replaces the table value,
// an unsupplied side keeps the table value for a known model (0 for an
// unknown one), and an unsupplied cached side falls back to the resolved
// input rate so cached tokens are never silently free under an override.
// A table entry without a published cached rate likewise resolves cached
// to its input rate. The second return value is false when neither an
// override nor a table entry exists — the caller then reports zero cost.
func resolveModelPricing(providerConfig map[string]any, model string) (modelPrice, bool) {
	in, hasIn := floatFromProviderConfig(providerConfig, providerConfigInputPricePer1M)
	out, hasOut := floatFromProviderConfig(providerConfig, providerConfigOutputPricePer1M)
	if (hasIn && in > 0) || (hasOut && out > 0) {
		table, _ := modelPricing[model]
		p := modelPrice{}
		if hasIn && in > 0 {
			p.input = in
		} else {
			p.input = table.input
		}
		if hasOut && out > 0 {
			p.output = out
		} else {
			p.output = table.output
		}
		if cached, hasCached := floatFromProviderConfig(providerConfig, providerConfigCachedPricePer1M); hasCached && cached >= 0 {
			p.cached = cached
		} else {
			p.cached = p.input
		}
		return p, true
	}
	p, ok := modelPricing[model]
	if !ok {
		return modelPrice{}, false
	}
	if p.cached == 0 {
		p.cached = p.input
	}
	return p, true
}

// calculateCostUSD computes the dollar cost for a token split against the
// resolved per-model pricing (dispatcher-supplied Spec.ProviderConfig
// prices first, the fallback table second). inputTokens is the total
// prompt count including cached tokens; the cached slice is priced at the
// cached rate, the remainder at the input rate. Unknown models return 0
// (the path is wired so adding a table entry is the only change needed
// for a new model).
func calculateCostUSD(inputTokens, cachedTokens, outputTokens int64, model string, providerConfig map[string]any) float64 {
	p, ok := resolveModelPricing(providerConfig, model)
	if !ok {
		return 0
	}
	freshInput := inputTokens - cachedTokens
	if freshInput < 0 {
		freshInput = 0
	}
	return (float64(freshInput)/1_000_000)*p.input +
		(float64(cachedTokens)/1_000_000)*p.cached +
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
