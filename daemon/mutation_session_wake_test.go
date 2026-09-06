package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/sessionshim"
)

// The fake harness. It is the specimen shape reduced to a shell line: a
// process that owns a terminal, reads it line by line, and answers each line
// with a marker.
//
//   - `-echo` so the terminal does not answer for the harness. Without it the
//     line discipline echoes every byte written and a test would pass on the
//     kernel's reply while the harness itself was frozen — exactly the false
//     reading this whole issue is about.
//   - `-isig` so the interrupt byte is DELIVERED AS DATA rather than turned
//     into a signal that kills the fixture. That is also the honest model of
//     the seats that wedge: a terminal UI in raw mode has signal generation
//     off, so rung 2's interrupt reaches it as bytes.
const wakeFixtureHarness = `stty -echo -isig; while IFS= read -r line; do printf 'ack\n'; done`

// wakeFixtureFrozenHarness never reads its terminal. It is the wedge itself:
// alive, holding the PTY, consuming no input, emitting nothing. Writes to it
// succeed at every layer and produce no output — which is the property that
// makes "delivered" and "answered" two different facts.
const wakeFixtureFrozenHarness = `stty -echo -isig; while :; do sleep 30; done`

type wakeFixture struct {
	daemon *Daemon
	id     sessionshim.Identity
	output <-chan []byte
}

func newWakeFixture(t *testing.T, harness string) *wakeFixture {
	t.Helper()
	// A unix socket path has a short platform limit, and the registry dir is
	// part of it — t.TempDir()'s long test-named path overflows it.
	dir, err := os.MkdirTemp("/tmp", "dwk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	registryDir := filepath.Join(dir, "r")
	registry, err := sessionshim.NewRegistry(registryDir)
	if err != nil {
		t.Fatal(err)
	}

	id := sessionshim.Identity{OrgID: "org-wake", SessionID: "session-wake"}
	shim, err := sessionshim.Start(sessionshim.Options{
		Identity: id, Registry: registry, ProcessEpoch: 1,
		Spec:         ptyhost.Spec{Command: []string{"/bin/sh", "-c", harness}},
		WorkareaPath: filepath.Join(dir, "w"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shim.Terminate(ctx)
	})

	adoption, err := sessionshim.Adopt(context.Background(), sessionshim.AdoptOptions{
		Registry: registry, ControllerID: "wake-controller",
	})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if len(adoption.Adopted) != 1 {
		t.Fatalf("adopted %d shims, want 1", len(adoption.Adopted))
	}
	ctrl := adoption.Adopted[0]
	t.Cleanup(adoption.Close)

	// The controller stalls if nobody drains it, so the drain IS the fixture's
	// observation point: terminal bytes the harness produced, and nothing else.
	output := make(chan []byte, 64)
	go func() {
		defer close(output)
		for ev := range ctrl.Events() {
			if ev.Kind != sessionshim.EventOutput || len(ev.Data) == 0 {
				continue
			}
			select {
			case output <- append([]byte(nil), ev.Data...):
			default:
			}
		}
	}()

	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{RegistryDir: registryDir}})
	d.shims.mu.Lock()
	d.shims.adopted[id] = adoptedShim{shimID: "shim-wake", controller: ctrl}
	d.shims.mu.Unlock()

	return &wakeFixture{daemon: d, id: id, output: output}
}

