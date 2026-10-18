package muse

import (
	"log/slog"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/google/uuid"
)

// DefaultModel is the model identifier used when Spec.Model is unset.
// The host's catalog default (muse-wonder-1.0) is the fallback; an
// explicit Spec.Model always wins. Never send a "default" sentinel.
const DefaultModel = "muse-wonder-1.0"

// ProviderID is the MSP provider routing selected on session/start.
// "meta" routes to the subscriber login; an API key (when the Spec
// sets one) takes priority on the host.
const ProviderID = "meta"

// museClientName is the initialize clientInfo.name. The host requires
// ^[a-z0-9_]+$.
const museClientName = "donmai"

// requestedCapabilities are the MSP capabilities this client asks for.
// sessionMcp is required for inline config.mcpServers.
var requestedCapabilities = []string{string(CapabilitySessionMCP)}

// newUUIDv7 mints a client-side UUIDv7 idempotency handle for commandId
// and sessionId. The host requires UUIDv7 and never mints one.
func newUUIDv7() string {
	id, err := uuid.NewV7()
	if err != nil {
		// UUIDv7 mints from time+entropy; failure is effectively
		// impossible. Fall back to v4 rather than failing the spawn.
		return uuid.NewString()
	}
	return id.String()
}

// resolveModel maps Spec.Model to the MSP modelId. Empty falls back to
// the host catalog default. The "default" sentinel is never forwarded:
// it is not a selectable model id.
func resolveModel(spec agent.Spec) string {
	if spec.Model != "" && spec.Model != "default" {
		return spec.Model
	}
	return DefaultModel
}

// resolveEffort maps Spec.Effort to the MSP reasoning tier. Empty (not
// configured) leaves the tier unset so the host default applies. The
// "minimal" tier has no agent.EffortLevel constant (the shared ladder
// runs low..max) but the host catalog lists it, so it is matched by its
// wire spelling. The "default" and "none" sentinels are never
// forwarded: an unset tier and a default tier are the same request to the
// host (run at its default), and "none" is not a selectable tier.
// "ultra" is a declared wire value (types.go) the shared ladder does
// not name: it is refused loudly rather than silently degraded to the
// host default, so a caller asking for the top tier never unknowingly
// runs at a lower one.
func resolveEffort(spec agent.Spec) *ReasoningEffort {
	switch spec.Effort {
	case agent.EffortLevel("minimal"):
		e := ReasoningEffortMinimal
		return &e
	case agent.EffortLow:
		e := ReasoningEffortLow
		return &e
	case agent.EffortMedium:
		e := ReasoningEffortMedium
		return &e
	case agent.EffortHigh:
		e := ReasoningEffortHigh
		return &e
	case agent.EffortXHigh:
		e := ReasoningEffortXHigh
		return &e
	case agent.EffortMax:
		e := ReasoningEffortMax
		return &e
	default:
		return nil
	}
}

// resolveApprovalMode maps the session posture to the MSP approval mode.
// The read-only lane always runs denyUnmatched. Autonomous sessions run
// allowAll only when the Spec carries no
// permission policy; an explicit PermissionConfig selects
// promptUnmatched so the approval bridge sees every unmatched call.
// Non-autonomous sessions default to promptUnmatched.
func resolveApprovalMode(spec agent.Spec) ApprovalMode {
	// The read-only lane is tighten-only: no other Spec field (an
	// autonomous posture, a permissive policy) widens it past
	// denyUnmatched.
	if spec.SandboxLevel == agent.SandboxReadOnly {
		return ApprovalModeDenyUnmatched
	}
	if spec.Autonomous && spec.PermissionConfig == nil {
		return ApprovalModeAllowAll
	}
	if spec.PermissionConfig != nil {
		if d := strings.ToLower(strings.TrimSpace(spec.PermissionConfig.DefaultDecision)); d == "deny" {
			return ApprovalModeDenyUnmatched
		}
	}
	if spec.Autonomous {
		return ApprovalModePromptUnmatched
	}
	return ApprovalModePromptUnmatched
}

// sessionStartParams builds the JSON-RPC params for `session/start`.
// The caller supplies the client-minted session and command ids.
func sessionStartParams(spec agent.Spec, sessionID, commandID string, workspaceRoot string, mcpServers map[string]SessionMCPServerConfig) map[string]any {
	model := resolveModel(spec)
	params := map[string]any{
		"sessionId":     sessionID,
		"commandId":     commandID,
		"workspaceRoot": workspaceRoot,
		"modelId":       model,
		"providerId":    ProviderID,
		"approvalMode":  string(resolveApprovalMode(spec)),
	}
	if len(mcpServers) > 0 {
		params["config"] = map[string]any{"mcpServers": mcpServers}
	}
	return params
}

