package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
	"github.com/RenseiAI/donmai/shimwire"
)

func TestOnAdoptionCanEmitFreshSnapshotBeforeControllerPublication(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = "host-takeover-snapshot"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	d.opts.SessionShim.CallbackTimeout = 500 * time.Millisecond
	d.opts.SessionShim.PrepareAdoption = func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
		return sessionshim.PreparedAdoption{
			ControllerGeneration: preparation.CurrentControllerGeneration + 1,
			Extensions:           shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "19"}},
		}, nil
	}
	var emitted shimwire.SnapshotResult
	var retainedProxy *SessionShimSnapshotProxy
	d.opts.SessionShim.OnAdoption = func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
		if evidence.SnapshotProxy == nil || !evidence.CarrierCompatible || evidence.ProtocolVersion != shimwire.V2 {
			return SessionShimAdoptionReceipt{}, fmt.Errorf("snapshot capability missing during adoption: %+v", evidence)
		}
		retainedProxy = evidence.SnapshotProxy
		var err error
		emitted, err = evidence.SnapshotProxy.Emit(ctx)
		if err != nil {
			return SessionShimAdoptionReceipt{}, err
		}
		return SessionShimAdoptionReceipt{DurableCorrelation: []byte("carrier-takeover-complete")}, nil
	}
	d.opts.SessionShim.OnSessionEventDurable = func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil }
	d.opts.SessionShim.OnAdoptionBatch = func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		return SessionShimAdoptionBatchReceipt{
			DurableCorrelation: []byte("snapshot-batch"), AdoptionRevision: "snapshot-revision",
		}, nil
	}
	d.opts.SessionShim.OnAdoptionPublished = func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
		if _, err := d.adoptedShimEntry(f.orgID, "takeover-snapshot"); err != nil {
			return nil, fmt.Errorf("activation ran before local publication: %w", err)
		}
		return []SessionShimCarrierActivationReceipt{{
			Activation: publication.Carriers[0], AckSeq: emitted.AtSeq + 1,
		}}, nil
	}

	spec := f.interactiveSpec("takeover-snapshot")
	if _, err := d.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	frame, err := attachwire.DecodeFrame(emitted.Bytes)
	if err != nil || frame.Type != attachwire.TypeSnapshot || !emitted.InStream {
		t.Fatalf("callback emit = frame %+v result %+v err=%v", frame, emitted, err)
	}
	entry, err := d.adoptedShimEntry(f.orgID, spec.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.adoption.SnapshotProxy != nil {
		t.Fatal("ephemeral adoption SnapshotProxy was retained after synchronous callback")
	}
	if _, err := retainedProxy.Inspect(context.Background()); !errors.Is(err, shimwire.ErrVersionMismatch) {
		t.Fatalf("retained adoption proxy remained active after callback: %v", err)
	}
	fresh, err := d.InspectAdoptedSessionShimSnapshot(context.Background(), f.orgID, spec.SessionID)
	if err != nil || fresh.InStream || len(fresh.Bytes) == 0 {
		t.Fatalf("published daemon snapshot proxy = %+v, %v", fresh, err)
	}
}

func TestOnAdoptionSnapshotDrainsMoreThanEventBufferReplayInDurableOrder(t *testing.T) {
	f := newShimSpawnFixture(t)
	spec := f.interactiveSpec("takeover-large-replay")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatal(err)
	}
	id := f.identity(spec.SessionID)
	for i := 0; i < 72; i++ {
		f.exchange(t, id, fmt.Sprintf("replay-%03d", i))
	}
	f.daemon.ReleaseAdoptedSessionShims()

	var durableCount int
	var durableSeq uint64
	var emitted shimwire.SnapshotResult
	var durableMu sync.Mutex
	var activationObservedForwarded uint64
	var replacement *Daemon
	replacement = New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true, RegistryDir: f.registry,
			HostID: "host-large-replay", RequireAuthoritativeSnapshot: true,
			PrepareAdoption: func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
				return sessionshim.PreparedAdoption{
					ControllerGeneration: preparation.CurrentControllerGeneration + 1,
					Extensions:           shimwire.Extensions{Values: map[string]string{shimwire.ExtCarrierEpoch: "23"}},
				}, nil
			},
			OnSessionEventDurable: func(_ sessionshim.Identity, event sessionshim.ControllerEvent) error {
				durableMu.Lock()
				defer durableMu.Unlock()
				if event.Kind == sessionshim.EventOutput || event.Kind == sessionshim.EventSnapshotFrame {
					if event.Seq <= durableSeq {
						return fmt.Errorf("durable stream reordered: %d after %d", event.Seq, durableSeq)
					}
					durableSeq = event.Seq
					durableCount++
				}
				return nil
			},
			OnAdoption: func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
				var err error
				emitted, err = evidence.SnapshotProxy.Emit(ctx)
				if err != nil {
					return SessionShimAdoptionReceipt{}, err
				}
				durableMu.Lock()
				defer durableMu.Unlock()
				if durableCount <= 64 || durableSeq != emitted.AtSeq {
					return SessionShimAdoptionReceipt{}, fmt.Errorf("replay/staged split = count=%d seq=%d snapshot=%d", durableCount, durableSeq, emitted.AtSeq+1)
				}
				return SessionShimAdoptionReceipt{DurableCorrelation: []byte("large-replay-complete")}, nil
			},
			OnAdoptionBatch: func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				return SessionShimAdoptionBatchReceipt{
					DurableCorrelation: []byte("large-replay-batch"), AdoptionRevision: "large-replay-revision",
				}, nil
			},
			OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
				activationObservedForwarded = replacement.SessionShimForwardedSeq(id.OrgID, id.SessionID)
				return []SessionShimCarrierActivationReceipt{{
					Activation: publication.Carriers[0], AckSeq: emitted.AtSeq + 1,
				}}, nil
			},
		},
	})
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := replacement.adoptSessionShims(ctx); err != nil {
		t.Fatalf("replacement adoption with >64 replay frames: %v", err)
	}
	if len(emitted.Bytes) == 0 || !emitted.InStream {
		t.Fatalf("takeover emitted snapshot = %+v", emitted)
	}
	if activationObservedForwarded >= emitted.AtSeq+1 {
		t.Fatalf("staged Snapshot advanced before carrier_active: forwarded=%d snapshot=%d", activationObservedForwarded, emitted.AtSeq+1)
	}
	if got := replacement.SessionShimForwardedSeq(id.OrgID, id.SessionID); got < emitted.AtSeq+1 {
		t.Fatalf("early durable high-water regressed at publication: got %d want >= %d", got, emitted.AtSeq+1)
	}
	fence, err := replacement.RequestSessionShimRestartFence(context.Background(), "immediate-after-takeover")
	if err != nil || len(fence.Sessions) != 1 || fence.Sessions[0].LastForwardedSeq < emitted.AtSeq+1 {
		t.Fatalf("immediate fence lost early durable high-water: fence=%+v err=%v", fence, err)
	}
}

