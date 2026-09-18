package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/shimwire"
)

var (
	errInjectedPostInstall = errors.New("injected post-install failure")
	errInjectedWireWrite   = errors.New("injected wire write failure")
)

// failMessageWriter preserves the real local Unix transport for every frame
// except the requested message header. It proves the actual write return path,
// rather than simulating a later socket close.
type failMessageWriter struct {
	io.Writer
	target  shimwire.MessageType
	seen    chan<- shimwire.MessageType
	entered chan<- struct{}
	release <-chan struct{}
}

func (w failMessageWriter) Write(p []byte) (int, error) {
	if len(p) == 5 && p[4] == byte(w.target) {
		w.seen <- w.target
		if w.entered != nil {
			w.entered <- struct{}{}
			<-w.release
		}
		return 0, errInjectedWireWrite
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

func TestHandshakePostInstallFailureRestoresOrphanTracking(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	tests := []struct {
		name       string
		selected   uint32
		resumeFrom uint64
		fillRing   bool
		seam       string
		wire       shimwire.MessageType
		proofBound bool
	}{
		// The nil-default seam covers returns without a writer: resume, the
		// subscription handoff, and loop ownership. The writer controls below
		// cover every reachable post-commit wire write.
		{name: "v1-resume", selected: shimwire.V1, seam: "resume"},
		{name: "v2-subscription", selected: shimwire.V2, seam: "subscription"},
		{name: "v3-adopted-write", selected: shimwire.V3, wire: shimwire.TypeAdopted, proofBound: true},
		{name: "v4-adopted-write", selected: shimwire.V4, wire: shimwire.TypeAdopted, proofBound: true},
		{name: "v1-gap-write", selected: shimwire.V1, resumeFrom: 2, fillRing: true, wire: shimwire.TypeGap},
		{name: "v2-snapshot-write", selected: shimwire.V2, resumeFrom: 2, fillRing: true, wire: shimwire.TypeSnapshot},
		{name: "v3-host-frame-write", selected: shimwire.V3, resumeFrom: 2, fillRing: true, wire: shimwire.TypeHostFrame},
		{name: "v4-host-frame-write", selected: shimwire.V4, resumeFrom: 2, fillRing: true, wire: shimwire.TypeHostFrame},
		{name: "v4-loop-start", selected: shimwire.V4, seam: "loopstart", proofBound: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := shortTempDir(t)
			reg, err := NewRegistry(dir)
			if err != nil {
				t.Fatal(err)
			}
			id := Identity{OrgID: "org-cleanup", SessionID: "session-post-install-" + tc.name}
			var failOnce sync.Once
			failureEntered := make(chan struct{}, 1)
			failureRelease := make(chan struct{})
			shim, err := Start(Options{
				Identity: id, Registry: reg, ProcessEpoch: 1,
				Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}, RingBytes: 48},
				WorkareaPath: dir + "/workarea",
				Orphan:       OrphanPolicy{Deadline: time.Hour, TerminationGrace: 250 * time.Millisecond},
				postInstallFailure: func(stage string) error {
					if stage == tc.seam {
						fail := false
						failOnce.Do(func() { fail = true })
						if fail {
							failureEntered <- struct{}{}
							<-failureRelease
							return errInjectedPostInstall
						}
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = shim.Terminate(ctx)
			})
			before, err := reg.Get(id)
			if err != nil {
				t.Fatalf("read initial record: %v", err)
			}
			beforeHarness := shim.HarnessIdentity()
			if tc.fillRing {
				for i := 0; i < 32; i++ {
					if err := shim.Session().EmitMarker(fmt.Sprintf("evict-%02d", i)); err != nil {
						t.Fatalf("emit ring marker: %v", err)
					}
				}
			}

			server, client := cleanupTestUnixPair(t)
			seen := make(chan shimwire.MessageType, 1)
			writer := shimwire.NewWriter(server)
			if tc.wire != 0 {
				writer = shimwire.NewWriter(failMessageWriter{
					Writer: server, target: tc.wire, seen: seen, entered: failureEntered, release: failureRelease,
				})
			}
			errCh := make(chan error, 1)
			go func() { errCh <- shim.handshake(server, writer, shimwire.NewReader(server)) }()
			message, err := shimwire.NewReader(client).Read()
			if err != nil || message.Type != shimwire.TypeHello {
				t.Fatalf("read Hello = %s, %v", message.Type, err)
			}
			hello, err := shimwire.DecodeHello(message.Body)
			if err != nil {
				t.Fatalf("decode Hello: %v", err)
			}
			welcome := shimwire.Welcome{
				Protocol: shimwire.ProtocolName, Selected: tc.selected,
				ControllerID: "controller-post-install", ProposedGeneration: hello.Generation + 1,
				ResumeFrom: tc.resumeFrom,
			}
			if tc.proofBound {
				welcome.Extensions = shimwire.Extensions{
					Values:   map[string]string{shimwire.ExtCarrierEpoch: "post-install-proof"},
					Required: []string{shimwire.ExtCarrierEpoch},
				}
			}
			if err := writeTyped(shimwire.NewWriter(client), shimwire.TypeWelcome, func() ([]byte, error) {
				return shimwire.EncodeWelcome(welcome)
			}); err != nil {
				t.Fatalf("write Welcome: %v", err)
			}
			if tc.proofBound {
				<-failureEntered
				markerDone := make(chan error, 1)
				go func() { markerDone <- shim.Session().EmitMarker("queued-behind-post-install-failure") }()
				select {
				case markerErr := <-markerDone:
					t.Fatalf("proof-bound marker crossed controller-owned barrier before failure cleanup: %v", markerErr)
				case <-time.After(50 * time.Millisecond):
				}
				close(failureRelease)
				err = <-errCh
				select {
				case markerErr := <-markerDone:
					if markerErr != nil {
						t.Fatalf("failure cleanup did not release proof-bound output barrier: %v", markerErr)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("failure cleanup left proof-bound output barrier held")
				}
			} else {
				close(failureRelease)
				err = <-errCh
			}
			want := errInjectedPostInstall
			if tc.wire != 0 {
				want = errInjectedWireWrite
				if got := <-seen; got != tc.wire {
					t.Fatalf("failed wire message = %s, want %s", got, tc.wire)
				}
			}
			if !errors.Is(err, want) {
				t.Fatalf("handshake error = %v, want %v", err, want)
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
			if record.PID != before.PID || record.ProcessStartedAt != before.ProcessStartedAt {
				t.Fatalf("failed handshake changed shim identity: before=%d/%d after=%d/%d", before.PID, before.ProcessStartedAt, record.PID, record.ProcessStartedAt)
			}
			if got := shim.HarnessIdentity(); got != beforeHarness {
				t.Fatalf("failed handshake changed harness identity: before=%+v after=%+v", beforeHarness, got)
			}
			if alive, aliveErr := beforeHarness.Alive(); aliveErr != nil || !alive {
				t.Fatalf("harness after failed adoption = alive %t err %v, want live", alive, aliveErr)
			}

			result, err := Adopt(context.Background(), AdoptOptions{Registry: reg, ControllerID: "controller-retry"})
			if err != nil || len(result.Adopted) != 1 {
				t.Fatalf("re-adopt same harness = %+v, %v", result, err)
			}
			t.Cleanup(result.Close)
			if got := result.Adopted[0].Generation(); got != generation+1 {
				t.Fatalf("retry generation = %d, want %d", got, generation+1)
			}
			if got := result.Adopted[0].HarnessIdentity(); got != beforeHarness {
				t.Fatalf("re-adoption changed harness identity: before=%+v after=%+v", beforeHarness, got)
			}
			recovered, err := reg.Get(id)
			if err != nil {
				t.Fatalf("read re-adopted record: %v", err)
			}
			if recovered.PID != before.PID || recovered.ProcessStartedAt != before.ProcessStartedAt || recovered.OrphanDeadlineUnixNano != 0 {
				t.Fatalf("re-adoption changed child or retained orphan metadata: %+v", recovered)
			}
		})
	}
}

func TestHandshakeAllowsFinalizedExitReplayAfterOrphanExpiry(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	dir := shortTempDir(t)
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	shim := startInProcessShim(t, reg, dir, Identity{OrgID: "org-cleanup", SessionID: "session-exit-replay"}, 1)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})
	// finalizeTerminal changes phase to exited after a timer has set this private
	// flag. A retained terminal replay remains adoptable; only the interval while
	// termination is still in progress is refused.
	shim.mu.Lock()
	shim.orphanExpiring = true
	shim.phase = shimwire.PhaseExited
	shim.mu.Unlock()

	server, client := cleanupTestUnixPair(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- shim.handshake(server, shimwire.NewWriter(failMessageWriter{
			Writer: server, target: shimwire.TypeAdopted, seen: make(chan shimwire.MessageType, 1),
		}), shimwire.NewReader(server))
	}()
	message, err := shimwire.NewReader(client).Read()
	if err != nil || message.Type != shimwire.TypeHello {
		t.Fatalf("read retained Exit Hello = %s, %v", message.Type, err)
	}
	hello, err := shimwire.DecodeHello(message.Body)
	if err != nil {
		t.Fatalf("decode retained Exit Hello: %v", err)
	}
	welcome := shimwire.Welcome{
		Protocol: shimwire.ProtocolName, Selected: shimwire.V2,
		ControllerID: "controller-exit-replay", ProposedGeneration: hello.Generation + 1,
	}
	if err := writeTyped(shimwire.NewWriter(client), shimwire.TypeWelcome, func() ([]byte, error) {
		return shimwire.EncodeWelcome(welcome)
	}); err != nil {
		t.Fatalf("write retained Exit Welcome: %v", err)
	}
	if err := <-errCh; err == nil {
		t.Fatal("retained Exit replay accepted injected Adopted write failure")
	}
	if got := shim.Generation(); got != hello.Generation+1 {
		t.Fatalf("retained Exit replay generation = %d, want %d", got, hello.Generation+1)
	}
}

func TestRetainedExitReplayDoesNotResurrectTerminalLiveness(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	dir := shortTempDir(t)
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-cleanup", SessionID: "session-retained-exit"}
	courtesyEntered := make(chan struct{})
	releaseCourtesy := make(chan struct{})
	var releaseOnce sync.Once
	shim, err := Start(Options{
		Identity: id, Registry: reg, ProcessEpoch: 1,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}},
		WorkareaPath: dir + "/workarea",
		Orphan:       OrphanPolicy{Deadline: time.Hour, TerminationGrace: 250 * time.Millisecond},
		onTerminalCourtesy: func() {
			close(courtesyEntered)
			<-releaseCourtesy
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseCourtesy) }) })
	shim.mu.Lock()
	episode := shim.orphanEpisode
	shim.mu.Unlock()
	if !shim.beginOrphanTermination(episode) {
		t.Fatal("orphan deadline did not win before terminal finalization")
	}
	terminalDone := make(chan error, 1)
	go func() {
		terminalDone <- shim.Terminate(context.Background())
	}()
	select {
	case <-courtesyEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal proof did not reach the courtesy boundary")
	}
	if _, err := reg.Get(id); err == nil {
		t.Fatal("terminal proof left a live discovery record")
	}
	before, err := reg.GetTombstone(id)
	if err != nil {
		t.Fatalf("read durable terminal proof: %v", err)
	}

	server, client := cleanupTestUnixPair(t)
	handshakeDone := make(chan error, 1)
	go func() {
		handshakeDone <- shim.handshake(server, shimwire.NewWriter(server), shimwire.NewReader(server))
	}()
	reader := shimwire.NewReader(client)
	helloMessage, err := reader.Read()
	if err != nil || helloMessage.Type != shimwire.TypeHello {
		t.Fatalf("read retained Exit Hello = %s, %v", helloMessage.Type, err)
	}
	hello, err := shimwire.DecodeHello(helloMessage.Body)
	if err != nil {
		t.Fatalf("decode retained Exit Hello: %v", err)
	}
	if hello.Phase != shimwire.PhaseExited {
		t.Fatalf("retained Exit Hello phase = %q, want exited", hello.Phase)
	}
	welcome := shimwire.Welcome{
		Protocol: shimwire.ProtocolName, Selected: shimwire.V2,
		ControllerID: "controller-retained-exit", ProposedGeneration: hello.Generation + 1,
	}
	if err := writeTyped(shimwire.NewWriter(client), shimwire.TypeWelcome, func() ([]byte, error) {
		return shimwire.EncodeWelcome(welcome)
	}); err != nil {
		t.Fatalf("write retained Exit Welcome: %v", err)
	}
	adoptedMessage, err := reader.Read()
	if err != nil || adoptedMessage.Type != shimwire.TypeAdopted {
		t.Fatalf("read retained Exit Adopted = %s, %v", adoptedMessage.Type, err)
	}
	adopted, err := shimwire.DecodeAdopted(adoptedMessage.Body)
	if err != nil {
		t.Fatalf("decode retained Exit Adopted: %v", err)
	}
	if adopted.Phase != shimwire.PhaseExited {
		t.Fatalf("retained Exit Adopted phase = %q, want exited", adopted.Phase)
	}
	exitMessage, err := reader.Read()
	if err != nil || exitMessage.Type != shimwire.TypeExit {
		t.Fatalf("read retained Exit frame = %s, %v", exitMessage.Type, err)
	}
	if _, err := shimwire.DecodeExit(exitMessage.Body); err != nil {
		t.Fatalf("decode retained Exit frame: %v", err)
	}
	if err := <-handshakeDone; err != nil {
		t.Fatalf("retained Exit handshake: %v", err)
	}
	if _, err := reg.Get(id); err == nil {
		t.Fatal("retained Exit replay resurrected a live discovery record")
	}
	after, err := reg.GetTombstone(id)
	if err != nil {
		t.Fatalf("read terminal proof after replay: %v", err)
	}
	if after != before {
		t.Fatalf("retained Exit replay changed terminal proof: before=%+v after=%+v", before, after)
	}
	releaseOnce.Do(func() { close(releaseCourtesy) })
	if err := <-terminalDone; err != nil {
		t.Fatalf("terminalization after retained replay: %v", err)
	}
}

