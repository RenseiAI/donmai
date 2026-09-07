package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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
//
// It reports back the EXACT BYTES of each line it read, hex-encoded. Asserting
// on "the harness produced something" would pass for rung 1 and rung 2 alike
// and prove nothing about the difference between them, which is the property
// rung 2 exists for.
//
// This fixture is CANONICAL mode: the kernel line discipline holds the pending
// line and honours its own kill character. That is the mode in which the draft
// assertion is meaningful, because there is a real buffer to clear.
const wakeFixtureHarness = wakeFixtureCanonicalStty + wakeFixtureReportLoop

// wakeFixtureRawHarness is NON-CANONICAL: every byte arrives as data and
// nothing is interpreted on the way in.
//
// It is the fixture for the DELIVERY assertions, for two reasons. It is the
// only mode in which the exact bytes a rung sent can be observed at all — in
// canonical mode the kill character is consumed by the kernel, taking the
// interrupt with it, so the two rungs become indistinguishable to the reader.
// And it is the closer model of the seats this class actually occurs on: raw
// full-screen terminal UIs.
//
// LIMIT, stated here rather than only in prose: a shell reading bytes is not a
// TUI. Neither fixture has a line editor, so neither can prove what Ctrl-A /
// Ctrl-K do inside one. What they prove is what this code controls — the exact
// bytes delivered, and that a kernel-held draft is killed.
const wakeFixtureRawHarness = wakeFixtureRawStty + wakeFixtureReportLoop

const (
	wakeFixtureCanonicalStty = `stty -echo -isig; `
	wakeFixtureRawStty       = `stty -echo -isig -icanon min 1 time 0; `
)

const wakeFixtureReportLoop = `while IFS= read -r line; do ` +
	`printf 'ack:'; printf '%s' "$line" | od -An -tx1 | tr -d ' \n'; printf '\n'; done`

// wakeFixtureFrozenHarness never reads its terminal. It is the wedge itself:
// alive, holding the PTY, consuming no input, emitting nothing. Writes to it
// succeed at every layer and produce no output — which is the property that
// makes "delivered" and "answered" two different facts.
const wakeFixtureFrozenHarness = `stty -echo -isig; while :; do sleep 30; done`

type wakeFixture struct {
	daemon *Daemon
	id     sessionshim.Identity
	output <-chan []byte
	// shim is the live harness host, so a test can take it away and make the
	// controller's next write fail for real rather than through a stub.
	shim *sessionshim.Shim
	// ctrl is the adopted controller, exposed so a test can plant ORDINARY
	// (non-system) input in the line editor — the draft a stalled seat is
	// likely to be sitting on.
	ctrl *sessionshim.Controller
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

	return &wakeFixture{daemon: d, id: id, output: output, ctrl: ctrl, shim: shim}
}

// awaitAck reports whether the fake harness answered at all. A wedged harness
// never does, so the timeout is the assertion, not a flake guard.
func (f *wakeFixture) awaitAck(t *testing.T, within time.Duration) bool {
	t.Helper()
	_, ok := f.awaitLine(t, within)
	return ok
}

// awaitLine returns the hex-encoded bytes of the next line the harness read.
//
// Note what these fixtures CAN and cannot show. Neither has a line editor, so
// the clear bytes are never interpreted: on the canonical fixture Ctrl-U is
// eaten by the line discipline and Ctrl-A/Ctrl-K arrive as data, so the line
// read back is "010b", not empty. An empty line is what a seat whose editor
// interprets the pair submits, and no fixture here can produce one. What these
// assertions do prove is the part this code controls — the exact bytes
// delivered, and that a kernel-held draft is killed rather than submitted.
func (f *wakeFixture) awaitLine(t *testing.T, within time.Duration) (string, bool) {
	t.Helper()
	deadline := time.After(within)
	var seen strings.Builder
	for {
		select {
		case data, ok := <-f.output:
			if !ok {
				return seen.String(), false
			}
			seen.Write(data)
			text := seen.String()
			idx := strings.Index(text, "ack:")
			if idx < 0 {
				continue
			}
			rest := text[idx+len("ack:"):]
			// The harness terminates each report with a newline; the tty turns
			// that into CRLF on the way out.
			if end := strings.IndexAny(rest, "\r\n"); end >= 0 {
				return strings.TrimSpace(rest[:end]), true
			}
		case <-deadline:
			return seen.String(), false
		}
	}
}

