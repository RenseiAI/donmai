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
	// Provider is the agent harness the emit runs on (claude, etc.). It must be
	// a HarnessProvider so the shared one-shot lane (agent.SpawnComplete) can
	// read its manifest; every real provider satisfies this after the two-axis
	// harness split.
	Provider agent.HarnessProvider
	// Model is the optional model id; empty falls back to the provider default.
	Model string
}

// providerEmitter is the production Emitter. It runs ONE constrained completion
// through the shared one-shot lane (agent.SpawnComplete) and returns the final
// assistant text.
//
// A KG emit IS a one-shot: an autonomous, tool-less, single-turn completion with
// the platform extractionSystemPrompt as the system instruction and the
// observation as the user turn. SpawnComplete builds exactly that Spec
// (Autonomous, MaxTurns=1, no tools), drives the session to its terminal
// ResultEvent, accumulates the assistant text (complete-message chunks; the JSON
// arrives as assistant_text, never the CLI completion banner), and returns it —
// replacing the hand-rolled spawn+drain this package used to carry. Under
// host-session this is an agentic `claude -p` invocation; under a native harness
// (gemini/ollama) it is a raw completion. The executor parses + validates the
// returned text.
//
// We deliberately do NOT pass ResponseSchema: kgextract owns the {nodes,edges}
// shape and validates it in parse.go (the double-parse-then-drop posture), and
// the extractionSystemPrompt already instructs JSON-only output.
type providerEmitter struct {
	provider agent.HarnessProvider
	model    string
}

// NewProviderEmitter builds a providerEmitter. Returns an error when no provider
// is configured (the caller cannot run a real emit without one).
func NewProviderEmitter(cfg ProviderEmitterConfig) (Emitter, error) {
	if cfg.Provider == nil {
		return nil, errors.New("kgextract: provider emitter requires a non-nil agent.HarnessProvider")
	}
	return &providerEmitter{
		provider: cfg.Provider,
		model:    cfg.Model,
	}, nil
}

// Emit runs one constrained completion via the shared lane and returns the
// accumulated assistant text. A spawn failure, provider error, failed result,
// or empty output is folded into an error the executor records as a
// per-observation failure.
func (e *providerEmitter) Emit(ctx context.Context, systemPrompt, userContent string) (string, error) {
	res, err := agent.SpawnComplete(ctx, e.provider, agent.OneShotRequest{
		System:   systemPrompt,
		Messages: []agent.Message{{Role: "user", Content: userContent}},
		Model:    e.model,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("kgextract: emit context: %w", ctxErr)
		}
		return "", fmt.Errorf("kgextract: emit: %w", err)
	}
	out := strings.TrimSpace(res.Text)
	if out == "" {
		// SpawnComplete treats empty-but-successful output as a soft result;
		// kgextract requires text to parse, so an empty emit is a failure here.
		return "", errors.New("kgextract: emit produced no assistant text")
	}
	return out, nil
}
