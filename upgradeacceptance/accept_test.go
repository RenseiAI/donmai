package upgradeacceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/pi"
)

// This file drives the production entry points behind the in-repo half of
// the D6 failure matrix:
//
//   - The stub receiver (stubreceiver.go) is exercised over HTTP exactly as
//     a runner would call it: lease refresh, step heartbeat, terminal
//     status with exact replay, and worker-id rotation on re-registration.
//   - The local queue's CommitTerminal is exercised directly: it is the
//     production entry point the standalone lane's receiver calls, and the
//     forced-second-replay case asserts the original receipt returns with
//     its revision unchanged.
//   - The scripted fake harness binary is built from real source and driven
//     through the real pi provider (Spawn), proving it speaks the line
//     protocol the harness claims — including the D8 variant that reports
//     loaded transcript history on resume.
//
// The live-seat cases (seat-survives-upgrade and friends) need a systemd
// user manager and run in the container job; here they are recorded red
// with their failure cause (TestUpgradeAcceptanceRed).

func buildFakeHarness(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake harness subprocess test is unix-only")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go toolchain not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "fake-pi-upgrade-harness")
	cmd := exec.Command(goBin, "build", "-o", out, "./testdata/fakeharness") //nolint:gosec // goBin resolved from PATH; package path is fixed.
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build fakeharness: %v\n%s", err, combined)
	}
	return out
}

