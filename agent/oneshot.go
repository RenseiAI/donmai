package agent

// oneshot.go — the one-shot / structured-completion lane (Phase 4a).
//
// See 02-two-axis-architecture.md §2.4 + §3.5. A one-shot is a non-interactive,
// optionally schema-constrained completion: "give me a JSON value matching this
// schema, prefer the user's subscription (≈$0), no agent loop." It is the shared
// primitive behind KG-extraction and arch-intel drift (which today each hand-roll
// their own single-turn LLM call) — Phase 5 re-homes them onto this lane.
//
// Two strictness postures, chosen by the resolved (auth, cell) — never a
// per-provider default (§3.5):
//
//   - SUBSCRIPTION / BringsOwnAuth cells deliver SOFT JSON via SpawnComplete:
//     drive the harness to terminal, append the schema instruction, then
//     validate-repair-drop on !SchemaOK. Cheap, not server-enforced. This is the
//     accepted KG posture (ADR-2026-06-03: parse-or-drop). Any HarnessProvider
//     gets this lane for free — that is what SpawnComplete provides.
//   - KEYED (byok/metered) cells deliver STRICT native structured: the raw
//     harness sets the protocol's structured primitive (responseSchema /
//     json_schema / response_format) and the model is constrained server-side.
//     Such a harness implements OneShotProvider.Complete DIRECTLY (Phase 4b).
//
// Per-token coalescing is a non-issue here: SpawnComplete reads to terminal and
// returns Structured; it never rides streaming deltas, so the "providers drop
// partial deltas" anti-spam rule is preserved — AssistantTextEvent already
// carries complete-message chunks.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Message is one turn in a one-shot request. Role is "user" or "assistant"
// (empty defaults to "user"). Kept minimal — the one-shot consumers (KG,
// arch-intel) are single-prompt; multi-turn is supported but rarely needed.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OneShotRequest is a schema-constrained, non-interactive completion request.
// Endpoint is the resolved (company, model, host, auth) binding — the same
// binding currency the interactive Spawn path uses (Spec.Endpoint). A nil
// Endpoint falls back to the harness/env default, mirroring Spec.
type OneShotRequest struct {
	System         string           `json:"system,omitempty"`
	Messages       []Message        `json:"messages"`
	Endpoint       *EndpointBinding `json:"endpoint,omitempty"`
	Effort         EffortLevel      `json:"effort,omitempty"`
	ResponseSchema json.RawMessage  `json:"responseSchema,omitempty"` // nil => free text
	MaxTokens      int              `json:"maxTokens,omitempty"`      // honored by native Complete; CLI harnesses self-limit
}

// OneShotResult is the projection of a completion onto a (text, structured)
// pair. Structured is populated iff ResponseSchema was set; SchemaOK reports
// whether it extracted AND validated — false means the caller retries, repairs,
// or drops (the bounded-injection-to-a-junk-node posture). Cost is nil for
// BringsOwnAuth cells (the user's own subscription/login pays — nothing metered
// to attribute).
type OneShotResult struct {
	Text          string          `json:"text"`
	Structured    json.RawMessage `json:"structured,omitempty"`
	SchemaOK      bool            `json:"schemaOk"`
	Cost          *CostData       `json:"cost,omitempty"`
	TransportUsed TransportKind   `json:"transportUsed,omitempty"`
}

// OneShotProvider is the schema-constrained completion lane. A harness MAY
// implement it directly for native strict structured output (raw over
// gemini/ollama with NativeJSONMode); CLI/pty harnesses instead get the soft
// lane for free by delegating Complete to SpawnComplete(ctx, self, req). Every
// OneShotProvider in this system is also a HarnessProvider, so the manifest is
// available via that interface — OneShotProvider deliberately carries only the
// completion verb.
type OneShotProvider interface {
	Complete(ctx context.Context, req OneShotRequest) (OneShotResult, error)
}

