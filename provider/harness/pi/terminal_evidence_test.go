package pi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// buildEvidenceFakePi compiles testdata/fakepi for evidence tests. It mirrors
// runner/buildFakePiBinary but lives in this package so the production-entry
// headless test drives a real subprocess without importing the runner.
func buildEvidenceFakePi(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("evidence subprocess fixtures are unix-only")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go toolchain not on PATH: %v", err)
	}
	out := filepath.Join(t.TempDir(), "fakepi-evidence")
	// #nosec G204 -- goBin resolved from PATH above; args are fixed.
	cmd := exec.Command(goBin, "build", "-o", out, "./testdata/fakepi")
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build testdata/fakepi: %v\n%s", err, combined)
	}
	return out
}

// drainToTerminal reads until the first terminal event or close, bounded by
// the test timeout. It mirrors handle_test.go's drain but stays local so this
// file needs no unexported helper beyond what it already uses.
func drainToTerminal(t *testing.T, h agent.Handle) []agent.Event {
	t.Helper()
	var out []agent.Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
			switch ev.(type) {
			case agent.ResultEvent, agent.ErrorEvent:
				return out
			}
		case <-timeout:
			t.Fatalf("timed out draining events; got %d so far", len(out))
		}
	}
}

// TestTerminalEvidence_HeadlessProductionEntry_TranscriptSurvivesWorkareaRemoval
// drives the production headless Spawn entry against a real subprocess that
// owns its transcript: the fake child writes one JSONL transcript under the
// exact per-session layout root, then completes its turn. Stop must make the
// transcript visible through the evidence capability, and a copy taken before
// teardown (the shape the failed-session archiver will take) must survive
// the workarea removal with its last line intact.
//
// RED proof: remove (*Handle).TerminalTranscripts and this test finds no
// transcript (the type assertion fails and no descriptor is retained).
func TestTerminalEvidence_HeadlessProductionEntry_TranscriptSurvivesWorkareaRemoval(t *testing.T) {
	bin := buildEvidenceFakePi(t)
	workdir := t.TempDir()
	const lastLine = "EVIDENCE_FINAL_LINE_9f3a1c"
	t.Setenv("FAKEPI_WRITE_TRANSCRIPT", "1")
	t.Setenv("FAKEPI_TRANSCRIPT_LAST_LINE", lastLine)
	t.Setenv("FAKEPI_REPLY", "evidence turn done")

	p, err := New(Options{
		PiBin:            bin,
		HandshakeTimeout: 20 * time.Second,
		VersionProbe:     func(context.Context, string) (string, error) { return PinnedVersion, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	h, err := p.Spawn(context.Background(), agent.Spec{Cwd: workdir, Prompt: "go"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	drainToTerminal(t, h)

	if _, err := h.(agent.TerminalTranscriptCapable).TerminalTranscripts(); err == nil {
		t.Fatal("TerminalTranscripts before Stop must refuse while writers may still be active")
	}

	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	capable, ok := h.(agent.TerminalTranscriptCapable)
	if !ok {
		t.Fatal("headless handle does not expose the terminal-transcript capability")
	}
	descs, err := capable.TerminalTranscripts()
	if err != nil {
		t.Fatalf("TerminalTranscripts after Stop: %v", err)
	}
	if len(descs) != 1 {
		t.Fatalf("descriptors = %+v; want exactly one transcript", descs)
	}
	raw, err := os.ReadFile(descs[0].Path) //nolint:gosec // path came from the session's own evidence capability under test
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(raw), lastLine) {
		t.Fatalf("transcript does not preserve its last line %q; got:\n%s", lastLine, raw)
	}

	// Simulate the failed-session archiver taking its copy before teardown:
	// the copy lives outside the workarea and must survive its removal.
	rescueDir := t.TempDir()
	archived := filepath.Join(rescueDir, "evidence.jsonl")
	if err := os.WriteFile(archived, raw, 0o600); err != nil { //nolint:gosec // test-owned rescue path joins a fixed filename under t.TempDir()
		t.Fatalf("archive copy: %v", err)
	}
	if err := os.RemoveAll(workdir); err != nil {
		t.Fatalf("remove workarea: %v", err)
	}
	kept, err := os.ReadFile(archived) //nolint:gosec // test-owned rescue copy
	if err != nil {
		t.Fatalf("archived transcript missing after workarea removal: %v", err)
	}
	if !strings.Contains(string(kept), lastLine) {
		t.Fatalf("archived transcript lost its last line %q after workarea removal", lastLine)
	}
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Fatalf("workarea still present after removal: %v", err)
	}
}

// TestTerminalEvidence_InteractiveFlushGate proves the interactive handle
// exposes its transcript only after the final flush and preserves the
// trailing write the child made on its way out.
//
// RED proof: report paths before the flush and the refusal assertion fails;
// drop the final flush wait and the trailing-line assertion fails.
func TestTerminalEvidence_InteractiveFlushGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	const lastLine = "INTERACTIVE_FINAL_LINE_4b7e2a"
	script := fakePiTranscriptPrelude + `
sleep 0.2
tool_line final FINAL_TOOL
printf '{"type":"message","id":"last-line","message":{"role":"assistant","content":[{"type":"text","text":"` + lastLine + `"}]}}\n' >> "$live"
`
	p := newFakeInteractivePiProvider(t, script)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	capable, ok := h.(agent.TerminalTranscriptCapable)
	if !ok {
		t.Fatal("interactive handle does not expose the terminal-transcript capability")
	}
	if _, err := capable.TerminalTranscripts(); err == nil {
		t.Fatal("interactive TerminalTranscripts before the final flush must refuse")
	}
	flusher, ok := h.(agent.InteractiveActivityFlusher)
	if !ok {
		t.Fatal("interactive handle does not signal its activity flush")
	}
	select {
	case <-flusher.ActivityFlushed():
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for the interactive final flush")
	}
	descs, err := capable.TerminalTranscripts()
	if err != nil {
		t.Fatalf("TerminalTranscripts after flush: %v", err)
	}
	if len(descs) != 1 {
		t.Fatalf("descriptors = %+v; want exactly this session's transcript", descs)
	}
	raw, err := os.ReadFile(descs[0].Path) //nolint:gosec // path came from the session's own evidence capability under test
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(raw), lastLine) {
		t.Fatalf("interactive transcript lost its trailing line %q; got:\n%s", lastLine, raw)
	}
}

// TestTerminalEvidence_Exclusions pins that nested legacy sessions/*
// transcripts are included while every non-transcript is absent: the
// per-session config home, injected extensions, session tmp/cache, credential
// files, policy extensions, symlinks, and non-JSONL files.
//
// RED proof: return the whole layout root and the exclusion assertions fail.
func TestTerminalEvidence_Exclusions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeJSONL := func(path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		writeTranscriptLine(t, path, transcriptMessage("m", map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "x"}},
		}))
	}
	top := filepath.Join(root, "20990101T000000Z_top.jsonl")
	nested := filepath.Join(root, "sessions", "20990101T000000Z_nested.jsonl")
	writeJSONL(top)
	writeJSONL(nested)

	excludedJSONL := []string{
		filepath.Join(root, agentHomeDir, "skip.jsonl"),
		filepath.Join(root, injectedExtensionsDir, "skip.jsonl"),
		filepath.Join(root, piSessionTmpDir, "skip.jsonl"),
		filepath.Join(root, piSessionCacheDir, "skip.jsonl"),
		filepath.Join(root, piSessionCacheDir, "sub", "skip.jsonl"),
	}
	for _, p := range excludedJSONL {
		writeJSONL(p)
	}
	// Credential, policy, and non-JSONL files hold no transcripts.
	for _, p := range []string{
		filepath.Join(root, credentialFileName),
		filepath.Join(root, extensionFileName),
		filepath.Join(root, noticeClaimFileName),
		filepath.Join(root, "notes.txt"),
		filepath.Join(root, "sessions", "notes.txt"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("not a transcript\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink to a transcript outside the layout root must not be read
	// through the evidence capability.
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "real.jsonl")
	writeJSONL(outsideFile)
	link := filepath.Join(root, "link.jsonl")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatal(err)
	}

	descs := collectPiTranscriptDescriptors(root)
	got := make(map[string]bool, len(descs))
	for _, d := range descs {
		got[d.Path] = true
	}
	if !got[top] || !got[nested] || len(descs) != 2 {
		t.Fatalf("descriptors = %+v; want exactly [%q %q]", descs, top, nested)
	}
	for _, p := range excludedJSONL {
		if got[p] {
			t.Errorf("excluded transcript %q was returned", p)
		}
	}
	if got[link] {
		t.Errorf("symlink %q was returned", link)
	}
}

