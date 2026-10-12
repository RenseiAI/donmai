package muse

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// DefaultModelID is the model identifier used when Spec.Model is
// unset. Observed live on 1.4.6 (model/list; the CLI default). The
// harness never sends a "default" sentinel: when this default applies,
// the modelId member is omitted and the host selects its default.
const DefaultModelID = "muse-wonder-1.0"

// museProviderID is the provider routing selected on session/start.
// "meta" is the host default and the only routing this harness uses.
const museProviderID = "meta"

// resolveModel maps agent.Spec to the MSP modelId. Precedence:
// Spec.Model → DefaultModelID (documented; the member is omitted so the
// host default applies). A literal "default" (any case) is never sent:
// the host treats it as an unknown model id, not as "use the default".
func resolveModel(spec agent.Spec) string {
	model := strings.TrimSpace(spec.Model)
	if model == "" || strings.EqualFold(model, "default") {
		return ""
	}
	return model
}

// resolveEffort maps agent.EffortLevel to the MSP reasoningEffort tier.
// The MSP vocabulary (closed): none, minimal, low, medium, high, xhigh,
// max, ultra. agent.EffortLevel has no "minimal"/"none" members, so the
// zero value maps to "" (unset — the host samples its default) and the
// five known levels map 1:1. Unknown values map to "" rather than
// erroring: effort is advisory, and an unset effort always degrades to
// the host default instead of refusing the turn.
func resolveEffort(effort agent.EffortLevel) string {
	switch effort {
	case agent.EffortLow:
		return "low"
	case agent.EffortMedium:
		return "medium"
	case agent.EffortHigh:
		return "high"
	case agent.EffortXHigh:
		return "xhigh"
	case agent.EffortMax:
		return "max"
	default:
		return ""
	}
}

// resolveApprovalMode maps agent.Spec to the MSP session/start
// approvalMode. The MSP vocabulary (closed): allowAll,
// promptUnmatched, onRequest, denyUnmatched. Autonomous sessions run
// "on-request" (the approval bridge answers); non-autonomous sessions
// leave the host default.
func resolveApprovalMode(spec agent.Spec) string {
	if spec.Autonomous {
		return "onRequest"
	}
	return ""
}

// resolveReadOnly reports whether the Spec requests the read-only lane:
// SandboxReadOnly maps to `muse serve --disable-shell --disable-write`
// plus the denyUnmatched approval mode. Workspace-write and full-access
// run the standard lane.
func resolveReadOnly(spec agent.Spec) bool {
	return spec.SandboxLevel == agent.SandboxReadOnly
}

// newUUIDv7 mints a random UUIDv7 (RFC 9562 §5.7): 48-bit big-endian
// millisecond timestamp, ver 0111, RFC 4122 variant. The host requires
// UUIDv7 sessionId/commandId values and rejects v4 with invalidParams.
func newUUIDv7() string {
	var b [16]byte
	ms := nowMillis()
	if ms < 0 {
		ms = 0
	}
	binary.BigEndian.PutUint64(b[0:8], uint64(ms)<<16) //nolint:gosec // G115: ms is clamped non-negative above.
	// Top 16 bits of the timestamp occupy b[0:6]; keep them and stamp
	// the version nibble onto b[6].
	if _, err := rand.Read(b[6:16]); err != nil {
		// rand.Read from crypto/rand fails only when the OS entropy
		// source is broken; fall back to a zero tail rather than
		// aborting session start — uniqueness still holds via the
		// millisecond prefix for all practical purposes.
		for i := 6; i < 16; i++ {
			b[i] = 0
		}
	}
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b[:])
}

func formatUUID(b []byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, 36)
	groups := []int{4, 2, 2, 2, 6}
	off := 0
	for gi, n := range groups {
		if gi > 0 {
			out = append(out, '-')
		}
		for i := 0; i < n; i++ {
			out = append(out, hexd[b[off]>>4], hexd[b[off]&0x0f])
			off++
		}
	}
	return string(out)
}