// sessionResumeParams builds the JSON-RPC params for `session/resume`.
// excludeItems keeps the resume light; history pages separately.
func sessionResumeParams(sessionID, commandID string, mcpServers map[string]SessionMCPServerConfig) map[string]any {
	params := map[string]any{
		"sessionId":    sessionID,
		"commandId":    commandID,
		"excludeItems": true,
	}
	if len(mcpServers) > 0 {
		params["config"] = map[string]any{"mcpServers": mcpServers}
	}
	return params
}

// promptInput translates Spec.Prompt (+ optional Spec.InitialContext and
// BaseInstructions) into the MSP turn/start input array.
//
// MSP has no system-prompt channel, so instructions ride a leading text
// part ahead of the prompt. Spec.InitialContext (e.g. recalled agent
// memory) is its own text part so it is delivered once with the first
// turn and then lives in conversation history.
func promptInput(spec agent.Spec) []TurnInputPart {
	parts := make([]TurnInputPart, 0, 3)
	text := func(s string) TurnInputPart {
		return TurnInputPart{Type: string(TurnInputText), Text: &s}
	}
	var instructions []string
	if base := strings.TrimSpace(spec.BaseInstructions); base != "" {
		instructions = append(instructions, base)
	}
	if appendText := strings.TrimSpace(spec.SystemPromptAppend); appendText != "" {
		instructions = append(instructions, appendText)
	}
	if len(instructions) > 0 {
		joined := strings.Join(instructions, "\n\n")
		parts = append(parts, text(joined))
	}
	if ctxText := strings.TrimSpace(spec.InitialContext); ctxText != "" {
		parts = append(parts, text(ctxText))
	}
	parts = append(parts, text(spec.Prompt))
	return parts
}

// turnStartParams builds the JSON-RPC params for `turn/start`.
func turnStartParams(sessionID, commandID string, spec agent.Spec, input []TurnInputPart, workspaceRoots []string) map[string]any {
	in := make([]map[string]any, 0, len(input))
	for _, p := range input {
		part := map[string]any{"type": p.Type}
		if p.Text != nil {
			part["text"] = *p.Text
		}
		if p.Base64Data != nil {
			part["base64Data"] = *p.Base64Data
		}
		if p.MediaType != nil {
			part["mediaType"] = *p.MediaType
		}
		if p.Width != nil {
			part["width"] = *p.Width
		}
		if p.Height != nil {
			part["height"] = *p.Height
		}
		in = append(in, part)
	}
	params := map[string]any{
		"sessionId": sessionID,
		"commandId": commandID,
		"input":     in,
		"ifBusy":    string(IfBusyQueue),
	}
	if spec.Prompt != "" || len(input) > 0 {
		display := spec.Prompt
		if len(display) > 240 {
			display = display[:240] + "…"
		}
		params["displayText"] = display
	}
	if len(workspaceRoots) > 0 {
		params["workspaceRoots"] = workspaceRoots
	}
	if effort := resolveEffort(spec); effort != nil {
		params["reasoningEffort"] = string(*effort)
	}
	return params
}