// TestTerminalEvidence_EmptyReturnsNil pins that a session with no transcript
// keeps today's cleanup behavior: no descriptor and no empty evidence
// artifact. Emitting a directory placeholder would fail this assertion.
func TestTerminalEvidence_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if descs := collectPiTranscriptDescriptors(root); len(descs) != 0 {
		t.Fatalf("empty layout descriptors = %+v; want none", descs)
	}
	if descs := collectPiTranscriptDescriptors(filepath.Join(root, "missing")); len(descs) != 0 {
		t.Fatalf("missing root descriptors = %+v; want none", descs)
	}
	if descs := collectPiTranscriptDescriptors(""); len(descs) != 0 {
		t.Fatalf("empty root descriptors = %+v; want none", descs)
	}

	// A handle stopped with no transcript reports no evidence, not a
	// placeholder.
	h := newHandle(nil, nil, agent.Spec{Cwd: root}, "token")
	h.setStateRoot(root)
	h.stopped.Store(true)
	descs, err := h.TerminalTranscripts()
	if err != nil {
		t.Fatalf("TerminalTranscripts: %v", err)
	}
	if len(descs) != 0 {
		t.Fatalf("descriptors = %+v; want none so no empty artifact is created", descs)
	}

	// The interactive handle agrees once its flush has completed.
	inter := &interactiveStateLossHandle{stateRoot: root, activityFlushed: make(chan struct{})}
	close(inter.activityFlushed)
	if descs, err := inter.TerminalTranscripts(); err != nil || len(descs) != 0 {
		t.Fatalf("interactive empty descriptors = %+v, err = %v; want none", descs, err)
	}
}

// TestTerminalEvidence_InteractiveRefusesBeforeFlush is the negative half of
// the flush gate: a handle whose tailer has not flushed yet refuses, even
// when a transcript file already exists on disk.
func TestTerminalEvidence_InteractiveRefusesBeforeFlush(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTranscriptLine(t, filepath.Join(root, "20990101T000000Z_early.jsonl"), transcriptMessage("m", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "early"}},
	}))
	inter := &interactiveStateLossHandle{stateRoot: root, activityFlushed: make(chan struct{})}
	if _, err := inter.TerminalTranscripts(); err == nil {
		t.Fatal("TerminalTranscripts before the final flush must refuse even with a transcript on disk")
	}
}