// primeWakeLedger gives a fixture the rung-1 history the ladder guarantees, so
// a rung-2 test exercises rung 2 rather than the ordering refusal.
func (f *wakeFixture) primeWakeLedger(t *testing.T) {
	t.Helper()
	f.daemon.commitWakeMutation(f.id, "", "session.wake")
}

// terminateShim tears the harness host down so the adopted controller's next
// write fails the way a dead socket would in production.
func (f *wakeFixture) terminateShim(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.shim.Terminate(ctx); err != nil {
		t.Fatalf("terminate shim: %v", err)
	}
	// Let the controller observe the closed transport.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := f.ctrl.WriteAttributedInput([]byte("probe"), []byte{0x00}); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("controller still accepts writes after the shim terminated")
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

// Each rung must deliver ITS OWN byte sequence, and the fixture reports the
// exact bytes so the two rungs cannot pass each other's assertion.
//
// The fixture runs with `-isig`, so the interrupt arrives as DATA and is
// therefore observable — which is the only reason rung 2's distinguishing
// mechanism can be pinned at all.
func TestSessionWakeVerbsDeliverTheirOwnByteSequence(t *testing.T) {
	tests := []struct {
		name    string
		op      string
		wantHex string
	}{
		{
			// Ctrl-U, Ctrl-A, Ctrl-K, then the submit. No interrupt.
			name: "wake clears the line and submits", op: "session.wake", wantHex: "15010b",
		},
		{
			// The interrupt FIRST, then the same clear-and-submit.
			name: "restart-harness interrupts, then clears and submits",
			op:   "session.restart-harness", wantHex: "0315010b",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newWakeFixture(t, wakeFixtureRawHarness)
			// Rung 2 refuses to precede a wake, so give it the rung-1 history
			// the ladder guarantees. Rung 1 gets no such preamble.
			if tc.op == "session.restart-harness" {
				f.primeWakeLedger(t)
			}
			m := wakeMutation(t, tc.op, "m-1", sessionWakeParams{
				SessionID: f.id.SessionID, OrgID: f.id.OrgID, Reason: "wedge",
			})
			if err := f.daemon.applyOneMutation(m); err != nil {
				t.Fatalf("applyOneMutation(%s) = %v, want applied", tc.op, err)
			}
			got, ok := f.awaitLine(t, 10*time.Second)
			if !ok {
				t.Fatalf("%s: harness produced no line; the keystrokes never reached the terminal", tc.op)
			}
			if got != tc.wantHex {
				t.Fatalf("%s delivered %q, want %q", tc.op, got, tc.wantHex)
			}
		})
	}
}

