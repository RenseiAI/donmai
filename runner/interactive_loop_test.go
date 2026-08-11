package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/provider/harness/claude"
	"github.com/RenseiAI/donmai/provider/harness/ptycli"
	"github.com/RenseiAI/donmai/provider/harness/shell"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
	"github.com/coder/websocket"
)

// ─── interactive PTY-backed stub provider ──────────────────────────────────
//
// interactivePTYProvider is a real-stack test provider: its Spawn wraps an
// actual ptyhost.Spawn of a shell command under a pseudo-terminal and returns
// a handle that is agent.InteractiveCapable. It is the fixture the full-stack
// e2e drives end to end (runner → ptyhost → attach client → stub relay →
// viewer), and the focused tests reuse its handle directly.
type interactivePTYProvider struct {
	command []string
	// caps is what the provider declares. Zero value = no capabilities, which
	// is what most interactive tests want; the runtime-inject tests declare
	// SupportsMessageInjection so runLoop wires the heartbeat's OnInject.
	caps agent.Capabilities
	// spawned, when non-nil, receives every live ptyhost.Session this
	// provider creates, so a test driving the FULL runner can subscribe to
	// the real PTY stream without an attach leg.
	spawned chan *ptyhost.Session
	// noticeDelivery is what this provider DECLARES on its manifest, and the
	// permission its PTY session is spawned with. Deliberately NOT defaulted:
	// the whole axis exists because the answer differs per harness, so a test
	// that wants notices delivered has to say so, exactly as a manifest does.
	noticeDelivery agent.NoticeDelivery
}

func (p *interactivePTYProvider) Name() agent.ProviderName         { return agent.ProviderShell }
func (p *interactivePTYProvider) Capabilities() agent.Capabilities { return p.caps }

// Manifest makes this an agent.HarnessProvider, which is how the runner reads
// the declared notice-delivery channel. It borrows the real shell manifest so
// everything else about the fixture stays faithful, and overrides only the
// axis under test.
func (p *interactivePTYProvider) Manifest() agent.HarnessManifest {
	m := (&shell.Provider{}).Manifest()
	m.Caps.NoticeDelivery = p.noticeDelivery
	return m
}

func (p *interactivePTYProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}
func (p *interactivePTYProvider) Shutdown(context.Context) error { return nil }

func (p *interactivePTYProvider) Spawn(ctx context.Context, spec agent.Spec) (agent.Handle, error) {
	adapted, err := agent.PreparePrompt(spec, (&shell.Provider{}).Manifest())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	ph := ptyhost.Spec{Command: p.command, Cwd: spec.Cwd, NoticeDelivery: p.noticeDelivery}
	if spec.Interactive != nil {
		// Geometry stays at ptyhost defaults (80×24); the runner sets only
		// RecordPath (loop.go), which we honor so the cast lands in the
		// session workarea.
		ph.RecordPath = spec.Interactive.RecordPath
	}
	sess, err := ptyhost.Spawn(ph)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", agent.ErrSpawnFailed, err)
	}
	if p.spawned != nil {
		select {
		case p.spawned <- sess:
		default:
		}
	}
	handle := newInteractivePTYHandle(sess)
	if err := ptycli.DeliverSeed(ctx, handle, sess, adapted.Prompt); err != nil {
		return nil, fmt.Errorf("%w: test PTY seed: %v", agent.ErrSpawnFailed, err)
	}
	return handle, nil
}

// interactivePTYHandle is agent.Handle + agent.InteractiveCapable over a live
// ptyhost.Session. Events() is an inert channel — an interactive PTY session
// produces no model events; the interactive dispatch path never reads it.
type interactivePTYHandle struct {
	sess   *ptyhost.Session
	events chan agent.Event
}

func newInteractivePTYHandle(sess *ptyhost.Session) *interactivePTYHandle {
	return &interactivePTYHandle{sess: sess, events: make(chan agent.Event)}
}

func (h *interactivePTYHandle) SessionID() string                    { return "interactive-pty-stub" }
func (h *interactivePTYHandle) Events() <-chan agent.Event           { return h.events }
func (h *interactivePTYHandle) Inject(context.Context, string) error { return agent.ErrUnsupported }
func (h *interactivePTYHandle) Stop(ctx context.Context) error       { return h.sess.Stop(ctx) }
func (h *interactivePTYHandle) InteractiveSession() agent.InteractiveSession {
	return h.sess
}

// testInteractiveHandle decorates any agent.Handle with a caller-supplied PTY
// surface. It lets focused tests record input while preserving the real handle's
// Stop semantics, and lets reconnect tests wrap a real ptyhost.Session without
// altering the production fixture.
type testInteractiveHandle struct {
	agent.Handle
	session agent.InteractiveSession
}

func (h *testInteractiveHandle) InteractiveSession() agent.InteractiveSession {
	return h.session
}

// recordingInteractiveSession records every accepted input byte. It embeds the
// remaining interface methods from an optional real session; focused local-only
// tests override Done/Exit and never call the promoted attach methods.
type recordingInteractiveSession struct {
	agent.InteractiveSession
	mu       sync.Mutex
	writes   [][]byte
	maxWrite int
	writeErr error
	done     chan struct{}
	exit     attachwire.ExitPayload
	exitOK   bool
	// refuseNotice models a human mid-composition: TryWriteNotice refuses
	// (false, nil) and writes nothing, exactly like ptyhost's gate.
	refuseNotice bool
	// noticeErr makes TryWriteNotice fail outright (a dead PTY master).
	noticeErr error
}

// TryWriteNotice makes the recorder an agent.InteractiveNotifier: an accepted
// notice is recorded as ONE write (so tests can assert single-write
// atomicity), a refused one records nothing.
func (s *recordingInteractiveSession) TryWriteNotice(p []byte) (bool, error) {
	s.mu.Lock()
	refuse, noticeErr := s.refuseNotice, s.noticeErr
	s.mu.Unlock()
	if noticeErr != nil {
		return false, noticeErr
	}
	if refuse {
		return false, nil
	}
	n, err := s.WriteInput(p)
	return n > 0, err
}

// setRefuseNotice flips the mid-composition gate.
func (s *recordingInteractiveSession) setRefuseNotice(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseNotice = v
}