// This file is the daemon-side half of the ADR-2026-08-17 acceptance suite.
//
// # Why a real second process
//
// The claim under test is a PRODUCTION claim: the daemon's interactive spawn
// path launches a per-session shim, hands it the launch contract, adopts it over
// shimwire, and then drives stop/input/output and terminal cleanup through that
// connection. A fake in-process launcher would prove the plumbing compiles and
// nothing about the ownership move, because the shim and the daemon would share
// a lifetime by construction — the exact coupling §D1 exists to remove.
//
// So the shim here is a genuinely separate OS process: this test binary
// re-executed in helper mode, reading the SAME launch environment the daemon
// composes for a real worker, owning a real PTY and a real harness child.
//
// # What this suite does NOT claim
//
// It does not claim the ADR's first proof obligation — the real launchd/systemd
// smoke against the INSTALLED service. A setsid-only implementation can pass
// every subprocess test here and still be reaped by a service manager; that
// fixture belongs to the smokes repo. What is proven here is everything above
// the service-manager boundary.

const envDaemonShimHelper = "DONMAI_TEST_DAEMON_SESSION_SHIM_HELPER"

// interactiveFixture is a real line-oriented interactive program: it blocks on
// terminal input and answers each line, so a round trip proves BOTH directions
// are live through the adopted connection.
const interactiveFixture = `while IFS= read -r line; do printf 'ack:%s\n' "$line"; done`

// TestMain routes this binary into shim-helper mode when the daemon's own launch
// contract is present in the environment. The daemon composes that environment;
// the helper consumes it exactly as a real worker's ptycli driver does.
func TestMain(m *testing.M) {
	if os.Getenv(envDaemonShimHelper) == "1" {
		os.Exit(runDaemonShimHelper())
	}
	os.Exit(m.Run())
}

func runDaemonShimHelper() int {
	launch, err := sessionshim.LaunchFromEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon shim helper: launch env:", err)
		return 1
	}
	// The worker resolves the same <parent>/<sessionID> leaf the daemon
	// publishes, which is what makes the adoption-time workarea comparison a real
	// check rather than a value compared against itself (§D7).
	workarea := filepath.Join(os.Getenv("DONMAI_TEST_DAEMON_SESSION_SHIM_WORKAREA_PARENT"), launch.Identity.SessionID)
	shim, err := sessionshim.StartFromEnv(launch,
		ptyhost.Spec{Command: []string{"/bin/sh", "-c", interactiveFixture}}, workarea)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon shim helper: start:", err)
		return 1
	}
	<-shim.Done()
	return 0
}

// shimSpawnFixture is a daemon configured to launch interactive sessions through
// a real shim process.
type shimSpawnFixture struct {
	daemon         *Daemon
	registry       string
	workareaParent string
	orgID          string
	events         *shimEventRecorder
}

// shimEventRecorder is the composing carrier's seat: it receives exactly what
// the daemon forwards from each adopted session. Reading output through this
// hook rather than off the controller's channel is not a convenience — the
// daemon's own consumer is the sole reader of that channel, and a test that
// raced it would be proving something no production consumer could rely on.
type shimEventRecorder struct {
	mu   sync.Mutex
	seen map[string]*strings.Builder
	seq  map[string]uint64
	gaps map[string]int
}

type exactFenceRecorder struct {
	mu       sync.Mutex
	requests []sessionshim.FenceRequest
	failOrg  string
}

func (r *exactFenceRecorder) AcknowledgeExact(_ context.Context, request sessionshim.FenceRequest) (sessionshim.FenceAcknowledgement, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.mu.Unlock()
	if len(request.Fence.Sessions) > 0 && request.Fence.Sessions[0].OrgID == r.failOrg {
		return sessionshim.FenceAcknowledgement{}, errors.New("organization fence unavailable")
	}
	return sessionshim.FenceAcknowledgement{
		RequestBytes:    append([]byte(nil), request.RequestBytes...),
		DurableRevision: "revision-" + request.Fence.Sessions[0].OrgID,
	}, nil
}

func (r *exactFenceRecorder) snapshot() []sessionshim.FenceRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sessionshim.FenceRequest(nil), r.requests...)
}

func newShimEventRecorder() *shimEventRecorder {
	return &shimEventRecorder{
		seen: map[string]*strings.Builder{},
		seq:  map[string]uint64{},
		gaps: map[string]int{},
	}
}

func (r *shimEventRecorder) record(id sessionshim.Identity, ev sessionshim.ControllerEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := id.Key()
	switch ev.Kind {
	case sessionshim.EventOutput:
		b, ok := r.seen[key]
		if !ok {
			b = &strings.Builder{}
			r.seen[key] = b
		}
		b.Write(ev.Data)
		if ev.Seq > r.seq[key] {
			r.seq[key] = ev.Seq
		}
	case sessionshim.EventGap:
		r.gaps[key]++
	}
}

func (r *shimEventRecorder) output(id sessionshim.Identity) (string, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := id.Key()
	if b, ok := r.seen[key]; ok {
		return b.String(), r.seq[key]
	}
	return "", r.seq[key]
}

func newShimSpawnFixture(t *testing.T) *shimSpawnFixture {
	t.Helper()
	// A Unix socket path has a short platform limit (as low as 104 bytes), and
	// t.TempDir() bakes the test name into the path. Keep the registry short.
	dir, err := os.MkdirTemp("/tmp", "dsp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	events := newShimEventRecorder()
	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption:  true,
			EnableOwnership: true,
			OrgID:           "test-org",
			RegistryDir:     dir + "/registry",
			LaunchTimeout:   60 * time.Second,
			OnSessionEvent:  events.record,
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error {
				return nil
			},
			Orphan: sessionshim.OrphanPolicy{
				Deadline:          2 * time.Second,
				TerminationGrace:  500 * time.Millisecond,
				PropagationMargin: 0,
			},
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 4,
		//nolint:gosec // G204: os.Args[0] is this test binary; helper mode is selected by env
		WorkerCommand:     []string{os.Args[0], "-test.run", "TestMain"},
		WorktreeParentDir: dir,
		BaseEnv: map[string]string{
			envDaemonShimHelper: "1",
			"DONMAI_TEST_DAEMON_SESSION_SHIM_WORKAREA_PARENT": dir,
			// The helper is a race-instrumented copy of this test binary. It has
			// three goroutines of real work, so extra Ps buy nothing and cost CPU
			// every other package's process-spawn deadlines need under a parallel
			// `go test -race ./...`.
			"GOMAXPROCS": "2",
		},
	})
	d.spawner.opts.ShimSpawn = d.launchSessionShim
	d.spawner.Resume()

	f := &shimSpawnFixture{daemon: d, registry: dir + "/registry", workareaParent: dir, orgID: "test-org", events: events}
	t.Cleanup(func() {
		for _, id := range d.AdoptedSessionShims() {
			_ = d.StopAdoptedSessionShim(id.OrgID, id.SessionID, shimwire.StopHostShutdown)
		}
		d.ReleaseAdoptedSessionShims()
	})
	return f
}

// interactiveSpec is a session spec whose run mode selects shim ownership.
func (f *shimSpawnFixture) interactiveSpec(sessionID string) SessionSpec {
	return SessionSpec{
		SessionID:  sessionID,
		ProjectID:  "p1",
		Repository: "https://example.invalid/x/y",
		Mode:       interactiveRunMode,
	}
}

func (f *shimSpawnFixture) identity(sessionID string) sessionshim.Identity {
	return sessionshim.Identity{OrgID: f.orgID, SessionID: sessionID}
}

