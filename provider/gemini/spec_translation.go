package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// requestPart is one chunk of a Gemini Content message. We only emit
// {Text} parts in v0.1 — function-call / file parts come later when
// SupportsToolPlugins flips on.
type requestPart struct {
	Text string `json:"text"`
}

// requestContent mirrors google.generativelanguage.Content. The Role
// must be "user" or "model"; we only emit "user" in spawn (the
// streaming response carries the assistant turn).
type requestContent struct {
	Role  string        `json:"role,omitempty"`
	Parts []requestPart `json:"parts"`
}

// requestSystemInstruction mirrors google.generativelanguage's
// system_instruction field. Optional.
type requestSystemInstruction struct {
	Parts []requestPart `json:"parts"`
}

// requestGenerationConfig mirrors GenerationConfig. We only set
// MaxOutputTokens today; thinkingBudget and friends arrive when
// SupportsReasoningEffort flips on.
type requestGenerationConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

// requestBody is the wire shape POSTed to streamGenerateContent.
type requestBody struct {
	Contents          []requestContent          `json:"contents"`
	SystemInstruction *requestSystemInstruction `json:"systemInstruction,omitempty"`
	GenerationConfig  *requestGenerationConfig  `json:"generationConfig,omitempty"`
}

// buildRequestBody translates an agent.Spec into the JSON bytes posted
// to streamGenerateContent. Empty Prompt is rejected — a session must
// have a directive for the model to act on.
//
// SystemPromptAppend (sourced from RepositoryConfig.systemPrompt) is
// folded into systemInstruction. BaseInstructions are honoured the
// same way (they merge so providers with NeedsBaseInstructions=true
// can still target a Gemini sub-agent).
func buildRequestBody(spec agent.Spec) ([]byte, error) {
	if strings.TrimSpace(spec.Prompt) == "" {
		return nil, fmt.Errorf("gemini: empty prompt")
	}

	body := requestBody{
		Contents: []requestContent{{
			Role:  "user",
			Parts: []requestPart{{Text: spec.Prompt}},
		}},
	}

	if sys := buildSystemInstruction(spec); sys != "" {
		body.SystemInstruction = &requestSystemInstruction{
			Parts: []requestPart{{Text: sys}},
		}
	}

	if spec.MaxTurns != nil && *spec.MaxTurns > 0 {
		// MaxTurns is a coarse stop-knob; map it into a generous
		// MaxOutputTokens so a runaway response cannot exceed the
		// caller's intent. 2048 tokens per turn is a safe default; it
		// keeps a 5-turn cap below the 10k budget Gemini-flash
		// honours by default.
		body.GenerationConfig = &requestGenerationConfig{
			MaxOutputTokens: 2048 * (*spec.MaxTurns),
		}
	}

	return json.Marshal(body)
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