// recordedWrites returns a copy of every recorded write, one entry per
// WriteInput/TryWriteNotice call that accepted bytes.
func (s *recordingInteractiveSession) recordedWrites() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.writes))
	for i, w := range s.writes {
		out[i] = append([]byte(nil), w...)
	}
	return out
}

func (s *recordingInteractiveSession) WriteInput(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	limit := len(p)
	if s.maxWrite > 0 && limit > s.maxWrite {
		limit = s.maxWrite
	}
	n, err := limit, error(nil)
	if s.InteractiveSession != nil {
		n, err = s.InteractiveSession.WriteInput(p[:limit])
	}
	if n > 0 && n <= limit {
		s.mu.Lock()
		s.writes = append(s.writes, append([]byte(nil), p[:n]...))
		s.mu.Unlock()
	}
	return n, err
}

func (s *recordingInteractiveSession) Done() <-chan struct{} {
	if s.done != nil {
		return s.done
	}
	return s.InteractiveSession.Done()
}

func (s *recordingInteractiveSession) Exit() (attachwire.ExitPayload, bool) {
	if s.done != nil {
		return s.exit, s.exitOK
	}
	return s.InteractiveSession.Exit()
}

func (s *recordingInteractiveSession) inputBytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []byte
	for _, write := range s.writes {
		out = append(out, write...)
	}
	return out
}

func (s *recordingInteractiveSession) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

// liveRecordingInteractiveSession stays ALIVE until the test closes done —
// the shape every supervisor-loop test needs (a session that has already
// exited returns from dispatchInteractive before the loop runs once).
func liveRecordingInteractiveSession() *recordingInteractiveSession {
	return &recordingInteractiveSession{
		done:   make(chan struct{}),
		exit:   attachwire.NewNormalExit(0),
		exitOK: true,
	}
}

func completedRecordingInteractiveSession() *recordingInteractiveSession {
	done := make(chan struct{})
	close(done)
	return &recordingInteractiveSession{
		done:   done,
		exit:   attachwire.NewNormalExit(0),
		exitOK: true,
	}
}

// assertNoInitialPromptReplay fails when any byte dispatchInteractive wrote
// to the PTY carries the initial prompt. An empty prompt asserts nothing was
// written at all (there is no content to look for).
func assertNoInitialPromptReplay(t *testing.T, session *recordingInteractiveSession, initialPrompt string) {
	t.Helper()
	writes := session.recordedWrites()
	if initialPrompt == "" {
		if len(writes) != 0 {
			t.Errorf("dispatchInteractive wrote %q with no initial prompt to replay", session.inputBytes())
		}
		return
	}
	for i, w := range writes {
		if bytes.Contains(w, []byte(initialPrompt)) {
			t.Errorf("dispatchInteractive replayed the initial prompt in write %d: %q", i, w)
		}
	}
}

// ─── test helpers ──────────────────────────────────────────────────────────

func requireSh(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh unavailable: %v", err)
	}
}

// fakeInteractiveJWT builds an UNSIGNED (unverified) compact JWT — the attach
// client + stub relay parse the payload leniently and never verify the
// signature (the real relay is authoritative). Mirrors attachclient's own
// fakeJWT test helper so the e2e mints the claims the client reads.
func fakeInteractiveJWT(claims map[string]any) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","typ":"JWT"}`))
	pb, _ := json.Marshal(claims)
	return strings.Join([]string{
		hdr,
		base64.RawURLEncoding.EncodeToString(pb),
		base64.RawURLEncoding.EncodeToString([]byte("sig")),
	}, ".")
}

func mkInteractiveHostToken(sessionID string, epoch int64) string {
	return fakeInteractiveJWT(map[string]any{
		"sessionId": sessionID,
		"roomId":    "room-1",
		"role":      "host",
		"aud":       "relay",
		"jti":       "host-jti-1",
		"epoch":     epoch,
		"exp":       time.Now().Add(time.Hour).Unix(),
	})
}

func mkInteractiveViewerToken(sessionID, userID, role string) string {
	return fakeInteractiveJWT(map[string]any{
		"sessionId": sessionID,
		"roomId":    "room-1",
		"userId":    userID,
		"role":      role,
		"aud":       "relay",
		"jti":       "vjti-" + userID,
		"exp":       time.Now().Add(time.Hour).Unix(),
	})
}

func waitRelayBound(t *testing.T, relay *attachtest.StubRelay) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if relay.HostBound() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the interactive host leg to bind")
}

// sendInputUntil re-sends data on the driver until effect() reports true or
// the deadline elapses — the §5 client resend discipline the one-shot helper
// lacks (the stub relay has no input_ack; see the call site).
func sendInputUntil(ctx context.Context, t *testing.T, v *attachtest.Viewer, data []byte, effect func() bool, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := v.SendInput(ctx, data); err != nil {
			t.Fatalf("SendInput: %v", err)
		}
		if effect() {
			return true
		}
	}
	return false
}

// waitForOutput drains the viewer's frames until an Output frame containing
// want is seen (returns true) or the deadline elapses (false).
func waitForOutput(v *attachtest.Viewer, want string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	var acc strings.Builder
	for {
		select {
		case f, ok := <-v.Frames():
			if !ok {
				return strings.Contains(acc.String(), want)
			}
			if f.Type == attachwire.TypeOutput {
				acc.Write(attachwire.DecodeOutput(f.Payload).Data)
				if strings.Contains(acc.String(), want) {
					return true
				}
			}
		case <-deadline:
			return strings.Contains(acc.String(), want)
		}
	}
}

// waitForTerminalText accepts either live/replayed Output frames or a Snapshot
// containing want. Pre-attach terminal history is allowed to converge through
// the snapshot path rather than being replayed as Output.
func waitForTerminalText(v *attachtest.Viewer, want string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	var output strings.Builder
	for {
		select {
		case f, ok := <-v.Frames():
			if !ok {
				return strings.Contains(output.String(), want)
			}
			switch f.Type {
			case attachwire.TypeOutput:
				output.Write(attachwire.DecodeOutput(f.Payload).Data)
				if strings.Contains(output.String(), want) {
					return true
				}
			case attachwire.TypeSnapshot:
				env, err := attachwire.DecodeSnapshotEnvelope(f.Payload)
				if err != nil {
					continue
				}
				screen, err := attachwire.DecodeScreen(env.Snap)
				if err != nil {
					continue
				}
				var snapshot strings.Builder
				for _, line := range screen.Scrollback {
					for _, cell := range line {
						snapshot.Write(cell.RuneBytes)
					}
					snapshot.WriteByte('\n')
				}
				cells := screen.Primary
				if screen.ActiveBuffer == attachwire.BufferAlt && screen.AltPresent {
					cells = screen.Alt
				}
				for _, cell := range cells {
					snapshot.Write(cell.RuneBytes)
				}
				if strings.Contains(snapshot.String(), want) {
					return true
				}
			}
		case <-deadline:
			return strings.Contains(output.String(), want)
		}
	}
}

