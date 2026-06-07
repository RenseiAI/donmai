package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// requestPart is one chunk of a Gemini Content message. A part carries
// EITHER text, a functionCall (model → caller tool request), or a
// functionResponse (caller → model tool result). The pointer fields are
// omitempty so a text-only part serializes as {"text":"..."}.
type requestPart struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

// functionCall mirrors the Gemini functionCall part. Gemini-3 requires
// an id on every call so the matching functionResponse can be paired.
type functionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// functionResponse mirrors the Gemini functionResponse part. The id
// must match the functionCall.id from the model turn (Gemini-3).
type functionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// requestContent mirrors google.generativelanguage.Content. Role is
// "user" or "model" — the public generateContent REST API accepts only
// those two values (the legacy "function"/"tool" roles were removed).
// The tool-result turn (functionResponse parts) therefore rides as a
// "user" turn. The maintained conversation history is a slice of these.
type requestContent struct {
	Role  string        `json:"role,omitempty"`
	Parts []requestPart `json:"parts"`
}

// requestSystemInstruction mirrors system_instruction. Optional.
type requestSystemInstruction struct {
	Parts []requestPart `json:"parts"`
}

// thinkingConfig mirrors GenerationConfig.thinkingConfig. Gemini exposes
// two mutually-exclusive knobs:
//
//   - ThinkingLevel ("minimal"|"low"|"medium"|"high") on the 3.x family.
//   - ThinkingBudget (token int; -1 dynamic, 0 off) on the 2.5 family.
//
// Exactly one is populated per model; the other stays nil/empty so the
// wire payload only carries the knob the target model understands.
type thinkingConfig struct {
	ThinkingLevel  string `json:"thinkingLevel,omitempty"`
	ThinkingBudget *int   `json:"thinkingBudget,omitempty"`
}

// requestGenerationConfig mirrors GenerationConfig. MaxOutputTokens caps
// a single response; thinkingConfig carries the reasoning-effort knob.
// ResponseMimeType + ResponseSchema carry Gemini's native structured-output
// primitive (the one-shot lane, P4b): when set, the model is constrained
// server-side to emit JSON matching the schema. Gemini requires the mime type
// to be "application/json" whenever a responseSchema is supplied.
type requestGenerationConfig struct {
	MaxOutputTokens  int             `json:"maxOutputTokens,omitempty"`
	ThinkingConfig   *thinkingConfig `json:"thinkingConfig,omitempty"`
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
}

// functionDeclaration mirrors one entry in tools[].functionDeclarations.
// Parameters is an OpenAPI-subset JSON schema object describing the
// tool's arguments.
type functionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// requestTool mirrors one entry in the request body's tools array.
type requestTool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations,omitempty"`
}

// functionCallingConfig mirrors toolConfig.functionCallingConfig. Mode
// is AUTO (model decides), ANY (must call a function), or NONE.
type functionCallingConfig struct {
	Mode string `json:"mode,omitempty"`
}

// requestToolConfig mirrors the request body's toolConfig field.
type requestToolConfig struct {
	FunctionCallingConfig *functionCallingConfig `json:"functionCallingConfig,omitempty"`
}

// requestBody is the wire shape POSTed to generateContent.
type requestBody struct {
	Contents          []requestContent          `json:"contents"`
	SystemInstruction *requestSystemInstruction `json:"systemInstruction,omitempty"`
	GenerationConfig  *requestGenerationConfig  `json:"generationConfig,omitempty"`
	Tools             []requestTool             `json:"tools,omitempty"`
	ToolConfig        *requestToolConfig        `json:"toolConfig,omitempty"`
}

// spawnPlan is the immutable per-session request scaffold. The Handle
// clones the static fields onto each generateContent turn and swaps in
// the running contents history.
type spawnPlan struct {
	// systemInstruction is the folded BaseInstructions + SystemPromptAppend.
	systemInstruction *requestSystemInstruction
	// generationConfig carries MaxOutputTokens + thinkingConfig.
	generationConfig *requestGenerationConfig
	// tools holds the functionDeclarations built from AllowedTools +
	// MCP servers. nil when the session declares no tools.
	tools []requestTool
	// toolConfig carries functionCallingConfig.mode. nil when no tools.
	toolConfig *requestToolConfig
	// initialContents is the opening user turn (the prompt).
	initialContents []requestContent
}

