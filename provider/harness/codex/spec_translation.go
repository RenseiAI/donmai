package codex

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/mcp"
)

// DefaultCodexModel is the model identifier used when Spec.Model is
// unset. Mirrors the legacy TS CODEX_DEFAULT_MODEL constant from
// ../donmai-libraries/packages/core/src/providers/codex-app-server-provider.ts.
const DefaultCodexModel = "gpt-5-codex"

// codexModelTierMap mirrors CODEX_MODEL_MAP from the legacy TS. When a
// caller passes an Anthropic-style tier name in Spec.Env["CODEX_MODEL_TIER"]
// the codex provider promotes it to the matching codex model.
var codexModelTierMap = map[string]string{
	"opus":   "gpt-5-codex",
	"sonnet": "gpt-5.2-codex",
	"haiku":  "gpt-5.3-codex",
}

// resolveModel mirrors resolveCodexModel from the legacy TS. The
// precedence is Spec.Model → CODEX_MODEL_TIER → CODEX_MODEL → default.
func resolveModel(spec agent.Spec) string {
	if spec.Model != "" {
		return spec.Model
	}
	if spec.Env != nil {
		if tier, ok := spec.Env["CODEX_MODEL_TIER"]; ok {
			if m, ok := codexModelTierMap[tier]; ok {
				return m
			}
		}
		if m, ok := spec.Env["CODEX_MODEL"]; ok && m != "" {
			return m
		}
	}
	return DefaultCodexModel
}

// resolveSandboxMode maps agent.SandboxLevel to the kebab-case codex
// thread/start sandbox parameter. Mirrors resolveSandboxMode in the
// legacy TS.
func resolveSandboxMode(spec agent.Spec) string {
	switch spec.SandboxLevel {
	case agent.SandboxReadOnly:
		return "read-only"
	case agent.SandboxWorkspaceWrite:
		return "workspace-write"
	case agent.SandboxFullAccess:
		return "danger-full-access"
	}
	if spec.SandboxEnabled {
		return "workspace-write"
	}
	return ""
}

// resolveSandboxPolicy maps agent.SandboxLevel to the rich codex
// turn/start sandbox object. Mirrors resolveSandboxPolicy in the
// legacy TS.
func resolveSandboxPolicy(spec agent.Spec) map[string]any {
	writableRoots := []string{spec.Cwd}
	if spec.RepositoryAuthority != nil {
		writableRoots = append([]string(nil), spec.RepositoryAuthority.MutablePaths...)
	}
	switch spec.SandboxLevel {
	case agent.SandboxReadOnly:
		return map[string]any{"type": "readOnly", "networkAccess": true}
	case agent.SandboxWorkspaceWrite:
		return map[string]any{
			"type":          "workspaceWrite",
			"writableRoots": writableRoots,
			"networkAccess": true,
		}
	case agent.SandboxFullAccess:
		return map[string]any{"type": "dangerFullAccess"}
	}
	if spec.SandboxEnabled {
		return map[string]any{
			"type":          "workspaceWrite",
			"writableRoots": writableRoots,
			"networkAccess": true,
		}
	}
	return nil
}

// resolveApprovalPolicy mirrors resolveApprovalPolicy in the legacy TS.
// Codex v0.117+ uses kebab-case approval policy strings.
func resolveApprovalPolicy(spec agent.Spec) string {
	if spec.Autonomous {
		return "on-request"
	}
	return "untrusted"
}

// threadStartParams builds the JSON-RPC params for `thread/start`.
//
// baseInstructions is the explicit replacement surface. Appended harness,
// policy, and role instructions use developerInstructions so Codex's built-in
// base prompt remains intact (verified in the 0.146.0 app-server schema).
func threadStartParams(spec agent.Spec) map[string]any {
	params := map[string]any{
		"cwd":            spec.Cwd,
		"approvalPolicy": resolveApprovalPolicy(spec),
		"serviceName":    "donmai",
		"model":          resolveModel(spec),
	}
	if spec.BaseInstructions != "" {
		params["baseInstructions"] = spec.BaseInstructions
	}
	if spec.SystemPromptAppend != "" {
		params["developerInstructions"] = spec.SystemPromptAppend
	}
	if mode := resolveSandboxMode(spec); mode != "" {
		params["sandbox"] = mode
	}
	return params
}