// promptInput translates Spec.Prompt (+ optional Spec.InitialContext)
// into the turn/start input array.
//
// MSP has no system-prompt channel, so session-constant instructions
// (BaseInstructions, SystemPromptAppend) ride a LEADING text part ahead
// of the prompt — the host joins multiple text parts in order into the
// turn's prompt. Spec.InitialContext (e.g. recalled agent memory) rides
// its own leading part for the same reason: delivered once with the
// first turn, then living in conversation history.
func promptInput(spec agent.Spec) []TurnInputPart {
	parts := make([]TurnInputPart, 0, 3)
	if text := strings.TrimSpace(spec.BaseInstructions); text != "" {
		parts = append(parts, TurnInputPart{Type: "text", Text: text})
	}
	if text := strings.TrimSpace(spec.SystemPromptAppend); text != "" {
		parts = append(parts, TurnInputPart{Type: "text", Text: text})
	}
	if text := strings.TrimSpace(spec.InitialContext); text != "" {
		parts = append(parts, TurnInputPart{Type: "text", Text: text})
	}
	parts = append(parts, TurnInputPart{Type: "text", Text: spec.Prompt})
	return parts
}

// steerInput wraps follow-up text for turn/steer (same content parts
// as turn/start).
func steerInput(text string) []TurnInputPart {
	return []TurnInputPart{{Type: "text", Text: text}}
}

