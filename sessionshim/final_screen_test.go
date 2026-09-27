package sessionshim

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

func finalScreenFixture(t *testing.T, version uint32, window time.Duration) (*Shim, *Controller, *Registry) {
	t.Helper()
	reg, err := NewRegistry(shortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	sh, err := Start(Options{
		Identity: Identity{OrgID: "org-final-screen", SessionID: "session-final-screen"}, Registry: reg, ProcessEpoch: 1,
		Spec: ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}}, Orphan: OrphanPolicy{Deadline: 30 * time.Second, TerminationGrace: 250 * time.Millisecond},
		FinalScreenWindow: window, ProtocolMin: shimwire.V1, ProtocolMax: version,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Terminate(context.Background()); _ = sh.Close() })
	adopted, err := Adopt(context.Background(), AdoptOptions{Registry: reg, ControllerID: "final-screen-controller", ProtocolMin: shimwire.V1, ProtocolMax: version, RequireFullHostFrames: version >= shimwire.V3})
	if err != nil || len(adopted.Adopted) != 1 {
		t.Fatalf("Adopt = %+v, %v", adopted, err)
	}
	t.Cleanup(adopted.Close)
	return sh, adopted.Adopted[0], reg
}

func awaitFinalScreenSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal(message)
	}
}

func awaitFinalScreenExit(t *testing.T, c *Controller) shimwire.ExitMsg {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-c.Events():
			if !ok {
				t.Fatal("controller closed before Exit")
			}
			if ev.Kind == EventExit || (ev.Kind == EventHostFrame && ev.FrameType == attachwire.TypeExit) {
				return ev.Exit
			}
		case <-timer.C:
			t.Fatal("Exit was not observed")
		}
	}
}

// The lifecycle barrier, not a delay after Exit, places the request AFTER
// terminal finalization. Reinstating Close-before-window deterministically
// closes the controller before this request, reproducing the original race.
func TestPostExitFinalScreenRetainsController(t *testing.T) {
	for _, version := range []uint32{shimwire.V2, shimwire.V3} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			sh, c, reg := finalScreenFixture(t, version, defaultFinalScreenWindow)
			exchange(t, c, "final-screen")
			if err := c.Stop(shimwire.StopOperator); err != nil {
				t.Fatal(err)
			}
			exit := awaitFinalScreenExit(t, c)
			if version >= shimwire.V3 {
				if err := c.Heartbeat(exit.Seq); err != nil {
					t.Fatalf("terminal acknowledgement: %v", err)
				}
			}
			awaitFinalScreenSignal(t, sh.finalScreenWindowStarted, "final-screen window never opened")
			awaitFinalScreenSignal(t, sh.TerminalDone(), "terminal proof not published")
			if _, err := reg.GetTombstone(sh.Identity()); err != nil {
				t.Fatal(err)
			}
			select {
			case <-sh.Done():
				t.Fatal("owner finished before final-screen service")
			default:
			}
			final, err := c.EmitSnapshot(context.Background())
			if err != nil {
				t.Fatalf("post-Exit snapshot: %v", err)
			}
			frame, err := attachwire.DecodeFrame(final.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			if final.InStream || final.AtSeq != exit.Seq || frame.Type != attachwire.TypeSnapshot || frame.Seq != attachwire.PostExitSnapshotSeq || frame.RelTime != 0 {
				t.Fatalf("post-Exit disposition = %+v, frame=%+v", final, frame)
			}
			_, seq, err := sh.Session().Snapshot()
			if err != nil || uint64(seq) != exit.Seq {
				t.Fatalf("post-Exit request advanced sequence: %d/%d %v", seq, exit.Seq, err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			awaitFinalScreenSignal(t, sh.Done(), "controller loss did not end final-screen service")
			if _, err := os.Stat(sh.SocketPath()); !os.IsNotExist(err) {
				t.Fatalf("listener path remains after Done: %v", err)
			}
		})
	}
}

