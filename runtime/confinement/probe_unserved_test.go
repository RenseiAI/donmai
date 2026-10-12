package confinement

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// unansweringProbeCommand writes a fake embedder main: a process that never
// serves the confinement probe. It ignores its environment (including the
// probe marker) and sleeps, the way an embedder binary whose main opens its
// own interface instead of calling RunProbeFromEnv behaves under the probe
// launch: alive, but never answering.
func unansweringProbeCommand(t *testing.T) []string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "embedder-main")
	body := "#!/bin/sh\nsleep 120\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake embedder main: %v", err)
	}
	return []string{script}
}

// TestSelfTest_UnansweredProbeFailsFastWithProbeUnserved pins the fail-fast
// contract: a probe child that never serves the probe — the fake embedder
// main above, standing in for a composing binary whose main does not serve
// the confinement probe — fails the self-test with the named probe_unserved
// cause instead of stalling on the proof budget.
//
// The proof budget is deliberately huge (minutes) while the handshake bound
// is seconds: the elapsed assertion proves the handshake bound fires on its
// own, separate from the cold-proof budget. Neither session mode is
// attested, and no attestation is recorded.
func TestSelfTest_UnansweredProbeFailsFastWithProbeUnserved(t *testing.T) {
	fake := unansweringProbeCommand(t)
	w := newSpecWorld(t)
	c := newTestConfiner(t, w, noopBackend{}, nil, "")
	const handshake = 2 * time.Second
	launchers := map[agent.PromptSessionMode]Launcher{
		agent.PromptModeAutonomous:      HeadlessLauncher(),
		agent.PromptModeHumanControlled: HeadlessLauncher(),
	}
	start := time.Now()
	record, err := c.SelfTest(t.Context(), SelfTestOptions{
		ProbeCommand:          fake,
		ScratchDir:            w.base,
		Launchers:             launchers,
		Timeout:               3 * time.Minute,
		ProbeHandshakeTimeout: handshake,
	})
	elapsed := time.Since(start)
	if reason, _ := ReasonOf(err); reason != ReasonProbeUnserved {
		t.Fatalf("SelfTest with an unanswering probe: err=%v, want reason %s", err, ReasonProbeUnserved)
	}
	// Seconds, not minutes: the handshake bound — not the proof budget —
	// ended the run. Sixty seconds is generous (fixture build alone can
	// take seconds on a loaded host); the three-minute proof budget must
	// never be reached.
	if elapsed >= time.Minute {
		t.Fatalf("SelfTest with an unanswering probe took %s, want well under the proof budget", elapsed.Round(time.Millisecond))
	}
	if record.Passed || len(record.SessionModes) != 0 {
		t.Fatalf("record passed=%v modes=%v, want no attested mode", record.Passed, record.SessionModes)
	}
	// Each mode records its own setup outcome naming the cause: the
	// self-test returns promptly with no attestation instead of blocking.
	modes := map[agent.PromptSessionMode]int{}
	for _, probe := range record.Probes {
		if probe.ID == "mode.setup" {
			modes[probe.Mode]++
			if !strings.Contains(probe.Detail, string(ReasonProbeUnserved)) {
				t.Errorf("mode.setup detail = %q, want it to name %s", probe.Detail, ReasonProbeUnserved)
			}
		}
	}
	for _, mode := range []agent.PromptSessionMode{agent.PromptModeAutonomous, agent.PromptModeHumanControlled} {
		if modes[mode] != 1 {
			t.Errorf("mode.setup outcomes for %s = %d, want 1", mode, modes[mode])
		}
	}
	if attestation, ok := c.Attestation(); ok {
		t.Fatalf("an unserved probe attests: %+v", attestation)
	}
}

// TestStartDecoy_UnansweredFakeFailsFast bounds the same shape one layer
// down: the widening decoy is another probe invocation of the same
// executable, and an executable that never answers must fail its wait in
// about the handshake bound, not hang the fixture build forever.
func TestStartDecoy_UnansweredFakeFailsFast(t *testing.T) {
	fake := unansweringProbeCommand(t)
	dir := t.TempDir()
	fx := &fixture{probeCommand: fake, probeHandshake: time.Second, out: dir}
	t.Cleanup(fx.cleanup)
	start := time.Now()
	_, err := fx.startDecoy(filepath.Join(dir, "signalled"))
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "never became ready") {
		t.Fatalf("startDecoy with an unanswering executable: err=%v, want a never-became-ready failure", err)
	}
	if elapsed >= 30*time.Second {
		t.Fatalf("startDecoy with an unanswering executable took %s, want about the handshake bound", elapsed.Round(time.Millisecond))
	}
}

// TestDefaultProbeHandshakeTimeout_IsShortAndSeparate pins the production
// default well below the per-mode proof budget (two minutes): the handshake
// is its own short bound, never a restatement of the cold-proof budget.
func TestDefaultProbeHandshakeTimeout_IsShortAndSeparate(t *testing.T) {
	if DefaultProbeHandshakeTimeout <= 0 || DefaultProbeHandshakeTimeout > time.Minute {
		t.Fatalf("DefaultProbeHandshakeTimeout = %s, want a positive bound well under the proof budget", DefaultProbeHandshakeTimeout)
	}
}