// exchange writes one line into the adopted session and waits for the harness's
// answer to reach the carrier hook, returning the highest sequence forwarded.
// It proves BOTH directions are live through the adopted connection.
func (f *shimSpawnFixture) exchange(t *testing.T, id sessionshim.Identity, token string) uint64 {
	t.Helper()
	if err := f.daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte(token+"\r")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInput: %v", err)
	}
	want := "ack:" + token
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, seq := f.events.output(id)
		if strings.Contains(out, want) {
			return seq
		}
		time.Sleep(20 * time.Millisecond)
	}
	out, _ := f.events.output(id)
	t.Fatalf("timed out waiting for %q to reach the carrier; saw %q", want, out)
	return 0
}

// TestInteractiveSpawnLaunchesThroughAShimAndAdoptsIt is the V16 anchor for the
// SELECTION rule: an interactive session accepted by this daemon must be owned
// by a separate shim process this daemon then adopts, not by a daemon-parented
// child.
//
// Bypassing the selection in shimOwnsSession (returning false, or dropping the
// Mode check) turns this test RED at the AdoptedSessionShims assertion, because
// the session then takes the ordinary direct-child path and no shim exists.
func TestInteractiveSpawnLaunchesThroughAShimAndAdoptsIt(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	handle, err := d.spawner.AcceptWork(f.interactiveSpec("sess-launch"))
	if err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-launch")

	adopted := d.AdoptedSessionShims()
	if len(adopted) != 1 || adopted[0] != id {
		t.Fatalf("AdoptedSessionShims = %+v, want exactly [%s] — the interactive spawn did not go through a shim", adopted, id)
	}

	// §D1: the daemon holds no process bookkeeping for a shim-owned session. A
	// spawner entry would mean a second owner, and the reaper attached to it
	// would end the session on the next daemon shutdown.
	if _, tracked := d.spawner.sessions[id.SessionID]; tracked {
		t.Fatal("the spawner registered a direct-child entry for a shim-owned session; the daemon must not be a second owner")
	}

	// The published PID is the HARNESS, reported by the shim — not the shim
	// process and not a daemon child. It is the value an unchanged-across-restart
	// comparison is made against (§D2).
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if handle.PID == 0 || handle.PID != entry.controller.HarnessIdentity().PID {
		t.Fatalf("handle PID = %d, want the shim-reported harness pid %d", handle.PID, entry.controller.HarnessIdentity().PID)
	}
	if handle.PID == os.Getpid() {
		t.Fatal("handle PID is this process; the harness must run under the shim, not the controller")
	}
	if !entry.controller.HarnessSurvived() {
		t.Fatal("HarnessSurvived = false immediately after launch")
	}
	if entry.controller.Generation() == 0 {
		t.Fatal("adoption did not commit a controller generation; single-controller fencing is unenforced")
	}
	if !entry.launched {
		t.Error("the launched shim is not marked as launched by this daemon")
	}
}

func TestInteractiveShimUsesPerSessionOrganizationAndGroupedExactFences(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	store := &exactFenceRecorder{}
	d.opts.SessionShim.HostIDForOrg = func(_ context.Context, orgID string) (string, error) {
		return "stable-host-" + orgID, nil
	}
	d.opts.SessionShim.ExactFenceStore = store

	for _, tc := range []struct {
		orgID, sessionID string
	}{
		{orgID: "org-alpha", sessionID: "sess-alpha"},
		{orgID: "org-beta", sessionID: "sess-beta"},
	} {
		spec := f.interactiveSpec(tc.sessionID)
		spec.OrganizationID = tc.orgID
		if _, err := d.spawner.AcceptWork(spec); err != nil {
			t.Fatalf("AcceptWork(%s): %v", tc.orgID, err)
		}
		if _, err := d.adoptedShimEntry(tc.orgID, tc.sessionID); err != nil {
			t.Fatalf("per-session lifecycle identity %s/%s was not adopted: %v", tc.orgID, tc.sessionID, err)
		}
	}

	// Prove the fence host does not come from the rotating worker/controller id.
	d.mu.Lock()
	d.workerID = "worker-controller-correlation"
	d.mu.Unlock()
	fences, err := d.RequestSessionShimRestartFences(context.Background(), "fence-shared-id")
	if err != nil {
		t.Fatalf("RequestSessionShimRestartFences: %v", err)
	}
	if len(fences) != 2 {
		t.Fatalf("fences = %+v, want one per organization", fences)
	}
	requests := store.snapshot()
	if len(requests) != 2 {
		t.Fatalf("exact store requests = %d, want 2", len(requests))
	}
	for _, request := range requests {
		if len(request.Fence.Sessions) != 1 {
			t.Fatalf("cross-organization exact request = %+v, want one homogeneous session", request.Fence.Sessions)
		}
		covered := request.Fence.Sessions[0]
		if request.Fence.HostID != "stable-host-"+covered.OrgID {
			t.Errorf("fence hostId = %q, want per-org stable host authority for %s", request.Fence.HostID, covered.OrgID)
		}
		if covered.OrgID == "" || covered.ControllerGeneration == 0 {
			t.Errorf("fence omitted per-session org/controller correlation: %+v", covered)
		}
		entry, err := d.adoptedShimEntry(covered.OrgID, covered.SessionID)
		if err != nil {
			t.Fatalf("adoptedShimEntry(%s): %v", covered.Identity(), err)
		}
		if covered.ControllerGeneration != uint64(entry.controller.Generation()) {
			t.Errorf("fenced generation = %d, exact shim generation = %d", covered.ControllerGeneration, entry.controller.Generation())
		}
		if entry.adoption.ControllerID != d.ControllerID() ||
			entry.adoption.ControllerID == request.Fence.HostID ||
			entry.adoption.ControllerID == d.WorkerID() {
			t.Errorf("controller/host correlations collapsed or drifted: controller=%q host=%q",
				entry.adoption.ControllerID, request.Fence.HostID)
		}
		for _, session := range request.Fence.Sessions {
			if session.OrgID != covered.OrgID {
				t.Fatalf("exact fence mixed organizations: %+v", request.Fence.Sessions)
			}
		}
	}
}

