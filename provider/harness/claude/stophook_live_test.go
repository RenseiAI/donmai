//go:build stophookspike

// Live acceptance for the promoted Stop-hook notice channel.
//
// The spike beside this file (stophook_spike_test.go) answered whether the CLI
// honours a `decision:block` at all, using a hand-built settings blob and a
// hand-written hook. This file answers the different question the promotion
// raises: does THE SHIPPED CODE PATH deliver — Provider.spawnInteractive's own
// --settings, its own generated hook script, and agent.NoticeChannel's own
// Offer/Consumed — against a real `claude` process.
//
// Everything in between is already covered by the ordinary unit suite. What
// cannot be unit-tested is the one thing that has been wrong eight times in
// this program: whether the third-party CLI does what the code assumes.
//
// Build-tagged out of the default suite because it launches the real CLI,
// issues real model requests, and costs money:
//
//	go test -tags=stophookspike -run TestStopHookLive -v -timeout 15m ./provider/harness/claude/
package claude

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/attachwire"
)

// liveProofToken is written by the agent only if it acted on a message that
// existed nowhere in its seed prompt.
const liveProofToken = "LIVE_NOTICE_DELIVERED_OK"

// liveSeedPrompt keeps the first turn busy for long enough that a notice
// offered right after spawn is outstanding when that turn ends.
//
// This is not test convenience — it is the channel's actual operating envelope.
// A Stop hook fires when a turn ENDS, so a session sitting idle at an empty
// prompt has already fired its last one and cannot be reached this way. Live
// delivery reaches a session that is WORKING; the durable mailbox is what
// reaches one that is not, which is why it is the floor and this is the
// optimisation.
const liveSeedPrompt = "Use the Bash tool to run `sleep 12`. Then reply with exactly the word READY and nothing else."

// TestStopHookLive_ShippedPathDeliversIntoALiveTurn is the acceptance test.
//
// Order is the evidence. Consumed() is asserted FALSE while the turn is still
// running and the hook has not fired — if it answered true there it would be
// answering from the offer rather than from the recipient's record, and every
// later green would be meaningless. Only then is the delivery awaited.
func TestStopHookLive_ShippedPathDeliversIntoALiveTurn(t *testing.T) {
	bin := requireClaudeBinary(t)
	ws := t.TempDir()
	proof := filepath.Join(ws, "PROOF.txt")

	p, err := New(Options{Binary: bin, LookPath: exec.LookPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt:      liveSeedPrompt,
		Autonomous:  true,
		Cwd:         ws,
		Interactive: &agent.InteractiveSpec{Cols: 120, Rows: 40},
		Env:         blankParentSessionEnv(),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(t.Context()) })

	capable, ok := h.(agent.NoticeChannelCapable)
	if !ok {
		t.Fatal("the shipped interactive spawn returned a handle with no notice channel")
	}
	nch := capable.NoticeChannel()
	if nch == nil {
		t.Fatal("the shipped interactive spawn exposed a nil notice channel")
	}
	isess := h.(agent.InteractiveCapable).InteractiveSession()

	// The message names an artefact that appears nowhere in the seed prompt,
	// so the model cannot produce it by accident.
	message := "Before you finish: use the Bash tool to run exactly this command, with no " +
		"modifications:\n\nprintf '" + liveProofToken + "' > " + proof +
		"\n\nAfter it succeeds, reply with the single word DONE and stop."
	if err := nch.Offer("dlv-live-1", message); err != nil {
		t.Fatalf("Offer: %v", err)
	}

	// RED, inline: the offer is placed and the turn has not ended, so nothing
	// has been consumed. A channel that acked here would ack every message it
	// ever silently dropped.
	answerTrustModal(t, isess, 12*time.Second)
	if consumed, err := nch.Consumed(); err != nil {
		t.Fatalf("Consumed (pre-delivery): %v", err)
	} else if consumed {
		t.Fatal("Consumed() answered true while the turn was still running and no hook had fired — " +
			"it is answering from the offer, not from the recipient's record")
	}
	if fileContains(proof, liveProofToken) {
		t.Fatal("the proof artefact exists before any delivery; the fixture proves nothing")
	}

	// GREEN: wait for the harness to collect it.
	deadline := time.Now().Add(5 * time.Minute)
	var consumed bool
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		answerTrustModal(t, isess, 0)
		if consumed, err = nch.Consumed(); err != nil {
			t.Fatalf("Consumed: %v", err)
		}
		if consumed {
			break
		}
		select {
		case <-isess.Done():
			t.Fatalf("the session exited before the notice was consumed; screen:\n%s", sessionScreen(isess))
		default:
		}
	}
	if !consumed {
		t.Fatalf("the notice was never consumed within the budget; screen:\n%s", sessionScreen(isess))
	}

	// Consumption means the message entered the conversation. Independently:
	// did the agent ACT on it? The artefact is the answer, and it is a separate
	// fact from the transcript record Consumed reads.
	if !waitFor(5*time.Minute, func() bool { return fileContains(proof, liveProofToken) }) {
		t.Fatalf("the message was consumed but the agent never acted on it; screen:\n%s", sessionScreen(isess))
	}

	// A consumed notice frees the channel; nothing is left outstanding to be
	// re-collected by the re-fire the block itself provokes.
	if retracted, err := nch.Retract(); err != nil || retracted {
		t.Fatalf("Retract() = %v, %v after consumption; want false, nil — a consumed message must "+
			"not still be sitting in the drop", retracted, err)
	}
}