// Neither rung may submit a draft the seat never finished.
//
// A bare Enter carries no content of its own but SUBMITS whatever the terminal
// already holds, and a stalled seat is the seat most likely to be holding a
// half-typed line. Both rungs therefore clear the line editor first; this
// plants a real draft as ordinary input and proves it is never read back.
func TestSessionWakeVerbsNeverSubmitAnUnsentDraft(t *testing.T) {
	for _, op := range []string{"session.wake", "session.restart-harness"} {
		t.Run(op, func(t *testing.T) {
			f := newWakeFixture(t, wakeFixtureHarness)
			if op == "session.restart-harness" {
				f.primeWakeLedger(t)
			}

			// A half-composed line: ordinary (human) input, deliberately with
			// no terminating CR, exactly as an abandoned draft would sit.
			const draft = "half typed draft"
			if err := f.ctrl.WriteInput([]byte(draft)); err != nil {
				t.Fatalf("plant draft: %v", err)
			}

			m := wakeMutation(t, op, "m-1", sessionWakeParams{
				SessionID: f.id.SessionID, OrgID: f.id.OrgID,
			})
			if err := f.daemon.applyOneMutation(m); err != nil {
				t.Fatalf("applyOneMutation(%s) = %v", op, err)
			}
			got, ok := f.awaitLine(t, 10*time.Second)
			if !ok {
				t.Fatalf("%s: harness produced no line", op)
			}
			if strings.Contains(got, hex.EncodeToString([]byte(draft))) {
				t.Fatalf("%s submitted the unsent draft: harness read %q", op, got)
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
			if op == "session.restart-harness" {
				f.primeWakeLedger(t)
			}
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

// Rung 2 may not be the first thing a seat ever receives.
//
// The control plane decides which rung to send, but the mutation channel can
// lose an ack or reorder a delivery, and an interrupt arriving as a seat's
// first contact is the escalation the ladder exists to prevent. The ordering
// is therefore enforced where the write happens, not only where it is decided.
func TestSessionRestartHarnessRefusesBeforeAWake(t *testing.T) {
	f := newWakeFixture(t, wakeFixtureRawHarness)
	m := wakeMutation(t, "session.restart-harness", "m-1", sessionWakeParams{
		SessionID: f.id.SessionID, OrgID: f.id.OrgID,
	})
	err := f.daemon.applyOneMutation(m)
	if err == nil {
		t.Fatal("restart-harness on an unwoken seat = nil error, want refusal")
	}
	if !errors.Is(err, errRestartBeforeWake) {
		t.Fatalf("err = %v, want errRestartBeforeWake", err)
	}
	// A refusal must not have written anything into the terminal.
	if got, ok := f.awaitLine(t, 2*time.Second); ok {
		t.Fatalf("refused rung 2 still delivered %q", got)
	}

	// After a wake, the same rung is accepted.
	if err := f.daemon.applyOneMutation(wakeMutation(t, "session.wake", "m-2", sessionWakeParams{
		SessionID: f.id.SessionID, OrgID: f.id.OrgID,
	})); err != nil {
		t.Fatalf("wake = %v", err)
	}
	if _, ok := f.awaitLine(t, 10*time.Second); !ok {
		t.Fatal("wake produced no line")
	}
	if err := f.daemon.applyOneMutation(wakeMutation(t, "session.restart-harness", "m-3", sessionWakeParams{
		SessionID: f.id.SessionID, OrgID: f.id.OrgID,
	})); err != nil {
		t.Fatalf("restart-harness after a wake = %v, want applied", err)
	}
	if got, ok := f.awaitLine(t, 10*time.Second); !ok || got != "0315010b" {
		t.Fatalf("restart-harness after a wake delivered %q (ok=%v), want 0315010b", got, ok)
	}
}

// The mutation channel is at-least-once, so the SAME mutation can arrive twice.
//
// Re-applying a kill is harmless. Re-applying an interrupt is not: the second
// one lands on a seat that may by then have recovered and started a real turn.
// A redelivery must therefore be a successful no-op, not a second write.
func TestSessionWakeVerbsAreIdempotentUnderRedelivery(t *testing.T) {
	f := newWakeFixture(t, wakeFixtureRawHarness)

	wake := wakeMutation(t, "session.wake", "m-wake", sessionWakeParams{
		SessionID: f.id.SessionID, OrgID: f.id.OrgID,
	})
	if err := f.daemon.applyOneMutation(wake); err != nil {
		t.Fatalf("wake = %v", err)
	}
	if got, ok := f.awaitLine(t, 10*time.Second); !ok || got != "15010b" {
		t.Fatalf("wake delivered %q (ok=%v), want 15010b", got, ok)
	}

	restart := wakeMutation(t, "session.restart-harness", "m-restart", sessionWakeParams{
		SessionID: f.id.SessionID, OrgID: f.id.OrgID,
	})
	if err := f.daemon.applyOneMutation(restart); err != nil {
		t.Fatalf("restart = %v", err)
	}
	if got, ok := f.awaitLine(t, 10*time.Second); !ok || got != "0315010b" {
		t.Fatalf("restart delivered %q (ok=%v), want 0315010b", got, ok)
	}

	// Both re-presented, byte-identical, as a lost ack would re-present them.
	// Each must ACK applied — the mutation did take effect, on an earlier beat.
	for _, m := range []PendingMutation{wake, restart} {
		if err := f.daemon.applyOneMutation(m); err != nil {
			t.Fatalf("redelivery of %s = %v, want a successful no-op", m.Op, err)
		}
	}
	if got, ok := f.awaitLine(t, 2*time.Second); ok {
		t.Fatalf("redelivery wrote to the terminal again: %q", got)
	}
}

// A rung 1 whose write FAILED must not satisfy the ordering guard.
//
// This is the case the guard exists for, and the one the happy path hides. The
// seats this rail targets are the ones whose transport is least healthy, so a
// failed write is ordinary — and if the ledger recorded the rung on the way in,
// the next interrupt would genuinely be the first byte the seat ever received.
func TestFailedWakeDoesNotAdmitRestartHarness(t *testing.T) {
	f := newWakeFixture(t, wakeFixtureRawHarness)

	// Take the shim away so the controller's write cannot land.
	f.terminateShim(t)

	err := f.daemon.applyOneMutation(wakeMutation(t, "session.wake", "m-1", sessionWakeParams{
		SessionID: f.id.SessionID, OrgID: f.id.OrgID,
	}))
	if err == nil {
		t.Fatal("wake against a dead shim = nil error, want a write failure")
	}

	// The ladder must still be closed: nothing was delivered, so nothing was
	// woken.
	restartErr := f.daemon.applyOneMutation(wakeMutation(t, "session.restart-harness", "m-2", sessionWakeParams{
		SessionID: f.id.SessionID, OrgID: f.id.OrgID,
	}))
	if !errors.Is(restartErr, errRestartBeforeWake) {
		t.Fatalf("restart-harness after a FAILED wake = %v, want errRestartBeforeWake", restartErr)
	}
}

// A mutation whose write failed must not be deduped into a false receipt.
//
// The id is only recorded once the keystrokes actually landed, so a redelivery
// after a failure re-applies rather than ACKing "already applied" for a
// delivery that never happened.
func TestRedeliveryAfterAFailedWriteReapplies(t *testing.T) {
	f := newWakeFixture(t, wakeFixtureRawHarness)
	m := wakeMutation(t, "session.wake", "m-1", sessionWakeParams{
		SessionID: f.id.SessionID, OrgID: f.id.OrgID,
	})

	// Fail the write by making the controller unusable, then restore a live
	// fixture and re-present the SAME mutation id.
	f.terminateShim(t)
	if err := f.daemon.applyOneMutation(m); err == nil {
		t.Fatal("wake against a dead shim = nil error, want a write failure")
	}

	// The SAME daemon's ledger must not remember it. Asked directly, because
	// that is the fact: a redelivery is only deduped when the write landed.
	already, err := f.daemon.checkWakeMutation(f.id, "m-1", "session.wake")
	if err != nil {
		t.Fatalf("checkWakeMutation = %v", err)
	}
	if already {
		t.Fatal("a mutation whose write FAILED was recorded as applied; a redelivery would ACK a delivery that never happened")
	}
}

// The committed path is still deduped — the split must not lose the property
// it was added for.
func TestRedeliveryAfterASuccessfulWriteIsDeduped(t *testing.T) {
	f := newWakeFixture(t, wakeFixtureRawHarness)
	m := wakeMutation(t, "session.wake", "m-1", sessionWakeParams{
		SessionID: f.id.SessionID, OrgID: f.id.OrgID,
	})
	if err := f.daemon.applyOneMutation(m); err != nil {
		t.Fatalf("wake = %v", err)
	}
	if got, ok := f.awaitLine(t, 10*time.Second); !ok || got != "15010b" {
		t.Fatalf("wake delivered %q (ok=%v), want 15010b", got, ok)
	}
	already, err := f.daemon.checkWakeMutation(f.id, "m-1", "session.wake")
	if err != nil {
		t.Fatalf("checkWakeMutation = %v", err)
	}
	if !already {
		t.Fatal("a delivered mutation was not recorded; a redelivery would write into the terminal twice")
	}
}

// Neither verb may end the session. A wedged seat still holds a worktree and an
// identity worth keeping, and the whole point of putting these rungs BELOW the
// stop rail is that they are recoverable attempts, not terminations.
func TestSessionWakeVerbsRetainTheShim(t *testing.T) {
	for _, op := range []string{"session.wake", "session.restart-harness"} {
		t.Run(op, func(t *testing.T) {
			f := newWakeFixture(t, wakeFixtureHarness)
			if op == "session.restart-harness" {
				f.primeWakeLedger(t)
			}
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