func TestSessionShimAdoptionAndTerminalCallbacksCarryExactCorrelation(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.opts.SessionShim.HostID = "host-callback"
	d.opts.SessionShim.RequireAuthoritativeSnapshot = true
	d.opts.SessionShim.PrepareAdoption = func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
		if preparation.Identity.OrgID != "org-callback" || preparation.HostID != "host-callback" ||
			preparation.ShimID == "" || preparation.ProcessEpoch == 0 {
			return sessionshim.PreparedAdoption{}, fmt.Errorf("incomplete preparation evidence: %+v", preparation)
		}
		return sessionshim.PreparedAdoption{
			ControllerGeneration: preparation.CurrentControllerGeneration + 7,
			Extensions: shimwire.Extensions{
				Values:   map[string]string{shimwire.ExtCarrierEpoch: "19"},
				Required: []string{shimwire.ExtCarrierEpoch},
			},
			Correlation: []byte(`{"fenceRevision":"73","expectedAdoptionRevision":"81"}`),
		}, nil
	}
	var adoption SessionShimAdoptionEvidence
	var emitted shimwire.SnapshotResult
	d.opts.SessionShim.OnAdoption = func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
		adoption = evidence
		var err error
		emitted, err = evidence.SnapshotProxy.Emit(ctx)
		if err != nil {
			return SessionShimAdoptionReceipt{}, err
		}
		return SessionShimAdoptionReceipt{DurableCorrelation: []byte(`{"fenceRevision":"73","adoptionRevision":"81"}`)}, nil
	}
	d.opts.SessionShim.OnSessionEventDurable = func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil }
	d.opts.SessionShim.OnAdoptionBatch = func(context.Context, SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
		return SessionShimAdoptionBatchReceipt{
			DurableCorrelation: []byte("callback-batch"), AdoptionRevision: "callback-revision",
		}, nil
	}
	d.opts.SessionShim.OnAdoptionPublished = func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
		return []SessionShimCarrierActivationReceipt{{
			Activation: publication.Carriers[0], AckSeq: emitted.AtSeq + 1,
		}}, nil
	}
	terminal := make(chan SessionShimTerminalEvidence, 1)
	d.opts.SessionShim.OnTerminalEvidence = func(_ context.Context, evidence SessionShimTerminalEvidence) error {
		registry, err := sessionshim.NewRegistry(f.registry)
		if err != nil {
			return err
		}
		if _, err := registry.GetTombstone(evidence.Identity); err != nil {
			return fmt.Errorf("terminal callback ran before tombstone publication: %w", err)
		}
		terminal <- evidence
		return nil
	}

	spec := f.interactiveSpec("sess-callback")
	spec.OrganizationID = "org-callback"
	if _, err := d.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := sessionshim.Identity{OrgID: spec.OrganizationID, SessionID: spec.SessionID}
	if adoption.Identity != id || adoption.HostID != "host-callback" {
		t.Fatalf("adoption evidence identity/host = %+v", adoption)
	}
	if adoption.ControllerGeneration != 7 || adoption.ProcessEpoch == 0 || adoption.ShimID == "" {
		t.Fatalf("adoption evidence omitted exact shim/process/controller correlation: %+v", adoption)
	}
	if got, ok := adoption.Extensions.Get(shimwire.ExtCarrierEpoch); !ok || got != "19" {
		t.Fatalf("adoption carrier_epoch = %q/%v, want 19/true", got, ok)
	}
	if string(adoption.PreparedCorrelation) != `{"fenceRevision":"73","expectedAdoptionRevision":"81"}` {
		t.Fatalf("prepared correlation changed before adoption: %s", adoption.PreparedCorrelation)
	}
	if !d.StopSession(spec.SessionID) {
		t.Fatal("StopSession did not reach shim")
	}

	select {
	case evidence := <-terminal:
		if evidence.Identity != id || evidence.HostID != adoption.HostID || evidence.Adoption == nil ||
			evidence.ShimID != adoption.ShimID || evidence.ProcessEpoch != adoption.ProcessEpoch ||
			evidence.Adoption.ControllerGeneration != adoption.ControllerGeneration {
			t.Fatalf("terminal correlation = %+v, want adoption %+v", evidence, adoption)
		}
		if string(evidence.DurableAdoptionCorrelation) != `{"fenceRevision":"73","adoptionRevision":"81"}` {
			t.Fatalf("opaque durable adoption correlation changed: %s", evidence.DurableAdoptionCorrelation)
		}
		if !evidence.Tombstone.GroupReaped {
			t.Fatal("terminal callback ran without positive process-group reap proof")
		}
		if evidence.Adoption.SnapshotProxy != nil {
			t.Fatal("terminal evidence retained the ephemeral adoption SnapshotProxy")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for terminal evidence callback")
	}
}

// TestShimOwnedSessionIsVisibleInSessionsAndCapacity pins §D7 at the two
// surfaces that decide whether more work is sent here.
func TestShimOwnedSessionIsVisibleInSessionsAndCapacity(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-visible")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}

	sessions := d.ActiveSessions()
	if len(sessions) != 1 || sessions[0].SessionID != "sess-visible" {
		t.Fatalf("ActiveSessions = %+v, want the shim-owned session listed", sessions)
	}
	if sessions[0].State != SessionRunning {
		t.Errorf("shim-owned session state = %q, want %q", sessions[0].State, SessionRunning)
	}
	if sessions[0].WorktreePath == "" {
		t.Error("shim-owned session has no worktree path; a local reader cannot find its .agent state")
	}

	active, interactive := d.spawnerActiveSessionCounts()
	if active != 1 || interactive != 1 {
		t.Fatalf("occupancy = (active %d, interactive %d), want (1, 1)", active, interactive)
	}
	if d.SessionShimOccupancy() != 1 {
		t.Fatalf("SessionShimOccupancy = %d, want 1", d.SessionShimOccupancy())
	}
}

// TestAdoptedSessionAcceptsInputAndProducesOutput proves both directions of the
// terminal are live through the adopted connection — the concrete meaning of
// "the session works after adoption" (§D5).
func TestAdoptedSessionAcceptsInputAndProducesOutput(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-io")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-io")

	first := f.exchange(t, id, "one")
	second := f.exchange(t, id, "two")
	if second <= first {
		t.Fatalf("host output sequence did not advance: %d then %d; the shim is the sole allocator and must be monotonic", first, second)
	}

	// Geometry is a mutating frame and must be accepted under this daemon's
	// committed generation.
	if err := d.ResizeAdoptedSessionShim(id.OrgID, id.SessionID, 100, 40, 0, 0); err != nil {
		t.Fatalf("ResizeAdoptedSessionShim: %v", err)
	}
}

// TestStopAndTerminalCleanupAfterAdoption is the terminal half of the contract:
// a generation-fenced Stop reaches the shim, the harness is reaped, capacity is
// released, and the durable tombstone is consumed rather than left behind (§D8,
// §D10).
func TestStopAndTerminalCleanupAfterAdoption(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-stop")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-stop")
	f.exchange(t, id, "alive")

	// The control-API id is a bare session id; it must reach the shim rather than
	// falling through to a direct-child stop that would find nothing.
	if !d.StopSession(id.SessionID) {
		t.Fatal("StopSession did not route to the adopted shim")
	}

	waitFor(t, 30*time.Second, "the adopted session to reach a terminal outcome", func() bool {
		return d.SessionShimOccupancy() == 0
	})

	if got := d.ActiveSessions(); len(got) != 0 {
		t.Fatalf("ActiveSessions after terminal outcome = %+v, want empty", got)
	}

	// The tombstone was the proof of death; once the outcome is durably recorded
	// it is disposed. Both halves matter — an undisposed tombstone would keep the
	// session in reconciliation forever.
	registry, err := sessionshim.NewRegistry(f.registry)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// Both halves, and in this order: the liveness claim is withdrawn first, then
	// the proof is disposed. Asserting only that the tombstone is gone would pass
	// against a registry still advertising a live shim for a session that ended.
	waitFor(t, 15*time.Second, "the discovery record to be withdrawn", func() bool {
		_, err := registry.Get(id)
		return err != nil
	})
	waitFor(t, 15*time.Second, "the tombstone to be disposed after the outcome was recorded", func() bool {
		_, err := registry.GetTombstone(id)
		return err != nil
	})
	if proof := d.SessionShimTerminalProof(id.OrgID, id.SessionID); !proof.Proves() {
		t.Fatal("no terminal proof retained for a session this daemon watched end; the outcome would be unresolvable")
	}
}