// TestStopHookLive_NothingOfferedNeverBlocks is the negative control for the
// shipped path: the hook is installed on every interactive session, so it must
// be inert when there is no message. A hook that emitted anything with an empty
// drop would refuse to let ANY turn end, hanging every session the runner
// spawns.
func TestStopHookLive_NothingOfferedNeverBlocks(t *testing.T) {
	bin := requireClaudeBinary(t)
	ws := t.TempDir()

	p, err := New(Options{Binary: bin, LookPath: exec.LookPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt:      "Reply with exactly the word READY and nothing else. Do not use any tools.",
		Autonomous:  true,
		Cwd:         ws,
		Interactive: &agent.InteractiveSpec{Cols: 120, Rows: 40},
		Env:         blankParentSessionEnv(),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(t.Context()) })
	isess := h.(agent.InteractiveCapable).InteractiveSession()

	answerTrustModal(t, isess, 12*time.Second)
	if !waitFor(3*time.Minute, func() bool {
		answerTrustModal(t, isess, 0)
		return strings.Contains(sessionScreen(isess), "READY")
	}) {
		t.Fatalf("the seeded turn never completed with an empty drop — the hook is not inert; screen:\n%s",
			sessionScreen(isess))
	}
	if got := sessionScreen(isess); strings.Contains(got, "Stop hook") {
		t.Fatalf("an empty drop still produced hook output; screen:\n%s", got)
	}
}

