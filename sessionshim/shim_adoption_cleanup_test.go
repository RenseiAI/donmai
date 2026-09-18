package sessionshim

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

// failAdoptedWriter preserves the real local Unix transport for every frame
// except the committed Adopted header. It makes the post-install failure
// boundary deterministic without relying on socket-close timing.
type failAdoptedWriter struct{ io.Writer }

func (w failAdoptedWriter) Write(p []byte) (int, error) {
	if len(p) == 5 && p[4] == byte(shimwire.TypeAdopted) {
		return 0, errors.New("injected Adopted header write failure")
	}
	return w.Writer.Write(p)
}

func cleanupTestUnixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	path := shortTempDir(t) + "/handshake.sock"
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan *net.UnixConn, 1)
	go func() {
		conn, acceptErr := listener.AcceptUnix()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server := <-accepted
	t.Cleanup(func() { _ = server.Close() })
	return server, client
}

func TestHandshakeAdoptedWriteFailureRearmsOrphanWithoutRewindingGeneration(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	dir := shortTempDir(t)
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-cleanup", SessionID: "session-adopted-write"}
	shim := startInProcessShim(t, reg, dir, id, 1)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})

	server, client := cleanupTestUnixPair(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- shim.handshake(server, shimwire.NewWriter(failAdoptedWriter{Writer: server}), shimwire.NewReader(server))
	}()
	clientReader := shimwire.NewReader(client)
	helloMessage, err := clientReader.Read()
	if err != nil || helloMessage.Type != shimwire.TypeHello {
		t.Fatalf("read Hello = %s, %v", helloMessage.Type, err)
	}
	hello, err := shimwire.DecodeHello(helloMessage.Body)
	if err != nil {
		t.Fatalf("decode Hello: %v", err)
	}
	welcome := shimwire.Welcome{
		Protocol: shimwire.ProtocolName, Selected: shimwire.V2,
		ControllerID: "controller-injected-loss", ProposedGeneration: hello.Generation + 1,
	}
	if err := writeTyped(shimwire.NewWriter(client), shimwire.TypeWelcome, func() ([]byte, error) {
		return shimwire.EncodeWelcome(welcome)
	}); err != nil {
		t.Fatalf("write Welcome: %v", err)
	}
	if err := <-errCh; err == nil {
		t.Fatal("handshake accepted an injected Adopted write failure")
	}

	shim.mu.Lock()
	ctrl, generation, phase, timer := shim.ctrl, shim.gen, shim.phase, shim.orphanTimer
	shim.mu.Unlock()
	if ctrl != nil {
		t.Fatal("failed pre-loop controller remained current")
	}
	if generation != hello.Generation+1 {
		t.Fatalf("generation = %d, want committed %d", generation, hello.Generation+1)
	}
	if phase != shimwire.PhaseOrphaned || timer == nil {
		t.Fatalf("post-failure lifecycle = phase %q timer %v, want orphaned with deadline", phase, timer != nil)
	}
	record, err := reg.Get(id)
	if err != nil {
		t.Fatalf("read orphan record: %v", err)
	}
	if record.Phase != shimwire.PhaseOrphaned || record.OrphanDeadlineUnixNano == 0 {
		t.Fatalf("published record = %+v, want orphan deadline", record)
	}
	if alive, aliveErr := shim.harness.Alive(); aliveErr != nil || !alive {
		t.Fatalf("harness after failed adoption = alive %t err %v, want live", alive, aliveErr)
	}

	result, err := Adopt(context.Background(), AdoptOptions{Registry: reg, ControllerID: "controller-retry"})
	if err != nil || len(result.Adopted) != 1 {
		t.Fatalf("re-adopt same harness = %+v, %v", result, err)
	}
	defer result.Close()
	if got := result.Adopted[0].Generation(); got != generation+1 {
		t.Fatalf("retry generation = %d, want %d", got, generation+1)
	}
}

