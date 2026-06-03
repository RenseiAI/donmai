package kgextract

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/RenseiAI/donmai/agent"
)

// Emitter runs ONE constrained, non-interactive LLM turn and returns the final
// assistant text. It is the seam the executor depends on; the production
// implementation (providerEmitter) drives an agent.Provider, and tests inject a
// deterministic stub. Keeping this an interface (rather than calling a provider
// directly) mirrors codesurvival's gitRunner/tsRunner injection idiom and lets
// the executor's per-observation logic be unit-tested without a live CLI.
//
// systemPrompt instructs the model to emit ONLY {nodes,edges} JSON;
// userContent is the observation content. The returned string is the raw
// assistant text (the executor parses + validates it). An error means the turn
// could not complete (spawn failure, provider error, no assistant text); the
// executor folds that into a per-observation failure.
type Emitter interface {
	Emit(ctx context.Context, systemPrompt, userContent string) (string, error)
}

// ProviderEmitterConfig configures a providerEmitter.
type ProviderEmitterConfig struct {
	// Provider is the agent runtime the emit runs on (claude, etc.).
	Provider agent.Provider
	// Model is the optional model id; empty falls back to the provider default.
	Model string
	// Cwd is the working directory the constrained turn runs in. Empty is
	// acceptable for a pure-completion emit (no filesystem work expected).
	Cwd string
}

// providerEmitter is the production Emitter. It drives a single constrained turn
// through an agent.Provider and reads the final assistant text from the event
// stream.
//
// ── PROVIDER-EMIT APPROACH (the novel core; e2e-verified later) ──────────────
// The agent.Provider interface is built for full agentic SESSIONS, not raw
// single-shot completions: Spawn(ctx, spec) returns a Handle whose Events()
// channel emits InitEvent → zero+ assistant/tool events → a terminal
// ResultEvent (or ErrorEvent), then closes. There is no dedicated
// "one constrained completion" entry point.
//
// So we drive the SIMPLEST correct shape the interface supports:
//
//  1. Build an autonomous, non-interactive Spec with:
//     - Prompt        = the observation content (the user turn).
//     - SystemPromptAppend = the platform extractionSystemPrompt (instructs
//     the model to emit ONLY {nodes,edges} JSON).
//     - Autonomous=true, MaxTurns=1 — a single round-trip, no tool loop.
//     - No AllowedTools / no MCP servers — a constrained, tool-less emit.
//     Under host-session this becomes an agentic `claude -p` invocation
//     (stream-json); under local it becomes a raw completion. The provider
//     implementation chosen by the platform's authMode decides the transport;
//     this emitter is transport-agnostic.
//  2. Consume Events() to completion, accumulating every AssistantTextEvent.Text.
//     The claude CLI streams assistant text as one or more chunks; the final
//     assistant text is the concatenation. (We intentionally do NOT use
//     ResultEvent.Message — for claude that is the CLI's completion banner, not
//     the model's emitted JSON; the JSON arrives as assistant_text.)
//  3. On a terminal ErrorEvent, or a ResultEvent with Success=false, or no
//     assistant text at all, return an error so the executor records a
//     per-observation failure.
//
// DEVIATION from a clean single-shot: MaxTurns=1 + Autonomous + empty tool list
// is the closest the session-oriented Provider gets to a constrained emit; a
// true raw-completion entry point would be cleaner but the interface does not
// expose one today. This is the documented seam to verify against a live daemon.
type providerEmitter struct {
	provider agent.Provider
	model    string
	cwd      string
}

// NewProviderEmitter builds a providerEmitter. Returns an error when no provider
// is configured (the caller cannot run a real emit without one).
func NewProviderEmitter(cfg ProviderEmitterConfig) (Emitter, error) {
	if cfg.Provider == nil {
		return nil, errors.New("kgextract: provider emitter requires a non-nil agent.Provider")
	}
	return &providerEmitter{
		provider: cfg.Provider,
		model:    cfg.Model,
		cwd:      cfg.Cwd,
	}, nil
}

// maxTurnsOne is the single-round-trip cap for the constrained emit, addressable
// so it can be taken by reference for Spec.MaxTurns.
var maxTurnsOne = 1

// Emit drives one constrained turn and returns the accumulated assistant text.
func (e *providerEmitter) Emit(ctx context.Context, systemPrompt, userContent string) (string, error) {
	spec := agent.Spec{
		Prompt:             userContent,
		Cwd:                e.cwd,
		Autonomous:         true,
		SystemPromptAppend: systemPrompt,
		MaxTurns:           &maxTurnsOne,
		Model:              e.model,
		// No AllowedTools / MCPServers: a tool-less, constrained completion.
	}

	handle, err := e.provider.Spawn(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("kgextract: spawn emit: %w", err)
	}
	// Best-effort stop so a child process never lingers past this turn. Safe to
	// call after the events channel closes (idempotent per the Handle contract).
	defer func() { _ = handle.Stop(context.Background()) }()

	var (
		text     strings.Builder
		gotError string
		failed   bool
	)
	for ev := range handle.Events() {
		switch v := ev.(type) {
		case agent.AssistantTextEvent:
			text.WriteString(v.Text)
		case agent.ErrorEvent:
			// Non-recoverable provider error; the channel closes after this.
			gotError = v.Message
			failed = true
		case agent.ResultEvent:
			if !v.Success {
				failed = true
				if len(v.Errors) > 0 {
					gotError = strings.Join(v.Errors, "; ")
				} else if v.Message != "" {
					gotError = v.Message
				}
			}
		}
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("kgextract: emit context: %w", ctxErr)
	}
	if failed {
		if gotError == "" {
			gotError = "provider reported failure"
		}
		return "", fmt.Errorf("kgextract: emit failed: %s", gotError)
	}
	out := strings.TrimSpace(text.String())
	if out == "" {
		return "", errors.New("kgextract: emit produced no assistant text")
	}
	return out, nil
}