func TestShimLaunchLifecycleTransfersPreSpawnCleanupExactlyOnce(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.spawner.opts.ShimOwns = d.shimOwnsSession

	var preSpawnCalls atomic.Int32
	var abortCalls atomic.Int32
	var startCalls atomic.Int32
	var endCalls atomic.Int32
	var cleanupCalls atomic.Int32
	var resourceOwned atomic.Bool
	ended := make(chan SessionEvent, 2)
	d.spawner.opts.OnPreSpawn = func(_ SessionSpec, env []string) ([]string, error) {
		preSpawnCalls.Add(1)
		if !resourceOwned.CompareAndSwap(false, true) {
			t.Error("OnPreSpawn acquired an already-owned resource")
		}
		return env, nil
	}
	d.spawner.opts.OnSpawnAborted = func(SessionSpec, error) {
		abortCalls.Add(1)
		resourceOwned.Store(false)
	}
	d.spawner.On(func(ev SessionEvent) {
		switch ev.Kind {
		case SessionEventStarted:
			startCalls.Add(1)
		case SessionEventEnded:
			endCalls.Add(1)
			if !resourceOwned.CompareAndSwap(true, false) {
				t.Error("SessionEventEnded did not receive OnPreSpawn resource ownership")
			}
			cleanupCalls.Add(1)
			ended <- ev
		}
	})

	spec := f.interactiveSpec("sess-lifecycle")
	handle, err := d.spawner.AcceptWork(spec)
	if err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	if got := preSpawnCalls.Load(); got != 1 {
		t.Fatalf("OnPreSpawn calls after launch = %d, want 1", got)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("SessionEventStarted calls after launch = %d, want 1", got)
	}
	if got := abortCalls.Load(); got != 0 {
		t.Fatalf("OnSpawnAborted calls after successful ownership transfer = %d, want 0", got)
	}
	if !d.StopSession(spec.SessionID) {
		t.Fatal("StopSession did not route to the launched shim")
	}

	var terminal SessionEvent
	select {
	case terminal = <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for shim SessionEventEnded")
	}
	if terminal.Spec.SessionID != spec.SessionID {
		t.Errorf("Ended spec session = %q, want %q", terminal.Spec.SessionID, spec.SessionID)
	}
	if terminal.Handle.SessionID != handle.SessionID || terminal.Handle.PID != handle.PID {
		t.Errorf("Ended handle = %+v, want original lifecycle handle %+v", terminal.Handle, *handle)
	}
	if terminal.Handle.State != SessionTerminated {
		t.Errorf("Ended state = %q, want %q after Stop", terminal.Handle.State, SessionTerminated)
	}
	waitFor(t, 15*time.Second, "terminal lifecycle release", func() bool {
		return d.SessionShimOccupancy() == 0
	})
	if got := endCalls.Load(); got != 1 {
		t.Errorf("SessionEventEnded calls = %d, want exactly 1", got)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Errorf("listener cleanup calls = %d, want exactly 1", got)
	}
	if resourceOwned.Load() {
		t.Error("OnPreSpawn resource remained owned after terminal lifecycle delivery")
	}
	if got := abortCalls.Load(); got != 0 {
		t.Errorf("OnSpawnAborted calls after terminal completion = %d, want 0", got)
	}
	select {
	case duplicate := <-ended:
		t.Fatalf("duplicate terminal lifecycle event: %+v", duplicate)
	default:
	}
}

func TestShimControllerDisconnectDoesNotEmitTerminalLifecycle(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	d.spawner.opts.ShimOwns = d.shimOwnsSession

	var endCalls atomic.Int32
	d.spawner.On(func(ev SessionEvent) {
		if ev.Kind == SessionEventEnded {
			endCalls.Add(1)
		}
	})
	spec := f.interactiveSpec("sess-controller-gap")
	if _, err := d.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)
	entry, err := d.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if err := entry.controller.Close(); err != nil {
		t.Fatalf("close controller: %v", err)
	}
	waitFor(t, 5*time.Second, "controller gap quarantine", func() bool {
		return len(d.AdoptedSessionShims()) == 0 && len(d.QuarantinedSessions()) == 1
	})
	if got := d.SessionShimOccupancy(); got != 1 {
		t.Fatalf("SessionShimOccupancy after controller disconnect = %d, want 1 while the harness remains live", got)
	}
	quarantined := d.QuarantinedSessions()
	if len(quarantined) != 1 {
		t.Fatalf("QuarantinedSessions after controller disconnect = %+v, want one visible survivor", quarantined)
	}
	q := quarantined[0]
	processEpoch := entry.controller.Hello().ProcessEpoch
	if q.Identity() != id || q.ShimID != entry.shimID || q.ProcessEpoch != processEpoch ||
		q.ControllerGeneration != uint64(entry.controller.Generation()) {
		t.Errorf("quarantine correlation = %s/%s/%d, want exact %s/%s/%d",
			q.Identity(), q.ShimID, q.ProcessEpoch, id, entry.shimID, processEpoch)
	}
	if q.Reason != sessionshim.QuarantineSocketUnreachable || !q.ConsumesCapacity {
		t.Errorf("quarantine = %+v, want socket_unreachable and consumesCapacity=true", q)
	}
	if q.Detail != "controller stream ended before a terminal observation" {
		t.Errorf("quarantine detail = %q, want bounded controller-loss detail", q.Detail)
	}
	// Repeated projections must not duplicate the same quarantine or capacity
	// charge while heartbeat/status readers race terminal reconciliation.
	for range 32 {
		if got := d.SessionShimOccupancy(); got != 1 {
			t.Fatalf("repeated occupancy during controller gap = %d, want 1", got)
		}
		if got := len(d.QuarantinedSessions()); got != 1 {
			t.Fatalf("repeated quarantine projection length = %d, want 1", got)
		}
	}
	if got := endCalls.Load(); got != 0 {
		t.Fatalf("SessionEventEnded calls after controller disconnect = %d, want 0", got)
	}

	// Let the shim-owned orphan rule reap the harness so the helper process does
	// not outlive the test. A tombstone is durable proof, but this disconnected
	// daemon did not receive the immutable Exit frame and still must not invent an
	// Ended event.
	registry, err := sessionshim.NewRegistry(f.registry)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	var tombstone sessionshim.Tombstone
	waitFor(t, 10*time.Second, "orphan tombstone after controller gap", func() bool {
		var err error
		tombstone, err = registry.GetTombstone(id)
		return err == nil
	})
	waitFor(t, 5*time.Second, "orphan tombstone publication to withdraw the live record", func() bool {
		_, err := registry.Get(id)
		return err != nil
	})
	if err := registry.RemoveTombstoneIncarnation(tombstone); err != nil {
		t.Fatalf("remove exact tombstone for wrong-epoch control: %v", err)
	}
	wrongEpoch := tombstone
	wrongEpoch.ProcessEpoch++
	if err := registry.PutTombstone(wrongEpoch); err != nil {
		t.Fatalf("publish wrong-epoch tombstone control: %v", err)
	}
	if got := d.SessionShimOccupancy(); got != 1 {
		t.Fatalf("occupancy with wrong-epoch tombstone = %d, want 1", got)
	}
	if got := len(d.QuarantinedSessions()); got != 1 {
		t.Fatalf("wrong-epoch tombstone removed quarantine; projection length = %d, want 1", got)
	}
	if err := registry.PutTombstone(tombstone); err != nil {
		t.Fatalf("restore exact tombstone: %v", err)
	}
	var reconcileReaders sync.WaitGroup
	reconcileReaders.Add(32)
	for range 32 {
		go func() {
			defer reconcileReaders.Done()
			if got := d.SessionShimOccupancy(); got != 0 {
				t.Errorf("concurrent reconciled occupancy = %d, want 0", got)
			}
			if got := len(d.QuarantinedSessions()); got != 0 {
				t.Errorf("concurrent reconciled quarantine length = %d, want 0", got)
			}
		}()
	}
	reconcileReaders.Wait()
	waitFor(t, 5*time.Second, "safe tombstone quarantine reconciliation", func() bool {
		return d.SessionShimOccupancy() == 0 && len(d.QuarantinedSessions()) == 0
	})
	proof := d.SessionShimTerminalProof(id.OrgID, id.SessionID)
	if !proof.Proves() {
		t.Fatal("terminal tombstone was not retained as durable proof after quarantine reconciliation")
	}
	if got := endCalls.Load(); got != 0 {
		t.Fatalf("SessionEventEnded calls after disconnected orphan completion = %d, want 0", got)
	}
}

