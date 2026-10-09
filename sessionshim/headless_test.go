package sessionshim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/shimwire"
)

// fakeRunner is a controllable RunnerProcess: a real spawned process the
// headless shim supervises without spawning anything of its own. It sleeps
// until stopped or finished, so its PID pins, its exit is observed, and the
// reap is proven against the OS rather than asserted.
type fakeRunner struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan struct{}
	exit     RunnerExit
	exitOK   bool
	stopped  bool
	stopOnce sync.Once
}

func newFakeRunner(t *testing.T) *fakeRunner {
	t.Helper()
	//nolint:gosec // G204: fixed test-only argv, no shell in the path
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake runner: %v", err)
	}
	f := &fakeRunner{cmd: cmd, done: make(chan struct{})}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	go func() {
		err := cmd.Wait()
		f.mu.Lock()
		defer f.mu.Unlock()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				f.exit = RunnerExit{ExitCode: uint64(exitErr.ExitCode())} //nolint:gosec // G115: exit codes are small and non-negative here
				f.exitOK = true
			}
		} else {
			f.exit = RunnerExit{}
			f.exitOK = true
		}
		close(f.done)
	}()
	return f
}

func (f *fakeRunner) PID() int {
	if f.cmd.Process == nil {
		return 0
	}
	return f.cmd.Process.Pid
}

func (f *fakeRunner) Done() <-chan struct{} { return f.done }

func (f *fakeRunner) Exit() (RunnerExit, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exit, f.exitOK
}

func (f *fakeRunner) Stop(ctx context.Context) error {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	f.stopOnce.Do(func() {
		if f.cmd.Process != nil {
			_ = f.cmd.Process.Kill()
		}
	})
	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeRunner) wasStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func shortOrphanPolicy() OrphanPolicy {
	return OrphanPolicy{
		Deadline:          300 * time.Millisecond,
		TerminationGrace:  100 * time.Millisecond,
		PropagationMargin: 0,
	}
}

// settledOrphanPolicy bounds only genuine abandonment: its deadline is far
// above any loaded-test round trip, so adoption, fencing, and tombstone
// tests never race the deadline they are not exercising. The short policy
// above belongs to the orphan-expiry test alone — production ships a
// fifteen-minute deadline, and an adoption test run under a hair-trigger
// one proves nothing but scheduling luck.
func settledOrphanPolicy() OrphanPolicy {
	return OrphanPolicy{
		Deadline:          30 * time.Second,
		TerminationGrace:  100 * time.Millisecond,
		PropagationMargin: 0,
	}
}

func startHeadlessFixture(t *testing.T, runner *fakeRunner, epoch uint64) (*Shim, *Registry, Identity) {
	t.Helper()
	return startHeadlessFixtureWithPolicy(t, runner, epoch, settledOrphanPolicy())
}

func startHeadlessFixtureWithPolicy(t *testing.T, runner *fakeRunner, epoch uint64, orphan OrphanPolicy) (*Shim, *Registry, Identity) {
	t.Helper()
	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-headless", SessionID: "sess-headless"}
	shim, err := StartHeadless(HeadlessOptions{
		Identity: id, Registry: registry, Runner: runner,
		WorkareaPath: dir + "/workarea",
		Orphan:       orphan,
		ProcessEpoch: epoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
		_ = shim.Close()
	})
	return shim, registry, id
}

