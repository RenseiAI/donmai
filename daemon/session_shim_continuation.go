package daemon

import (
	"context"
	"fmt"

	"github.com/RenseiAI/donmai/attachwire"
)

// SupportsAdoptedSessionShimContinuationFor reports complete-state support only
// for the exact currently adopted identity, shim, process and generation.
func (d *Daemon) SupportsAdoptedSessionShimContinuationFor(ref SessionShimControlRef) bool {
	controller, err := d.adoptedSessionShimControllerFor(ref)
	return err == nil && controller.SupportsContinuation()
}

// InspectAdoptedSessionShimContinuationFor proxies one bounded read using the caller context
// through exact adoption authority. It holds no daemon lock during local-wire
// I/O and rechecks the captured authority before releasing state, so a replaced
// controller cannot return a stale checkpoint as current authority.
func (d *Daemon) InspectAdoptedSessionShimContinuationFor(ctx context.Context, ref SessionShimControlRef, selected string) (attachwire.ContinuationCheckpoint, error) {
	controller, err := d.adoptedSessionShimControllerFor(ref)
	if err != nil {
		return attachwire.ContinuationCheckpoint{}, err
	}
	checkpoint, err := controller.InspectContinuation(ctx, selected)
	if err != nil {
		return attachwire.ContinuationCheckpoint{}, err
	}
	current, err := d.adoptedSessionShimControllerFor(ref)
	if err != nil {
		return attachwire.ContinuationCheckpoint{}, err
	}
	if current != controller {
		return attachwire.ContinuationCheckpoint{}, fmt.Errorf("session shim: continuation authority was replaced during capture")
	}
	return checkpoint, nil
}
