package sessionshim

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
)

type closeCountingSubscription struct {
	frames chan attachwire.Frame
	once   sync.Once
	closes atomic.Int32
}

func newCloseCountingSubscription() *closeCountingSubscription {
	return &closeCountingSubscription{frames: make(chan attachwire.Frame)}
}

func (s *closeCountingSubscription) Frames() <-chan attachwire.Frame { return s.frames }

func (s *closeCountingSubscription) Close() error {
	s.closes.Add(1)
	s.once.Do(func() { close(s.frames) })
	return nil
}

func TestControllerConnCloseRejectsLateSubscriptionAndLoopStart(t *testing.T) {
	t.Parallel()
	ctrl := lifecycleTestController(t)
	ctrl.close()
	sub := newCloseCountingSubscription()

	if ctrl.installSubscription(sub) {
		t.Fatal("subscription installed after controller close")
	}
	if got := sub.closes.Load(); got != 1 {
		t.Fatalf("late subscription closes = %d, want exactly 1", got)
	}
	if (&Shim{}).startControllerLoops(ctrl, nil) {
		t.Fatal("live loops accepted after controller close")
	}
	ctrl.close()
	if got := sub.closes.Load(); got != 1 {
		t.Fatalf("late subscription closes after repeated close = %d, want exactly 1", got)
	}
}

func TestControllerConnInstallAndCloseOwnSubscriptionExactlyOnce(t *testing.T) {
	t.Parallel()
	ctrl := lifecycleTestController(t)
	sub := newCloseCountingSubscription()
	if !ctrl.installSubscription(sub) {
		t.Fatal("open controller refused subscription")
	}

	ctrl.close()
	ctrl.close()
	if got := sub.closes.Load(); got != 1 {
		t.Fatalf("installed subscription closes = %d, want exactly 1", got)
	}
	if (&Shim{}).startControllerLoops(ctrl, nil) {
		t.Fatal("closed controller accepted another live-loop start")
	}
}

func TestControllerConnInstallCloseRaceOwnsSubscriptionExactlyOnce(t *testing.T) {
	t.Parallel()
	ctrl := lifecycleTestController(t)
	sub := newCloseCountingSubscription()
	ready := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-ready
		ctrl.installSubscription(sub)
	}()
	go func() {
		defer wg.Done()
		<-ready
		ctrl.close()
	}()
	close(ready)
	wg.Wait()

	if got := sub.closes.Load(); got != 1 {
		t.Fatalf("racing subscription closes = %d, want exactly 1", got)
	}
	if (&Shim{}).startControllerLoops(ctrl, nil) {
		t.Fatal("closed controller accepted live-loop start after install race")
	}
}

func lifecycleTestController(t *testing.T) *controllerConn {
	t.Helper()
	path := shortTempDir(t) + "/controller.sock"
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan *net.UnixConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptUnix()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	select {
	case server := <-accepted:
		t.Cleanup(func() { _ = server.Close() })
		return &controllerConn{conn: server}
	case err := <-acceptErr:
		t.Fatal(err)
		return nil
	}
}