// waitForSnapshot drains the viewer's frames until a Snapshot frame with a
// decodable envelope (atSeq > 0) is seen — the late-join convergence signal.
func waitForSnapshot(v *attachtest.Viewer, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case f, ok := <-v.Frames():
			if !ok {
				return false
			}
			if f.Type == attachwire.TypeSnapshot {
				if env, err := attachwire.DecodeSnapshotEnvelope(f.Payload); err == nil && env.AtSeq > 0 {
					return true
				}
			}
		case <-deadline:
			return false
		}
	}
}

// ─── focused tests (no worktree clone) ─────────────────────────────────────

// TestInteractive_CapabilityFailure: a handle that is NOT InteractiveCapable
// is a config/capability failure (harness lacks PTY transport), not a crash.
func TestInteractive_CapabilityFailure(t *testing.T) {
	r := minimalRunner(t)
	res := &Result{SessionID: "s"}
	res.ProviderName = agent.ProviderStub
	h := &fakeHandle{events: make(chan agent.Event)}

	qw := QueuedWork{}
	qw.SessionID = "s"
	out, err := r.dispatchInteractive(context.Background(), h, t.TempDir(), qw, res, noopSink{}, nil, nil, agent.NoticeDeliveryPTYNotice)
	if err == nil {
		t.Fatal("expected a terminal error for a non-interactive handle")
	}
	if out.Status != "failed" || out.FailureMode != FailureInteractiveUnsupported {
		t.Fatalf("status=%q mode=%q; want failed/%s", out.Status, out.FailureMode, FailureInteractiveUnsupported)
	}
	if !strings.Contains(out.Error, "PTY transport") {
		t.Fatalf("error should name the missing PTY transport: %q", out.Error)
	}
}

// TestInteractive_InitialPromptContract locks the supervisor boundary:
// dispatchInteractive never writes task bytes after Provider.Spawn. A
// non-empty interactive seed is reported as already delivered by the native
// harness surface, while headless/interview direct calls remain no-ops.
func TestInteractive_InitialPromptContract(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	tests := []struct {
		name          string
		mode          string
		initialPrompt string
		wantDelivered bool
	}{
		{name: "absent", mode: interactiveRunMode},
		{name: "explicit empty", mode: interactiveRunMode, initialPrompt: ""},
		{name: "whitespace native delivery", mode: interactiveRunMode, initialPrompt: "  ", wantDelivered: true},
		{name: "unicode multiline native delivery", mode: interactiveRunMode, initialPrompt: "こんにちは 🌱\nsecond line", wantDelivered: true},
		{name: "headless excluded", mode: "", initialPrompt: "headless seed must not run"},
		{name: "interview excluded", mode: "interview", initialPrompt: "interview seed must not run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := completedRecordingInteractiveSession()
			handle := &testInteractiveHandle{
				Handle:  &fakeHandle{events: make(chan agent.Event)},
				session: session,
			}
			sink := &recordingSink{}
			qw := QueuedWork{QueuedWork: prompt.QueuedWork{
				SessionID:     "seed-contract",
				Mode:          tt.mode,
				InitialPrompt: tt.initialPrompt,
			}}
			res := &Result{SessionID: qw.SessionID}

			out, err := minimalRunner(t).dispatchInteractive(
				context.Background(), handle, t.TempDir(), qw, res, sink, nil, nil, agent.NoticeDeliveryPTYNotice,
			)
			if err != nil {
				t.Fatalf("dispatchInteractive: %v", err)
			}
			if out.Status != "completed" {
				t.Fatalf("status=%q error=%q; want completed", out.Status, out.Error)
			}
			// The contract (interactive_loop.go) is that dispatchInteractive
			// never REPLAYS QueuedWork.InitialPrompt: Provider.Spawn already
			// delivered it on the harness's native first-turn surface, so a
			// replay would duplicate the seed and bypass the receipt.
			//
			// Asserted on CONTENT, not on a write count of zero. The same
			// supervisor now also writes runtime notices into the live PTY,
			// so a zero-write assertion would forbid the feature instead of
			// the defect — while a content assertion still fails the instant
			// the seed is replayed.
			assertNoInitialPromptReplay(t, session, tt.initialPrompt)

			var subtypes []string
			for _, ev := range sink.events {
				system, ok := ev.(agent.SystemEvent)
				if !ok {
					continue
				}
				subtypes = append(subtypes, system.Subtype)
				if tt.initialPrompt != "" && strings.Contains(system.Message, tt.initialPrompt) {
					t.Errorf("activity message leaked initialPrompt content: %q", system.Message)
				}
			}
			wantSubtypes := "interactive-session-started,interactive-session-ended"
			if tt.wantDelivered {
				wantSubtypes = "interactive-session-started,interactive-initial-prompt-delivered,interactive-session-ended"
			}
			if got := strings.Join(subtypes, ","); got != wantSubtypes {
				t.Errorf("activity subtypes = %q, want %q", got, wantSubtypes)
			}
		})
	}
}