// buildSpawnPlan translates an agent.Spec + resolved model into the
// per-session request scaffold. Empty Prompt is rejected — a session
// must carry a directive for the model to act on.
//
// SystemPromptAppend (RepositoryConfig.systemPrompt) and BaseInstructions
// are folded into systemInstruction. AllowedTools + MCPServers become
// functionDeclarations; reasoning-effort becomes thinkingConfig sized to
// the model family; ProviderConfig.maxOutputTokens / MaxTurns cap output.
func buildSpawnPlan(spec agent.Spec, model string) (spawnPlan, error) {
	if strings.TrimSpace(spec.Prompt) == "" {
		return spawnPlan{}, fmt.Errorf("gemini: empty prompt")
	}

	plan := spawnPlan{
		initialContents: []requestContent{{
			Role:  "user",
			Parts: []requestPart{{Text: spec.Prompt}},
		}},
	}

	if sys := buildSystemInstruction(spec); sys != "" {
		plan.systemInstruction = &requestSystemInstruction{
			Parts: []requestPart{{Text: sys}},
		}
	}

	if gc := buildGenerationConfig(spec, model); gc != nil {
		plan.generationConfig = gc
	}

	if tools := toolsFromSpec(spec); len(tools) > 0 {
		plan.tools = tools
		plan.toolConfig = &requestToolConfig{
			FunctionCallingConfig: &functionCallingConfig{
				Mode: functionCallingMode(spec),
			},
		}
	}

	return plan, nil
}

// buildGenerationConfig assembles the generationConfig. Returns nil when
// neither an output cap nor a thinking knob is requested.
func buildGenerationConfig(spec agent.Spec, model string) *requestGenerationConfig {
	gc := &requestGenerationConfig{}
	set := false

	if maxOut := maxOutputTokens(spec); maxOut > 0 {
		gc.MaxOutputTokens = maxOut
		set = true
	}

	if tc := thinkingConfigFor(spec, model); tc != nil {
		gc.ThinkingConfig = tc
		set = true
	}

	// Native structured output (P4b one-shot lane). When the spec carries a
	// ResponseSchema, constrain the model server-side to JSON matching it.
	// Gemini requires responseMimeType="application/json" alongside the schema.
	// One-shot requests carry no tools (functionCalling + responseSchema are
	// mutually exclusive on Gemini), so there is no conflict here.
	if len(spec.ResponseSchema) > 0 {
		gc.ResponseMimeType = "application/json"
		gc.ResponseSchema = spec.ResponseSchema
		set = true
	}

	if !set {
		return nil
	}
	return gc
}

// maxOutputTokens resolves the per-response output cap. Precedence:
// ProviderConfig.maxOutputTokens (platform-resolved from the model
// catalog) → MaxTurns * 2048 (coarse runaway guard). Zero means "use the
// model default".
func maxOutputTokens(spec agent.Spec) int {
	if v, ok := intFromProviderConfig(spec.ProviderConfig, "maxOutputTokens"); ok && v > 0 {
		return v
	}
	if spec.MaxTurns != nil && *spec.MaxTurns > 0 {
		// MaxTurns is a coarse stop-knob; map it into a generous
		// MaxOutputTokens so a runaway response cannot exceed the
		// caller's intent (2048 tokens/turn).
		return 2048 * (*spec.MaxTurns)
	}
	return 0
}

// buildSystemInstruction concatenates BaseInstructions and
// SystemPromptAppend with a blank-line separator. Either can be empty;
// both empty returns "".
func buildSystemInstruction(spec agent.Spec) string {
	parts := make([]string, 0, 2)
	if s := strings.TrimSpace(spec.BaseInstructions); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(spec.SystemPromptAppend); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n")
}

// intFromProviderConfig reads an int-valued key from the opaque
// ProviderConfig map. JSON decoding yields float64 for numbers, so both
// int and float64 are accepted.
func intFromProviderConfig(pc map[string]any, key string) (int, bool) {
	if len(pc) == 0 {
		return 0, false
	}
	switch v := pc[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	default:
		return 0, false
	}
}