// mcpServersConfig builds the inline session/start config.mcpServers
// value. Each HTTP server becomes {transport:"streamableHttp",
// mode:"optional", url, headers}; stdio servers pass through
// {transport:"stdio", command, args, env, mode:"optional"}.
//
// Soft-fail vs hard-deny policy: config-time validation soft-fails
// (a malformed entry — empty Name, missing Command/URL, unknown
// transport — is skipped with a warning so one bad entry cannot sink
// the session's other servers), while a missing sessionMcp capability
// grant hard-denies the session at handshake when the session carries
// MCP servers: without the grant the host would silently run with no
// MCP tools, which is worse than refusing to start. Every admitted
// server rides mode:"optional" so a single unreachable server at
// runtime degrades that server, not the turn.
//
// Servers that fail validation (empty Name, missing Command/URL) are
// skipped with a warning rather than aborting the whole map.
func mcpServersConfig(servers []agent.MCPServerConfig) map[string]SessionMCPServerConfig {
	if len(servers) == 0 {
		return nil
	}
	out := make(map[string]SessionMCPServerConfig, len(servers))
	for _, s := range servers {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			slog.Warn("muse: skipping MCP server entry with empty name")
			continue
		}
		mode := SessionMCPServerModeOptional
		switch strings.ToLower(strings.TrimSpace(s.Type)) {
		case "", "stdio":
			if strings.TrimSpace(s.Command) == "" {
				slog.Warn("muse: skipping invalid stdio MCP server entry", "server", name)
				continue
			}
			out[name] = SessionMCPServerConfig{
				Transport: "stdio",
				Command:   s.Command,
				Args:      append([]string(nil), s.Args...),
				Env:       appendStringMap(nil, s.Env),
				Mode:      &mode,
			}
		case "http", "streamablehttp", "streamable-http":
			if strings.TrimSpace(s.URL) == "" {
				slog.Warn("muse: skipping invalid HTTP MCP server entry", "server", name)
				continue
			}
			out[name] = SessionMCPServerConfig{
				Transport: "streamableHttp",
				URL:       s.URL,
				Headers:   appendStringMap(nil, s.Headers),
				Mode:      &mode,
			}
		default:
			slog.Warn("muse: skipping MCP server entry with unknown transport", "server", name, "type", s.Type)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func appendStringMap(dst, src map[string]string) map[string]string {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// SpawnPlan is the bag of JSON-RPC params Provider.Spawn assembles up
// front. It exists as a separate value so spec_translation_test.go can
// table-test the full Spec → params translation without touching live
// stdio.
type SpawnPlan struct {
	// MCPServers is the inline config.mcpServers value, or nil when
	// Spec.MCPServers is empty.
	MCPServers map[string]SessionMCPServerConfig

	// PromptInput is the turn input array reused for the first turn
	// and for steering.
	PromptInput []TurnInputPart

	// WorkspaceRoot is the canonical realpath cwd the session starts in.
	WorkspaceRoot string

	// IgnoredFields lists the agent.Spec fields the muse provider
	// does NOT translate.
	IgnoredFields []SpecFieldNote
}

// SpecFieldNote is one entry in SpawnPlan.IgnoredFields.
type SpecFieldNote struct {
	Field  string
	Reason string
}

// NewSpawnPlan returns the JSON-RPC params for Spawn, plus the
// accounting of which Spec fields were translated and which were
// dropped.
func NewSpawnPlan(spec agent.Spec, workspaceRoot string) SpawnPlan {
	plan := SpawnPlan{
		MCPServers:    mcpServersConfig(spec.MCPServers),
		PromptInput:   promptInput(spec),
		WorkspaceRoot: workspaceRoot,
	}
	plan.IgnoredFields = ignoredSpecFields(spec)
	return plan
}

// ignoredSpecFields documents each agent.Spec field the muse provider
// either does not translate or only consumes via downstream paths.
func ignoredSpecFields(spec agent.Spec) []SpecFieldNote {
	var notes []SpecFieldNote

	if len(spec.Env) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "Env",
			Reason: "merged into the muse serve subprocess environment, not a session/start param",
		})
	}
	if len(spec.AllowedTools) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "AllowedTools",
			Reason: "muse uses approval-mode + bridge patterns (Spec.PermissionConfig), not a flat allow list",
		})
	}
	if len(spec.DisallowedTools) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "DisallowedTools",
			Reason: "muse uses approval-mode + bridge patterns (Spec.PermissionConfig), not a flat deny list",
		})
	}
	if len(spec.MCPToolNames) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "MCPToolNames",
			Reason: "muse auto-discovers tools from inline config.mcpServers; explicit names are ignored",
		})
	}
	if spec.MaxTurns != nil {
		notes = append(notes, SpecFieldNote{
			Field:  "MaxTurns",
			Reason: "muse serve has no per-session turn cap; runner enforces wall-clock timeout",
		})
	}
	if spec.PermissionConfig != nil {
		notes = append(notes, SpecFieldNote{
			Field:  "PermissionConfig",
			Reason: "consumed by the approval bridge (approval.go), not via session/start params",
		})
	}
	if spec.CodeIntelEnforcement != nil {
		notes = append(notes, SpecFieldNote{
			Field:  "CodeIntelEnforcement",
			Reason: "muse has no canUseTool callback; silently ignored",
		})
	}
	if len(spec.ProviderConfig) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "ProviderConfig",
			Reason: "muse defines no providerConfig keys today; field is reserved",
		})
	}
	if spec.SubAgentProvider != "" {
		notes = append(notes, SpecFieldNote{
			Field:  "SubAgentProvider",
			Reason: "muse subagent delegation surfaces as subagent items; coordination flows happen at the runner layer",
		})
	}
	if len(spec.ResponseSchema) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "ResponseSchema",
			Reason: "muse turn/start has no structured-output primitive; the one-shot lane keeps its soft prompt instruction",
		})
	}
	return notes
}

// approvalChoiceFor maps a bridge decision to the MSP approval choice:
// allow → the first approval choice whose decision is an approval,
// decline → the first choice whose decision is a denial/abort.
func approvalChoiceFor(req ApprovalRequestParams, allow bool) (ApprovalChoice, bool) {
	for _, c := range req.AvailableChoices {
		switch c.Decision {
		case MSPApprovalApproved, MSPApprovalApprovedForSession, MSPApprovalApprovedPolicyAmendment:
			if allow {
				return c, true
			}
		case MSPApprovalDenied, MSPApprovalDeniedPolicyAmendment, MSPApprovalAbort:
			if !allow {
				return c, true
			}
		}
	}
	return ApprovalChoice{}, false
}