// TestShimOwnershipIsOffByDefaultForInteractiveSessions pins §D11's migration
// law at the selection rule: shipping this code must not change who owns a
// terminal until an operator says so.
func TestShimOwnershipIsOffByDefaultForInteractiveSessions(t *testing.T) {
	t.Parallel()

	d := New(Options{SkipRegistration: true})
	spec := SessionSpec{SessionID: "s1", Mode: interactiveRunMode}
	if d.shimOwnsSession(spec) {
		t.Fatal("shim ownership is on by default; §D11 step 1 ships the protocol with ownership OFF")
	}
	handle, err := d.launchSessionShim(spec, ProjectConfig{ID: "p"}, nil)
	if err != nil {
		t.Fatalf("launchSessionShim with ownership disabled: %v", err)
	}
	if handle != nil {
		t.Fatalf("launchSessionShim returned %+v with ownership disabled, want nil (fall through to the direct path)", handle)
	}
}

// TestOnlyInteractiveSessionsAreShimOwned pins the other half of the selection
// rule. The first delivery is interactive-only (§D11): a headless worker that
// dies with its daemon is re-dispatched, a human's terminal is not.
func TestOnlyInteractiveSessionsAreShimOwned(t *testing.T) {
	t.Parallel()

	d := New(Options{
		SkipRegistration: true,
		SessionShim:      SessionShimConfig{EnableOwnership: true},
	})
	cases := []struct {
		mode string
		want bool
	}{
		{mode: interactiveRunMode, want: true},
		{mode: "", want: false},
		{mode: "interview", want: false},
		{mode: "batch", want: false},
	}
	for _, tc := range cases {
		if got := d.shimOwnsSession(SessionSpec{SessionID: "s", Mode: tc.mode}); got != tc.want {
			t.Errorf("shimOwnsSession(mode=%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

// TestLaunchFailureFailsTheAcceptClosed proves the spawner does not quietly
// demote a shim-owned session to a daemon-parented child when the launch fails.
// A silent demotion would produce exactly the terminal-dies-on-upgrade behaviour
// the ADR exists to remove, with nothing in the logs saying so.
func TestLaunchFailureFailsTheAcceptClosed(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("/tmp", "dsp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	d := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableOwnership: true,
			OrgID:           "test-org",
			RegistryDir:     dir + "/registry",
			LaunchTimeout:   750 * time.Millisecond,
		},
	})
	d.spawner = NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		// A worker that exits immediately and never publishes a record.
		WorkerCommand:     []string{"/bin/sh", "-c", "exit 0"},
		WorktreeParentDir: dir,
	})
	d.spawner.opts.ShimSpawn = d.launchSessionShim
	d.spawner.Resume()

	handle, err := d.spawner.AcceptWork(SessionSpec{
		SessionID: "sess-fail", ProjectID: "p1",
		Repository: "https://example.invalid/x/y", Mode: interactiveRunMode,
	})
	if err == nil {
		t.Fatalf("AcceptWork = %+v, nil; a shim launch that never announced itself must fail the accept", handle)
	}
	if !strings.Contains(err.Error(), "session shim") {
		t.Errorf("error %q does not name the shim launch as the cause", err)
	}
	if got := d.SessionShimOccupancy(); got != 0 {
		t.Errorf("SessionShimOccupancy after a failed launch = %d, want 0", got)
	}
	if _, tracked := d.spawner.sessions["sess-fail"]; tracked {
		t.Error("a failed shim launch left a direct-child entry behind")
	}
}

// TestLaunchEnvironmentCarriesTheContractAndNoSecrets pins §D6's no-secret bound
// at the carrier the daemon actually writes. The launch env is visible in the
// process table, so anything secret here would be leaked by the carrier itself.
func TestLaunchEnvironmentCarriesTheContractAndNoSecrets(t *testing.T) {
	t.Parallel()

	launch := sessionshim.Launch{
		Identity:     sessionshim.Identity{OrgID: "o", SessionID: "s"},
		RegistryDir:  "/tmp/reg",
		Orphan:       sessionshim.DefaultOrphanPolicy(),
		ProcessEpoch: 3,
	}
	pairs := envPairs(launch.Env())
	joined := strings.Join(pairs, "\n")
	for _, key := range sessionshim.EnvKeys() {
		if !strings.Contains(joined, key+"=") {
			t.Errorf("launch environment is missing %s", key)
		}
	}
	// Sorted, so a spawn environment is byte-stable across runs.
	for i := 1; i < len(pairs); i++ {
		if pairs[i-1] > pairs[i] {
			t.Fatalf("launch environment is not sorted: %q before %q", pairs[i-1], pairs[i])
		}
	}
	// Round trip: the only producer and the only consumer must agree.
	env := launch.Env()
	got, err := sessionshim.LaunchFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("LaunchFromEnv on the daemon's own overlay: %v", err)
	}
	if got.Identity != launch.Identity || got.RegistryDir != launch.RegistryDir ||
		got.ProcessEpoch != launch.ProcessEpoch || got.Orphan != launch.Orphan {
		t.Fatalf("round trip = %+v, want %+v", got, launch)
	}
}

// TestForwardedSequenceIsRecordedNotAllocated pins §D5's division of labour: the
// daemon records the highest sequence it forwarded so a LATER adoption can
// resume from it, and never allocates one itself.
func TestForwardedSequenceIsRecordedNotAllocated(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon

	if _, err := d.spawner.AcceptWork(f.interactiveSpec("sess-seq")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-seq")
	f.exchange(t, id, "seq")

	waitFor(t, 20*time.Second, "the daemon to record a forwarded sequence", func() bool {
		return d.SessionShimForwardedSeq(id.OrgID, id.SessionID) > 0
	})
	if got := d.SessionShimForwardedSeq(id.OrgID, "not-a-session"); got != 0 {
		t.Errorf("forwarded sequence for an unknown session = %d, want 0", got)
	}
}

func TestForwardedSequenceRequiresDurableCarrier(t *testing.T) {
	f := newShimSpawnFixture(t)
	// The ordinary event hook is intentionally still present: it is an observer,
	// not proof that a composing carrier durably accepted the frame.
	f.daemon.opts.SessionShim.OnSessionEventDurable = nil

	spec := f.interactiveSpec("sess-observer-only")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)
	seq := f.exchange(t, id, "observer-only")
	if seq == 0 {
		t.Fatal("observer did not receive output")
	}
	time.Sleep(250 * time.Millisecond)
	if got := f.daemon.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != 0 {
		t.Fatalf("observer-only forwarded sequence = %d, want durable cursor unchanged at 0", got)
	}
}

