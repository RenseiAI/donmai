package upgradeacceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/afclient"
	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/daemon"
	"github.com/RenseiAI/donmai/provider/harness/pi"
	heartbeat "github.com/RenseiAI/donmai/runtime/heartbeat"
	stepheartbeat "github.com/RenseiAI/donmai/runtime/stepheartbeat"
)

// This file drives the production entry points behind the in-repo half of
// the D6 failure matrix:
//
//   - The stub receiver (stubreceiver.go) is exercised over HTTP exactly as
//     a runner would call it: lease refresh, step heartbeat, terminal
//     status with exact replay, and worker-id rotation on re-registration.
//   - The production heartbeat pulser and step-heartbeat emitter run
//     against the stub receiver for the bearer-lapse and credential-push
//     rows.
//   - The production restart-preflight route runs against a real daemon
//     Start for the direct-owned preflight row.
//   - The scripted fake harness binary is built from real source and driven
//     through the real pi provider (Spawn, Resume), proving it speaks the
//     line protocol the harness claims — including the D8 variant that
//     reports loaded transcript history on resume — and that a scope the
//     harness cannot render is refused before any harness process starts.
//
// The standalone lane's exact-replay commit (the local queue's
// CommitTerminal) is pinned by its owning suite, internal/localqueue; the
// stub receiver mirrors its semantics for the hosted shape.
//
// The live-seat cases (seat-survives-upgrade and friends) need a systemd
// user manager and are recorded by the container lane; here the red they
// share is pinned with its cause (TestUpgradeAcceptanceRed).

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

// TestReceiverLeaseAcrossGap drives the receiver-lease-across-gap matrix
// row through the production heartbeat and step-heartbeat entry points:
// a pulser and an emitter that follow the credential rotation through the
// credential provider never miss a tick — no strike accrues and every step
// beat lands. A beat sent with a lapsed bearer is refused: the lease fuse
// is the only writer after rotation.
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

	// A beat with a lapsed bearer is refused, not recorded: rotating the
	// pair retires the old bearer for step heartbeats exactly as it does
	// for lease refresh.
	if code, raw := postJSON(t, client, r.url()+"/api/workers/register", bearer, map[string]any{"workerId": worker}); code != http.StatusOK {
		t.Fatalf("re-register = %d (%s), want 200", code, raw)
	}
	if code, raw := postJSON(t, client, base+"/step-heartbeat", bearer, map[string]any{"workerId": worker}); code != http.StatusUnauthorized {
		t.Fatalf("beat with lapsed bearer = %d (%s), want 401", code, raw)
	}
	if _, beats, _ := r.counts(); beats != 3 {
		t.Fatalf("beats after lapsed-beat refusal = %d, want 3 (refused beats are not recorded)", beats)
	}

	// Committing the terminal flips refresh to stop without releasing the
	// evidence: the stored terminal stays readable.
	newWorker, newBearer := r.currentWorker()
	body := map[string]any{"workerId": newWorker, "status": "completed", "attempt": "att-3"}
	if code, raw := postJSON(t, client, base+"/status", newBearer, body); code != http.StatusOK {
		t.Fatalf("terminal commit = %d (%s), want 200", code, raw)
	}
	code, raw := postJSON(t, client, base+"/lock-refresh", newBearer, map[string]any{"workerId": newWorker})
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
	started := filepath.Join(t.TempDir(), "started")
	t.Setenv("FAKE_HARNESS_STARTED", started)
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
	// Positive control for TestScopeCreationRefused: a spawned session
	// marks its start, so that test's absent marker means no harness
	// process started rather than a marker that is never written.
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("started marker after a successful spawn: %v; the fake did not record its start", err)
	}
}

