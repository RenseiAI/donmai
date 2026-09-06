package sessionshim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
)

func flowTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	// The socket lives beside the record and AF_UNIX sun_path is short.
	dir, err := os.MkdirTemp("/tmp", "ssf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	registry, err := NewRegistry(filepath.Join(dir, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	return registry, dir
}

// TestStreamFlowSidecarRoundTripsAndValidates is the schema pin. The state is
// secret-free, incarnation-bound, bounded, and strictly decoded, exactly like
// the durable acknowledgement cursor it sits beside.
func TestStreamFlowSidecarRoundTripsAndValidates(t *testing.T) {
	t.Parallel()
	valid := StreamFlowControl{
		SchemaVersion: streamFlowSchemaVersion,
		OrgID:         "org-flow", SessionID: "session-flow",
		ShimID: "shim-1", ProcessEpoch: 4,
		Paused: true, PausedSinceUnixNano: 1700, PendingBytes: 9000,
		ObservedAtUnixNano: 1800,
	}
	tests := []struct {
		name    string
		state   StreamFlowControl
		wantErr string
	}{
		{name: "valid", state: valid},
		{
			name:    "wrong schema version",
			state:   func() StreamFlowControl { s := valid; s.SchemaVersion = 2; return s }(),
			wantErr: "schemaVersion",
		},
		{
			name:    "missing shim id",
			state:   func() StreamFlowControl { s := valid; s.ShimID = ""; return s }(),
			wantErr: "shim id",
		},
		{
			name:    "missing observation time",
			state:   func() StreamFlowControl { s := valid; s.ObservedAtUnixNano = 0; return s }(),
			wantErr: "observedAt",
		},
		{
			name:    "missing identity",
			state:   func() StreamFlowControl { s := valid; s.SessionID = ""; return s }(),
			wantErr: "session",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := tc.state.encode()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("encode(%+v) = %v, want an error naming %q", tc.state, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("encode a valid state: %v", err)
			}
			decoded, err := decodeStreamFlow(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if decoded != tc.state {
				t.Fatalf("round trip = %+v, want %+v", decoded, tc.state)
			}
			if !decoded.Degraded() || decoded.PausedSince().UnixNano() != tc.state.PausedSinceUnixNano {
				t.Fatalf("accessors on %+v disagree with the fields", decoded)
			}
			if strings.Contains(string(raw), "/") {
				t.Fatalf("published state carries a path-shaped value: %s", raw)
			}
		})
	}
	if _, err := decodeStreamFlow([]byte(`{"schemaVersion":1,"orgId":"o","sessionId":"s","shimId":"x","token":"secret"}`)); err == nil {
		t.Fatal("an unknown field was accepted; the sidecar schema is not closed")
	}
}

