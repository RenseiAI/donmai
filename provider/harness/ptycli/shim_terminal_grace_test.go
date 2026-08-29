package ptycli

import (
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// TestShimTerminalGraceExceedsTheShimsOwnFinalizeBound is the derivation this
// file got wrong.
//
// It used to wait a flat 10s beside a finalize path whose worst case is exactly
// 10s. Zero margin is not a grace window: measured on an installed host, the
// wait expired in the same instant the shim's tombstone was about to be
// written, this process exited, and a provably-reaped harness left no proof at
// all. The grace must be DERIVED from the bound the shim actually enforces.
func TestShimTerminalGraceExceedsTheShimsOwnFinalizeBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grace time.Duration
	}{
		{name: "default policy", grace: sessionshim.DefaultTerminationGrace},
		{name: "short policy", grace: 250 * time.Millisecond},
		{name: "policy at the per-window cap", grace: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bound := sessionshim.FinalizeBoundFor(sessionshim.OrphanPolicy{
				Deadline:          sessionshim.DefaultOrphanDeadline,
				TerminationGrace:  tc.grace,
				PropagationMargin: sessionshim.DefaultPropagationMargin,
			})
			got := shimTerminalGrace(bound)
			if got <= bound {
				t.Fatalf("grace %s does not exceed the shim's finalize bound %s — the process can exit "+
					"in the same instant the tombstone is written", got, bound)
			}
		})
	}
}