func waitForHeadlessTombstone(t *testing.T, registry *Registry, id Identity) Tombstone {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		tomb, err := registry.GetTombstone(id)
		if err == nil {
			return tomb
		}
		if time.Now().After(deadline) {
			t.Fatalf("no headless tombstone: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHeadlessRecordCarriesWorkloadAndSchema2 pins the registry half of the
// headless contract: the discovery record carries workload headless beside
// schema 2, and re-encodes through the strict decoder unchanged.
func TestHeadlessRecordCarriesWorkloadAndSchema2(t *testing.T) {
	t.Parallel()

	runner := newFakeRunner(t)
	shim, registry, id := startHeadlessFixture(t, runner, 4)

	rec, err := registry.Get(id)
	if err != nil {
		t.Fatalf("Get headless record: %v", err)
	}
	if rec.Workload != WorkloadHeadless {
		t.Fatalf("record workload = %q, want %q", string(rec.Workload), string(WorkloadHeadless))
	}
	if rec.SchemaVersion != HeadlessRecordSchemaVersion {
		t.Fatalf("record schemaVersion = %d, want %d", rec.SchemaVersion, HeadlessRecordSchemaVersion)
	}
	if rec.ProcessEpoch != 4 {
		t.Fatalf("record processEpoch = %d, want 4", rec.ProcessEpoch)
	}
	if rec.ShimID != shim.ShimID() {
		t.Fatalf("record shimId = %q, want %q", rec.ShimID, shim.ShimID())
	}
	raw, err := rec.encode()
	if err != nil {
		t.Fatalf("re-encode headless record: %v", err)
	}
	decoded, err := decodeRecord(raw)
	if err != nil {
		t.Fatalf("strict decode of the headless record: %v", err)
	}
	if decoded.Workload != WorkloadHeadless || decoded.SchemaVersion != HeadlessRecordSchemaVersion {
		t.Fatalf("round trip = %+v, want workload headless schema 2", decoded)
	}
}

// TestHeadlessAdoptionServesHelloAdoptedHeartbeat pins the wire half: a
// controller dials the headless record, completes Hello → Welcome → Adopted
// through Dial, and exchanges a fenced Heartbeat on the live connection.
func TestHeadlessAdoptionServesHelloAdoptedHeartbeat(t *testing.T) {
	t.Parallel()

	runner := newFakeRunner(t)
	shim, registry, id := startHeadlessFixture(t, runner, 1)
	_ = shim

	rec, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctrl, err := Dial(ctx, rec, ControllerOptions{Workload: WorkloadHeadless, ControllerID: "controller-headless"})
	if err != nil {
		t.Fatalf("Dial headless record: %v", err)
	}
	defer func() { _ = ctrl.Close() }()

	hello := ctrl.Hello()
	if hello.ShimID != rec.ShimID || hello.ProcessEpoch != rec.ProcessEpoch {
		t.Fatalf("hello correlation = %s/%d, want %s/%d", hello.ShimID, hello.ProcessEpoch, rec.ShimID, rec.ProcessEpoch)
	}
	if got := hello.Extensions.Values[shimwire.ExtWorkload]; got != string(WorkloadHeadless) {
		t.Fatalf("hello workload extension = %q, want %q", got, string(WorkloadHeadless))
	}
	if err := ctrl.Heartbeat(0); err != nil {
		t.Fatalf("Heartbeat on an adopted headless connection: %v", err)
	}
}

// TestHeadlessFenceRefusesStaleGeneration pins the generation fence on the
// headless profile: a Heartbeat carrying a generation the shim never
// committed is refused with stale_generation, and a Stop with the committed
// generation ends the run with the completed cause and the runner's exit.
func TestHeadlessFenceRefusesStaleGeneration(t *testing.T) {
	t.Parallel()

	runner := newFakeRunner(t)
	_, registry, id := startHeadlessFixture(t, runner, 1)

	rec, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctrl, err := Dial(ctx, rec, ControllerOptions{Workload: WorkloadHeadless, ControllerID: "controller-fence"})
	if err != nil {
		t.Fatalf("Dial headless record: %v", err)
	}
	defer func() { _ = ctrl.Close() }()

	committed := ctrl.Generation()
	if committed == 0 {
		t.Fatal("adopted headless controller has generation 0")
	}
	stale, err := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{Generation: committed + 100})
	if err != nil {
		t.Fatal(err)
	}
	// A raw write on the controller's own connection: the Controller type has
	// no stale-generation sender (every typed sender carries the committed
	// generation), so the fence is exercised at the wire, through the same
	// connection an adopted controller holds.
	if err := ctrl.w.Write(shimwire.TypeHeartbeat, stale); err != nil {
		t.Fatalf("write stale heartbeat: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case event := <-ctrl.Events():
			if event.Kind != EventError {
				continue
			}
			if event.Err.Code != shimwire.CodeStaleGeneration {
				t.Fatalf("stale headless heartbeat answer = %s, want %s", event.Err.Code, shimwire.CodeStaleGeneration)
			}
			goto fenced
		case <-time.After(time.Until(deadline)):
			t.Fatal("no refusal arrived for the stale headless heartbeat")
		}
	}
fenced:
	if err := ctrl.Stop(shimwire.StopOperator); err != nil {
		t.Fatalf("Stop on an adopted headless connection: %v", err)
	}
	tomb := waitForHeadlessTombstone(t, registry, id)
	if tomb.Workload != WorkloadHeadless {
		t.Fatalf("tombstone workload = %q, want headless", string(tomb.Workload))
	}
	if tomb.SchemaVersion != HeadlessRecordSchemaVersion {
		t.Fatalf("tombstone schemaVersion = %d, want %d", tomb.SchemaVersion, HeadlessRecordSchemaVersion)
	}
	if tomb.Cause != HeadlessExitCompleted {
		t.Fatalf("tombstone cause = %q, want completed", string(tomb.Cause))
	}
	if tomb.ExitCode == 0 && tomb.Signal == "" {
		t.Fatal("tombstone carries neither an exit code nor a signal for a killed runner")
	}
	if !tomb.GroupReaped {
		t.Fatal("tombstone does not prove the runner group reaped")
	}
	if tomb.LastSeq != headlessTerminalSeq {
		t.Fatalf("tombstone lastSeq = %d, want %d", tomb.LastSeq, headlessTerminalSeq)
	}
}

// TestHeadlessOrphanExpiryReapsAndTombstones pins the orphan half: with no
// controller ever adopting, the deadline stops the runner's group and leaves
// a tombstone with the orphaned cause.
func TestHeadlessOrphanExpiryReapsAndTombstones(t *testing.T) {
	t.Parallel()

	runner := newFakeRunner(t)
	shim, registry, id := startHeadlessFixtureWithPolicy(t, runner, 1, shortOrphanPolicy())
	_ = shim

	tomb := waitForHeadlessTombstone(t, registry, id)
	if tomb.Cause != HeadlessExitOrphaned {
		t.Fatalf("orphan tombstone cause = %q, want orphaned", string(tomb.Cause))
	}
	if !tomb.GroupReaped {
		t.Fatal("orphan tombstone does not prove the runner group reaped")
	}
	if !runner.wasStopped() {
		t.Fatal("orphan deadline did not stop the runner's process group")
	}
}

// oldSchemaRecord mirrors the discovery record as a released daemon knows it:
// schema 1, no workload field, unknown fields refused. It is the shape of an
// older daemon meeting a headless record.
type oldSchemaRecord struct {
	SchemaVersion int    `json:"schemaVersion"`
	OrgID         string `json:"orgId"`
	SessionID     string `json:"sessionId"`
}

func decodeRecordSchema1Only(data []byte) error {
	var r oldSchemaRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return fmt.Errorf("%w: %v", ErrRecordInvalid, err)
	}
	if r.SchemaVersion != RecordSchemaVersion {
		return fmt.Errorf("%w: schemaVersion %d, want %d", ErrRecordInvalid, r.SchemaVersion, RecordSchemaVersion)
	}
	return nil
}

// TestOldSchemaDecoderRefusesHeadlessRecord is the fixture the slice demands:
// a decoder that knows only schema 1 and rejects unknown fields — the shape
// of a released daemon meeting a headless record — refuses it as
// record_malformed, so the startup pass quarantines rather than adopts.
func TestOldSchemaDecoderRefusesHeadlessRecord(t *testing.T) {
	t.Parallel()

	runner := newFakeRunner(t)
	_, registry, id := startHeadlessFixture(t, runner, 1)

	raw, err := os.ReadFile(registry.Dir() + "/" + id.RecordName())
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeRecordSchema1Only(raw); !errors.Is(err, ErrRecordInvalid) {
		t.Fatalf("old-schema decode of a headless record = %v, want ErrRecordInvalid (record_malformed)", err)
	}
}

// TestHeadlessPublishFailureReturnsRatherThanHangs pins the unserved-start
// teardown: when the discovery record cannot be published, StartHeadless
// returns the error instead of joining a serve loop that never started.
func TestHeadlessPublishFailureReturnsRatherThanHangs(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner(t)
	// Squat on the record filename with a directory: the socket listens
	// fine (the directory stays writable) but the durable publish fails
	// when the atomic rename lands on a non-file. The squat is removed by
	// cleanup so the temp dir can be collected.
	id := Identity{OrgID: "org-headless", SessionID: "sess-unpublishable"}
	if err := os.Mkdir(registry.Dir()+"/"+id.RecordName(), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(registry.Dir() + "/" + id.RecordName()) })

	done := make(chan error, 1)
	go func() {
		_, err := StartHeadless(HeadlessOptions{
			Identity: id,
			Registry: registry, Runner: runner,
			WorkareaPath: dir + "/workarea",
			Orphan:       shortOrphanPolicy(),
			ProcessEpoch: 1,
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("StartHeadless with an unwritable registry succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("StartHeadless did not return after the record publish failed")
	}
}

// TestStartRefusesExplicitHeadlessWorkload pins the fail-closed entry-point
// split: Start serves the interactive profile only, so a caller that meant
// to start a headless shim gets an error naming the headless entry — never
// a silently downgraded PTY session — and nothing is started.
func TestStartRefusesExplicitHeadlessWorkload(t *testing.T) {
	t.Parallel()

	dir := shortTempDir(t)
	registry, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{OrgID: "org-refused", SessionID: "sess-refused"}
	shim, err := Start(Options{Identity: id, Registry: registry, Workload: WorkloadHeadless})
	if err == nil {
		_ = shim.Close()
		t.Fatal("Start with an explicit headless workload succeeded; want refusal toward the headless entry")
	}
	if !strings.Contains(err.Error(), "StartHeadless") {
		t.Fatalf("Start refusal = %v, want the headless entry named", err)
	}
	// The refusal happens before the serve core runs: no discovery record is
	// published, so a downgraded session could never be adopted, and no
	// adoption socket is left behind.
	if _, getErr := registry.Get(id); getErr == nil {
		t.Fatal("refused Start published a discovery record; a downgraded session would be adoptable")
	}
	if _, statErr := os.Stat(registry.SocketPath(id)); !os.IsNotExist(statErr) {
		t.Fatalf("refused Start left an adoption socket behind: %v", statErr)
	}
}

// TestHeadlessHeartbeatBeyondTerminalSequenceIsMalformed pins the
// terminal-courtesy bound on the headless profile: the only sequence a
// headless shim ever allocates is 1, so a controller heartbeat claiming an
// acknowledgement of 2 is refused as malformed — before any tombstone
// exists, not just after it.
func TestHeadlessHeartbeatBeyondTerminalSequenceIsMalformed(t *testing.T) {
	t.Parallel()

	runner := newFakeRunner(t)
	_, registry, id := startHeadlessFixture(t, runner, 1)

	rec, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctrl, err := Dial(ctx, rec, ControllerOptions{Workload: WorkloadHeadless, ControllerID: "controller-ahead"})
	if err != nil {
		t.Fatalf("Dial headless record: %v", err)
	}
	defer func() { _ = ctrl.Close() }()

	committed := ctrl.Generation()
	if committed == 0 {
		t.Fatal("adopted headless controller has generation 0")
	}
	// A raw write on the controller's own connection, as the stale-generation
	// test does: the Controller type has no ahead-of-terminal sender, so the
	// bound is exercised at the wire, through the same connection an adopted
	// controller holds.
	ahead, err := shimwire.EncodeHeartbeat(shimwire.HeartbeatMsg{Generation: committed, AckedSeq: headlessTerminalSeq + 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := ctrl.w.Write(shimwire.TypeHeartbeat, ahead); err != nil {
		t.Fatalf("write ahead headless heartbeat: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case event := <-ctrl.Events():
			if event.Kind != EventError {
				continue
			}
			if event.Err.Code != shimwire.CodeMalformed {
				t.Fatalf("ahead headless heartbeat answer = %s, want %s", event.Err.Code, shimwire.CodeMalformed)
			}
			return
		case <-time.After(time.Until(deadline)):
			t.Fatal("no malformed refusal arrived for the ahead-of-terminal headless heartbeat")
		}
	}
}

// TestHeadlessNewerControllerSupersedesLiveOld is the headless form of the
// split-brain fence: a newer controller adopts while the old one still holds
// its socket. The newer Hello must be served, the old socket taken away on
// commit, a Stop carrying the superseded generation refused without reaching
// the runner, and the new controller left working.
func TestHeadlessNewerControllerSupersedesLiveOld(t *testing.T) {
	t.Parallel()

	runner := newFakeRunner(t)
	_, registry, id := startHeadlessFixture(t, runner, 1)

	rec, err := registry.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	old, err := Dial(ctx, rec, ControllerOptions{Workload: WorkloadHeadless, ControllerID: "controller-old"})
	if err != nil {
		t.Fatalf("Dial old controller: %v", err)
	}
	defer func() { _ = old.Close() }()
	if err := old.Heartbeat(0); err != nil {
		t.Fatalf("old controller heartbeat: %v", err)
	}
	oldGen := old.Generation()

	// The old controller still holds its socket open here.
	fresh, err := Dial(ctx, rec, ControllerOptions{Workload: WorkloadHeadless, ControllerID: "controller-new", DialTimeout: 8 * time.Second})
	if err != nil {
		t.Fatalf("Dial newer controller while the old one holds its socket: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	if fresh.Generation() <= oldGen {
		t.Fatalf("generation did not advance: was %d, now %d", oldGen, fresh.Generation())
	}
	select {
	case <-old.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the superseded headless controller's socket was not closed")
	}

	stale, err := shimwire.EncodeStop(shimwire.StopMsg{Generation: oldGen, Reason: shimwire.StopOperator})
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.w.Write(shimwire.TypeStop, stale); err != nil {
		t.Fatalf("write stale-generation stop: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case event := <-fresh.Events():
			if event.Kind != EventError {
				continue
			}
			if event.Err.Code != shimwire.CodeStaleGeneration {
				t.Fatalf("stale headless stop answer = %s, want %s", event.Err.Code, shimwire.CodeStaleGeneration)
			}
			if runner.wasStopped() {
				t.Fatal("a superseded generation's Stop reached the runner")
			}
			if err := fresh.Heartbeat(0); err != nil {
				t.Fatalf("current controller heartbeat after fencing the old one: %v", err)
			}
			return
		case <-time.After(time.Until(deadline)):
			t.Fatal("no stale_generation refusal for the superseded generation's Stop")
		}
	}
}