// mcpServersConfig builds the session/start config.mcpServers map from
// Spec.MCPServers. MSP takes inline servers only as streamableHttp with
// mode "optional" (this slice's contract) or stdio; stdio entries ride
// command + args + env. Entries with neither a URL nor a command are
// skipped: a single bad entry must not silence the whole map.
func mcpServersConfig(servers []agent.MCPServerConfig) map[string]SessionMCPServer {
	if len(servers) == 0 {
		return nil
	}
	out := make(map[string]SessionMCPServer, len(servers))
	for _, s := range servers {
		name := strings.TrimSpace(s.Name)
		if name == "" {
			continue
		}
		switch {
		case strings.TrimSpace(s.URL) != "":
			headers := map[string]string{}
			for k, v := range s.Headers {
				headers[k] = v
			}
			out[name] = SessionMCPServer{
				Transport: "streamableHttp",
				Mode:      "optional",
				URL:       s.URL,
				Headers:   headers,
			}
		case strings.TrimSpace(s.Command) != "":
			out[name] = SessionMCPServer{
				Transport: "stdio",
				Mode:      "optional",
				Command:   s.Command,
				Args:      append([]string(nil), s.Args...),
				Env:       appendMap(nil, s.Env),
			}
		default:
			continue
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func appendMap(dst, src map[string]string) map[string]string {
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

// sessionStartParams builds the JSON-RPC params for `session/start`.
// sessionID and commandID are client-minted UUIDv7 values; workspaceRoot
// must already be canonicalized to its realpath by the caller.
func sessionStartParams(sessionID, commandID string, spec agent.Spec, workspaceRoot string) map[string]any {
	params := map[string]any{
		"sessionId":     sessionID,
		"commandId":     commandID,
		"workspaceRoot": workspaceRoot,
		"providerId":    museProviderID,
	}
	if model := resolveModel(spec); model != "" {
		params["modelId"] = model
	}
	if mode := resolveApprovalMode(spec); mode != "" {
		params["approvalMode"] = mode
	}
	if readOnly := resolveReadOnly(spec); readOnly {
		// The read-only lane pins denyUnmatched regardless of the
		// autonomous default above: no unmatched approval may prompt.
		params["approvalMode"] = "denyUnmatched"
	}
	if mcp := mcpServersConfig(spec.MCPServers); len(mcp) > 0 {
		servers := make(map[string]any, len(mcp))
		for name, srv := range mcp {
			servers[name] = serverToParams(srv)
		}
		params["config"] = map[string]any{"mcpServers": servers}
	}
	return params
}

func serverToParams(srv SessionMCPServer) map[string]any {
	out := map[string]any{
		"transport": srv.Transport,
		"mode":      srv.Mode,
	}
	switch srv.Transport {
	case "streamableHttp":
		out["url"] = srv.URL
		if len(srv.Headers) > 0 {
			out["headers"] = srv.Headers
		}
	case "stdio":
		out["command"] = srv.Command
		if len(srv.Args) > 0 {
			out["args"] = srv.Args
		}
		if len(srv.Env) > 0 {
			out["env"] = srv.Env
		}
	}
	return out
}

// sessionResumeParams builds the JSON-RPC params for `session/resume`.
// excludeItems keeps the resume light; history pages via view/page.
func sessionResumeParams(sessionID, commandID string, spec agent.Spec) map[string]any {
	params := map[string]any{
		"sessionId":    sessionID,
		"commandId":    commandID,
		"excludeItems": true,
	}
	if mcp := mcpServersConfig(spec.MCPServers); len(mcp) > 0 {
		servers := make(map[string]any, len(mcp))
		for name, srv := range mcp {
			servers[name] = serverToParams(srv)
		}
		params["config"] = map[string]any{"mcpServers": servers}
	}
	return params
}

// turnStartParams builds the JSON-RPC params for `turn/start`. The
// fresh turn's turnId derives from commandID. displayText is the
// transcript presentation form (never model-visible).
func turnStartParams(sessionID, commandID string, spec agent.Spec, input []TurnInputPart, workspaceRoots []string) map[string]any {
	wire := make([]map[string]any, 0, len(input))
	for _, part := range input {
		wire = append(wire, turnPartToParams(part))
	}
	params := map[string]any{
		"sessionId": sessionID,
		"commandId": commandID,
		"input":     wire,
		"ifBusy":    "queue",
	}
	if spec.Prompt != "" {
		params["displayText"] = truncateDisplayText(spec.Prompt)
	}
	if len(workspaceRoots) > 0 {
		params["workspaceRoots"] = workspaceRoots
	}
	if effort := resolveEffort(spec.Effort); effort != "" {
		params["reasoningEffort"] = effort
	}
	return params
}

func turnPartToParams(part TurnInputPart) map[string]any {
	out := map[string]any{"type": part.Type}
	switch part.Type {
	case "text":
		out["text"] = part.Text
	case "image":
		out["base64Data"] = part.Base64Data
		out["mediaType"] = part.MediaType
		if part.Width != nil && part.Height != nil {
			out["width"] = *part.Width
			out["height"] = *part.Height
		}
	}
	return out
}

// truncateDisplayText bounds the transcript presentation form so a
// multi-KB prompt does not bloat every transcript row. The model still
// receives the full input; only displayText is cut.
func truncateDisplayText(prompt string) string {
	const limit = 240
	if len(prompt) <= limit {
		return prompt
	}
	return prompt[:limit] + "…"
}

// SpawnPlan is the bag of JSON-RPC params Provider.Spawn assembles up
// front. It exists as a separate value so spec_translation_test.go can
// table-test the full Spec → params translation without touching live
// stdio.
type SpawnPlan struct {
	// SessionID is the client-minted UUIDv7 session identity.
	SessionID string
	// CommandID is the client-minted UUIDv7 idempotency handle for
	// session/start.
	CommandID string
	// SessionStart is the params for `session/start`.
	SessionStart map[string]any
	// TurnStart is the params for the first `turn/start`. The turn's
	// own commandId is minted at send time (one UUIDv7 per turn).
	TurnStart map[string]any
	// PromptInput is the input array for the first turn.
	PromptInput []TurnInputPart
	// WorkspaceRoot is the canonical realpath cwd.
	WorkspaceRoot string
	// ReadOnly marks the read-only lane (serve flags + denyUnmatched).
	ReadOnly bool
	// IgnoredFields lists the agent.Spec fields this harness does NOT
	// translate — surfaced for tests + observability.
	IgnoredFields []SpecFieldNote
}

// SpecFieldNote is one entry in SpawnPlan.IgnoredFields.
type SpecFieldNote struct {
	Field  string
	Reason string
}

// NewSpawnPlan returns the JSON-RPC params for Spawn plus the
// accounting of dropped Spec fields. workspaceRoot must be the
// canonical realpath; the plan records it verbatim.
func NewSpawnPlan(spec agent.Spec, workspaceRoot string) SpawnPlan {
	sessionID := newUUIDv7()
	commandID := newUUIDv7()
	input := promptInput(spec)
	plan := SpawnPlan{
		SessionID:     sessionID,
		CommandID:     commandID,
		SessionStart:  sessionStartParams(sessionID, commandID, spec, workspaceRoot),
		PromptInput:   input,
		WorkspaceRoot: workspaceRoot,
		ReadOnly:      resolveReadOnly(spec),
	}
	plan.TurnStart = turnStartParams(sessionID, newUUIDv7(), spec, input, []string{workspaceRoot})
	plan.IgnoredFields = ignoredSpecFields(spec)
	return plan
}

// ignoredSpecFields documents each agent.Spec field this harness
// either does not translate or consumes via a downstream path.
func ignoredSpecFields(spec agent.Spec) []SpecFieldNote {
	var notes []SpecFieldNote

	// Env: not a JSON-RPC param. Composed into the `muse serve` child
	// environment in muse.go (with META_API_KEY stripped unless set).
	if len(spec.Env) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "Env",
			Reason: "merged into the serve child environment, not a session/start param",
		})
	}

	// AllowedTools / DisallowedTools: MSP has no flat allow/deny list
	// surface on session/start; per-tool permission flows through the
	// approval bridge (PermissionConfig) on approval/requested.
	if len(spec.AllowedTools) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "AllowedTools",
			Reason: "MSP has no flat allow list; tool permission flows through the approval bridge (Spec.PermissionConfig)",
		})
	}
	if len(spec.DisallowedTools) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "DisallowedTools",
			Reason: "MSP has no flat deny list; tool permission flows through the approval bridge (Spec.PermissionConfig)",
		})
	}

	// MCPToolNames: only meaningful for harnesses with an explicit
	// tool-name allowlist surface. MSP auto-discovers tools from the
	// inline mcpServers map.
	if len(spec.MCPToolNames) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "MCPToolNames",
			Reason: "MSP auto-discovers MCP tools from inline mcpServers; explicit names are ignored",
		})
	}

	// MaxTurns: MSP has no per-session turn cap. The runner enforces a
	// wall-clock timeout instead.
	if spec.MaxTurns != nil {
		notes = append(notes, SpecFieldNote{
			Field:  "MaxTurns",
			Reason: "MSP has no maxTurns parameter; runner enforces wall-clock timeout",
		})
	}

	// PermissionConfig: consumed by the approval bridge (approval.go),
	// not by session/start.
	if spec.PermissionConfig != nil {
		notes = append(notes, SpecFieldNote{
			Field:  "PermissionConfig",
			Reason: "consumed by the approval bridge (approval.go), not via session/start params",
		})
	}

	// CodeIntelEnforcement: MSP exposes no tool-callback surface.
	if spec.CodeIntelEnforcement != nil {
		notes = append(notes, SpecFieldNote{
			Field:  "CodeIntelEnforcement",
			Reason: "MSP has no canUseTool callback; silently ignored",
		})
	}

	// ProviderConfig: opaque per-provider extension bag with no
	// defined MSP keys; reserved.
	if len(spec.ProviderConfig) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "ProviderConfig",
			Reason: "MSP defines no providerConfig keys today; field is reserved",
		})
	}

	// SubAgentProvider: coordination flows happen at the runner layer;
	// subagent items surface as events (mapped), not as dispatch.
	if spec.SubAgentProvider != "" {
		notes = append(notes, SpecFieldNote{
			Field:  "SubAgentProvider",
			Reason: "subagent transcripts surface as mapped events; dispatch stays at the runner layer",
		})
	}

	// ResponseSchema: MSP turn/start has no output-schema member;
	// structured output stays on the soft prompt-instruction path.
	if len(spec.ResponseSchema) > 0 {
		notes = append(notes, SpecFieldNote{
			Field:  "ResponseSchema",
			Reason: "MSP turn/start carries no output-schema member; one-shot validation stays soft",
		})
	}
	return notes
}

// nowMillis reports wall-clock milliseconds for UUIDv7 minting.
func nowMillis() int64 {
	return time.Now().UnixMilli()
}
