package daemon

import (
	"sync"
	"testing"
)

// The adoption gate's first-batch hold has one arm per wait outcome. Each
// test below arranges exactly one ready channel so the select is
// deterministic: no controller, no stream timing, no finish-versus-close
// race. The shim-level tests pin the pipeline; these pin the branches.

// TestShimAdoptionGateInstalledPersists pins the latch arm: once the carrier
// is installed the parked batch persists, even while the adoption verdict is
// still open.
func TestShimAdoptionGateInstalledPersists(t *testing.T) {
	t.Parallel()
	gate := newShimAdoptionGate()
	gate.finishCarrierInstalled()
	stream := make(chan struct{}) // never closed: the stream is still open
	if !gate.awaitCarrierOrGate(stream) {
		t.Fatal("installed carrier must release the parked batch")
	}
}

// TestShimAdoptionGateCommittedPersists pins the verdict arm: a committed
// adoption persists the batch even where the latch never armed.
func TestShimAdoptionGateCommittedPersists(t *testing.T) {
	t.Parallel()
	gate := newShimAdoptionGate()
	gate.finish(true)
	stream := make(chan struct{})
	if !gate.awaitCarrierOrGate(stream) {
		t.Fatal("committed adoption must persist the parked batch")
	}
}

// TestShimAdoptionGateRefusedStops pins the refusal arm: a refused adoption
// stops the parked consumer without persisting.
func TestShimAdoptionGateRefusedStops(t *testing.T) {
	t.Parallel()
	gate := newShimAdoptionGate()
	gate.finish(false)
	stream := make(chan struct{})
	if gate.awaitCarrierOrGate(stream) {
		t.Fatal("refused adoption must stop the parked consumer without persisting")
	}
}

// TestShimAdoptionGateStreamClosedStops pins the stream arm: when the stream
// ends underneath a still-open adoption the parked consumer stops without
// persisting.
func TestShimAdoptionGateStreamClosedStops(t *testing.T) {
	t.Parallel()
	gate := newShimAdoptionGate()
	stream := make(chan struct{})
	close(stream)
	if gate.awaitCarrierOrGate(stream) {
		t.Fatal("closed stream must stop the parked consumer without persisting")
	}
}

// TestShimAdoptionGateNilPassesThrough pins the unwired path: a nil gate
// returns at once so consumers without an adoption verdict never park.
func TestShimAdoptionGateNilPassesThrough(t *testing.T) {
	t.Parallel()
	var gate *shimAdoptionGate
	stream := make(chan struct{})
	if !gate.awaitCarrierOrGate(stream) {
		t.Fatal("nil gate must not hold the batch")
	}
}

// TestShimAdoptionGateInstalledLatchConcurrent pins the latch's idempotence:
// Emit stages the mandatory Snapshot on the adoption goroutine while parked
// consumers wait on the latch, so concurrent firings must not race. The
// start barrier lines every goroutine up on the same firing: a
// check-then-close latch panics here with a double close, while the
// single-fire latch passes every run.
func TestShimAdoptionGateInstalledLatchConcurrent(t *testing.T) {
	t.Parallel()
	// Fresh gate per round: a check-then-close latch survives any single
	// round often enough to look safe, but across rounds two firings land
	// inside the same check-then-close window and panic on the double
	// close. The single-fire latch passes every round.
	for round := 0; round < 50; round++ {
		gate := newShimAdoptionGate()
		var ready sync.WaitGroup
		ready.Add(1)
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready.Wait()
				gate.finishCarrierInstalled()
			}()
		}
		ready.Done()
		wg.Wait()
		stream := make(chan struct{})
		if !gate.awaitCarrierOrGate(stream) {
			t.Fatalf("round %d: installed carrier must release the parked batch", round)
		}
	}
}