func TestInteractive_InitialPromptOversizeFailsBeforeWrite(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	initialPrompt := strings.Repeat("oversized-seed-", 1500)
	session := completedRecordingInteractiveSession()
	handle := &testInteractiveHandle{
		Handle:  &fakeHandle{events: make(chan agent.Event)},
		session: session,
	}
	sink := &recordingSink{}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{
		SessionID:     "seed-oversize",
		Mode:          interactiveRunMode,
		InitialPrompt: initialPrompt,
	}}

	started := time.Now()
	out, err := minimalRunner(t).dispatchInteractive(
		context.Background(), handle, t.TempDir(), qw, &Result{SessionID: qw.SessionID}, sink, nil, nil, agent.NoticeDeliveryPTYNotice,
	)
	if err == nil {
		t.Fatal("expected oversize initial-prompt failure")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("oversize prompt failed after %v, want immediate pre-write rejection", elapsed)
	}
	if out.Status != "failed" || out.FailureMode != FailureInteractiveInput {
		t.Fatalf("status=%q mode=%q; want failed/%s", out.Status, out.FailureMode, FailureInteractiveInput)
	}
	if got := session.writeCount(); got != 0 {
		t.Fatalf("oversize initial prompt wrote %d time(s), want 0", got)
	}
	if !strings.Contains(out.Error, fmt.Sprintf("%d UTF-8 bytes", len(initialPrompt))) ||
		!strings.Contains(out.Error, fmt.Sprintf("limit is %d bytes", maxInitialPromptBytes)) {
		t.Fatalf("result error = %q, want actual byte count and limit", out.Error)
	}
	if strings.Contains(out.Error, initialPrompt) {
		t.Fatalf("result error leaked prompt content: %q", out.Error)
	}
	for _, ev := range sink.events {
		if system, ok := ev.(agent.SystemEvent); ok && system.Subtype == "interactive-initial-prompt-delivered" {
			t.Fatal("delivery activity emitted for rejected oversize prompt")
		}
	}
}

// TestInteractive_HalfConfiguredAttachFails: exactly one of ATTACH_URL /
// ATTACH_TOKEN is a deployment misconfiguration — fail loud.
func TestInteractive_HalfConfiguredAttachFails(t *testing.T) {
	requireSh(t)
	t.Setenv(envAttachURL, "ws://127.0.0.1:0/v1/rooms/room-1")
	t.Setenv(envAttachToken, "") // present-but-empty ≡ absent

	r := minimalRunner(t)
	sess, err := ptyhost.Spawn(ptyhost.Spec{Command: []string{"/bin/sh", "-c", "sleep 30"}})
	if err != nil {
		t.Fatalf("ptyhost.Spawn: %v", err)
	}
	h := newInteractivePTYHandle(sess)
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	res := &Result{SessionID: "s"}
	qw := QueuedWork{}
	qw.SessionID = "s"
	out, err := r.dispatchInteractive(context.Background(), h, t.TempDir(), qw, res, noopSink{}, nil, nil, agent.NoticeDeliveryPTYNotice)
	if err == nil {
		t.Fatal("expected a config failure for a half-configured attach")
	}
	if out.Status != "failed" || out.FailureMode != FailureInteractiveConfig {
		t.Fatalf("status=%q mode=%q; want failed/%s", out.Status, out.FailureMode, FailureInteractiveConfig)
	}
}

// TestInteractive_LocalOnlyCompletes: with neither attach env set the session
// runs local-only and, on a clean exit 0, produces a completed Result.
func TestInteractive_LocalOnlyCompletes(t *testing.T) {
	requireSh(t)
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	r := minimalRunner(t)
	sess, err := ptyhost.Spawn(ptyhost.Spec{Command: []string{"/bin/sh", "-c", "exit 0"}})
	if err != nil {
		t.Fatalf("ptyhost.Spawn: %v", err)
	}
	h := newInteractivePTYHandle(sess)

	res := &Result{SessionID: "s"}
	qw := QueuedWork{}
	qw.SessionID = "s"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := r.dispatchInteractive(ctx, h, t.TempDir(), qw, res, noopSink{}, nil, nil, agent.NoticeDeliveryPTYNotice)
	if err != nil {
		t.Fatalf("dispatchInteractive: unexpected err %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status=%q error=%q; want completed", out.Status, out.Error)
	}
}

// boolPtr returns a pointer to b, for constructing the *bool
// RecordingEnabled wire value in table-driven tests.
func boolPtr(b bool) *bool { return &b }