// TestFakeHarnessTriggerVerdicts pins the three ways the trigger gate ends
// a held turn, which the upgrade flow tells apart: a trigger that appears
// releases the turn with its reply, a trigger holding "abort" ends it as
// aborted, and a trigger that never appears within the wait ends it as
// timeout — a slow runner is never recorded as an operator abort.
func TestFakeHarnessTriggerVerdicts(t *testing.T) {
	bin := buildFakeHarness(t)
	for _, tc := range []struct {
		name    string
		trigger string // file content; empty means the file never appears
		want    string
	}{
		{name: "released", trigger: "go\n", want: "ok"},
		{name: "aborted", trigger: "abort\n", want: "aborted"},
		{name: "timed-out", want: "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trigger := filepath.Join(t.TempDir(), "trigger")
			if tc.trigger != "" {
				if err := os.WriteFile(trigger, []byte(tc.trigger), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := fakeHarnessTurnText(t, bin, "FAKE_HARNESS_TRIGGER="+trigger, "FAKE_HARNESS_TRIGGER_WAIT=300ms")
			if got != tc.want {
				t.Fatalf("turn text = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeHarnessTurnText runs the fake harness binary for one prompt over the
// pi RPC line protocol on stdio and returns the turn's assistant text.
func fakeHarnessTurnText(t *testing.T, bin string, env ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin) //nolint:gosec // bin is the fake harness this test built.
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, env...)
	cmd.Stdin = strings.NewReader(`{"type":"prompt","message":"hold"}` + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fake harness turn: %v\n%s", err, out)
	}
	var text string
	settled := false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var ev struct {
			Type  string `json:"type"`
			Event struct {
				Delta string `json:"delta"`
			} `json:"assistantMessageEvent"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("fake harness emitted a non-JSON line %q: %v", line, err)
		}
		switch ev.Type {
		case "message_update":
			text += ev.Event.Delta
		case "agent_settled":
			settled = true
		}
	}
	if !settled {
		t.Fatalf("fake harness turn never settled:\n%s", out)
	}
	return text
}

// TestFakeHarnessReportsThePinnedVersion keeps the fake's --version answer
// equal to the pi provider's pin, so the version probe labels the fake
// verified exactly as it labels the real binary.
func TestFakeHarnessReportsThePinnedVersion(t *testing.T) {
	bin := buildFakeHarness(t)
	out, err := exec.Command(bin, "--version").Output() //nolint:gosec // bin is the fake harness this test built.
	if err != nil {
		t.Fatalf("fake --version: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != pi.PinnedVersion {
		t.Fatalf("fake --version = %q, want the pi provider pin %q", got, pi.PinnedVersion)
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
	resumed, err2 := resumeFakeHarness(context.Background(), provider, dir, sessionID, "after resume")
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
//
// The gate follows the production selection predicate (headlessShimLaunch-
// Enabled asks the daemon's own SessionShimOwnsSession about a headless
// spec), so the record flips exactly when the rule flips — an edit that
// hard-codes the gate green fails the probe-consistency test below, and a
// landed behaviour with a stale red record fails here.
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

// TestHeadlessLaunchProbeFollowsTheSelectionRule pins the B3 contract both
// ways: the probe the red record and the container driver follow reports
// the production selection rule's own answer for a headless spec, with an
// interactive control proving the rule discriminates. On current main the
// headless answer is false while the interactive answer is true; once the
// adoption slices flip the rule, the headless answer flips with it and the
// red record above must become the green acceptance assertion.
func TestHeadlessLaunchProbeFollowsTheSelectionRule(t *testing.T) {
	t.Parallel()
	probe := adoptionProbeForHeadlessSpec(defaultAdoptionProbeDaemon())
	if probe.ownsHeadless {
		t.Fatal("production selection rule owns the headless probe spec; replace the red record with the green acceptance assertion")
	}
	if !probe.ownsControl {
		t.Fatal("production selection rule does not own the interactive control spec; the probe is not reading the rule")
	}
	if headlessShimLaunchEnabled() != probe.ownsHeadless {
		t.Fatalf("headlessShimLaunchEnabled() = %v, production rule = %v; the gate must follow the rule",
			headlessShimLaunchEnabled(), probe.ownsHeadless)
	}
}

// TestHeadlessLaunchProbeTurnsGreenWithTheRule proves the green arm is
// reachable without an edit to the gate: the probe predicate already
// answers true for the session class the rule owns, so flipping the rule
// to cover headless seats flips the probe with it. A red record whose gate
// could never turn green — a dead green arm — fails here.
func TestHeadlessLaunchProbeTurnsGreenWithTheRule(t *testing.T) {
	t.Parallel()
	d := daemon.New(daemon.Options{
		SkipRegistration: true,
		SessionShim:      daemon.SessionShimConfig{EnableAdoption: true, EnableOwnership: true},
	})
	if got := d.SessionShimOwnsSession(daemon.SessionSpec{SessionID: "acceptance-probe-interactive", Mode: "interactive"}); !got {
		t.Fatalf("SessionShimOwnsSession(interactive) with ownership enabled = %v, want true; "+
			"without a reachable green arm the driver's green branch is dead text", got)
	}
	if got := d.SessionShimOwnsSession(daemon.SessionSpec{SessionID: "acceptance-probe-headless"}); got {
		t.Fatalf("SessionShimOwnsSession(headless) with ownership enabled = %v, want false "+
			"until the adoption slices flip the rule; the red record pins this false", got)
	}
}

// TestFailureMatrixRegistered pins the matrix shape: the row count, the
// local/live split, and that every local row names the in-repo test that
// drives it while no live row claims one (only the container records live
// rows). The matrix is a harness, not a list: a new local row cannot land
// without its driver, and TestSuiteDefinesEveryNamedDriver proves each
// named driver exists.
func TestFailureMatrixRegistered(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range failureMatrix {
		if c.name == "" || c.want == "" {
			t.Fatalf("matrix row %+v needs a stable name and a required outcome", c)
		}
		if seen[c.name] {
			t.Fatalf("duplicate matrix case %q", c.name)
		}
		seen[c.name] = true
	}
	const wantCases = 22
	if len(failureMatrix) != wantCases {
		t.Fatalf("matrix has %d cases, want %d", len(failureMatrix), wantCases)
	}
	if len(matrixLiveNames()) == 0 {
		t.Fatal("matrix lists no live-seat cases; the container record would carry nothing")
	}
	local := 0
	for _, c := range failureMatrix {
		switch {
		case c.local && c.driver == "":
			t.Fatalf("local matrix case %q names no driving test", c.name)
		case !c.local && c.driver != "":
			t.Fatalf("live matrix case %q names in-repo driver %q; live cases are recorded by the container lane", c.name, c.driver)
		case c.local:
			local++
		}
	}
	const wantLocal = 9
	if local != wantLocal {
		t.Fatalf("matrix has %d local cases, want %d", local, wantLocal)
	}
}

// TestSuiteDefinesEveryNamedDriver proves every driver a local matrix row
// names is a real top-level test in this package: it parses the package's
// own _test.go files rather than trusting a list kept by hand, so renaming
// or deleting a driver test orphans its row loudly.
func TestSuiteDefinesEveryNamedDriver(t *testing.T) {
	t.Parallel()
	defined := packageTestFunctions(t)
	for _, c := range failureMatrix {
		if c.driver == "" {
			continue
		}
		if !defined[c.driver] {
			t.Fatalf("matrix row %q names driver %q, which no _test.go file in this package defines", c.name, c.driver)
		}
	}
}

// packageTestFunctions returns the names of the top-level test functions
// (func TestXxx(t *testing.T)) the package's _test.go files declare.
func packageTestFunctions(t *testing.T) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("list test files: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no _test.go files found; the test must run from the package directory")
	}
	fset := token.NewFileSet()
	out := map[string]bool{}
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			if params := fn.Type.Params.List; len(params) == 1 {
				if star, ok := params[0].Type.(*ast.StarExpr); ok {
					if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "T" {
						out[fn.Name.Name] = true
					}
				}
			}
		}
	}
	return out
}

// startAcceptanceDaemon starts a real daemon against a throwaway config:
// the production Start path (config load, spawner, lifecycle) with a
// short-lived worker command, so restart-preflight tests drive the same
// admission and preflight the container job exercises.
func startAcceptanceDaemon(t *testing.T) *daemon.Daemon {
	t.Helper()
	dir := t.TempDir()
	cfg := daemon.DefaultConfig()
	cfg.Machine.ID = "acceptance-harness"
	cfg.Capacity.MaxConcurrentSessions = 2
	cfg.Projects = []daemon.ProjectConfig{{ID: "demo", Repository: "example.invalid/demo"}}
	cfg.Orchestrator.URL = "http://127.0.0.1:1"
	cfg.Orchestrator.AuthToken = "local-stub-no-token"
	cfgPath := filepath.Join(dir, "daemon.yaml")
	if err := daemon.WriteConfig(cfgPath, cfg); err != nil {
		t.Fatalf("write daemon config: %v", err)
	}
	d := daemon.New(daemon.Options{
		ConfigPath: cfgPath, JWTPath: filepath.Join(dir, "daemon.jwt"),
		HTTPHost: "127.0.0.1", HTTPPort: 0, SkipWizard: true, SkipRegistration: true,
		SpawnerOptions: daemon.SpawnerOptions{WorkerCommand: []string{"sleep", "30"}},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("daemon Start: %v", err)
	}
	t.Cleanup(func() { _ = d.Stop(context.Background()) })
	return d
}

// acceptDirectSeat dispatches one headless seat onto the daemon's spawner:
// a direct-owned child of the daemon, the seat class the upgrade flow
// holds across the N to N+1 restart.
func acceptDirectSeat(t *testing.T, d *daemon.Daemon, sessionID string) {
	t.Helper()
	if _, err := d.Spawner().AcceptWork(daemon.SessionSpec{SessionID: sessionID, Repository: "example.invalid/demo"}); err != nil {
		t.Fatalf("AcceptWork %q: %v", sessionID, err)
	}
}

// waitSeatGone polls until the spawner releases the seat or the timeout
// ends. StopSession only signals; the reaper releases capacity
// asynchronously, so the preflight afterwards must observe the release,
// not the signal.
func waitSeatGone(t *testing.T, d *daemon.Daemon, sessionID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !seatActive(d, sessionID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("seat %q still active after %v", sessionID, timeout)
}

// seatActive reports whether the spawner still holds sessionID.
func seatActive(d *daemon.Daemon, sessionID string) bool {
	for _, h := range d.Spawner().ActiveSessions() {
		if h.SessionID == sessionID {
			return true
		}
	}
	return false
}

// prepareRestart drives the production restart-preflight route the same
// way the container driver does: through the control API's typed client,
// against a live server bound to the daemon under test.
func prepareRestart(t *testing.T, d *daemon.Daemon) (*afclient.DaemonRestartPreflightResponse, error) {
	t.Helper()
	srv := daemon.NewServer(d)
	if _, err := srv.Start(); err != nil {
		t.Fatalf("control server Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return afclient.NewDaemonClientFromURL("http://" + srv.Addr()).PrepareRestartContext(ctx)
}

// TestDirectOwnedPlusShimOwnedPreflight drives the
// direct-owned-plus-shim-owned-preflight matrix row through the production
// restart-preflight route: while a direct-owned seat is live the preflight
// refuses with the direct-owned count, and once that seat ends the same
// route returns prepared (not_required with no scopes) instead of a
// refusal. The second half proves the refusal counted the direct owner
// rather than the route being unconditionally closed.
func TestDirectOwnedPlusShimOwnedPreflight(t *testing.T) {
	d := startAcceptanceDaemon(t)
	acceptDirectSeat(t, d, "direct-preflight-seat")

	// The seat the refusal must count is live right now: exactly one
	// active session, and it is this direct-owned seat. Without this
	// anchor the refusal below could come from any other stage (a fence,
	// registry, or lifecycle refusal) and the test would still pass.
	if active, _ := d.Spawner().ActiveSessionCounts(); active != 1 {
		t.Fatalf("active sessions = %d, want 1 (the direct-owned seat under test)", active)
	}
	if !seatActive(d, "direct-preflight-seat") {
		t.Fatal("direct-preflight-seat not among active sessions; the refusal below would count the wrong seat")
	}

	if _, err := prepareRestart(t, d); err == nil {
		t.Fatal("preflight with a live direct-owned seat succeeded, want the direct-owned refusal")
	} else {
		var refusal *afclient.DaemonRestartPreflightRefusalError
		if !errors.As(err, &refusal) {
			t.Fatalf("preflight error = %v (%T), want the typed preflight refusal", err, err)
		}
		if refusal.Code != afclient.DaemonRestartPreflightRefusalCode {
			t.Fatalf("preflight refusal code = %q, want %q", refusal.Code, afclient.DaemonRestartPreflightRefusalCode)
		}
		// The uncovered-direct-owned stage has no assigned stage token:
		// it raises the untyped refusal, and the route's typed writer
		// carries the closed top-level code through as the cause. An
		// empty cause would mean the refusal came from a daemon older
		// than the cause registry — not this build — and any other
		// token would name a different stage (fence, registry, or
		// lifecycle) than the direct-owned count under test.
		if refusal.Cause != afclient.DaemonRestartPreflightRefusalCode {
			t.Fatalf("preflight refusal cause = %q, want the closed top-level code %q (the uncovered-direct-owned stage)", refusal.Cause, afclient.DaemonRestartPreflightRefusalCode)
		}
	}

	// The direct-owned seat ends; the next preflight is prepared, proving
	// the refusal above counted that seat and nothing else.
	if !d.StopSession("direct-preflight-seat") {
		t.Fatal("StopSession refused the live direct-owned seat")
	}
	waitSeatGone(t, d, "direct-preflight-seat", 30*time.Second)
	res, err := prepareRestart(t, d)
	if err != nil {
		t.Fatalf("preflight after the direct-owned seat ended = %v, want prepared", err)
	}
	if res.State != afclient.DaemonRestartNotRequired {
		t.Fatalf("preflight state = %q, want %q", res.State, afclient.DaemonRestartNotRequired)
	}
}

// TestBearerExpiresWhileNoDaemonRuns drives the bearer-expires-while-no-
// daemon-runs row through the production heartbeat entry point: a pulser
// presenting a stale worker/bearer pair trips the 3-strike fuse and closes
// LostOwnership (the lease fuse ending the seat), while the terminal
// evidence the seat already committed stays stored on the receiver —
// nothing is released without it.
func TestBearerExpiresWhileNoDaemonRuns(t *testing.T) {
	r := newStubReceiver(t)
	worker, bearer := r.currentWorker()
	base := r.url() + "/api/sessions/sess-bearer-lapse"
	client := r.client()

	// The seat commits its terminal evidence while its bearer is current.
	if code, raw := postJSON(t, client, base+"/status", bearer, map[string]any{"workerId": worker, "status": "completed", "attempt": "att-lapse", "summary": "done"}); code != http.StatusOK {
		t.Fatalf("terminal commit = %d (%s), want 200", code, raw)
	}

	// The bearer lapses (rotation retires it with no daemon left to push
	// the fresh pair): the production heartbeat pulser trips its fuse on
	// the stale pair instead of refreshing forever.
	if code, raw := postJSON(t, client, r.url()+"/api/workers/register", bearer, map[string]any{"workerId": worker}); code != http.StatusOK {
		t.Fatalf("re-register = %d (%s), want 200", code, raw)
	}
	p, err := heartbeat.New(heartbeat.Config{
		SessionID: "sess-bearer-lapse", WorkerID: worker, BaseURL: r.url(),
		AuthToken: bearer, Interval: 20 * time.Millisecond, HTTPClient: client,
		MaxAttemptsPerTick: 1, StrikesUntilLost: 2,
	})
	if err != nil {
		t.Fatalf("heartbeat New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("heartbeat Start: %v", err)
	}
	defer func() { _ = p.Stop() }()
	select {
	case <-p.LostOwnership():
	case <-ctx.Done():
		t.Fatalf("lease fuse never tripped on the lapsed bearer (strikes=%d)", p.Strikes())
	}
	if p.StopRequested() {
		t.Fatal("StopRequested set on a bearer lapse; the fuse must report lost ownership, not an operator cancel")
	}
	// The fuse ends the seat but the terminal evidence stays stored:
	// nothing is released without it.
	if _, done := r.terminal("sess-bearer-lapse"); !done {
		t.Fatal("terminal evidence missing after the lease fuse tripped")
	}
}

// TestDaemonReturnsBeforeBearerExpires drives the daemon-returns-before-
// bearer-expires row through the production heartbeat and step-heartbeat
// entry points. Both beaters start on the original worker pair; the
// returning daemon re-registers (a fresh pair, with the old one still valid
// for the overlap) and pushes the rotated pair to the runner, the stand-in
// for CredentialUpdate. Only then does the old bearer lapse. The beaters
// must follow the push through their credential providers: the lease keeps
// refreshing under the rotated pair, no strike ever accrues, and step beats
// carry the rotated worker id. A beater that ignored the push would keep
// presenting the lapsed pair and strike out.
func TestDaemonReturnsBeforeBearerExpires(t *testing.T) {
	r := newStubReceiver(t)
	worker, bearer := r.currentWorker()
	client := r.client()

	// pushed is what the runner last received from its daemon: the
	// original pair at launch, the rotated pair once the update lands.
	var pushedMu sync.Mutex
	pushedWorker, pushedBearer := worker, bearer
	pushed := func() (string, string) {
		pushedMu.Lock()
		defer pushedMu.Unlock()
		return pushedWorker, pushedBearer
	}
	provider := func(context.Context) (heartbeat.RuntimeCredentials, error) {
		w, b := pushed()
		return heartbeat.RuntimeCredentials{WorkerID: w, AuthToken: b}, nil
	}
	stepProvider := func(context.Context) (stepheartbeat.RuntimeCredentials, error) {
		w, b := pushed()
		return stepheartbeat.RuntimeCredentials{WorkerID: w, AuthToken: b}, nil
	}
	p, err := heartbeat.New(heartbeat.Config{
		SessionID: "sess-credential-push", WorkerID: worker, BaseURL: r.url(),
		AuthToken: bearer, CredentialProvider: provider,
		Interval: 20 * time.Millisecond, HTTPClient: client,
		MaxAttemptsPerTick: 1, StrikesUntilLost: 3,
	})
	if err != nil {
		t.Fatalf("heartbeat New: %v", err)
	}
	e, err := stepheartbeat.New(stepheartbeat.Config{
		SessionID: "sess-credential-push", WorkerID: worker, BaseURL: r.url(),
		AuthToken: bearer, CredentialProvider: stepProvider,
		Interval: 20 * time.Millisecond, HTTPClient: client,
	})
	if err != nil {
		t.Fatalf("step-heartbeat New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("heartbeat Start: %v", err)
	}
	defer func() { _ = p.Stop() }()
	if err := e.Start(ctx); err != nil {
		t.Fatalf("step-heartbeat Start: %v", err)
	}
	defer func() { _ = e.Stop() }()

	waitUntil := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s never happened (strikes=%d)", what, p.Strikes())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitUntil("a lease refresh on the original pair", func() bool { return r.refreshesFor(worker) > 0 })

	// The daemon returns: re-registration rotates the pair while the old
	// bearer is still valid, and the rotated pair is pushed to the runner.
	newWorker, newBearer, retire := r.rotateWithOverlap()
	pushedMu.Lock()
	pushedWorker, pushedBearer = newWorker, newBearer
	pushedMu.Unlock()
	waitUntil("a lease refresh on the pushed pair", func() bool { return r.refreshesFor(newWorker) > 0 })
	waitUntil("a step beat on the pushed pair", func() bool { return stepBeatsFrom(t, r, newWorker) > 0 })

	// The old bearer lapses; the lease keeps refreshing on the pushed pair.
	retire()
	after := r.refreshesFor(newWorker)
	waitUntil("three more lease refreshes after the old bearer lapsed", func() bool { return r.refreshesFor(newWorker) >= after+3 })
	select {
	case <-p.LostOwnership():
		t.Fatalf("lease fuse tripped across the credential push (strikes=%d)", p.Strikes())
	default:
	}
	if got := p.Strikes(); got != 0 {
		t.Fatalf("heartbeat strikes = %d across the credential push, want 0 (the lease never misses a tick)", got)
	}
}

// stepBeatsFrom counts the recorded step heartbeats attributed to workerID.
func stepBeatsFrom(t *testing.T, r *stubReceiver, workerID string) int {
	t.Helper()
	n := 0
	for _, raw := range r.stepBeatBodies() {
		var body struct {
			WorkerID string `json:"workerId"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("step beat body %q: %v", raw, err)
		}
		if body.WorkerID == workerID {
			n++
		}
	}
	return n
}

// TestScopeCreationRefused drives the scope-creation-refused row through
// the production pi spawn entry point: a session stamping a scoped
// execution level the pi harness cannot render is refused before spawn
// with the typed execution_security_unrenderable reason — never launched
// into a weaker scope as a fallback.
func TestScopeCreationRefused(t *testing.T) {
	bin := buildFakeHarness(t)
	// Every RPC session process the fake starts appends its PID to this
	// marker (TestFakeHarnessSpeaksPiRPC is the positive control), so its
	// absence after the refusal proves no harness process started.
	started := filepath.Join(t.TempDir(), "started")
	t.Setenv("FAKE_HARNESS_STARTED", started)
	t.Setenv("FAKE_HARNESS_TRIGGER", filepath.Join(t.TempDir(), "trigger"))
	provider, err := pi.New(pi.Options{
		PiBin:            bin,
		HandshakeTimeout: 20 * time.Second,
		VersionProbe:     func(context.Context, string) (string, error) { return pi.PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("pi.New: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	levels := agent.IndexZeroExecutionSecurityLevels()
	levels.Isolation = agent.IsolationOSSandbox
	spec := piSpec(t.TempDir(), "scoped turn")
	spec.ExecutionSecurity = &agent.ExecutionSecurity{
		Version: agent.ExecutionSecurityWireVersion,
		Levels:  levels,
		Digest:  agent.ExecutionSecurityLevelsDigest(levels),
	}
	handle, err := provider.Spawn(context.Background(), spec)
	if err == nil {
		_ = handle.Stop(context.Background())
		t.Fatal("Spawn of a session stamping a scope the harness cannot render succeeded, want the typed scope refusal")
	}
	if !errors.Is(err, agent.ErrSpawnFailed) {
		t.Fatalf("Spawn error = %v, want a spawn failure", err)
	}
	if code := agent.ExecutionSecurityErrorCode(err); code != agent.ExecutionSecurityUnrenderable {
		t.Fatalf("Spawn error = %v (code %q), want the typed %q scope refusal", err, code, agent.ExecutionSecurityUnrenderable)
	}
	// The refusal happens before spawn: a late-failing fallback launch
	// would satisfy the typed-refusal shape above, so the absent marker is
	// what proves the seat never launched.
	if raw, statErr := os.ReadFile(started); !os.IsNotExist(statErr) {
		t.Fatalf("harness process started before the scope refusal (marker %q, err %v); the seat must be refused before spawn", raw, statErr)
	}
}