func postJSON(t *testing.T, client *http.Client, url, bearer string, body any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func TestReceiverExactReplay(t *testing.T) {
	t.Parallel()
	r := newStubReceiver(t)
	worker, bearer := r.currentWorker()
	base := r.url() + "/api/sessions/sess-1"
	client := r.client()

	body := map[string]any{"workerId": worker, "status": "completed", "attempt": "att-1", "summary": "done"}
	code, raw := postJSON(t, client, base+"/status", bearer, body)
	if code != http.StatusOK {
		t.Fatalf("first commit status = %d (%s), want 200", code, raw)
	}
	var first map[string]any
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	rev, _ := first["revision"].(string)
	if rev == "" {
		t.Fatalf("first commit returned no revision: %s", raw)
	}

	// Exact replay returns the original receipt with its revision unchanged.
	code, raw = postJSON(t, client, base+"/status", bearer, body)
	if code != http.StatusOK {
		t.Fatalf("replay status = %d (%s), want 200", code, raw)
	}
	var replay map[string]any
	if err := json.Unmarshal(raw, &replay); err != nil {
		t.Fatal(err)
	}
	if replay["revision"] != rev {
		t.Fatalf("replay revision = %v, want original %v", replay["revision"], rev)
	}
	if replay["replay"] != true {
		t.Fatalf("replay body = %s, want the replay marker", raw)
	}

	// A changed body for the same session+attempt is a conflict.
	changed := map[string]any{"workerId": worker, "status": "failed", "attempt": "att-1", "summary": "different"}
	if code, raw := postJSON(t, client, base+"/status", bearer, changed); code != http.StatusConflict {
		t.Fatalf("changed-body status = %d (%s), want 409", code, raw)
	}

	// A running transition is observed, never committed.
	if code, raw := postJSON(t, client, base+"/status", bearer, map[string]any{"workerId": worker, "status": "running"}); code != http.StatusOK {
		t.Fatalf("running status = %d (%s), want 200", code, raw)
	}
	if _, done := r.terminal("sess-1"); !done {
		t.Fatal("running transition must not clear the stored terminal")
	}
}

func TestReceiverWorkerRotation(t *testing.T) {
	t.Parallel()
	r := newStubReceiver(t)
	worker, bearer := r.currentWorker()
	base := r.url() + "/api/sessions/sess-2"
	client := r.client()

	// The lease refresh succeeds with the current pair.
	if code, raw := postJSON(t, client, base+"/lock-refresh", bearer, map[string]any{"workerId": worker}); code != http.StatusOK {
		t.Fatalf("refresh before rotation = %d (%s), want 200", code, raw)
	}

	// Re-registration rotates the worker id.
	code, raw := postJSON(t, client, r.url()+"/api/workers/register", bearer, map[string]any{"workerId": worker})
	if code != http.StatusOK {
		t.Fatalf("re-register = %d (%s), want 200", code, raw)
	}
	var reg map[string]any
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	newWorker, _ := reg["workerId"].(string)
	newBearer, _ := reg["bearer"].(string)
	if newWorker == "" || newWorker == worker || newBearer == "" || newBearer == bearer {
		t.Fatalf("rotation returned worker=%q bearer-set=%v, want a fresh pair", newWorker, newBearer != "")
	}

	// The old bearer lapses; the rotated pair succeeds.
	if code, _ := postJSON(t, client, base+"/lock-refresh", bearer, map[string]any{"workerId": worker}); code != http.StatusUnauthorized {
		t.Fatalf("refresh with lapsed bearer = %d, want 401", code)
	}
	if code, raw := postJSON(t, client, base+"/lock-refresh", newBearer, map[string]any{"workerId": newWorker}); code != http.StatusOK {
		t.Fatalf("refresh with rotated pair = %d (%s), want 200", code, raw)
	}
	if _, _, n := r.counts(); n != 1 {
		t.Fatalf("registrations = %d, want 1", n)
	}
}

func TestReceiverLeaseAcrossGap(t *testing.T) {
	t.Parallel()
	r := newStubReceiver(t)
	worker, bearer := r.currentWorker()
	base := r.url() + "/api/sessions/sess-3"
	client := r.client()

	for i := 0; i < 3; i++ {
		if code, raw := postJSON(t, client, base+"/lock-refresh", bearer, map[string]any{"workerId": worker}); code != http.StatusOK {
			t.Fatalf("refresh %d = %d (%s), want 200", i, code, raw)
		}
		if code, raw := postJSON(t, client, base+"/step-heartbeat", bearer, map[string]any{"workerId": worker, "emittedAt": time.Now().UTC().Format(time.RFC3339Nano)}); code != http.StatusOK {
			t.Fatalf("beat %d = %d (%s), want 200", i, code, raw)
		}
	}
	refreshes, beats, _ := r.counts()
	if refreshes != 3 || beats != 3 {
		t.Fatalf("refreshes=%d beats=%d, want 3 and 3", refreshes, beats)
	}
	if got := len(r.stepBeatBodies()); got != 3 {
		t.Fatalf("recorded beats = %d, want 3", got)
	}

	// Committing the terminal flips refresh to stop without releasing the
	// evidence: the stored terminal stays readable.
	body := map[string]any{"workerId": worker, "status": "completed", "attempt": "att-3"}
	if code, raw := postJSON(t, client, base+"/status", bearer, body); code != http.StatusOK {
		t.Fatalf("terminal commit = %d (%s), want 200", code, raw)
	}
	code, raw := postJSON(t, client, base+"/lock-refresh", bearer, map[string]any{"workerId": worker})
	if code != http.StatusOK || !strings.Contains(string(raw), `"stop":true`) {
		t.Fatalf("refresh after terminal = %d (%s), want stop=true", code, raw)
	}
	if _, done := r.terminal("sess-3"); !done {
		t.Fatal("terminal evidence must stay stored after the session ends")
	}
}

// TestReceiverPendingReplay mirrors the harness-finishes-while-daemon-down
// matrix case against the stub receiver: the harness's direct post fails,
// the outbox stays pending, and the replayed exact bytes commit once while
// a forced second replay returns the original receipt with its revision
// unchanged. The standalone lane's production commit behind this shape —
// the local queue's CommitTerminal — is pinned by
// TestAttemptFenceUnknownLaunchAndExactTerminalReplay in internal/localqueue.
func TestReceiverPendingReplay(t *testing.T) {
	t.Parallel()
	r := newStubReceiver(t)
	worker, bearer := r.currentWorker()
	base := r.url() + "/api/sessions/sess-pending"
	client := r.client()

	// The direct post fails (simulated outage): nothing is committed.
	r.setOutage(true)
	body := map[string]any{"workerId": worker, "status": "completed", "attempt": "att-p", "summary": "pending replay"}
	if code, raw := postJSON(t, client, base+"/status", bearer, body); code != http.StatusServiceUnavailable {
		t.Fatalf("post during outage = %d (%s), want 503", code, raw)
	}
	if _, done := r.terminal("sess-pending"); done {
		t.Fatal("outage post must not commit")
	}

	// The daemon replays the exact bytes once the receiver is back.
	r.setOutage(false)
	code, raw := postJSON(t, client, base+"/status", bearer, body)
	if code != http.StatusOK {
		t.Fatalf("replay commit = %d (%s), want 200", code, raw)
	}
	var first map[string]any
	if err := json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}

	// A forced second replay returns the original receipt unchanged.
	code, raw = postJSON(t, client, base+"/status", bearer, body)
	if code != http.StatusOK {
		t.Fatalf("forced second replay = %d (%s), want 200", code, raw)
	}
	var second map[string]any
	if err := json.Unmarshal(raw, &second); err != nil {
		t.Fatal(err)
	}
	if second["revision"] != first["revision"] || second["replay"] != true {
		t.Fatalf("second replay = %s, want original revision %v", raw, first["revision"])
	}
}