// TestRun_InteractiveRecordPathGatedByRecordingEnabled proves the runner's
// spec-construction step (runner/loop.go) gates
// agent.InteractiveSpec.RecordPath on QueuedWork.RecordingEnabled: nil
// (absent, standalone or a pre-field platform) and explicit true both leave
// the cast destination populated (the mixed-version-safe default is
// "allowed"); explicit false leaves it empty so ptyhost's recorder becomes a
// no-op (see newRecorder) while the interactive PTY surface itself
// (Spec.Interactive) is still constructed either way — recording is gated,
// the terminal is not.
func TestRun_InteractiveRecordPathGatedByRecordingEnabled(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cases := []struct {
		name             string
		recordingEnabled *bool
		wantRecording    bool
	}{
		{"nil — no platform decision — default allowed", nil, true},
		{"explicit true — allowed", boolPtr(true), true},
		{"explicit false — disabled", boolPtr(false), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envAttachURL, "")
			t.Setenv(envAttachToken, "")
			server := mockPlatformServer(t)
			t.Cleanup(server.Close)
			manager, err := worktree.NewManager(worktree.Options{ParentDir: t.TempDir()})
			if err != nil {
				t.Fatalf("worktree.NewManager: %v", err)
			}
			poster, err := result.NewPoster(result.Options{
				PlatformURL: server.URL, WorkerID: "worker-1", AuthToken: "token",
				HTTPClient: server.Client(), BaseDelay: 1,
			})
			if err != nil {
				t.Fatalf("result.NewPoster: %v", err)
			}
			provider := &promptCaptureInteractiveProvider{
				name:     agent.ProviderClaude,
				caps:     (&claude.Provider{}).Capabilities(),
				manifest: (&claude.Provider{}).Manifest(),
			}
			registry := NewRegistry()
			if err := registry.Register(provider); err != nil {
				t.Fatalf("Register: %v", err)
			}
			r, err := New(Options{
				Registry: registry, WorktreeManager: manager, Poster: poster, HTTPClient: server.Client(),
				MaxSessionDuration: -1, SkipBackstop: true, SkipSteering: true, SkipPostSession: true,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			qw := QueuedWork{
				QueuedWork: prompt.QueuedWork{
					SessionID:        "record-gate-" + strings.ReplaceAll(tc.name, " ", "-"),
					IssueID:          "issue-id",
					IssueIdentifier:  "ISSUE-REC",
					WorkType:         "development",
					Mode:             prompt.InteractiveRunMode,
					InitialPrompt:    "seed",
					Repository:       makeBareRepo(t),
					RecordingEnabled: tc.recordingEnabled,
				},
				WorkerID:        "worker-1",
				AuthToken:       "token",
				PlatformURL:     server.URL,
				ResolvedProfile: ResolvedProfile{Provider: agent.ProviderClaude},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			got, err := r.Run(ctx, qw)
			if err != nil || got.Status != "completed" {
				t.Fatalf("Run result=%+v err=%v", got, err)
			}
			if provider.raw.Interactive == nil {
				t.Fatal("Spec.Interactive was nil — every interactive session must get a PTY surface regardless of recording policy")
			}
			gotPath := provider.raw.Interactive.RecordPath
			if tc.wantRecording {
				wantPath := termCastPath(provider.raw.Cwd)
				if gotPath != wantPath {
					t.Errorf("RecordPath = %q, want %q (recording allowed)", gotPath, wantPath)
				}
			} else if gotPath != "" {
				t.Errorf("RecordPath = %q, want empty (recording disabled by policy)", gotPath)
			}
		})
	}
}

// TestInteractive_RecordingCleanup proves dispatchInteractive best-effort
// deletes the on-disk cast once the session reaches a terminal state, on
// both the success and the failure exit path, UNLESS QueuedWork.RetainRecording
// suppresses it (the standalone --keep-recording flag). The cast file is
// created (via a real ptyhost.Session, so newRecorder actually runs) at the
// same path termCastPath would compute for the workarea, mirroring how
// runner/loop.go wires agent.InteractiveSpec.RecordPath in production.
func TestInteractive_RecordingCleanup(t *testing.T) {
	requireSh(t)
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	cases := []struct {
		name            string
		command         []string
		retainRecording bool
		wantExists      bool
	}{
		{"success — cleaned up", []string{"/bin/sh", "-c", "exit 0"}, false, false},
		{"failure — cleaned up", []string{"/bin/sh", "-c", "exit 3"}, false, false},
		{"success — retained", []string{"/bin/sh", "-c", "exit 0"}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := minimalRunner(t)
			worktreePath := t.TempDir()
			castPath := termCastPath(worktreePath)
			if err := os.MkdirAll(filepath.Dir(castPath), 0o750); err != nil {
				t.Fatalf("mkdir workarea dir: %v", err)
			}
			sess, err := ptyhost.Spawn(ptyhost.Spec{Command: tc.command, RecordPath: castPath})
			if err != nil {
				t.Fatalf("ptyhost.Spawn: %v", err)
			}
			h := newInteractivePTYHandle(sess)

			if _, err := os.Stat(castPath); err != nil {
				t.Fatalf("cast file was not created before dispatch: %v", err)
			}

			res := &Result{SessionID: "s"}
			qw := QueuedWork{RetainRecording: tc.retainRecording}
			qw.SessionID = "s"
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := r.dispatchInteractive(ctx, h, worktreePath, qw, res, noopSink{}, nil, nil, agent.NoticeDeliveryPTYNotice); err != nil {
				// The nonzero-exit case returns a nil error from
				// dispatchInteractive (the failure rides Result.Status), so
				// only fail here if it's genuinely unexpected.
				t.Logf("dispatchInteractive returned err=%v (status=%q)", err, res.Status)
			}

			_, statErr := os.Stat(castPath)
			exists := statErr == nil
			if exists != tc.wantExists {
				t.Errorf("cast exists=%v after dispatch, want %v (statErr=%v)", exists, tc.wantExists, statErr)
			}
		})
	}
}

// TestInteractive_LocalOnlyNonzeroExitFails: a nonzero PTY child exit maps to
// a failed Result carrying the exit detail (Exit-payload → Result mapping).
func TestInteractive_LocalOnlyNonzeroExitFails(t *testing.T) {
	requireSh(t)
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	r := minimalRunner(t)
	sess, err := ptyhost.Spawn(ptyhost.Spec{Command: []string{"/bin/sh", "-c", "exit 3"}})
	if err != nil {
		t.Fatalf("ptyhost.Spawn: %v", err)
	}
	h := newInteractivePTYHandle(sess)

	res := &Result{SessionID: "s"}
	qw := QueuedWork{}
	qw.SessionID = "s"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, _ := r.dispatchInteractive(ctx, h, t.TempDir(), qw, res, noopSink{}, nil, nil, agent.NoticeDeliveryPTYNotice)
	if out.Status != "failed" {
		t.Fatalf("status=%q; want failed", out.Status)
	}
	if !strings.Contains(out.Error, "exit 3") {
		t.Fatalf("error should carry the exit detail: %q", out.Error)
	}
}

// TestInteractive_ExitDetailIsNotASummary: the terminal Result's Summary field
// carries the AGENT's account of the work, and consumers read it as content. A
// session that ends without one leaves it EMPTY rather than synthesizing a
// lifecycle line ("the process exited") that downstream readers then have to
// recognise and filter back out. The exit detail still travels on the channels
// built for it — Error on the failure path, plus the session-ended activity
// event and the log line.
func TestInteractive_ExitDetailIsNotASummary(t *testing.T) {
	requireSh(t)

	cases := []struct {
		name       string
		command    string
		wantStatus string
		wantErr    string
	}{
		{"clean_exit", "exit 0", "completed", ""},
		{"nonzero_exit", "exit 7", "failed", "exit 7"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envAttachURL, "")
			t.Setenv(envAttachToken, "")

			r := minimalRunner(t)
			sess, err := ptyhost.Spawn(ptyhost.Spec{Command: []string{"/bin/sh", "-c", tc.command}})
			if err != nil {
				t.Fatalf("ptyhost.Spawn: %v", err)
			}
			h := newInteractivePTYHandle(sess)

			res := &Result{SessionID: "s"}
			qw := QueuedWork{}
			qw.SessionID = "s"
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(cancel)

			out, _ := r.dispatchInteractive(ctx, h, t.TempDir(), qw, res, noopSink{}, nil, nil, agent.NoticeDeliveryPTYNotice)
			if out.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (error=%q)", out.Status, tc.wantStatus, out.Error)
			}
			if out.Summary != "" {
				t.Errorf("Summary = %q, want empty: a session lifecycle line is not a summary", out.Summary)
			}
			if tc.wantErr != "" && !strings.Contains(out.Error, tc.wantErr) {
				t.Errorf("Error = %q, want it to carry the exit detail %q", out.Error, tc.wantErr)
			}
		})
	}
}