func TestControllerLossAndOrphanCallbacksAreExactOwnerAndEpisodeBound(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	dir := shortTempDir(t)
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	shim := startInProcessShim(t, reg, dir, Identity{OrgID: "org-cleanup", SessionID: "session-owner"}, 1)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})

	old := lifecycleTestController(t)
	newer := lifecycleTestController(t)
	shim.disarmOrphan()
	shim.mu.Lock()
	shim.ctrl = newer
	shim.phase = shimwire.PhaseRunning
	shim.mu.Unlock()
	shim.loseController(old)
	shim.mu.Lock()
	oldCleanupChanged := shim.ctrl != newer || shim.phase != shimwire.PhaseRunning || shim.orphanTimer != nil
	oldCtrl, oldPhase, oldTimer := shim.ctrl, shim.phase, shim.orphanTimer
	shim.mu.Unlock()
	if oldCleanupChanged {
		t.Fatalf("old cleanup altered newer owner: ctrl=%p phase=%q timer=%v", oldCtrl, oldPhase, oldTimer != nil)
	}

	shim.loseController(newer)
	shim.mu.Lock()
	episodeA := shim.orphanEpisode
	replacement := lifecycleTestController(t)
	callbackStarted := make(chan struct{})
	callbackDone := make(chan struct{})
	go func() {
		close(callbackStarted)
		shim.onOrphanDeadline(episodeA)
		close(callbackDone)
	}()
	<-callbackStarted
	// The adoption holds s.mu while it revokes episode A and makes replacement
	// current. The pending callback must observe that completed transition after
	// the lock is released; it cannot terminalize the replacement in between.
	shim.installControllerLocked(replacement)
	shim.mu.Unlock()
	select {
	case <-callbackDone:
	case <-time.After(2 * time.Second):
		t.Fatal("old deadline callback did not return after adoption won the lock")
	}
	shim.recordMu.Lock()
	if err := shim.publishRecordWithDeadlineLocked(time.Time{}); err != nil {
		shim.recordMu.Unlock()
		t.Fatalf("publish replacement record: %v", err)
	}
	shim.recordMu.Unlock()
	if err := shim.publishOrphanRecord(episodeA, time.Unix(1, 0)); err != nil {
		t.Fatalf("stale orphan publication: %v", err)
	}
	record, err := reg.Get(shim.id)
	if err != nil {
		t.Fatalf("read replacement record: %v", err)
	}
	if record.Phase != shimwire.PhaseRunning || record.OrphanDeadlineUnixNano != 0 {
		t.Fatalf("stale orphan publication changed running record: %+v", record)
	}
	shim.mu.Lock()
	staleAdoptionChanged := shim.ctrl != replacement || shim.phase != shimwire.PhaseRunning || shim.orphanExpiring
	staleCtrl, stalePhase, staleExpiring := shim.ctrl, shim.phase, shim.orphanExpiring
	shim.mu.Unlock()
	if staleAdoptionChanged {
		t.Fatalf("stale callback after adoption altered replacement: ctrl=%p phase=%q expiring=%t", staleCtrl, stalePhase, staleExpiring)
	}

	shim.loseController(replacement)
	shim.mu.Lock()
	episodeB, timerB := shim.orphanEpisode, shim.orphanTimer
	shim.mu.Unlock()
	if episodeB == episodeA || timerB == nil {
		t.Fatalf("new orphan episode = %d timer=%v, want a distinct armed episode", episodeB, timerB != nil)
	}
	if err := shim.publishOrphanRecord(episodeA, time.Unix(1, 0)); err != nil {
		t.Fatalf("stale orphan publication after episode B: %v", err)
	}
	shim.mu.Lock()
	staleEpisodeChanged := shim.orphanEpisode != episodeB || shim.orphanTimer != timerB || shim.orphanExpiring
	gotEpisode, gotTimer, gotExpiring := shim.orphanEpisode, shim.orphanTimer, shim.orphanExpiring
	shim.mu.Unlock()
	if staleEpisodeChanged {
		t.Fatalf("stale callback after later orphan changed episode: got=%d timer=%p expiring=%t", gotEpisode, gotTimer, gotExpiring)
	}
	record, err = reg.Get(shim.id)
	if err != nil {
		t.Fatalf("read episode B record: %v", err)
	}
	if record.Phase != shimwire.PhaseOrphaned || record.OrphanDeadlineUnixNano == time.Unix(1, 0).UnixNano() {
		t.Fatalf("stale orphan publication changed episode B record: %+v", record)
	}
}