// TestStreamFlowSidecarIsInvisibleToTheRecordScan is the compatibility pin, and
// it is the reason this state is a sidecar at all.
//
// The §D6 discovery Record is decoded with DisallowUnknownFields, so a shim that
// wrote a degraded-state FIELD into it would be refused — and the live session
// quarantined — by every daemon built before that field existed. A marker that
// makes an older controller quarantine a healthy shim is the exact failure class
// this whole change removes, so the state goes beside the record. Scan must
// therefore not see it, and must not be made unhappy by it.
func TestStreamFlowSidecarIsInvisibleToTheRecordScan(t *testing.T) {
	t.Parallel()
	registry, _ := flowTestRegistry(t)
	id := Identity{OrgID: "org-flow", SessionID: "session-flow"}
	rec := Record{
		SchemaVersion: RecordSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: "shim-scan", ProcessEpoch: 2,
		PID: os.Getpid(), ProcessStartedAt: time.Now().UnixNano(),
		SocketPath:  filepath.Join(registry.Dir(), id.SocketName()),
		ProtocolMin: 1, ProtocolMax: 3, Phase: "running",
		CreatedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.Put(rec); err != nil {
		t.Fatal(err)
	}
	state := StreamFlowControl{
		SchemaVersion: streamFlowSchemaVersion,
		OrgID:         id.OrgID, SessionID: id.SessionID,
		ShimID: rec.ShimID, ProcessEpoch: rec.ProcessEpoch,
		Paused: true, PendingBytes: 12345, ObservedAtUnixNano: time.Now().UnixNano(),
	}
	if err := registry.PutStreamFlow(state); err != nil {
		t.Fatal(err)
	}

	entries, err := registry.Scan()
	if err != nil {
		t.Fatalf("scan with a flow sidecar present: %v", err)
	}
	if len(entries) != 1 || entries[0].Err != nil || entries[0].Record.ShimID != rec.ShimID {
		t.Fatalf("scan = %+v, want exactly the one discovery record and no error", entries)
	}

	read, err := registry.StreamFlow(id, rec.ShimID, rec.ProcessEpoch)
	if err != nil || !read.Paused || read.PendingBytes != 12345 {
		t.Fatalf("read back = %+v err=%v, want the published state", read, err)
	}
	// A different incarnation must not read this one's state: the sidecar is
	// evidence about a specific live shim, not about the identity, so a shim id
	// or epoch that does not match addresses a different file entirely.
	other, err := registry.StreamFlow(id, "other-shim", rec.ProcessEpoch)
	if err != nil || other != (StreamFlowControl{}) {
		t.Fatalf("another incarnation read %+v err=%v, want the zero value", other, err)
	}
	if newer, err := registry.StreamFlow(id, rec.ShimID, rec.ProcessEpoch+1); err != nil || newer != (StreamFlowControl{}) {
		t.Fatalf("a later epoch read %+v err=%v, want the zero value", newer, err)
	}

	if err := registry.RemoveStreamFlow(id, rec.ShimID, rec.ProcessEpoch); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Missing is the ordinary case — no back-pressure observed — not a failure.
	gone, err := registry.StreamFlow(id, rec.ShimID, rec.ProcessEpoch)
	if err != nil || gone != (StreamFlowControl{}) {
		t.Fatalf("read after removal = %+v err=%v, want the zero value and no error", gone, err)
	}
	if err := registry.RemoveStreamFlow(id, rec.ShimID, rec.ProcessEpoch); err != nil {
		t.Fatalf("removal is not idempotent: %v", err)
	}
}

// TestShimPausesItsHarnessAndPublishesTheDegradationEndToEnd is the shim-level
// pin: the back-pressure a stalled consumer applies reaches the harness, and it
// is published while it lasts.
//
// A shim that has stopped reading looks EXACTLY like an idle terminal from every
// other surface — no frames, no error, no drop — which is how the condition can
// last for minutes with nothing naming it. Removing the OnChange wiring in Start
// turns this RED at the sidecar assertion; removing the flow control entirely
// turns it RED at the pause.
func TestShimPausesItsHarnessAndPublishesTheDegradationEndToEnd(t *testing.T) {
	registry, dir := flowTestRegistry(t)
	id := Identity{OrgID: "org-flow", SessionID: "session-flow"}
	const lines = 4000
	shim, err := Start(Options{
		Identity: id, Registry: registry, ProcessEpoch: 9,
		WorkareaPath: filepath.Join(dir, "workarea"),
		Spec: ptyhost.Spec{
			Command: []string{
				"/bin/sh", "-c",
				"stty -echo; read -r _; i=0; while [ $i -lt 4000 ]; do " +
					"printf 'ln%04d-0123456789012345678901234567890123456789\\n' \"$i\"; i=$((i+1)); done",
			},
			OutputFlowControl: &ptyhost.OutputFlowControl{HighWaterBytes: 2048, PauseBound: time.Minute},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
		_ = shim.Close()
	})

	sub, err := shim.Session().Subscribe(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := shim.Session().WriteInput([]byte("go\n")); err != nil {
		t.Fatal(err)
	}

	waitForShimFlow(t, "the shim to stop reading its harness", func() bool {
		return shim.OutputFlowState().Paused
	})

	var published StreamFlowControl
	waitForShimFlow(t, "the degradation to be published in the registry", func() bool {
		state, err := registry.StreamFlow(id, shim.ShimID(), 9)
		if err != nil {
			t.Errorf("read published flow state: %v", err)
			return true
		}
		published = state
		return state.Paused
	})
	if !published.Degraded() || published.PendingBytes <= 2048 || published.PausedSince().IsZero() {
		t.Fatalf("published degradation = %+v, want a paused reader with the bytes it is holding", published)
	}

	// An older daemon's classification pass must be entirely unaffected by it.
	entries, err := registry.Scan()
	if err != nil || len(entries) != 1 || entries[0].Err != nil {
		t.Fatalf("scan during a degradation = %+v err=%v, want exactly the live record", entries, err)
	}

	// The consumer returns. The stream is whole — back-pressure loses nothing —
	// and the degradation is withdrawn.
	var (
		seen  strings.Builder
		seqs  []uint64
		drain = make(chan struct{})
	)
	go func() {
		defer close(drain)
		for frame := range sub.Frames() {
			seqs = append(seqs, frame.Seq)
			if frame.Type == attachwire.TypeOutput {
				seen.Write(attachwire.DecodeOutput(frame.Payload).Data)
			}
		}
	}()

	waitForShimFlow(t, "the degradation to be withdrawn", func() bool {
		if shim.OutputFlowState().Paused {
			return false
		}
		state, err := registry.StreamFlow(id, shim.ShimID(), 9)
		return err == nil && !state.Degraded()
	})

	select {
	case <-drain:
	case <-time.After(60 * time.Second):
		t.Fatal("the subscription never completed after the reader resumed")
	}
	for i, seq := range seqs {
		if seq != uint64(i+1) {
			t.Fatalf("host sequence %d of %d = %d, want %d: a back-pressured stream lost or reordered frames",
				i, len(seqs), seq, i+1)
		}
	}
	for _, line := range []int{0, lines / 2, lines - 1} {
		want := fmt.Sprintf("ln%04d-", line)
		if !strings.Contains(seen.String(), want) {
			t.Fatalf("line %q is missing from a stream that was only ever back-pressured", want)
		}
	}

	// And the sidecar does not outlive the shim that published it: a state left
	// behind would be read as a live degradation by the next daemon to look.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := shim.Terminate(ctx); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	after, err := registry.StreamFlow(id, shim.ShimID(), 9)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read flow state after termination: %v", err)
	}
	if after != (StreamFlowControl{}) {
		t.Fatalf("flow state survived the shim that published it: %+v", after)
	}
}

func waitForShimFlow(t *testing.T, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestStartNeverMutatesTheCallersFlowControl pins ownership of the caller's
// configuration.
//
// Start installs its own OnChange hook, and it used to do so by writing through
// the pointer the caller supplied. Two shims started from one shared
// *OutputFlowControl would then share one hook: the second would overwrite the
// first's, and the FIRST shim would silently stop publishing its degradations —
// a bug whose only symptom is a shim that has stopped reading and says nothing
// about it, which is the exact silence this state exists to break.
//
// Restoring the write-through (`opts.Spec.OutputFlowControl.OnChange = ...`)
// turns this RED at the first assertion.
func TestStartNeverMutatesTheCallersFlowControl(t *testing.T) {
	registry, dir := flowTestRegistry(t)
	shared := &ptyhost.OutputFlowControl{HighWaterBytes: 4096, LowWaterBytes: 1024, PauseBound: time.Minute}
	idle := []string{"/bin/sh", "-c", "stty -echo; while IFS= read -r line; do printf 'ack:%s\n' \"$line\"; done"}

	shims := make([]*Shim, 0, 2)
	for i, session := range []string{"session-one", "session-two"} {
		id := Identity{OrgID: "org-flow", SessionID: session}
		shim, err := Start(Options{
			Identity: id, Registry: registry, ProcessEpoch: uint64(i + 1),
			WorkareaPath: filepath.Join(dir, "workarea"),
			Spec:         ptyhost.Spec{Command: idle, OutputFlowControl: shared},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = shim.Terminate(ctx)
			_ = shim.Close()
		})
		shims = append(shims, shim)
	}

	if shared.OnChange != nil {
		t.Fatal("Start wrote its hook into the caller's configuration: a second shim started from the " +
			"same struct would steal the first's callback and the first would stop publishing")
	}
	// The caller's own marks are still what the caller set, and still reached
	// both sessions.
	if shared.HighWaterBytes != 4096 || shared.LowWaterBytes != 1024 || shared.PauseBound != time.Minute {
		t.Fatalf("the caller's configuration was modified: %+v", *shared)
	}
	for i, shim := range shims {
		if state := shim.OutputFlowState(); state.Paused {
			t.Fatalf("shim %d reports paused on an idle session: %+v", i, state)
		}
	}
}