func TestForwardedSequenceRejectsDurableCarrierError(t *testing.T) {
	f := newShimSpawnFixture(t)
	f.daemon.opts.SessionShim.OnSessionEventDurable = func(sessionshim.Identity, sessionshim.ControllerEvent) error {
		return errors.New("carrier unavailable")
	}

	spec := f.interactiveSpec("sess-carrier-error")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)
	if err := f.daemon.WriteAdoptedSessionShimInput(id.OrgID, id.SessionID, []byte("carrier-error\r")); err != nil {
		t.Fatalf("WriteAdoptedSessionShimInput: %v", err)
	}
	waitFor(t, 5*time.Second, "the observer to receive the rejected frame", func() bool {
		out, _ := f.events.output(id)
		return strings.Contains(out, "carrier-error")
	})
	if got := f.daemon.SessionShimForwardedSeq(id.OrgID, id.SessionID); got != 0 {
		t.Fatalf("carrier-error forwarded sequence = %d, want durable cursor unchanged at 0", got)
	}
}

// TestRestartFenceRetainsTheAdoptionResumeCursor pins the replacement-daemon
// seam: before any new output arrives, the fence must still report the durable
// last-forwarded sequence from which this controller resumed. Resetting it to
// zero would make the composing store acknowledge a correlation older than the
// carrier's durable state.
func TestRestartFenceRetainsTheAdoptionResumeCursor(t *testing.T) {
	f := newShimSpawnFixture(t)
	first := f.daemon

	if _, err := first.spawner.AcceptWork(f.interactiveSpec("sess-resume-fence")); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity("sess-resume-fence")
	f.exchange(t, id, "resume-fence")
	waitFor(t, 20*time.Second, "the first daemon to record forwarded output", func() bool {
		return first.SessionShimForwardedSeq(id.OrgID, id.SessionID) > 0
	})
	lastForwarded := first.SessionShimForwardedSeq(id.OrgID, id.SessionID)
	first.ReleaseAdoptedSessionShims()

	replacement := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true,
			OrgID:          f.orgID,
			RegistryDir:    f.registry,
			ResumeFrom: func(orgID, sessionID string) uint64 {
				if orgID == id.OrgID && sessionID == id.SessionID {
					return lastForwarded + 1
				}
				return 0
			},
			Orphan: sessionshim.OrphanPolicy{
				Deadline:          2 * time.Second,
				TerminationGrace:  500 * time.Millisecond,
				PropagationMargin: 0,
			},
		},
	})
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)
	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("replacement adoptSessionShims: %v", err)
	}

	fence, err := replacement.RequestSessionShimRestartFence(context.Background(), "fence-resume")
	if err != nil {
		t.Fatalf("RequestSessionShimRestartFence: %v", err)
	}
	if len(fence.Sessions) != 1 {
		t.Fatalf("fence sessions = %+v, want one resumed session", fence.Sessions)
	}
	if got := fence.Sessions[0].LastForwardedSeq; got != lastForwarded {
		t.Fatalf("fence lastForwardedSeq = %d, want durable adoption cursor %d", got, lastForwarded)
	}
}

func TestStartupAdoptionRefusesReadyUntilDurableCarrierRehydration(t *testing.T) {
	f := newShimSpawnFixture(t)
	// Give two replacement attempts ample room before the shim-owned orphan
	// deadline. The first is deliberately refused by the composing callback.
	f.daemon.opts.SessionShim.Orphan.Deadline = 15 * time.Second
	spec := f.interactiveSpec("sess-startup-callback")
	if _, err := f.daemon.spawner.AcceptWork(spec); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	id := f.identity(spec.SessionID)
	f.daemon.ReleaseAdoptedSessionShims()

	refusing := New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption: true,
			RegistryDir:    f.registry,
			HostID:         "host-startup",
			OnAdoption: func(context.Context, SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
				return SessionShimAdoptionReceipt{}, errors.New("durable carrier unavailable")
			},
		},
	})
	refusing.config = &Config{Capacity: CapacityConfig{MaxConcurrentSessions: 4}}
	refusing.setState(StateRunning)
	err := refusing.adoptSessionShims(context.Background())
	if err == nil || !strings.Contains(err.Error(), "durable carrier unavailable") {
		t.Fatalf("adoptSessionShims = %v, want durable carrier refusal", err)
	}
	if refusing.SessionShimAdoptionComplete() {
		t.Fatal("adoption reads complete after durable carrier refusal")
	}
	if got := refusing.RegistrationStatus(); got != RegistrationDraining {
		t.Fatalf("RegistrationStatus after callback refusal = %q, want draining", got)
	}

	var emitted shimwire.SnapshotResult
	var replacement *Daemon
	replacement = New(Options{
		SkipRegistration: true,
		SessionShim: SessionShimConfig{
			EnableAdoption:               true,
			RegistryDir:                  f.registry,
			HostID:                       "host-startup",
			RequireAuthoritativeSnapshot: true,
			PrepareAdoption: func(_ context.Context, preparation SessionShimAdoptionPreparation) (sessionshim.PreparedAdoption, error) {
				return sessionshim.PreparedAdoption{Extensions: shimwire.Extensions{
					Values: map[string]string{shimwire.ExtCarrierEpoch: "20"},
				}, ControllerGeneration: preparation.CurrentControllerGeneration + 1}, nil
			},
			OnAdoption: func(ctx context.Context, evidence SessionShimAdoptionEvidence) (SessionShimAdoptionReceipt, error) {
				if evidence.Identity != id {
					return SessionShimAdoptionReceipt{}, fmt.Errorf("wrong identity %s", evidence.Identity)
				}
				var err error
				emitted, err = evidence.SnapshotProxy.Emit(ctx)
				if err != nil {
					return SessionShimAdoptionReceipt{}, err
				}
				return SessionShimAdoptionReceipt{DurableCorrelation: []byte("durable-startup")}, nil
			},
			OnSessionEventDurable: func(sessionshim.Identity, sessionshim.ControllerEvent) error { return nil },
			PrepareAdoptionBatch: func(context.Context, string, string) ([]byte, error) {
				return []byte("expected-startup-batch"), nil
			},
			OnAdoptionBatch: func(_ context.Context, batch SessionShimAdoptionBatch) (SessionShimAdoptionBatchReceipt, error) {
				if len(batch.Adopted) != 1 || batch.Adopted[0].Evidence.SnapshotProxy != nil {
					return SessionShimAdoptionBatchReceipt{}, fmt.Errorf("batch retained ephemeral snapshot proxy: %+v", batch.Adopted)
				}
				return SessionShimAdoptionBatchReceipt{
					DurableCorrelation: []byte("startup-batch-revision"), AdoptionRevision: "startup-revision",
				}, nil
			},
			OnAdoptionPublished: func(_ context.Context, publication SessionShimAdoptionPublication) ([]SessionShimCarrierActivationReceipt, error) {
				if _, err := replacement.adoptedShimEntry(id.OrgID, id.SessionID); err != nil {
					return nil, fmt.Errorf("activation before publication: %w", err)
				}
				return []SessionShimCarrierActivationReceipt{{
					Activation: publication.Carriers[0], AckSeq: emitted.AtSeq + 1,
				}}, nil
			},
		},
	})
	t.Cleanup(replacement.ReleaseAdoptedSessionShims)
	if err := replacement.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("replacement adoptSessionShims: %v", err)
	}
	if !replacement.SessionShimAdoptionComplete() {
		t.Fatal("adoption did not complete after durable carrier handoff")
	}
	entry, err := replacement.adoptedShimEntry(id.OrgID, id.SessionID)
	if err != nil {
		t.Fatalf("adoptedShimEntry: %v", err)
	}
	if got, _ := entry.adoption.Extensions.Get(shimwire.ExtCarrierEpoch); got != "20" {
		t.Fatalf("replacement adoption carrier_epoch = %q, want 20", got)
	}
	if err := replacement.StopAdoptedSessionShim(id.OrgID, id.SessionID, shimwire.StopHostShutdown); err != nil {
		t.Fatalf("StopAdoptedSessionShim: %v", err)
	}
	waitFor(t, 30*time.Second, "replacement terminal cleanup", func() bool {
		return replacement.SessionShimOccupancy() == 0
	})
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// TestAdmissionRefusesWorkAgainstShimHeldCapacity closes the double-booking gap
// §D7 opens at the ADMISSION boundary rather than the advertisement one.
//
// A shim-owned session never enters the spawner's own registry by design, so a
// host that reported its occupancy honestly and then admitted against its
// direct-child count alone would still accept work it has no core to run.
func TestAdmissionRefusesWorkAgainstShimHeldCapacity(t *testing.T) {
	t.Parallel()

	held := 0
	s := NewWorkerSpawner(SpawnerOptions{
		Projects:              []ProjectConfig{{ID: "p1", Repository: "https://example.invalid/x/y"}},
		EnabledProjectIDs:     []string{"p1"},
		MaxConcurrentSessions: 2,
		WorkerCommand:         []string{"/bin/sh", "-c", "sleep 30"},
		ExternalOccupancy:     func() int { return held },
	})
	s.Resume()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.DrainContext(ctx)
	})

	spec := func(id string) SessionSpec {
		return SessionSpec{SessionID: id, ProjectID: "p1", Repository: "https://example.invalid/x/y"}
	}

	// Two shims already hold this host's whole envelope. Nothing may be admitted,
	// even though the spawner parents no children at all.
	held = 2
	if _, err := s.AcceptWork(spec("s1")); err == nil {
		t.Fatal("AcceptWork succeeded while shims held every slot on the host")
	}

	// One slot frees up; exactly one session fits, and the next is refused.
	held = 1
	if _, err := s.AcceptWork(spec("s2")); err != nil {
		t.Fatalf("AcceptWork with one free slot: %v", err)
	}
	if _, err := s.AcceptWork(spec("s3")); err == nil {
		t.Fatal("AcceptWork admitted a third session into a two-slot host")
	}
}