// awaitAck waits for the fake harness to answer. A wedged harness never does,
// so the timeout is the assertion, not a flake guard.
func (f *wakeFixture) awaitAck(t *testing.T, within time.Duration) bool {
	t.Helper()
	deadline := time.After(within)
	var seen strings.Builder
	for {
		select {
		case data, ok := <-f.output:
			if !ok {
				return false
			}
			seen.Write(data)
			if strings.Contains(seen.String(), "ack") {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func wakeMutation(t *testing.T, op, id string, params any) PendingMutation {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return PendingMutation{ID: id, Op: op, Params: raw}
}

// ---------------------------------------------------------------------------
// Vocabulary + dispatch
// ---------------------------------------------------------------------------

func TestIsSessionMutationOp(t *testing.T) {
	tests := []struct {
		name string
		op   string
		want bool
	}{
		{"kill is a session op", "session.kill", true},
		{"wake is a session op", "session.wake", true},
		{"restart-harness is a session op", "session.restart-harness", true},
		{"config op is not", "project.enable", false},
		{"pool op is not", "pool.deleted", false},
		{"unknown op is not", "session.teleport", false},
		{"empty op is not", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSessionMutationOp(tc.op); got != tc.want {
				t.Fatalf("isSessionMutationOp(%q) = %v, want %v", tc.op, got, tc.want)
			}
		})
	}
}

// The two dispatch sites must agree on the vocabulary. They are separate call
// sites — the full daemon pipeline and the session-only embedder pipeline —
// and a verb reaching one but not the other is a silent hole: an embedder's
// wedged seat would sit unremediated with the mutation ACKed as skipped.
func TestSessionMutationVocabularyIsSingleSourced(t *testing.T) {
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{RegistryDir: t.TempDir()}})
	for _, op := range []string{"session.wake", "session.restart-harness"} {
		t.Run(op, func(t *testing.T) {
			m := wakeMutation(t, op, "m-1", sessionWakeParams{SessionID: "absent", OrgID: "org-x"})

			// Both pipelines must REACH the handler. Reaching it fails here
			// (no such shim), and that failure is the proof of arrival: an
			// unrouted verb would come back "unsupported" or be skipped whole.
			err := d.applyOneMutation(m)
			if err == nil || strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("applyOneMutation(%s) = %v, want a handler-level error", op, err)
			}

			applied, failures := d.ApplySessionMutations(context.Background(), []PendingMutation{m})
			if len(applied) != 0 {
				t.Fatalf("ApplySessionMutations applied = %v, want none", applied)
			}
			if len(failures) != 1 {
				t.Fatalf("ApplySessionMutations failures = %d, want 1 (verb must be routed, not skipped)", len(failures))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Params
// ---------------------------------------------------------------------------

func TestDecodeSessionWakeParams(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantSessionID string
		wantOrgID     string
		wantErrSubstr string
	}{
		{
			name: "full params", raw: `{"sessionId":"s-1","orgId":"o-1","reason":"wedged"}`,
			wantSessionID: "s-1", wantOrgID: "o-1",
		},
		{
			name: "org id is optional", raw: `{"sessionId":"s-1"}`,
			wantSessionID: "s-1", wantOrgID: "",
		},
		{
			name: "surrounding whitespace is trimmed", raw: `{"sessionId":"  s-1  ","orgId":" o-1 "}`,
			wantSessionID: "s-1", wantOrgID: "o-1",
		},
		{
			name: "missing session id", raw: `{"orgId":"o-1"}`,
			wantErrSubstr: "requires sessionId",
		},
		{
			name: "whitespace-only session id", raw: `{"sessionId":"   "}`,
			wantErrSubstr: "requires sessionId",
		},
		{
			name: "malformed json", raw: `{`,
			wantErrSubstr: "decode params",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeSessionWakeParams("session.wake", json.RawMessage(tc.raw))
			if tc.wantErrSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErrSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got.SessionID != tc.wantSessionID || got.OrgID != tc.wantOrgID {
				t.Fatalf("params = %+v, want sessionId=%q orgId=%q", got, tc.wantSessionID, tc.wantOrgID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Controller resolution
// ---------------------------------------------------------------------------

func TestResolveWakeControllerRefusesAmbiguousSessionID(t *testing.T) {
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{RegistryDir: t.TempDir()}})
	d.shims.mu.Lock()
	d.shims.adopted[sessionshim.Identity{OrgID: "org-a", SessionID: "shared"}] = adoptedShim{shimID: "a"}
	d.shims.adopted[sessionshim.Identity{OrgID: "org-b", SessionID: "shared"}] = adoptedShim{shimID: "b"}
	d.shims.mu.Unlock()

	// Writing keystrokes into the wrong tenant's terminal is worse than not
	// remediating, so a bare id that names two tenants must refuse, not pick.
	if _, err := d.resolveWakeController(sessionWakeParams{SessionID: "shared"}); err == nil {
		t.Fatal("resolveWakeController on an ambiguous session id = nil error, want refusal")
	} else if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want an ambiguity refusal", err)
	}

	// The same id WITH its org is not ambiguous at all — it is exact.
	if _, err := d.resolveWakeController(sessionWakeParams{SessionID: "shared", OrgID: "org-a"}); err == nil ||
		strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("org-scoped lookup err = %v, want a non-ambiguity error (no live controller)", err)
	}
}

func TestResolveWakeControllerUnknownSession(t *testing.T) {
	d := New(Options{SkipRegistration: true, SessionShim: SessionShimConfig{RegistryDir: t.TempDir()}})
	for _, params := range []sessionWakeParams{
		{SessionID: "nope"},
		{SessionID: "nope", OrgID: "org-a"},
	} {
		if _, err := d.resolveWakeController(params); err == nil {
			t.Fatalf("resolveWakeController(%+v) = nil error, want not-adopted", params)
		}
	}
}

// ---------------------------------------------------------------------------
// Delivery against a live fake harness
// ---------------------------------------------------------------------------

func TestSessionWakeVerbsReachAResponsiveHarness(t *testing.T) {
	tests := []struct {
		name string
		op   string
	}{
		{"wake delivers a keystroke", "session.wake"},
		{"restart-harness delivers interrupt then keystroke", "session.restart-harness"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newWakeFixture(t, wakeFixtureHarness)
			m := wakeMutation(t, tc.op, "m-1", sessionWakeParams{
				SessionID: f.id.SessionID, OrgID: f.id.OrgID, Reason: "wedge",
			})
			if err := f.daemon.applyOneMutation(m); err != nil {
				t.Fatalf("applyOneMutation(%s) = %v, want applied", tc.op, err)
			}
			if !f.awaitAck(t, 10*time.Second) {
				t.Fatalf("%s: harness produced no output; the keystroke never reached the terminal", tc.op)
			}
		})
	}
}

// The specimen, mechanized. A harness that never reads its terminal takes both
// rungs without answering: every write SUCCEEDS and no output follows.
//
// This is the fact the whole issue turns on. "Delivered" is not "answered", so
// a rail that reports delivery — or a cursor that keeps advancing — scores this
// seat healthy. Only the absence of terminal output tells the truth, which is
// why the control-plane predicate keys on output progress and why this handler
// returning nil is a receipt for delivery and nothing more.
func TestSessionWakeVerbsDeliverToAFrozenHarnessWithoutRecovery(t *testing.T) {
	for _, op := range []string{"session.wake", "session.restart-harness"} {
		t.Run(op, func(t *testing.T) {
			f := newWakeFixture(t, wakeFixtureFrozenHarness)
			m := wakeMutation(t, op, "m-1", sessionWakeParams{
				SessionID: f.id.SessionID, OrgID: f.id.OrgID, Reason: "wedge",
			})
			if err := f.daemon.applyOneMutation(m); err != nil {
				t.Fatalf("applyOneMutation(%s) = %v; delivery to a frozen harness must still succeed", op, err)
			}
			if f.awaitAck(t, 2*time.Second) {
				t.Fatalf("%s: a harness that never reads its terminal answered; fixture is not frozen", op)
			}
		})
	}
}

// Neither verb may end the session. A wedged seat still holds a worktree and an
// identity worth keeping, and the whole point of putting these rungs BELOW the
// stop rail is that they are recoverable attempts, not terminations.
func TestSessionWakeVerbsRetainTheShim(t *testing.T) {
	for _, op := range []string{"session.wake", "session.restart-harness"} {
		t.Run(op, func(t *testing.T) {
			f := newWakeFixture(t, wakeFixtureHarness)
			m := wakeMutation(t, op, "m-1", sessionWakeParams{
				SessionID: f.id.SessionID, OrgID: f.id.OrgID,
			})
			if err := f.daemon.applyOneMutation(m); err != nil {
				t.Fatalf("applyOneMutation(%s) = %v", op, err)
			}
			f.daemon.shims.mu.RLock()
			entry, ok := f.daemon.shims.adopted[f.id]
			f.daemon.shims.mu.RUnlock()
			if !ok || entry.controller == nil {
				t.Fatalf("%s: session is no longer adopted; remediation must retain the shim", op)
			}
		})
	}
}