// turnStartParams builds the JSON-RPC params for `turn/start`. The
// legacy TS sets `threadId`, `input`, `cwd`, `approvalPolicy`,
// `model`, optional `reasoningEffort`, and optional `sandboxPolicy`
// (the rich form). We keep that shape verbatim.
//
// Spec.ResponseSchema (the one-shot lane's structured-output schema)
// rides as `outputSchema` — TurnStartParams.outputSchema, "Optional
// JSON Schema used to constrain the final assistant message for this
// turn", verified against the codex app-server v2 protocol dump
// (`codex app-server generate-json-schema`, codex-cli 0.139.0). This
// upgrades codex one-shots from soft (prompt-instruction +
// validate-repair-drop) to native strict. App-server versions that
// predate the field tolerate unknown optional params (serde default),
// so older binaries silently degrade to the soft path: the schema
// instruction is still appended to the prompt by the one-shot lane and
// SpawnComplete's validate-repair-drop still certifies SchemaOK.
//
// The first turn's input carries the Spec.Prompt; resume + steering
// flows reuse this builder with a different input slice.
func turnStartParams(threadID string, spec agent.Spec, input []map[string]any) map[string]any {
	params := map[string]any{
		"threadId":       threadID,
		"input":          input,
		"cwd":            spec.Cwd,
		"approvalPolicy": resolveApprovalPolicy(spec),
		"model":          resolveModel(spec),
	}
	if spec.Effort != "" {
		params["reasoningEffort"] = string(spec.Effort)
	}
	if policy := resolveSandboxPolicy(spec); policy != nil {
		params["sandboxPolicy"] = policy
	}
	if len(spec.ResponseSchema) > 0 {
		params["outputSchema"] = spec.ResponseSchema
	}
	return params
}

// promptInput translates Spec.Prompt (+ optional Spec.InitialContext)
// into the codex turn/start input array.
//
// Codex models a turn input as an array of typed parts. v0.5.0 only
// emits text parts; image / attachment support belongs to F.5.
//
// Spec.InitialContext (e.g. recalled agent memory) is prepended as its
// own text part so it is delivered ONCE with the first turn and then
// lives in cached conversation history — it is deliberately kept OUT of
// thread/start's session-level instructions, which Codex re-includes in the
// model prompt on EVERY turn. Folding large/volatile context into those
// instructions produces O(turns × prefix) token amplification on
// long sessions; routing it here avoids that while preserving multi-turn
// coherence (the model still has the context for the whole session).
//
// SystemPromptAppend is intentionally NOT emitted on the per-turn input
// — it carries session-constant harness/role instructions, which belong in
// developerInstructions on thread/start (see NewSpawnPlan).
func promptInput(spec agent.Spec) []map[string]any {
	parts := make([]map[string]any, 0, 2)
	if ctxText := strings.TrimSpace(spec.InitialContext); ctxText != "" {
		parts = append(parts, map[string]any{"type": "text", "text": ctxText})
	}
	parts = append(parts, map[string]any{"type": "text", "text": spec.Prompt})
	return parts
}