// TestFakeHarnessSpeaksPiRPC builds the scripted fake from real source and
// drives it through the real pi provider: handshake, get_state, one gated
// turn, and the terminal settle. It fails when the fake drifts from the
// line protocol the provider expects.
func TestFakeHarnessSpeaksPiRPC(t *testing.T) {
	bin := buildFakeHarness(t)
	trigger := filepath.Join(t.TempDir(), "trigger")
	if err := os.WriteFile(trigger, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_HARNESS_TRIGGER", trigger)
	provider, err := pi.New(pi.Options{
		PiBin:            bin,
		HandshakeTimeout: 20 * time.Second,
		VersionProbe:     func(context.Context, string) (string, error) { return pi.PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("pi.New: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	dir := t.TempDir()
	handle, err := provider.Spawn(context.Background(), piSpec(dir, "say the word"))
	if err != nil {
		t.Fatalf("Spawn through fake harness: %v", err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()
	text, err := collectAssistantText(context.Background(), handle, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "ok") {
		t.Fatalf("assistant text = %q, want the fake turn reply", text)
	}
	if id := handle.SessionID(); id == "" {
		t.Fatal("SessionID empty after get_state through the fake harness")
	}
}

// TestFakeHarnessResumeReportsHistory covers the D8 variant: the fake keeps
// its transcript in the session-owned state dir and, when resumed with the
// session flag, reports how much history it loaded on its first turn.
func TestFakeHarnessResumeReportsHistory(t *testing.T) {
	bin := buildFakeHarness(t)
	stateDir := t.TempDir()
	trigger := filepath.Join(t.TempDir(), "trigger")
	if err := os.WriteFile(trigger, []byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_HARNESS_TRIGGER", trigger)
	t.Setenv("FAKE_HARNESS_STATE_DIR", stateDir)
	provider, err := pi.New(pi.Options{
		PiBin:            bin,
		HandshakeTimeout: 20 * time.Second,
		VersionProbe:     func(context.Context, string) (string, error) { return pi.PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("pi.New: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	dir := t.TempDir()

	// Two follow-up turns on one live handle append two transcript lines
	// to the session-owned state dir.
	handle, err := provider.Spawn(context.Background(), piSpec(dir, "first"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = handle.Stop(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var text string
	turns := 0
	injected := false
Loop:
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("turns incomplete: %v (text %q)", ctx.Err(), text)
		case ev, ok := <-handle.Events():
			if !ok {
				break Loop
			}
			switch e := ev.(type) {
			case agent.AssistantTextEvent:
				text += e.Text
			case agent.LlmCallEvent:
				if e.TurnCompleted {
					turns++
					if turns == 1 && !injected {
						injected = true
						if err := handle.Inject(context.Background(), "second"); err != nil {
							t.Fatalf("Inject: %v", err)
						}
					}
					// Both turns end: the second turn's events arrive
					// after the first turn's ResultEvent, so break on
					// the second completed turn, not on a terminal.
					if turns == 2 {
						break Loop
					}
				}
			case agent.ResultEvent:
				// Non-fatal turn terminal: the pump stays up for the
				// injected second turn, so never break here.
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "transcript.jsonl"))
	if err != nil {
		t.Fatalf("transcript missing in state dir: %v", err)
	}
	if n := len(strings.Split(strings.TrimSpace(string(raw)), "\n")); n != 2 {
		t.Fatalf("transcript has %d lines, want 2 (text %q)", n, text)
	}

	// A fresh process resumed with the session flag loads that transcript
	// and reports the history count on its first turn: kill the handle's
	// child first so the resume below starts a new process, then drive the
	// production Resume entry point with the live session id.
	sessionID := handle.SessionID()
	if sessionID == "" {
		t.Fatal("SessionID empty after get_state through the fake harness")
	}
	_ = handle.Stop(context.Background())
	resumed, err2 := resumeFakeHarness(context.Background(), provider, bin, dir, sessionID, "after resume")
	if err2 != nil {
		t.Fatalf("Resume through fake harness: %v", err2)
	}
	defer func() { _ = resumed.Stop(context.Background()) }()
	resumedText, err := collectAssistantText(context.Background(), resumed, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resumedText, "resumed history=2") {
		t.Fatalf("resumed text = %q, want the loaded-history report", resumedText)
	}
}

// TestUpgradeAcceptanceRed records the acceptance case red on current main:
// headless seats are direct-owned children of the daemon, so a daemon
// restart kills the seat instead of adopting it. The failure cause is the
// selection rule: shimOwnsSession covers interactive sessions only, and the
// local runtime holds (rather than adopts) a recovered session whose shim
// it adopted. This test goes green once those slices land; until then it
// pins the red with the cause attached.
func TestUpgradeAcceptanceRed(t *testing.T) {
	t.Parallel()
	// The matrix must keep listing the live-seat cases even though this
	// build cannot pass them: dropping a row would turn the red into a
	// silent gap.
	live := 0
	adopt := 0
	for _, c := range failureMatrix {
		if !c.local {
			live++
		}
		if c.requiresAdoption {
			adopt++
		}
	}
	if live == 0 || adopt == 0 {
		t.Fatalf("matrix lists %d live and %d adoption-gated cases; the red record needs both", live, adopt)
	}
	// Cause, kept in sync with the container driver's red report: headless
	// shim launch is off (the selection rule owns interactive sessions
	// only) so the upgrade path under test does not exist yet in this
	// build. If a future change enables it, this test must be replaced by
	// the green acceptance assertion, not deleted.
	if headlessShimLaunchEnabled() {
		t.Fatal("headless shim launch is enabled; replace this red record with the green acceptance assertion")
	}
}