// SpawnComplete gives any HarnessProvider the one-shot lane for free: spawn an
// interactive session, drive it to terminal, and project the assistant output
// onto a OneShotResult. Native-JSON harnesses (raw) override with a direct
// Complete(); everything else (claude-code, codex, amp, antigravity, opencode,
// stub) rides this soft path.
//
// SOFT JSON: when req.ResponseSchema is set, the schema instruction is appended
// to the prompt and the collected text is run through extractAndValidate. A
// !SchemaOK result is NOT an error — it is the documented validate-repair-drop
// signal returned on the OneShotResult for the caller to handle.
//
// A session that ends with a failure ResultEvent, an ErrorEvent, or no terminal
// event at all IS an error (distinct from a successful-but-unparseable result).
func SpawnComplete(ctx context.Context, h HarnessProvider, req OneShotRequest) (OneShotResult, error) {
	spec := specFromOneShot(req)
	sess, err := h.Spawn(ctx, spec)
	if err != nil {
		return OneShotResult{}, fmt.Errorf("oneshot: spawn: %w", err)
	}
	// Always stop the session, even if ctx was cancelled mid-drive — give Stop
	// its own bounded, non-cancelled context so cleanup (SIGTERM→grace→SIGKILL)
	// actually runs rather than returning instantly on the dead parent ctx.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = sess.Stop(stopCtx)
	}()

	var text strings.Builder
	var cost *CostData
	var sawResult bool
	var failErr error
	for ev := range sess.Events() {
		switch e := ev.(type) {
		case AssistantTextEvent:
			// Complete-message chunks only (the provider anti-spam rule already
			// guarantees this) — safe to concatenate without delta coalescing.
			text.WriteString(e.Text)
		case ResultEvent:
			sawResult = true
			cost = e.Cost
			if !e.Success {
				failErr = fmt.Errorf("oneshot: session failed: %s", strings.Join(e.Errors, "; "))
			}
		case ErrorEvent:
			failErr = fmt.Errorf("oneshot: session error: %s", e.Message)
		}
	}
	if failErr != nil {
		return OneShotResult{}, failErr
	}
	if !sawResult {
		return OneShotResult{}, fmt.Errorf("oneshot: session ended without a terminal result event")
	}

	out := OneShotResult{
		Text:          text.String(),
		Cost:          cost,
		TransportUsed: h.Manifest().Caps.Transport,
	}
	if len(req.ResponseSchema) > 0 {
		out.Structured, out.SchemaOK = extractAndValidate(out.Text, req.ResponseSchema)
	}
	return out, nil
}

// specFromOneShot translates a OneShotRequest into the interactive Spec
// SpawnComplete drives. The schema instruction (if any) is appended to the
// prompt — CLI harnesses have no JSON-schema flag, so soft-instruct is the only
// lever (§3.5). MaxTurns is pinned to 1: a one-shot is a single completion, not
// an agent loop.
func specFromOneShot(req OneShotRequest) Spec {
	prompt := flattenMessages(req.Messages)
	if len(req.ResponseSchema) > 0 {
		if prompt != "" {
			prompt += "\n\n"
		}
		prompt += schemaInstruction(req.ResponseSchema)
	}
	one := 1
	spec := Spec{
		Prompt:             prompt,
		SystemPromptAppend: req.System,
		Autonomous:         true,
		Effort:             req.Effort,
		MaxTurns:           &one,
	}
	if req.Endpoint != nil {
		spec.Endpoint = req.Endpoint
		spec.Model = req.Endpoint.Model
	}
	return spec
}

// flattenMessages renders a turn list into a single prompt string. A single
// message is returned verbatim (the common KG/arch-intel single-turn case);
// multi-turn is rendered "role: content" separated by blank lines.
func flattenMessages(msgs []Message) string {
	if len(msgs) == 1 {
		return msgs[0].Content
	}
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		role := m.Role
		if role == "" {
			role = "user"
		}
		parts = append(parts, role+": "+m.Content)
	}
	return strings.Join(parts, "\n\n")
}

// schemaInstruction is the soft-JSON directive appended to the prompt for CLI
// harnesses. It asks for a raw JSON value, no prose, no code fences.
func schemaInstruction(schema json.RawMessage) string {
	return "Respond ONLY with a single JSON value that conforms to this JSON Schema. " +
		"Output no prose, no explanation, and no Markdown code fences — just the raw JSON:\n" +
		string(schema)
}

// extractAndValidate extracts the first balanced JSON value from free text and,
// when a schema is provided, validates it. Returns (rawJSON, true) only when a
// well-formed JSON value was found AND (no schema, or the value validates).
// On any failure it returns (nil, false) — the validate-repair-drop signal.
func extractAndValidate(text string, schema json.RawMessage) (json.RawMessage, bool) {
	extracted, ok := extractJSONValue(text)
	if !ok {
		return nil, false
	}
	if len(schema) == 0 {
		return extracted, true
	}
	if !validateAgainstSchema(extracted, schema) {
		return nil, false
	}
	return extracted, true
}

// extractJSONValue finds the first balanced top-level JSON object or array in a
// string, tolerating surrounding prose and Markdown code fences (models often
// wrap JSON despite instructions). It respects string literals and escapes so a
// brace inside a string never throws off the depth count, and confirms the
// candidate with json.Valid.
func extractJSONValue(text string) (json.RawMessage, bool) {
	start := strings.IndexAny(text, "{[")
	if start < 0 {
		return nil, false
	}
	open := text[start]
	closeCh := byte('}')
	if open == '[' {
		closeCh = ']'
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				candidate := text[start : i+1]
				if json.Valid([]byte(candidate)) {
					return json.RawMessage(candidate), true
				}
				return nil, false
			}
		}
	}
	return nil, false
}

// validateAgainstSchema reports whether instance validates against the given
// JSON Schema. A malformed schema or instance returns false (fail-closed for
// the SchemaOK signal — an unparseable schema can't certify anything).
func validateAgainstSchema(instance, schema json.RawMessage) bool {
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return false
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("oneshot://response-schema", schemaDoc); err != nil {
		return false
	}
	sch, err := c.Compile("oneshot://response-schema")
	if err != nil {
		return false
	}
	instDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(instance))
	if err != nil {
		return false
	}
	return sch.Validate(instDoc) == nil
}