func TestOrphanExpiryWinsBeforeLiveControllerInstallation(t *testing.T) {
	if !peerCredSupported() {
		t.Skip("session shim adoption is unsupported on this platform")
	}
	dir := shortTempDir(t)
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	shim := startInProcessShim(t, reg, dir, Identity{OrgID: "org-cleanup", SessionID: "session-expiry-wins"}, 1)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})
	shim.mu.Lock()
	episode := shim.orphanEpisode
	shim.mu.Unlock()
	if !shim.beginOrphanTermination(episode) {
		t.Fatal("orphan deadline did not win its pre-adoption transition")
	}

	server, client := cleanupTestUnixPair(t)
	errCh := make(chan error, 1)
	go func() { errCh <- shim.handshake(server, shimwire.NewWriter(server), shimwire.NewReader(server)) }()
	message, err := shimwire.NewReader(client).Read()
	if err != nil || message.Type != shimwire.TypeHello {
		t.Fatalf("read expiry-win Hello = %s, %v", message.Type, err)
	}
	hello, err := shimwire.DecodeHello(message.Body)
	if err != nil {
		t.Fatalf("decode expiry-win Hello: %v", err)
	}
	welcome := shimwire.Welcome{
		Protocol: shimwire.ProtocolName, Selected: shimwire.V2,
		ControllerID: "controller-after-expiry", ProposedGeneration: hello.Generation + 1,
	}
	if err := writeTyped(shimwire.NewWriter(client), shimwire.TypeWelcome, func() ([]byte, error) {
		return shimwire.EncodeWelcome(welcome)
	}); err != nil {
		t.Fatalf("write expiry-win Welcome: %v", err)
	}
	if err := <-errCh; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("handshake after expiry win = %v, want net.ErrClosed", err)
	}
	shim.mu.Lock()
	generation, ctrl, phase, expiring := shim.gen, shim.ctrl, shim.phase, shim.orphanExpiring
	shim.mu.Unlock()
	if generation != hello.Generation || ctrl != nil || phase != shimwire.PhaseOrphaned || !expiring {
		t.Fatalf("expiry-win state changed by live adoption: generation=%d ctrl=%p phase=%q expiring=%t", generation, ctrl, phase, expiring)
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
	replacement.selected = shimwire.V3
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
	shim.onOrphanDeadline(episodeA)
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
