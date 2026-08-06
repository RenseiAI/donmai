package ptycli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// DeliverSeed applies a prompt profile's PTY-seed delivery after the child is
// spawned and before its handle is returned to the runner. It writes the exact
// adapted bytes followed by one Enter key, retries short writes, and stops the
// child if cancellation or a write failure prevents complete delivery.
//
// Callers must pass the adapted Spec.Prompt, never an out-of-band queue field;
// this keeps the bytes on the same authority/receipt path as every other
// native prompt surface.
func DeliverSeed(ctx context.Context, handle agent.Handle, session agent.InteractiveSession, seed string) error {
	if seed == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- writeSeedBytes(session, seed)
	}()

	select {
	case err := <-writeDone:
		if err == nil {
			return nil
		}
		return stopAfterSeedFailure(handle, err)
	case <-ctx.Done():
		return stopAfterSeedFailure(handle, ctx.Err())
	}
}

func writeSeedBytes(session agent.InteractiveSession, seed string) error {
	remaining := append([]byte(seed), '\n')
	for len(remaining) > 0 {
		n, err := session.WriteInput(remaining)
		if n < 0 || n > len(remaining) {
			return fmt.Errorf("PTY seed returned invalid write count %d for %d bytes", n, len(remaining))
		}
		if n > 0 {
			remaining = remaining[n:]
		}
		if err != nil {
			return fmt.Errorf("write PTY seed: %w", err)
		}
		if n == 0 {
			return errors.New("write PTY seed: zero-byte write")
		}
	}
	return nil
}

func stopAfterSeedFailure(handle agent.Handle, cause error) error {
	stopCtx, stopCancel := context.WithTimeout(context.Background(), stopGrace+2*time.Second)
	defer stopCancel()
	if err := handle.Stop(stopCtx); err != nil {
		return fmt.Errorf("%w: stop handle after incomplete PTY seed: %v", cause, err)
	}
	return cause
}
