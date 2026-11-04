package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/pi"
	"github.com/RenseiAI/donmai/provider/harness/stub"
)

// TestRun_PiFailedSession_TranscriptDeclaredAndWorkareaRemoved drives the
// production Pi Spawn entry through a full Runner Run with a real
// subprocess that owns its transcript: the fake child writes one JSONL
// transcript under the exact per-session layout root, then replies with a
// failed verdict. The runner must retain the transcript descriptor from the
// final handle after Stop and then remove the workarea. The transcript copy
// the failed-session archiver will take rides these descriptors; the Pi-side
// preservation of the trailing write is pinned in
// provider/harness/pi/terminal_evidence_test.go.
//
// RED proof: remove (*pi.Handle).TerminalTranscripts and this test retains
// no descriptor (the production-entry proof finds no transcript).
func TestRun_PiFailedSession_TranscriptDeclaredAndWorkareaRemoved(t *testing.T) {
	bin := buildFakePiBinary(t)
	const lastLine = "RUNNER_EVIDENCE_FINAL_LINE_7c1d9e"
	t.Setenv("FAKEPI_WRITE_TRANSCRIPT", "1")
	t.Setenv("FAKEPI_TRANSCRIPT_LAST_LINE", lastLine)
	t.Setenv("FAKEPI_REPLY", "evidence turn done\nWORK_RESULT:failed")

	piProvider, err := pi.New(pi.Options{
		PiBin:            bin,
		HandshakeTimeout: 20 * time.Second,
		VersionProbe:     func(context.Context, string) (string, error) { return pi.PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("pi.New: %v", err)
	}
	t.Cleanup(func() { _ = piProvider.Shutdown(context.Background()) })

	base, err := stub.New()
	if err != nil {
		t.Fatalf("stub.New: %v", err)
	}
	harness, ok := base.(agent.HarnessProvider)
	if !ok {
		t.Fatal("stub provider is not a HarnessProvider")
	}
	provider := &fakePiSessionProvider{HarnessProvider: harness, pi: piProvider}

	h := newRunnerHarness(t)
	reg := NewRegistry()
	if err := reg.Register(provider); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h.runner.registry = reg
	// Remove the workarea on teardown so the test proves the descriptor
	// survives the cleanup the archiver runs against.
	h.runner.preserveAlways = false

	qw := h.queuedWork("EVIDENCE-PI-1")
	qw.WorkType = "development"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.runner.Run(ctx, qw)
	if err != nil {
		t.Fatalf("Run: %v (failureMode=%q error=%q)", err, res.FailureMode, res.Error)
	}
	if res.WorkResult != "failed" {
		t.Fatalf("WorkResult = %q; want failed so the evidence path is the failed-verdict one (status=%q failureMode=%q error=%q)", res.WorkResult, res.Status, res.FailureMode, res.Error)
	}
	if len(res.terminalTranscripts) != 1 {
		t.Fatalf("terminalTranscripts = %+v; want exactly one Pi transcript descriptor", res.terminalTranscripts)
	}
	descPath := res.terminalTranscripts[0].Path
	if filepath.Ext(descPath) != ".jsonl" || !strings.Contains(descPath, ".pi-") {
		t.Fatalf("descriptor path = %q; want a .jsonl transcript under the per-session state root", descPath)
	}
	// The workarea that owned the transcript is gone: the descriptor is the
	// only thing that survives for the archiver to copy before this point.
	workarea := filepath.Dir(filepath.Dir(descPath))
	if _, err := os.Stat(workarea); !os.IsNotExist(err) {
		t.Fatalf("workarea %q still present after failed teardown (err=%v); want it removed", workarea, err)
	}
	if _, err := os.Stat(descPath); !os.IsNotExist(err) {
		t.Fatalf("transcript %q still present after workarea removal (err=%v); want the workarea lifecycle to own it", descPath, err)
	}
}
