package codex

import (
	"github.com/RenseiAI/agentfactory-tui/agent"
)

// DefaultCodexModel is the model identifier used when Spec.Model is
// unset. Mirrors the legacy TS CODEX_DEFAULT_MODEL constant from
// ../agentfactory/packages/core/src/providers/codex-app-server-provider.ts.
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
	switch spec.SandboxLevel {
	case agent.SandboxReadOnly:
		return map[string]any{"type": "readOnly", "networkAccess": true}
	case agent.SandboxWorkspaceWrite:
		return map[string]any{
			"type":          "workspaceWrite",
			"writableRoots": []string{spec.Cwd},
			"networkAccess": true,
		}
	case agent.SandboxFullAccess:
		return map[string]any{"type": "dangerFullAccess"}
	}
	if spec.SandboxEnabled {
		return map[string]any{
			"type":          "workspaceWrite",
			"writableRoots": []string{spec.Cwd},
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
// The legacy TS sets `cwd`, `approvalPolicy`, `serviceName`, optional
// `baseInstructions`, `model`, and `sandbox`. We keep that shape so a
// known-working codex (>= 0.117) behaves identically. The mapping is
// also the place every Spec field gets accounted for; see specFieldsMap
// for the exhaustive 19-field accounting.
func threadStartParams(spec agent.Spec) map[string]any {
	params := map[string]any{
		"cwd":            spec.Cwd,
		"approvalPolicy": resolveApprovalPolicy(spec),
		"serviceName":    "agentfactory",
		"model":          resolveModel(spec),
	}
	if spec.BaseInstructions != "" {
		params["baseInstructions"] = spec.BaseInstructions
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
	return params
}

// promptInput translates Spec.Prompt + optional SystemPromptAppend into
// the codex turn/start input array.
//
// Codex models a turn input as an array of typed parts. v0.5.0 only
// emits text parts; image / attachment support belongs to F.5.
func promptInput(spec agent.Spec) []map[string]any {
	parts := []map[string]any{
		{"type": "text", "text": spec.Prompt},
	}
	// SystemPromptAppend is intentionally NOT emitted on the per-turn
	// input — the legacy TS folds it into baseInstructions on
	// thread/start. We do the same in NewSpawnPlan.
	return parts
}

// mcpServersConfig builds the value passed to `config/batchWrite` for
// the `mcpServers` keyPath. Codex expects a map keyed by server name,
// not the flat array we hold in Spec.MCPServers. The mapping mirrors
// configureMcpServers in the legacy TS.
func mcpServersConfig(servers []agent.MCPServerConfig) map[string]any {
	if len(servers) == 0 {
		return nil
	}
	out := make(map[string]any, len(servers))
	for _, s := range servers {
		entry := map[string]any{
			"command": s.Command,
			"args":    s.Args,
		}
		if len(s.Env) > 0 {
			entry["env"] = s.Env
		}
		out[s.Name] = entry
	}
	return out
}

// SpawnPlan is the bag of JSON-RPC params Provider.Spawn assembles up
// front. It exists as a separate value so spec_translation_test.go can
// table-test the full Spec → params translation without touching live
// stdio.
type SpawnPlan struct {
	// MCPConfig is the value for `config/batchWrite` mcpServers, or
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
// ensure every one of the 19 Spec fields is either translated or
// explicitly noted.
func NewSpawnPlan(spec agent.Spec) SpawnPlan {
	threadStart := threadStartParams(spec)
	// Carry SystemPromptAppend through baseInstructions so
	// per-template additions (e.g. CLAUDE.md content) survive without
	// a separate codex parameter — codex has only baseInstructions.
	if spec.SystemPromptAppend != "" {
		if existing, ok := threadStart["baseInstructions"].(string); ok && existing != "" {
			threadStart["baseInstructions"] = existing + "\n\n" + spec.SystemPromptAppend
		} else {
			threadStart["baseInstructions"] = spec.SystemPromptAppend
		}
	}

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
	// Codex auto-discovers tools from registered mcpServers.
	if len(spec.MCPToolNames) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "MCPToolNames",
			Reason: "codex auto-discovers MCP tools from configured mcpServers; explicit names are ignored",
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