// mcpServersConfig builds the value passed to `config/batchWrite` for the
// native `mcp_servers` keyPath. Codex expects a map keyed by server name, not
// the flat array we hold in Spec.MCPServers.
//
// It delegates to mcp.BuildConfigFile so the stdio/http transport logic
// and omitempty field rules live in exactly one place. Servers that
// fail validation (empty Name, missing Command/URL) are skipped with a
// warning rather than aborting the whole map — a single bad entry must
// not silence all MCP servers.
func mcpServersConfig(servers []agent.MCPServerConfig) map[string]any {
	if len(servers) == 0 {
		return nil
	}

	// Build one-at-a-time so a bad entry is skipped, not fatal.
	out := make(map[string]any, len(servers))
	for _, s := range servers {
		cfg, err := mcp.BuildConfigFile([]agent.MCPServerConfig{s})
		if err != nil {
			slog.Warn("codex: skipping invalid MCP server entry",
				"server", s.Name, "err", err)
			continue
		}
		// Marshal the typed Server struct (with omitempty tags) then
		// unmarshal into map[string]any to get exactly the right JSON
		// shape without null fields.
		srv, ok := cfg.MCPServers[s.Name]
		if !ok {
			// Shouldn't happen — BuildConfigFile uses s.Name as key.
			continue
		}
		b, err := json.Marshal(srv)
		if err != nil {
			slog.Warn("codex: failed to marshal MCP server entry",
				"server", s.Name, "err", err)
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(b, &entry); err != nil {
			slog.Warn("codex: failed to unmarshal MCP server entry",
				"server", s.Name, "err", err)
			continue
		}
		// runtime/mcp's shared JSON shape includes a transport discriminator
		// and calls HTTP headers "headers". Codex's config.toml layer infers
		// transport from command/url and names the latter "http_headers".
		// The app-server accepts the shared stdio shape, but rejects the shared
		// HTTP shape and silently ignores the old camel-case key path.
		delete(entry, "type")
		if headers, ok := entry["headers"]; ok {
			entry["http_headers"] = headers
			delete(entry, "headers")
		}
		// MCP tool approvals are handled inside Codex and do not travel over
		// the app-server command/file approval bridge. Headless sessions have
		// no user to answer that prompt, so pre-approve tools only on the exact
		// server entries this spawn requested.
		entry["default_tools_approval_mode"] = codexMCPToolsApprovalApprove
		out[s.Name] = entry
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SpawnPlan is the bag of JSON-RPC params Provider.Spawn assembles up
// front. It exists as a separate value so spec_translation_test.go can
// table-test the full Spec → params translation without touching live
// stdio.
type SpawnPlan struct {
	// MCPConfig is the value for `config/batchWrite` mcp_servers, or
	// nil when Spec.MCPServers is empty.
	MCPConfig map[string]any

	// ThreadStart is the params for the JSON-RPC `thread/start`
	// request that opens a fresh session.
	ThreadStart map[string]any

	// TurnStart is the params for the first JSON-RPC `turn/start`
	// request after thread creation. ThreadID is empty here and is
	// filled in by the Handle once thread/start returns.
	TurnStart map[string]any

	// PromptInput is the input array reused for steering / resume
	// when a fresh turn must be started on an existing thread.
	PromptInput []map[string]any

	// IgnoredFields lists the agent.Spec fields the codex provider
	// does NOT translate — surfaced for tests + observability so we
	// know which fields are silently dropped vs. silently lost.
	IgnoredFields []SpecFieldNote
}

// SpecFieldNote is one entry in SpawnPlan.IgnoredFields — names a
// dropped Spec field and the reason it was dropped.
type SpecFieldNote struct {
	Field  string
	Reason string
}

// NewSpawnPlan returns the JSON-RPC params for Spawn, plus the
// accounting of which Spec fields were translated and which were
// dropped. The accounting is exercised by spec_translation_test.go to
// ensure every relevant Spec field is either translated or
// explicitly noted.
func NewSpawnPlan(spec agent.Spec) SpawnPlan {
	threadStart := threadStartParams(spec)

	plan := SpawnPlan{
		MCPConfig:   mcpServersConfig(spec.MCPServers),
		ThreadStart: threadStart,
		PromptInput: promptInput(spec),
	}
	plan.TurnStart = turnStartParams("", spec, plan.PromptInput)

	plan.IgnoredFields = ignoredSpecFields(spec)
	return plan
}

// ignoredSpecFields documents each agent.Spec field the codex provider
// either does not translate or only consumes via downstream paths
// (e.g. PermissionConfig flows into the approval bridge, not the
// thread/start params).
func ignoredSpecFields(spec agent.Spec) []SpecFieldNote {
	var notes []SpecFieldNote

	// Env: not a JSON-RPC param. Plumbed via the codex app-server
	// child process environment in codex.go (process.env merge).
	if len(spec.Env) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "Env",
			Reason: "merged into the codex app-server subprocess environment, not a thread/start param",
		})
	}

	// AllowedTools / DisallowedTools: codex handles tool-level
	// permission via the approval bridge (PermissionConfig), not via
	// flat allow/deny lists. The bridge consults Spec.PermissionConfig
	// directly. Tools fields are dropped here with a note.
	if len(spec.AllowedTools) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "AllowedTools",
			Reason: "codex uses approval-bridge patterns (Spec.PermissionConfig), not a flat allow list",
		})
	}
	if len(spec.DisallowedTools) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "DisallowedTools",
			Reason: "codex uses approval-bridge patterns (Spec.PermissionConfig), not a flat deny list",
		})
	}

	// MCPToolNames: only meaningful for Claude's --allowedTools list.
	// Codex auto-discovers tools from registered mcp_servers.
	if len(spec.MCPToolNames) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "MCPToolNames",
			Reason: "codex auto-discovers MCP tools from configured mcp_servers; explicit names are ignored",
		})
	}

	// MaxTurns: codex has no per-thread turn cap today. The runner
	// enforces a wall-clock timeout instead.
	if spec.MaxTurns != nil {
		notes = append(notes, SpecFieldNote{
			Field:  "MaxTurns",
			Reason: "codex app-server has no maxTurns parameter; runner enforces wall-clock timeout",
		})
	}

	// PermissionConfig: passed to the approval bridge in handle.go,
	// not to thread/start. Mention here for completeness.
	if spec.PermissionConfig != nil {
		notes = append(notes, SpecFieldNote{
			Field:  "PermissionConfig",
			Reason: "consumed by the approval bridge (approval.go), not via thread/start params",
		})
	}

	// CodeIntelEnforcement: codex has no canUseTool callback today;
	// flagged so observers know the field is silently ignored.
	if spec.CodeIntelEnforcement != nil {
		notes = append(notes, SpecFieldNote{
			Field:  "CodeIntelEnforcement",
			Reason: "codex has no canUseTool callback; F.5 + a wrapper sidecar would re-enable",
		})
	}

	// ProviderConfig: opaque per-provider extension bag. The codex
	// provider has no defined keys today; the field is reserved.
	if len(spec.ProviderConfig) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "ProviderConfig",
			Reason: "codex defines no providerConfig keys today; field is reserved",
		})
	}

	// SubAgentProvider: only meaningful when the codex agent spawns
	// downstream agents — codex has no Anthropic Task tool, so this
	// is silently ignored.
	if spec.SubAgentProvider != "" {
		notes = append(notes, SpecFieldNote{
			Field:  "SubAgentProvider",
			Reason: "codex has no native subagent dispatch (no Task tool); coordination flows happen at the runner layer",
		})
	}

	// OnProcessSpawned: codex shares the same app-server pid for
	// every Handle. The Provider invokes the callback once per
	// session with the shared pid so cost/heartbeat hooks still fire.
	// This is documented as honored, not ignored.
	return notes
}
