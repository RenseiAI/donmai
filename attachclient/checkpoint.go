package attachclient

import (
	"context"
	"fmt"

	"github.com/RenseiAI/donmai/attachwire"
)

// ContinuationSource is an additive complete-state provider. Implementations
// explicitly select a schema and preserve the existing Session methods.
type ContinuationSource interface {
	SupportsContinuation() bool
	InspectContinuation(ctx context.Context, selected string) (attachwire.ContinuationCheckpoint, error)
}

// OpenContinuation captures complete state then requests unchanged canonical
// frames strictly after its anchor. A ring eviction fails without returning a
// checkpoint or advancing any consumer state; callers may request a fresh
// checkpoint. This avoids presenting a partial state-plus-tail handoff.
func OpenContinuation(ctx context.Context, session Session, selected string) (attachwire.ContinuationCheckpoint, Subscription, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return attachwire.ContinuationCheckpoint{}, nil, err
	}
	if selected != attachwire.ContinuationSchema {
		return attachwire.ContinuationCheckpoint{}, nil, fmt.Errorf("attachclient: unsupported continuation schema")
	}
	source, ok := session.(ContinuationSource)
	if !ok || !source.SupportsContinuation() {
		return attachwire.ContinuationCheckpoint{}, nil, fmt.Errorf("attachclient: session has no complete continuation support")
	}
	c, err := source.InspectContinuation(ctx, selected)
	if err != nil {
		return attachwire.ContinuationCheckpoint{}, nil, fmt.Errorf("attachclient: checkpoint: %w", err)
	}
	if _, err := c.Encode(selected); err != nil {
		return attachwire.ContinuationCheckpoint{}, nil, fmt.Errorf("attachclient: invalid checkpoint: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return attachwire.ContinuationCheckpoint{}, nil, err
	}
	sub, err := session.Subscribe(c.AtSeq)
	if err != nil {
		return attachwire.ContinuationCheckpoint{}, nil, fmt.Errorf("attachclient: checkpoint tail unavailable: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = sub.Close()
		return attachwire.ContinuationCheckpoint{}, nil, err
	}
	return c, sub, nil
}
