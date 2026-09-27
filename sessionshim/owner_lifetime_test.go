package sessionshim

import (
	"context"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
)

func TestOwnerLifetimeJoinsReservedStartupAndSealsNewStarts(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "command-cancelled"}[cancelled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ownedCtx, owner := WithOwnerLifetime(ctx)
			entered, release := make(chan struct{}), make(chan struct{})
			reg, err := NewRegistry(shortTempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			created := make(chan *Shim, 1)
			started := make(chan *Shim, 1)
			startupErr := make(chan error, 1)
			go func() {
				sh, err := startOwned(ownedCtx, func() (*Shim, error) {
					close(entered)
					<-release
					sh, err := Start(Options{Identity: Identity{OrgID: "org-owner", SessionID: "session-owner"}, Registry: reg, ProcessEpoch: 1, Spec: ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}}})
					created <- sh
					return sh, err
				})
				started <- sh
				startupErr <- err
			}()
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				sh := <-started
				select {
				case actual := <-created:
					sh = actual
				default:
				}
				if sh != nil {
					_ = sh.Terminate(context.Background())
					_ = sh.Close()
				}
			})
			awaitFinalScreenSignal(t, entered, "startup never reached its reserved barrier")
			drained := make(chan error, 1)
			go func() { drained <- owner.Wait(ctx) }()
			awaitFinalScreenSignal(t, owner.drainStarted, "drain never sealed enrollment")
			if cancelled {
				cancel()
			}
			select {
			case err := <-drained:
				t.Fatalf("drain missed in-flight startup: %v", err)
			default:
			}
			invoked := false
			if _, err := startOwned(ownedCtx, func() (*Shim, error) { invoked = true; return nil, nil }); err == nil || invoked {
				t.Fatal("new startup admitted after drain began")
			}
			close(release)
			select {
			case err := <-drained:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("owner drain did not finish")
			}
			if err := <-startupErr; err != nil {
				t.Fatal(err)
			}
			// The cleanup consumes the exact returned shim; preserve it on its channel
			// after checking the finished owner, rather than looking up any sibling.
			sh := <-started
			started <- sh
			awaitFinalScreenSignal(t, sh.Done(), "late owner was not fully stopped")
			if alive, err := sh.HarnessIdentity().Alive(); err != nil || alive {
				t.Fatalf("owned harness still alive=%v err=%v", alive, err)
			}
		})
	}
}
