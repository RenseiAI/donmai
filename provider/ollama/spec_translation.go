package ollama

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/RenseiAI/agentfactory-tui/agent"
)

// chatMessage is one entry in the Ollama /api/chat `messages` array.
//
// Ollama's role vocabulary mirrors OpenAI's: "system" | "user" |
// "assistant" | "tool". This provider only emits "system" and "user"
// messages because SupportsToolPlugins is false; tool / assistant
// history would only matter for resume, which is also not advertised.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the wire shape POST /api/chat consumes. We map only
// the fields that mean something for the v0.1 capability set; advanced
// knobs (think, format, options.num_ctx, …) are left unset so Ollama
// applies its model defaults. Adding a knob requires advertising the
// matching Capability.
//
// Reference: https://github.com/ollama/ollama/blob/main/docs/api.md#generate-a-chat-completion
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

// buildChatRequest translates an agent.Spec into a serialized Ollama
// chat request body. Returns the JSON bytes ready for the request body
// and any encoding error.
//
// Capability mapping:
//
//   - Spec.SystemPromptAppend prepends a system message when non-empty
//     so the runner-injected instructions (RepositoryConfig.systemPrompt)
//     reach the model.
//   - Spec.Prompt is the user message. The provider rejects empty
//     prompts because POST /api/chat with zero messages is undefined
//     behavior server-side and silently produces an empty response.
//   - Stream is always true; the Handle is built around NDJSON streaming
//     and the synchronous response shape would force buffering.
//   - Spec.AllowedTools / DisallowedTools / MCPServers / PermissionConfig /
//     Effort / CodeIntelEnforcement are silently ignored — see the
//     capability flags in ollama.go.
func buildChatRequest(model string, spec agent.Spec) ([]byte, error) {
	if spec.Prompt == "" {
		return nil, fmt.Errorf("ollama: Spec.Prompt required (cannot send empty messages array)")
	}
	msgs := make([]chatMessage, 0, 2)
	if sp := spec.SystemPromptAppend; sp != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: sp})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: spec.Prompt})

	req := chatRequest{
		Model:    model,
		Messages: msgs,
		Stream:   true,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Disable HTML escaping so prompts containing <, >, & flow through
	// unchanged — relevant for code prompts. JSON validity is unaffected.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&req); err != nil {
		return nil, fmt.Errorf("encode chat request: %w", err)
	}
	// Encoder appends a trailing newline; the Ollama server tolerates
	// it but trim for tidiness so the bytes match what tests assert
	// against literal JSON fixtures.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
