package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/sessionshim"
)

// Drive the actual terminal cleanup with the real shim's immutable Exit, then
// hold its synchronous listener precisely while the terminal entry is retained.
// A retained snapshot transport must not admit another wake mutation.
func TestWakeRefusesTerminalTargetWhileListenerRetainsController(t *testing.T) {
	for _, op := range []string{"session.wake", "session.restart-harness"} {
		for _, scoped := range []bool{true, false} {
			name := op + "/scoped"
			if !scoped {
				name = op + "/unscoped"
			}
			t.Run(name, func(t *testing.T) {
				f := newWakeFixture(t, wakeFixtureRawHarness)
				if op == "session.restart-harness" {
					f.primeWakeLedger(t)
				}
				d := f.daemon
				d.spawner = &WorkerSpawner{}
				d.shims.mu.Lock()
				entry := d.shims.adopted[f.id]
				entry.launched = true
				d.shims.adopted[f.id] = entry
				d.shims.mu.Unlock()
				entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				d.spawner.On(func(ev SessionEvent) {
					if ev.Kind == SessionEventEnded {
						close(entered)
						<-release
					}
				})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := f.shim.Terminate(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case ev := <-f.terminal:
					go func() { defer close(finished); d.finishAdoptedShim(f.id, ev.Exit) }()
				case <-ctx.Done():
					t.Fatal("real shim did not emit Exit")
				}
				t.Cleanup(func() {
					unblock()
					select {
					case <-finished:
					case <-time.After(5 * time.Second):
						t.Error("terminal listener cleanup did not finish")
					}
				})
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("terminal listener was not reached")
				}
				d.shims.mu.RLock()
				terminal := d.shims.adopted[f.id].terminal
				d.shims.mu.RUnlock()
				if !terminal {
					t.Fatal("listener barrier preceded terminal marking")
				}
				params := sessionWakeParams{SessionID: f.id.SessionID}
				if scoped {
					params.OrgID = f.id.OrgID
				}
				err := d.applyOneMutation(wakeMutation(t, op, "terminal-mutation", params))
				applied, ledgerErr := d.checkWakeMutation(f.id, "terminal-mutation", op)
				if !errors.Is(err, sessionshim.ErrShimExited) {
					t.Errorf("terminal wake error = %v, want ErrShimExited", err)
				}
				if ledgerErr != nil {
					t.Errorf("ledger query: %v", ledgerErr)
				}
				if applied {
					t.Error("terminal mutation was falsely committed to wake ledger")
				}
				if _, err := d.InspectAdoptedSessionShimSnapshot(ctx, f.id.OrgID, f.id.SessionID); err != nil {
					t.Errorf("terminal snapshot inspection must remain available: %v", err)
				}
				if _, err := d.EmitAdoptedSessionShimSnapshot(ctx, f.id.OrgID, f.id.SessionID); err != nil {
					t.Errorf("terminal snapshot emission must remain available: %v", err)
				}
				unblock()
				select {
				case <-finished:
				case <-ctx.Done():
					t.Fatal("terminal cleanup did not finish")
				}
			})
		}
	}
}