func TestPostExitFinalScreenTimerExpires(t *testing.T) {
	sh, c, _ := finalScreenFixture(t, shimwire.V2, 25*time.Millisecond)
	if err := c.Stop(shimwire.StopOperator); err != nil {
		t.Fatal(err)
	}
	awaitFinalScreenSignal(t, sh.finalScreenWindowStarted, "final-screen timer never armed")
	awaitFinalScreenSignal(t, sh.Done(), "final-screen timer did not finish owner")
	awaitFinalScreenSignal(t, c.Done(), "final-screen timer did not close controller")
	if _, err := os.Stat(sh.SocketPath()); !os.IsNotExist(err) {
		t.Fatalf("listener path remains after expiry: %v", err)
	}
}

func TestFinalScreenDoneRequiresDurableTerminalProof(t *testing.T) {
	reg, err := NewRegistry(shortTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-proof", SessionID: "session-proof"}
	sh, err := Start(Options{Identity: id, Registry: reg, ProcessEpoch: 1, Spec: ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Terminate(context.Background()); _ = sh.Close() })
	// A real rename-to-directory failure, independent of the test process UID.
	blocked := filepath.Join(reg.dir, tombstoneIncarnationName(Tombstone{OrgID: id.OrgID, SessionID: id.SessionID, ShimID: sh.ShimID(), ProcessEpoch: 1}))
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = sh.Terminate(context.Background())
	if err := sh.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sh.TerminalDone():
		t.Fatal("TerminalDone closed without durable proof")
	default:
	}
	select {
	case <-sh.Done():
		t.Fatal("Done closed without durable proof")
	default:
	}
}

// A socket accepted before listener teardown must not become a controller after
// full stop. The Hello/Welcome split is the barrier; no scheduling delay is used.
func TestClosedShimRefusesPendingWelcome(t *testing.T) {
	for _, version := range []uint32{shimwire.V2, shimwire.V3} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			reg, err := NewRegistry(shortTempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			sh, err := Start(Options{Identity: Identity{OrgID: "org-late-welcome", SessionID: "session-late-welcome"}, Registry: reg, ProcessEpoch: 1, Spec: ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}}, ProtocolMin: shimwire.V1, ProtocolMax: version})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sh.Terminate(context.Background()); _ = sh.Close() })
			conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sh.SocketPath(), Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			reader := shimwire.NewReader(conn)
			message, err := reader.Read()
			if err != nil || message.Type != shimwire.TypeHello {
				t.Fatalf("Hello=%+v err=%v", message, err)
			}
			hello, err := shimwire.DecodeHello(message.Body)
			if err != nil {
				t.Fatal(err)
			}
			if version == shimwire.V2 {
				if err := sh.Terminate(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if err := sh.Close(); err != nil {
				t.Fatal(err)
			}
			if version == shimwire.V2 {
				awaitFinalScreenSignal(t, sh.Done(), "terminal owner did not fully stop")
			}
			welcome := shimwire.Welcome{Protocol: shimwire.ProtocolName, Selected: version, ControllerID: "late-controller", ProposedGeneration: hello.Generation + 1}
			if version >= shimwire.V3 {
				welcome.Extensions = shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "late-proof"}, Required: []string{shimwire.ExtCarrierEpoch}}
			}
			if err := writeTyped(shimwire.NewWriter(conn), shimwire.TypeWelcome, func() ([]byte, error) { return shimwire.EncodeWelcome(welcome) }); err != nil {
				t.Fatal(err)
			}
			response, err := reader.Read()
			if err != nil {
				t.Fatalf("closed-owner refusal unreadable: %v", err)
			}
			if response.Type != shimwire.TypeError {
				t.Fatalf("closed owner accepted pending Welcome: response=%s", response.Type)
			}
			refusal, err := shimwire.DecodeError(response.Body)
			if err != nil || refusal.Code != shimwire.CodePhaseUnknown {
				t.Fatalf("refusal=%+v err=%v", refusal, err)
			}
			if sh.currentController() != nil || sh.Generation() != hello.Generation {
				t.Fatal("closed owner installed a controller or advanced generation")
			}
		})
	}
}