func TestStatusAndDoctorExposeRealSecretFreeSessionShimDiagnostics(t *testing.T) {
	f := newShimSpawnFixture(t)
	d := f.daemon
	if err := d.adoptSessionShims(context.Background()); err != nil {
		t.Fatalf("initial adoption pass: %v", err)
	}
	d.config = DefaultConfig()
	d.setState(StateRunning)
	id := f.identity("diagnostic-live")
	if _, err := d.AcceptWork(f.interactiveSpec(id.SessionID)); err != nil {
		t.Fatalf("AcceptWork: %v", err)
	}
	seq := f.exchange(t, id, "diagnostic-output")
	d.shims.mu.Lock()
	d.shims.quarantined = append(d.shims.quarantined, sessionshim.QuarantinedSession{
		OrgID: "test-org", SessionID: "diagnostic-quarantine", ShimID: "shim-quarantine",
		ProcessEpoch: 9, ControllerGeneration: 11, ProtocolMin: 1, ProtocolMax: 1,
		Reason: sessionshim.QuarantineDuplicateIdentity, Detail: "socket /private/secret/path",
		AgeSeconds: 3, ConsumesCapacity: true,
	})
	d.shims.mu.Unlock()

	server := NewServer(d)
	statusRecorder := httptest.NewRecorder()
	server.handleStatus(statusRecorder, httptest.NewRequest("GET", "/api/daemon/status", nil))
	var status afclient.DaemonStatusResponse
	if err := json.NewDecoder(statusRecorder.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	diagnostic := status.SessionShim
	if diagnostic.OwnershipMode != afclient.DaemonSessionShimAdoptionAndOwnership ||
		!diagnostic.AdoptionComplete || diagnostic.AdoptionCompletedAt == "" || diagnostic.OccupiedSlots != 2 {
		t.Fatalf("status sessionShim summary = %+v", diagnostic)
	}
	if len(diagnostic.Adopted) != 1 {
		t.Fatalf("status adopted = %+v, want one", diagnostic.Adopted)
	}
	adopted := diagnostic.Adopted[0]
	if adopted.OrgID != id.OrgID || adopted.SessionID != id.SessionID || adopted.ShimID == "" ||
		adopted.ProcessEpoch == 0 || adopted.ControllerGeneration == 0 || adopted.LastForwardedSeq != seq ||
		adopted.HarnessPID <= 0 || adopted.HarnessStartedAt <= 0 || adopted.ProtocolMin != 1 || adopted.ProtocolMax != 2 ||
		adopted.ProtocolVersion != 2 || !adopted.AuthoritativeSnapshot || adopted.ControllerID != d.ControllerID() ||
		adopted.Phase == "" || !adopted.ConsumesCapacity || diagnostic.ControllerID != d.ControllerID() {
		t.Fatalf("status adopted correlation = %+v", adopted)
	}
	if len(diagnostic.Quarantined) != 1 || diagnostic.Quarantined[0].ShimID != "shim-quarantine" ||
		diagnostic.Quarantined[0].ControllerGeneration != 11 ||
		diagnostic.Quarantined[0].Detail != "" || !diagnostic.Quarantined[0].ConsumesCapacity {
		t.Fatalf("status quarantine = %+v", diagnostic.Quarantined)
	}
	raw, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"hostId"`, `"workarea"`, `"token"`, `"receipt"`, `"path"`, `"data"`} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatalf("session-shim diagnostic contains forbidden field %s: %s", forbidden, raw)
		}
	}

	doctorRecorder := httptest.NewRecorder()
	server.handleDoctor(doctorRecorder, httptest.NewRequest("GET", "/api/daemon/doctor", nil))
	var doctor struct {
		SessionShim afclient.DaemonSessionShimStatus `json:"sessionShim"`
	}
	if err := json.NewDecoder(doctorRecorder.Body).Decode(&doctor); err != nil {
		t.Fatalf("decode doctor: %v", err)
	}
	if !reflect.DeepEqual(doctor.SessionShim, diagnostic) {
		t.Fatalf("doctor/status session-shim drift:\ndoctor=%+v\nstatus=%+v", doctor.SessionShim, diagnostic)
	}
}
