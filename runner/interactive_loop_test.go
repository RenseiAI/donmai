package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachclient/attachtest"
	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/ptyhost"
	"github.com/RenseiAI/donmai/result"
	"github.com/RenseiAI/donmai/runtime/worktree"
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

	const sessionID = "sess-interactive-e2e"

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