// TestStopHookLive_DoesNotDisplaceAnExistingStopHook answers the one merge
// question the channel's design depends on and cannot reason its way to.
//
// The channel installs its Stop hook via --settings, which the CLI applies
// additively ON TOP OF the base layer. "Additive" is measured for distinct
// top-level keys — a --settings carrying only `hooks` leaves an operator's
// statusLine intact — but this hook shares a key path with any Stop hook the
// operator already configured. If same-path arrays REPLACE rather than
// concatenate, every interactive session this runner spawns silently loses the
// operator's own Stop hooks, and nothing anywhere reports it.
//
// So it is measured: a project-scoped settings file declares its own Stop hook
// alongside ours, and both must leave a marker.
func TestStopHookLive_DoesNotDisplaceAnExistingStopHook(t *testing.T) {
	bin := requireClaudeBinary(t)
	ws := t.TempDir()
	operatorMarker := filepath.Join(ws, "OPERATOR_HOOK_RAN")

	// A project-scoped hook, exactly as an operator would configure one.
	cfgDir := filepath.Join(ws, ".claude")
	if err := os.MkdirAll(cfgDir, 0o750); err != nil {
		t.Fatalf("mkdir project settings: %v", err)
	}
	operatorSettings := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/bin/touch ` +
		operatorMarker + `","timeout":10}]}]}}`
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(operatorSettings), 0o600); err != nil {
		t.Fatalf("write project settings: %v", err)
	}

	p, err := New(Options{Binary: bin, LookPath: exec.LookPath})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := p.Spawn(t.Context(), agent.Spec{
		Prompt:      liveSeedPrompt,
		Autonomous:  true,
		Cwd:         ws,
		Interactive: &agent.InteractiveSpec{Cols: 120, Rows: 40},
		Env:         blankParentSessionEnv(),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(t.Context()) })
	isess := h.(agent.InteractiveCapable).InteractiveSession()
	nch := h.(agent.NoticeChannelCapable).NoticeChannel()

	// Both directions matter. Asserting only the operator's marker would also
	// pass if the base layer had displaced OURS instead — the failure mode is
	// symmetric, so the test has to be.
	if err := nch.Offer("dlv-coexist", "Reply with the single word COEXIST and stop."); err != nil {
		t.Fatalf("Offer: %v", err)
	}

	answerTrustModal(t, isess, 12*time.Second)
	var operatorRan, ourNoticeConsumed bool
	waitFor(4*time.Minute, func() bool {
		answerTrustModal(t, isess, 0)
		if _, err := os.Stat(operatorMarker); err == nil {
			operatorRan = true
		}
		if !ourNoticeConsumed {
			if consumed, err := nch.Consumed(); err == nil && consumed {
				ourNoticeConsumed = true
			}
		}
		return operatorRan && ourNoticeConsumed
	})

	if !operatorRan {
		t.Fatalf("the operator's own Stop hook never ran alongside ours — installing the notice "+
			"channel silently disables operator hooks in every spawned session; screen:\n%s",
			sessionScreen(isess))
	}
	if !ourNoticeConsumed {
		t.Fatalf("the operator's Stop hook displaced the notice channel — live delivery is dead "+
			"on any host with its own Stop hook configured; screen:\n%s", sessionScreen(isess))
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

func requireClaudeBinary(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	return bin
}

// blankParentSessionEnv blanks the markers that would make the child treat
// itself as a nested run of whatever session launched the test.
func blankParentSessionEnv() map[string]string {
	env := map[string]string{}
	for _, k := range sessionEnvVars {
		env[k] = ""
	}
	return env
}

// answerTrustModal stands in for the human at the terminal.
//
// The CLI raises a per-cwd workspace-trust prompt on a directory it has not
// seen, and a session parked on it is unreachable — no turn runs, so no Stop
// hook ever fires. The production spawn path does not pre-seed trust, which is
// a real gap in headless interactive launch and is NOT this channel's to fix;
// answering it here isolates the delivery question from it.
func answerTrustModal(t *testing.T, isess agent.InteractiveSession, wait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for {
		if strings.Contains(sessionScreen(isess), "Yes, I trust this folder") {
			_, _ = isess.WriteInput([]byte("\r"))
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// sessionScreen renders the live screen for failure messages. Separate from the
// spike's screenText, which takes the raw ptyhost session.
func sessionScreen(isess agent.InteractiveSession) string {
	scr, _, err := isess.Snapshot()
	if err != nil {
		return "<snapshot error: " + err.Error() + ">"
	}
	cells := scr.Primary
	if scr.ActiveBuffer == attachwire.BufferAlt && scr.AltPresent {
		cells = scr.Alt
	}
	if scr.Cols == 0 || scr.Cols > math.MaxInt32 || scr.Cols > uint64(len(cells)) {
		return "<empty screen>"
	}
	cols := int(scr.Cols)
	var b strings.Builder
	for row := 0; row < len(cells)/cols; row++ {
		var line strings.Builder
		for col := range cols {
			c := cells[row*cols+col]
			if len(c.RuneBytes) == 0 {
				line.WriteByte(' ')
				continue
			}
			line.Write(c.RuneBytes)
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func fileContains(path, want string) bool {
	b, err := os.ReadFile(path) //nolint:gosec // test fixture path
	return err == nil && strings.Contains(string(b), want)
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return cond()
}
