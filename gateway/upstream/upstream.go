// Package upstream holds the outbound backends the gateway dials. Each backend
// ENCODES the canonical IR out to a provider wire and DECODES the reply back to
// IR (via translate/). M1 ships the OpenAI-compatible backend
// (openai_compat.go): direct OpenAI plus any OpenAI-compat URL (aggregators
// like OpenRouter / Vercel AI Gateway / LiteLLM, and self-hosted vLLM / Ollama
// /v1). Anthropic / Gemini / Class-E / sanctioned Class-S backends land in
// later milestones behind this same interface.
package upstream

import (
	"context"
	"fmt"

	"github.com/RenseiAI/donmai/gateway/ir"
	"github.com/RenseiAI/donmai/gateway/pool"
)

// Upstream is one outbound backend. Invoke translates req to the backend wire,
// dials with cred, and returns the Outcome. Streaming and non-streaming both
// flow through Invoke: it mirrors req.Stream (a streaming request yields
// Outcome.Stream; a non-streaming request yields Outcome.Response), so a
// streaming client never buffers a whole response on the hot path (08 §4).
type Upstream interface {
	// Name is the company/provider identity for cost attribution (the primary
	// attribution key alongside host="gateway").
	Name() string
	// Invoke performs the exchange. On a non-2xx HTTP status it returns an
	// Outcome carrying that Status (so the caller can drive pool cooldown) and
	// a non-nil error; on success err is nil and exactly one of
	// Outcome.Stream / Outcome.Response is set.
	Invoke(ctx context.Context, req ir.Request, cred pool.Credential) (Outcome, error)
}

// Outcome bundles an upstream exchange result. Status is always the observed
// HTTP status (0 for a pre-dial/transport failure). On success, Stream is set
// for a streaming request and Response for a non-streaming one.
type Outcome struct {
	Status   int
	Stream   *ir.Stream
	Response *ir.Response
}

// Error is a typed upstream failure carrying the HTTP status so the gateway can
// map it onto a client status and a pool-cooldown decision.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("gateway/upstream: status %d: %s", e.Status, e.Message)
}
