package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
}

func (p *interactivePTYProvider) Name() agent.ProviderName         { return agent.ProviderShell }
func (p *interactivePTYProvider) Capabilities() agent.Capabilities { return agent.Capabilities{} }

func (p *interactivePTYProvider) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, agent.ErrUnsupported
}
func (p *interactivePTYProvider) Shutdown(context.Context) error { return nil }

func (p *interactivePTYProvider) Spawn(_ context.Context, spec agent.Spec) (agent.Handle, error) {
	ph := ptyhost.Spec{Command: p.command, Cwd: spec.Cwd}
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
	return newInteractivePTYHandle(sess), nil
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

func completedRecordingInteractiveSession() *recordingInteractiveSession {
	done := make(chan struct{})
	close(done)
	return &recordingInteractiveSession{
		done:   done,
		exit:   attachwire.NewNormalExit(0),
		exitOK: true,
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
	out, err := r.dispatchInteractive(context.Background(), h, t.TempDir(), qw, res, noopSink{}, nil)
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

// TestInteractive_InitialPromptContract locks the leaf-consumer semantics:
// absent/explicit-empty inputs are no-ops, non-empty data is written verbatim
// plus one newline, and headless/interview modes never receive it even when a
// direct caller reaches dispatchInteractive.
func TestInteractive_InitialPromptContract(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	tests := []struct {
		name          string
		mode          string
		initialPrompt string
		wantInput     string
		wantDelivered bool
	}{
		{name: "absent", mode: interactiveRunMode},
		{name: "explicit empty", mode: interactiveRunMode, initialPrompt: ""},
		{name: "whitespace preserved", mode: interactiveRunMode, initialPrompt: "  ", wantInput: "  \n", wantDelivered: true},
		{name: "unicode multiline", mode: interactiveRunMode, initialPrompt: "こんにちは 🌱\nsecond line", wantInput: "こんにちは 🌱\nsecond line\n", wantDelivered: true},
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
				context.Background(), handle, t.TempDir(), qw, res, sink, nil,
			)
			if err != nil {
				t.Fatalf("dispatchInteractive: %v", err)
			}
			if out.Status != "completed" {
				t.Fatalf("status=%q error=%q; want completed", out.Status, out.Error)
			}
			if got := string(session.inputBytes()); got != tt.wantInput {
				t.Errorf("PTY input = %q, want %q", got, tt.wantInput)
			}
			wantWrites := 0
			if tt.wantDelivered {
				wantWrites = 1
			}
			if got := session.writeCount(); got != wantWrites {
				t.Errorf("WriteInput calls = %d, want %d", got, wantWrites)
			}

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

// TestWriteInitialPromptInput_RetriesShortWrites makes the exact-byte contract
// sensitive to a mutation that assumes one WriteInput call always consumes the
// full Unicode payload.
func TestWriteInitialPromptInput_RetriesShortWrites(t *testing.T) {
	session := completedRecordingInteractiveSession()
	session.maxWrite = 3
	const seed = "雪だるま\nline two"
	if err := writeInitialPromptInput(session, seed); err != nil {
		t.Fatalf("writeInitialPromptInput: %v", err)
	}
	if got, want := string(session.inputBytes()), seed+"\n"; got != want {
		t.Fatalf("PTY input = %q, want %q", got, want)
	}
	if session.writeCount() < 2 {
		t.Fatalf("short-write fixture recorded %d call(s), want multiple", session.writeCount())
	}
}

func TestInteractive_InitialPromptWriteFailure(t *testing.T) {
	t.Setenv(envAttachURL, "")
	t.Setenv(envAttachToken, "")

	session := completedRecordingInteractiveSession()
	session.writeErr = fmt.Errorf("PTY closed")
	handle := &testInteractiveHandle{
		Handle:  &fakeHandle{events: make(chan agent.Event)},
		session: session,
	}
	sink := &recordingSink{}
	qw := QueuedWork{QueuedWork: prompt.QueuedWork{
		SessionID:     "seed-failure",
		Mode:          interactiveRunMode,
		InitialPrompt: "must be delivered",
	}}
	out, err := minimalRunner(t).dispatchInteractive(
		context.Background(), handle, t.TempDir(), qw, &Result{SessionID: qw.SessionID}, sink, nil,
	)
	if err == nil {
		t.Fatal("expected initial-prompt write failure")
	}
	if out.Status != "failed" || out.FailureMode != FailureSpawn {
		t.Fatalf("status=%q mode=%q; want failed/%s", out.Status, out.FailureMode, FailureSpawn)
	}
	for _, ev := range sink.events {
		if system, ok := ev.(agent.SystemEvent); ok && system.Subtype == "interactive-initial-prompt-delivered" {
			t.Fatal("delivery activity emitted after failed PTY write")
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
	out, err := r.dispatchInteractive(context.Background(), h, t.TempDir(), qw, res, noopSink{}, nil)
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
	out, err := r.dispatchInteractive(ctx, h, t.TempDir(), qw, res, noopSink{}, nil)
	if err != nil {
		t.Fatalf("dispatchInteractive: unexpected err %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("status=%q error=%q; want completed", out.Status, out.Error)
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
	out, _ := r.dispatchInteractive(ctx, h, t.TempDir(), qw, res, noopSink{}, nil)
	if out.Status != "failed" {
		t.Fatalf("status=%q; want failed", out.Status)
	}
	if !strings.Contains(out.Error, "exit 3") {
		t.Fatalf("error should carry the exit detail: %q", out.Error)
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
		out, err := r.dispatchInteractive(ctx, h, t.TempDir(), qw, res, noopSink{}, nil)
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
	if got, want := string(recordedSession.inputBytes()), initialPrompt+"\n"; got != want {
		t.Fatalf("initial prompt after reconnect = %q, want %q", got, want)
	}
	if got := recordedSession.writeCount(); got != 1 {
		t.Fatalf("initial prompt WriteInput calls after reconnect = %d, want 1", got)
	}

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

	// The composing daemon would inject these; the OSS runner reads them
	// from its process env.
	t.Setenv(envAttachURL, relay.BaseWSURL())
	t.Setenv(envAttachToken, mkInteractiveHostToken(sessionID, 1))

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
	// A shell that echoes each line as "got:<line>" and exits cleanly on
	// "quit" — keeps the PTY alive across viewer joins, then exits 0.
	prov := &interactivePTYProvider{command: []string{
		"/bin/sh", "-c",
		`while IFS= read -r line; do echo "got:$line"; [ "$line" = quit ] && break; done`,
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
	if !waitForTerminalText(driver, "got:"+initialPrompt, 30*time.Second) {
		t.Fatal("driver never observed the initial prompt as the PTY's first input")
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
	tests := []struct {
		name    string
		content string // written to the token file before the call
		noFile  bool   // point at a path that does not exist
		want    string
	}{
		{name: "fresh token", content: "fresh-tok", want: "fresh-tok"},
		{name: "trailing newline trimmed", content: "fresh-tok\n", want: "fresh-tok"},
		{name: "surrounding whitespace trimmed", content: "  fresh-tok \n\n", want: "fresh-tok"},
		{name: "empty file falls back to static", content: "", want: "static-tok"},
		{name: "whitespace-only file falls back to static", content: " \n\t\n", want: "static-tok"},
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
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
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
	if err := os.WriteFile(path, []byte("tok-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := attachTokenSource("static-tok", path, nil)

	if tok, _ := src(context.Background()); tok != "tok-1" {
		t.Fatalf("first read tok=%q; want tok-1", tok)
	}

	// Provisioner refresh: atomic replace (tmp + rename), as documented.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("tok-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if tok, _ := src(context.Background()); tok != "tok-2" {
		t.Fatalf("post-rewrite tok=%q; want tok-2", tok)
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

	callConcurrently := func(want string) {
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
				if err != nil {
					errCh <- fmt.Errorf("unexpected source error: %w", err)
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
	callConcurrently("static-tok")
	callConcurrently("static-tok")
	if got := strings.Count(logs.String(), "attach token file unreadable"); got != 1 {
		t.Fatalf("unreadable warning count=%d; want 1 before recovery\nlogs:\n%s", got, logs.String())
	}

	// A different failure condition gets its own warning.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	callConcurrently("static-tok")
	if got := strings.Count(logs.String(), "attach token file empty"); got != 1 {
		t.Fatalf("empty warning count=%d; want 1\nlogs:\n%s", got, logs.String())
	}

	// A successful read clears the warning state. A later recurrence of the
	// missing-file condition must therefore warn exactly once again.
	if err := os.WriteFile(path, []byte("fresh-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	callConcurrently("fresh-tok")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	callConcurrently("static-tok")
	if got := strings.Count(logs.String(), "attach token file unreadable"); got != 2 {
		t.Fatalf("unreadable warning count=%d; want 2 after recovery and recurrence\nlogs:\n%s", got, logs.String())
	}
}