func TestInteractive_AttachTokenFileRotatesAcrossReconnect(t *testing.T) {
	requireSh(t)

	const (
		sessionID     = "sess-token-file-reconnect"
		roomPath      = "/v1/rooms/room-1"
		initialPrompt = "reconnect seed 🌱"
	)
	initialExp := time.Now().Add(300 * time.Millisecond)
	initialToken := fakeInteractiveJWT(map[string]any{
		"sessionId": sessionID,
		"roomId":    "room-1",
		"role":      "host",
		"aud":       "relay",
		"jti":       "host-initial",
		"epoch":     int64(1),
		"exp":       initialExp.Unix(),
	})
	replacementToken := fakeInteractiveJWT(map[string]any{
		"sessionId": sessionID,
		"roomId":    "room-1",
		"role":      "host",
		"aud":       "relay",
		"jti":       "host-replacement",
		"epoch":     int64(1),
		"exp":       time.Now().Add(time.Hour).Unix(),
	})

	var allowedToken atomic.Value
	allowedToken.Store(initialToken)
	admitted := make(chan string, 2)
	dropInitial := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != roomPath {
			http.NotFound(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != allowedToken.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:       []string{attachwire.SubprotocolVersion},
			InsecureSkipVerify: true,
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()                                //nolint:errcheck
		if _, _, err := conn.Read(r.Context()); err != nil { // host subscribe
			return
		}
		admitted <- token
		if token == initialToken {
			select {
			case <-dropInitial:
				return
			case <-r.Context().Done():
				return
			}
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "attach-token") // intentionally missing initially
	donePath := filepath.Join(t.TempDir(), "done")
	t.Setenv(envAttachURL, strings.Replace(srv.URL, "http://", "ws://", 1)+roomPath)
	t.Setenv(envAttachToken, initialToken)
	t.Setenv(envAttachTokenFile, tokenPath)

	r := minimalRunner(t)
	sess, err := ptyhost.Spawn(ptyhost.Spec{Command: []string{
		"/bin/sh", "-c", `while [ ! -f "$1" ]; do sleep 0.05; done`, "sh", donePath,
	}})
	if err != nil {
		t.Fatalf("ptyhost.Spawn: %v", err)
	}
	baseHandle := newInteractivePTYHandle(sess)
	recordedSession := &recordingInteractiveSession{InteractiveSession: sess}
	h := &testInteractiveHandle{Handle: baseHandle, session: recordedSession}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	type dispatchResult struct {
		res *Result
		err error
	}
	resultCh := make(chan dispatchResult, 1)
	go func() {
		res := &Result{SessionID: sessionID}
		res.ProviderName = agent.ProviderShell
		qw := QueuedWork{}
		qw.SessionID = sessionID
		qw.Mode = interactiveRunMode
		qw.InitialPrompt = initialPrompt
		out, err := r.dispatchInteractive(ctx, h, t.TempDir(), qw, res, noopSink{}, nil, nil, agent.NoticeDeliveryPTYNotice)
		resultCh <- dispatchResult{res: out, err: err}
	}()

	select {
	case got := <-admitted:
		if got != initialToken {
			t.Fatalf("initial admission token=%q; want static token", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("initial static attach token was not admitted")
	}

	// The live connection may remain valid until it drops. Once the initial JWT's
	// exp has passed, simulate relay re-admission policy, atomically publish the
	// replacement file, and force a reconnect.
	if wait := time.Until(initialExp.Add(20 * time.Millisecond)); wait > 0 {
		time.Sleep(wait)
	}
	tmp := tokenPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(replacementToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, tokenPath); err != nil {
		t.Fatal(err)
	}
	allowedToken.Store(replacementToken)
	close(dropInitial)

	select {
	case got := <-admitted:
		if got != replacementToken {
			t.Fatalf("reconnect admission token=%q; want replacement token", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay reconnect did not re-read ATTACH_TOKEN_FILE")
	}
	// Same contract as TestInteractive_InitialPromptContract, asserted on
	// content rather than on a write count so a runtime notice is allowed
	// through while a seed replay still fails.
	assertNoInitialPromptReplay(t, recordedSession, initialPrompt)

	if err := os.WriteFile(donePath, []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		if got.err != nil {
			t.Fatalf("dispatchInteractive: %v", got.err)
		}
		if got.res.Status != "completed" {
			t.Fatalf("status=%q error=%q; want completed", got.res.Status, got.res.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatchInteractive did not finish after token-file reconnect")
	}
}

// ─── full-stack attach e2e ─────────────────────────────────────────────────

// TestInteractive_FullStackAttachE2E wires the REAL stack end to end:
// runner → interactive PTY handle → attach client → in-process stub relay →
// viewer. It asserts (a) a viewer's stamped input reaches the shell and the
// echoed output rides back to the viewer, (b) a late-joining viewer converges
// on a snapshot, (c) the runner produces a completed Result on child exit, and
// (d) the lock-refresh double observed sessionClass=="interactive" (the W4
// cross-repo stamp W3 reads).
func TestInteractive_FullStackAttachE2E(t *testing.T) {
	requireSh(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	const (
		sessionID     = "sess-interactive-e2e"
		initialPrompt = "seed-first-雪"
	)

	relay := attachtest.New(attachtest.Config{RoomID: "room-1"})
	if err := relay.Start(); err != nil {
		t.Fatalf("relay start: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	platform := newRecordingPlatformServer(t)

	// The composing daemon would inject these; the OSS runner reads them from
	// its process env but must not pass the runner-only controls to the PTY child.
	hostToken := mkInteractiveHostToken(sessionID, 1)
	tokenPath := filepath.Join(t.TempDir(), "attach-token")
	if err := os.WriteFile(tokenPath, []byte(hostToken+"\n"), 0o600); err != nil {
		t.Fatalf("write attach token file: %v", err)
	}
	t.Setenv(envAttachURL, relay.BaseWSURL())
	t.Setenv(envAttachToken, hostToken)
	t.Setenv(envAttachTokenFile, tokenPath)

	bareRepo := makeBareRepo(t)
	wtParent := t.TempDir()
	wtm, err := worktree.NewManager(worktree.Options{ParentDir: wtParent})
	if err != nil {
		t.Fatalf("worktree.NewManager: %v", err)
	}
	poster, err := result.NewPoster(result.Options{
		PlatformURL: platform.URL,
		WorkerID:    "w1",
		AuthToken:   "tok",
		HTTPClient:  platform.Client(),
		BaseDelay:   1,
	})
	if err != nil {
		t.Fatalf("result.NewPoster: %v", err)
	}

	reg := NewRegistry()
	// A shell first reports whether any runner-only attach control reached its
	// real PTY environment, then echoes input until "quit".
	prov := &interactivePTYProvider{command: []string{
		"/bin/sh", "-c",
		`if [ "${ATTACH_TOKEN+x}${ATTACH_TOKEN_FILE+x}${ATTACH_URL+x}" = "" ]; then env_status=attach-env-clean; else env_status=attach-env-leaked; fi; while IFS= read -r line; do echo "$env_status:got:$line"; [ "$line" = quit ] && break; done`,
	}}
	if err := reg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}

	r, err := New(Options{
		Registry:           reg,
		WorktreeManager:    wtm,
		Poster:             poster,
		HTTPClient:         platform.Client(),
		HeartbeatInterval:  50 * time.Millisecond,
		MaxSessionDuration: -1, // do not cap a human-driven session
		SkipBackstop:       true,
		SkipSteering:       true,
		SkipPostSession:    true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	qw := QueuedWork{
		QueuedWork: prompt.QueuedWork{
			SessionID:       sessionID,
			IssueID:         "issue-int-e2e",
			IssueIdentifier: "REN-INT-E2E",
			WorkType:        "development",
			Body:            "interactive session",
			Mode:            interactiveRunMode,
			InitialPrompt:   initialPrompt,
			Repository:      bareRepo,
		},
		WorkerID:        "w1",
		AuthToken:       "tok",
		PlatformURL:     platform.URL,
		ResolvedProfile: ResolvedProfile{Provider: agent.ProviderShell},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	resCh := make(chan *Result, 1)
	go func() {
		res, _ := r.Run(ctx, qw)
		resCh <- res
	}()

	// The host leg dials out to the relay.
	waitRelayBound(t, relay)

	// A driver joins (fresh: snapshot + tail), drives input, and observes the
	// echoed output riding back.
	driverTok := mkInteractiveViewerToken(sessionID, "user-driver", "driver")
	driver, err := attachtest.AttachViewer(ctx, relay.BaseWSURL(), driverTok, attachwire.RoleDriver, nil)
	if err != nil {
		t.Fatalf("attach driver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	// The seed was written before the relay host leg started, so it is the
	// shell's first input and reaches a later viewer through the PTY ring replay.
	// The prefix proves the runner kept all three controls for its own attach path
	// while the real PTY child observed none of them.
	if !waitForTerminalText(driver, "attach-env-clean:got:"+initialPrompt, 30*time.Second) {
		t.Fatal("PTY child observed a runner-only attach control or missed the initial prompt")
	}

	// Resend-until-echo: §5's delivery contract is client resend from
	// ack+1 (input_ack); the stub relay implements no input_ack and its
	// host sink DROPS on overflow / during host-rebind windows
	// (attachtest/room.go sendToHost), so a one-shot send can be lost by
	// design under load. Resending with fresh inputSeqs is the
	// protocol-correct client behavior; duplicate echoes are harmless to
	// the substring assertion.
	if !sendInputUntil(ctx, t, driver, []byte("hello\n"), func() bool {
		return waitForOutput(driver, "got:hello", 2*time.Second)
	}, 30*time.Second) {
		t.Fatal("driver never observed the shell echo (stamped input did not reach the PTY)")
	}

	// A late viewer converges on a snapshot of the live screen.
	lateTok := mkInteractiveViewerToken(sessionID, "user-late", "viewer")
	late, err := attachtest.AttachViewer(ctx, relay.BaseWSURL(), lateTok, attachwire.RoleViewer, nil)
	if err != nil {
		t.Fatalf("attach late viewer: %v", err)
	}
	t.Cleanup(func() { _ = late.Close() })
	if !waitForSnapshot(late, 30*time.Second) {
		t.Fatal("late-joining viewer never converged on a Snapshot")
	}

	// End the session cleanly → child exits 0 → completed Result. Same
	// resend discipline as above (the quit line can be dropped by the
	// stub's host sink too; repeated "quit" lines are harmless — the
	// shell exits on the first one it reads).
	var res *Result
	quitDeadline := time.After(40 * time.Second)
quitLoop:
	for {
		if err := driver.SendInput(ctx, []byte("quit\n")); err != nil {
			t.Fatalf("driver SendInput quit: %v", err)
		}
		select {
		case res = <-resCh:
			break quitLoop
		case <-quitDeadline:
			t.Fatal("Run did not complete after the interactive child exited")
		case <-time.After(2 * time.Second):
		}
	}
	if res.Status != "completed" {
		t.Fatalf("Run status=%q error=%q; want completed", res.Status, res.Error)
	}

	// The sessionClass stamp reached the lock-refresh double (W4 → W3 rail).
	sc, n := platform.lastSessionClass()
	if n == 0 {
		t.Fatal("no lock-refresh observed by the platform double")
	}
	if sc != "interactive" {
		t.Fatalf("lock-refresh sessionClass=%q; want interactive", sc)
	}
}

// ─── attachTokenSource — per-attempt token re-resolution (refresh rail) ────

func TestAttachTokenSource_NoFileReturnsStatic(t *testing.T) {
	src := attachTokenSource("static-tok", "", nil)
	for i := 0; i < 3; i++ {
		tok, err := src(context.Background())
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if tok != "static-tok" {
			t.Fatalf("call %d: tok=%q; want static-tok", i, tok)
		}
	}
}

func TestAttachTokenSource_FileVariants(t *testing.T) {
	validToken := mkInteractiveHostToken("sess-token-source", 1)
	tests := []struct {
		name    string
		content string // written to the token file before the call
		noFile  bool   // point at a path that does not exist
		want    string
		wantErr error
	}{
		{name: "fresh token", content: validToken, want: validToken},
		{name: "trailing newline trimmed", content: validToken + "\n", want: validToken},
		{name: "surrounding whitespace trimmed", content: "  " + validToken + " \n\n", want: validToken},
		{name: "empty file fails", content: "", wantErr: errAttachTokenFileEmpty},
		{name: "whitespace-only file fails", content: " \n\t\n", wantErr: errAttachTokenFileEmpty},
		{name: "malformed token fails", content: "not-a-compact-jwt", wantErr: errAttachTokenFileMalformed},
		{name: "invalid payload fails", content: "e30.bm90LWpzb24.c2ln", wantErr: errAttachTokenFileMalformed},
		{name: "oversized file fails", content: strings.Repeat("x", maxAttachTokenFileBytes+1), wantErr: errAttachTokenFileOversized},
		{name: "missing file falls back to static", noFile: true, want: "static-tok"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if !tc.noFile {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			src := attachTokenSource("static-tok", path, nil)
			tok, err := src(context.Background())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v; want %v", err, tc.wantErr)
			}
			if tok != tc.want {
				t.Fatalf("tok=%q; want %q", tok, tc.want)
			}
		})
	}
}

// The load-bearing behavior of the refresh rail: the file is re-read on EVERY
// attempt, so a provisioner rewriting it between dials swaps the presented
// token — and a file that degrades (removed) falls back to the static token
// without erroring.
func TestAttachTokenSource_PerAttemptReRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	token1 := mkInteractiveHostToken("sess-token-1", 1)
	token2 := mkInteractiveHostToken("sess-token-2", 2)
	if err := os.WriteFile(path, []byte(token1+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := attachTokenSource("static-tok", path, nil)

	if tok, err := src(context.Background()); err != nil || tok != token1 {
		t.Fatalf("first read tok=%q err=%v; want first valid token", tok, err)
	}

	// Provisioner refresh: atomic replace (tmp + rename), as documented.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token2+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if tok, err := src(context.Background()); err != nil || tok != token2 {
		t.Fatalf("post-rewrite tok=%q err=%v; want second valid token", tok, err)
	}

	// File vanishes → degrade to the static token, no error (an error would
	// only burn a backoff cycle; the static token may still admit).
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	tok, err := src(context.Background())
	if err != nil {
		t.Fatalf("unexpected error after remove: %v", err)
	}
	if tok != "static-tok" {
		t.Fatalf("post-remove tok=%q; want static-tok", tok)
	}
}

func TestAttachTokenSource_InvalidFileRecoversOnNextAttempt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("malformed-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := attachTokenSource("static-tok", path, nil)

	if tok, err := src(context.Background()); tok != "" || !errors.Is(err, errAttachTokenFileMalformed) {
		t.Fatalf("malformed read tok=%q err=%v; want explicit malformed failure", tok, err)
	}

	fresh := mkInteractiveHostToken("sess-recovered", 3)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(fresh+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if tok, err := src(context.Background()); err != nil || tok != fresh {
		t.Fatalf("recovery read tok=%q err=%v; want refreshed token", tok, err)
	}
}

func TestAttachTokenSource_FailuresDoNotExposeTokenContent(t *testing.T) {
	const secretMarker = "do-not-log-this-token-content"
	path := filepath.Join(t.TempDir(), "token")
	malformed := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"secret":"`+secretMarker+`"}`)) + ".%%%"
	if err := os.WriteFile(path, []byte(malformed), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	src := attachTokenSource("static-tok", path, logger)

	_, err := src(context.Background())
	if !errors.Is(err, errAttachTokenFileMalformed) {
		t.Fatalf("err=%v; want malformed failure", err)
	}
	if strings.Contains(err.Error(), secretMarker) {
		t.Fatalf("error exposed token content: %v", err)
	}
	// The token source emits no malformed-token diagnostic itself; RunHost logs
	// only the content-free sentinel returned above.
	if strings.Contains(logs.String(), secretMarker) {
		t.Fatal("logs exposed malformed token content")
	}
}

// TokenSource is a concurrent-use contract: the degraded carrier can re-mint
// from its POST-up, SSE, and upgrade-probe paths at the same time. Exercise the
// warning-state transitions under a synchronized fan-out so -race observes any
// unsynchronized closure state, while the assertions also pin one warning per
// distinct failure until a successful read clears the condition.
func TestAttachTokenSource_ConcurrentWarningState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	src := attachTokenSource("static-tok", path, logger)

	callConcurrently := func(want string, wantErr error) {
		t.Helper()
		const callers = 64
		start := make(chan struct{})
		errCh := make(chan error, callers)
		var wg sync.WaitGroup
		wg.Add(callers)
		for i := 0; i < callers; i++ {
			go func() {
				defer wg.Done()
				<-start
				tok, err := src(context.Background())
				if !errors.Is(err, wantErr) {
					errCh <- fmt.Errorf("source err=%v; want %v", err, wantErr)
					return
				}
				if tok != want {
					errCh <- fmt.Errorf("tok=%q; want %q", tok, want)
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Error(err)
		}
	}

	// A persistent missing file warns once even when many callers observe it.
	callConcurrently("static-tok", nil)
	callConcurrently("static-tok", nil)
	if got := strings.Count(logs.String(), "attach token file unreadable"); got != 1 {
		t.Fatalf("unreadable warning count=%d; want 1 before recovery\nlogs:\n%s", got, logs.String())
	}

	// Present-but-invalid files fail explicitly under concurrent resolution.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	callConcurrently("", errAttachTokenFileEmpty)
	if err := os.WriteFile(path, []byte("malformed-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	callConcurrently("", errAttachTokenFileMalformed)

	// A successful read clears the warning state. A later recurrence of the
	// missing-file condition must therefore warn exactly once again.
	fresh := mkInteractiveHostToken("sess-concurrent-refresh", 4)
	if err := os.WriteFile(path, []byte(fresh+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	callConcurrently(fresh, nil)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	callConcurrently("static-tok", nil)
	if got := strings.Count(logs.String(), "attach token file unreadable"); got != 2 {
		t.Fatalf("unreadable warning count=%d; want 2 after recovery and recurrence\nlogs:\n%s", got, logs.String())
	}
}
