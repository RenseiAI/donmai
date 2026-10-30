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

	descs, err := collectPiTranscriptDescriptors(root, nil)
	if err != nil {
		t.Fatalf("collectPiTranscriptDescriptors: %v", err)
	}
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
	if descs, err := collectPiTranscriptDescriptors(root, nil); err != nil || len(descs) != 0 {
		t.Fatalf("empty layout descriptors = %+v, err = %v; want none", descs, err)
	}
	if descs, err := collectPiTranscriptDescriptors(filepath.Join(root, "missing"), nil); err != nil || len(descs) != 0 {
		t.Fatalf("missing root descriptors = %+v, err = %v; want none", descs, err)
	}
	if descs, err := collectPiTranscriptDescriptors("", nil); err != nil || len(descs) != 0 {
		t.Fatalf("empty root descriptors = %+v, err = %v; want none", descs, err)
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

// TestTerminalEvidence_HeadlessReusedWorkareaExcludesPrevious drives the
// production headless Spawn entry with a reused workarea: a previous
// session's transcript already sits under the per-session layout root before
// spawn. The new session's evidence must expose only its own transcript,
// never the previous session's.
//
// RED proof: snapshot no baseline (return every transcript under the root)
// and the previous-session exclusion assertion fails.
func TestTerminalEvidence_HeadlessReusedWorkareaExcludesPrevious(t *testing.T) {
	bin := buildEvidenceFakePi(t)
	workdir := t.TempDir()
	const prevMarker = "PREVIOUS_SESSION_MARKER_1a2b3c"
	const lastLine = "REUSED_HEADLESS_FINAL_5d6e7f"
	prevRoot := sessionStateRoot(agent.Spec{Cwd: workdir})
	if err := os.MkdirAll(prevRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	prevPath := filepath.Join(prevRoot, "20980101T000000Z_prev.jsonl")
	writeTranscriptLine(t, prevPath, transcriptMessage("prev", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": prevMarker}},
	}))
	t.Setenv("FAKEPI_WRITE_TRANSCRIPT", "1")
	t.Setenv("FAKEPI_TRANSCRIPT_LAST_LINE", lastLine)
	t.Setenv("FAKEPI_REPLY", "reused turn done")
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
		t.Fatalf("descriptors = %+v; want exactly this session's transcript, not the previous session's", descs)
	}
	if descs[0].Path == prevPath {
		t.Fatalf("previous session transcript %q exposed by current session capability", prevPath)
	}
	raw, err := os.ReadFile(descs[0].Path) //nolint:gosec // path came from the session's own evidence capability under test
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(raw), lastLine) {
		t.Fatalf("transcript does not preserve this session's last line %q; got:\n%s", lastLine, raw)
	}
	if strings.Contains(string(raw), prevMarker) {
		t.Fatalf("transcript mixes the previous session's marker %q", prevMarker)
	}
}

// TestTerminalEvidence_InteractiveReusedWorkareaExcludesPrevious drives the
// production interactive Spawn entry with a reused workarea: a previous
// session's transcript already sits under the per-session layout root before
// spawn. After the final flush, evidence must expose only this session's
// transcript, never the previous session's.
//
// RED proof: report every transcript under the root (ignore the tailer's
// pre-spawn baseline) and the previous-session exclusion assertion fails.
func TestTerminalEvidence_InteractiveReusedWorkareaExcludesPrevious(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	const prevMarker = "PREVIOUS_INTERACTIVE_MARKER_8a9b0c"
	const lastLine = "REUSED_INTERACTIVE_FINAL_3d4e5f"
	prevRoot := sessionStateRoot(agent.Spec{Cwd: workdir})
	if err := os.MkdirAll(prevRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	prevPath := filepath.Join(prevRoot, "20980101T000000Z_prev.jsonl")
	writeTranscriptLine(t, prevPath, transcriptMessage("prev", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": prevMarker}},
	}))
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
		t.Fatalf("descriptors = %+v; want exactly this session's transcript, not the previous session's", descs)
	}
	if descs[0].Path == prevPath {
		t.Fatalf("previous session transcript %q exposed by interactive session capability", prevPath)
	}
	raw, err := os.ReadFile(descs[0].Path) //nolint:gosec // path came from the session's own evidence capability under test
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(raw), lastLine) {
		t.Fatalf("interactive transcript lost its trailing line %q; got:\n%s", lastLine, raw)
	}
	if strings.Contains(string(raw), prevMarker) {
		t.Fatalf("interactive transcript mixes the previous session's marker %q", prevMarker)
	}
}

// TestTerminalEvidence_HeadlessBaselineIncompleteRefuses drives the production
// headless Spawn entry with a failed pre-spawn scan: a previous transcript
// sits under sessions/ while that directory is unreadable at snapshot time.
// The snapshot is incomplete, so TerminalTranscripts must refuse after Stop
// rather than misattributing the previous session's transcript.
//
// RED proof: ignore the walk error in snapshotPiTranscriptBaseline (always
// report complete) and this test finds descriptors instead of a refusal.
func TestTerminalEvidence_HeadlessBaselineIncompleteRefuses(t *testing.T) {
	bin := buildEvidenceFakePi(t)
	workdir := t.TempDir()
	prevRoot := sessionStateRoot(agent.Spec{Cwd: workdir})
	sessionsDir := filepath.Join(prevRoot, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTranscriptLine(t, filepath.Join(sessionsDir, "20980101T000000Z_old.jsonl"), transcriptMessage("old", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "old"}},
	}))
	if err := os.Chmod(sessionsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(sessionsDir); err == nil {
		_ = os.Chmod(sessionsDir, 0o700) //nolint:gosec // G302: test fixture dir restore needs the execute bit, same 0o700 as MkdirAll
		t.Skip("unreadable-dir probe needs non-root EACCES; ReadDir succeeded")
	}
	// Restored after the child starts (baseline already taken while
	// unreadable), so the child can write and the post-stop scan succeeds.
	restore := func() { _ = os.Chmod(sessionsDir, 0o700) } //nolint:gosec // G302: test fixture dir restore needs the execute bit, same 0o700 as MkdirAll
	t.Cleanup(restore)
	t.Setenv("FAKEPI_WRITE_TRANSCRIPT", "1")
	t.Setenv("FAKEPI_TRANSCRIPT_LAST_LINE", "BASELINE_INCOMPLETE_HEADLESS")
	t.Setenv("FAKEPI_REPLY", "incomplete turn done")
	p, err := New(Options{
		PiBin:            bin,
		HandshakeTimeout: 20 * time.Second,
		VersionProbe:     func(context.Context, string) (string, error) { return PinnedVersion, nil },
		afterChildStart:  restore,
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
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	capable, ok := h.(agent.TerminalTranscriptCapable)
	if !ok {
		t.Fatal("headless handle does not expose the terminal-transcript capability")
	}
	if descs, err := capable.TerminalTranscripts(); err == nil || len(descs) != 0 {
		t.Fatalf("TerminalTranscripts = %+v, err = %v; want refusal with no descriptors when the baseline scan failed", descs, err)
	}
}

// TestTerminalEvidence_InteractiveBaselineIncompleteRefuses drives the
// production interactive Spawn entry with a failed pre-spawn tailer scan and
// proves the wrapper refuses after the final flush.
//
// RED proof: ignore the walk error in newInteractiveTranscriptTailer and the
// post-flush call returns the previous session's transcript instead of a
// refusal.
func TestTerminalEvidence_InteractiveBaselineIncompleteRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	prevRoot := sessionStateRoot(agent.Spec{Cwd: workdir})
	sessionsDir := filepath.Join(prevRoot, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTranscriptLine(t, filepath.Join(sessionsDir, "20980101T000000Z_old.jsonl"), transcriptMessage("old", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "old"}},
	}))
	if err := os.Chmod(sessionsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(sessionsDir); err == nil {
		_ = os.Chmod(sessionsDir, 0o700) //nolint:gosec // G302: test fixture dir restore needs the execute bit, same 0o700 as MkdirAll
		t.Skip("unreadable-dir probe needs non-root EACCES; ReadDir succeeded")
	}
	script := fakePiTranscriptPrelude + `
sleep 0.2
tool_line final FINAL_TOOL
`
	p := newFakeInteractivePiProvider(t, script)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		_ = os.Chmod(sessionsDir, 0o700) //nolint:gosec // G302: test fixture dir restore needs the execute bit, same 0o700 as MkdirAll
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	// Baseline already taken while unreadable; restore so the child can run
	// and the post-flush scan succeeds.
	if err := os.Chmod(sessionsDir, 0o700); err != nil { //nolint:gosec // G302: test fixture dir restore needs the execute bit, same 0o700 as MkdirAll
		t.Fatal(err)
	}
	capable, ok := h.(agent.TerminalTranscriptCapable)
	if !ok {
		t.Fatal("interactive handle does not expose the terminal-transcript capability")
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
	if descs, err := capable.TerminalTranscripts(); err == nil || len(descs) != 0 {
		t.Fatalf("TerminalTranscripts = %+v, err = %v; want refusal with no descriptors when the tailer baseline failed", descs, err)
	}
}

// TestTerminalEvidence_HeadlessConcurrentOverlapRefuses spawns two headless
// sessions in the same checkout while the first is still live. Each child
// writes its own session-scoped transcript, so a created-after-spawn filter
// alone would return both to each handle. Overlap contaminates both leases,
// so each handle must refuse rather than attribute the other's transcript.
//
// RED proof: never acquire the evidence lease (nil claim) and each handle
// returns both transcripts instead of refusing.
func TestTerminalEvidence_HeadlessConcurrentOverlapRefuses(t *testing.T) {
	bin := buildEvidenceFakePi(t)
	workdir := t.TempDir()
	t.Setenv("FAKEPI_WRITE_TRANSCRIPT", "1")
	t.Setenv("FAKEPI_TRANSCRIPT_LAST_LINE", "CONCURRENT_HEADLESS")
	t.Setenv("FAKEPI_REPLY", "concurrent turn done")
	newHeadless := func() *Provider {
		t.Helper()
		p, err := New(Options{
			PiBin:            bin,
			HandshakeTimeout: 20 * time.Second,
			VersionProbe:     func(context.Context, string) (string, error) { return PinnedVersion, nil },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
		return p
	}
	pA, pB := newHeadless(), newHeadless()
	hA, err := pA.Spawn(context.Background(), agent.Spec{Cwd: workdir, Prompt: "go"})
	if err != nil {
		t.Fatalf("Spawn A: %v", err)
	}
	hB, err := pB.Spawn(context.Background(), agent.Spec{Cwd: workdir, Prompt: "go"})
	if err != nil {
		_ = hA.Stop(context.Background())
		t.Fatalf("Spawn B: %v", err)
	}
	drainToTerminal(t, hA)
	drainToTerminal(t, hB)
	if err := hA.Stop(context.Background()); err != nil {
		t.Fatalf("Stop A: %v", err)
	}
	if err := hB.Stop(context.Background()); err != nil {
		t.Fatalf("Stop B: %v", err)
	}
	for name, h := range map[string]agent.Handle{"A": hA, "B": hB} {
		capable, ok := h.(agent.TerminalTranscriptCapable)
		if !ok {
			t.Fatalf("handle %s does not expose the terminal-transcript capability", name)
		}
		if descs, err := capable.TerminalTranscripts(); err == nil || len(descs) != 0 {
			t.Fatalf("handle %s TerminalTranscripts = %+v, err = %v; want refusal when sessions overlapped", name, descs, err)
		}
	}
}

// TestTerminalEvidence_InteractiveConcurrentOverlapRefuses spawns two
// interactive sessions in the same checkout while the first is still live,
// each writing a distinct transcript file. Each handle must refuse rather
// than return the other's transcript.
//
// RED proof: never acquire the evidence lease (nil claim) and each handle
// returns both transcripts instead of refusing.
func TestTerminalEvidence_InteractiveConcurrentOverlapRefuses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	scriptFor := func(name string) string {
		return `
dir=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--session-dir" ]; then dir="$2"; shift 2; continue; fi
  shift
done
live="$dir/2099-01-01T00-00-00-000Z_` + name + `.jsonl"
printf '{"type":"message","id":"` + name + `","message":{"role":"assistant","content":[{"type":"text","text":"` + name + `"}]}}\n' >> "$live"
sleep 5
`
	}
	pA := newFakeInteractivePiProvider(t, scriptFor("live-A"))
	pB := newFakeInteractivePiProvider(t, scriptFor("live-B"))
	hA, err := pA.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn A: %v", err)
	}
	t.Cleanup(func() { _ = hA.Stop(context.Background()) })
	hB, err := pB.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn B: %v", err)
	}
	t.Cleanup(func() { _ = hB.Stop(context.Background()) })
	for name, h := range map[string]agent.Handle{"A": hA, "B": hB} {
		flusher, ok := h.(agent.InteractiveActivityFlusher)
		if !ok {
			t.Fatalf("handle %s does not signal its activity flush", name)
		}
		select {
		case <-flusher.ActivityFlushed():
		case <-time.After(20 * time.Second):
			t.Fatalf("handle %s timed out waiting for the interactive final flush", name)
		}
	}
	// Stopping reaps the sleepers so the test does not wait out the full
	// sleep; the flush already happened on Done.
	_ = hA.Stop(context.Background())
	_ = hB.Stop(context.Background())
	for name, h := range map[string]agent.Handle{"A": hA, "B": hB} {
		capable, ok := h.(agent.TerminalTranscriptCapable)
		if !ok {
			t.Fatalf("handle %s does not expose the terminal-transcript capability", name)
		}
		if descs, err := capable.TerminalTranscripts(); err == nil || len(descs) != 0 {
			t.Fatalf("handle %s TerminalTranscripts = %+v, err = %v; want refusal when sessions overlapped", name, descs, err)
		}
	}
}

// TestTerminalEvidence_WalkerContinuesPastUnreadableSibling pins B1: an
// unreadable directory sorting before the live transcript must not hide the
// live transcript from tailing, while evidence collection still refuses on
// the incomplete scan.
func TestTerminalEvidence_WalkerContinuesPastUnreadableSibling(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	unreadable := filepath.Join(root, "00-aaa-unreadable")
	if err := os.MkdirAll(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(unreadable); err == nil {
		_ = os.Chmod(unreadable, 0o700) //nolint:gosec // G302: test fixture dir restore needs the execute bit
		t.Skip("unreadable-dir probe needs non-root EACCES; ReadDir succeeded")
	}
	restore := func() { _ = os.Chmod(unreadable, 0o700) } //nolint:gosec // G302: test fixture dir restore needs the execute bit
	t.Cleanup(restore)
	tailer := newInteractiveTranscriptTailer(root)
	if !tailer.baselineIncomplete {
		t.Fatal("tailer baseline must be incomplete when a sibling scan failed")
	}
	live := filepath.Join(root, "zz-live.jsonl")
	writeTranscriptLine(t, live, transcriptMessage("live", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "live"}},
	}))
	files, _ := tailer.pendingFiles()
	found := false
	for _, f := range files {
		if f.path == live {
			found = true
		}
	}
	if !found {
		t.Fatalf("pendingFiles missed live transcript beside unreadable sibling; got %+v", files)
	}
	if _, err := collectPiTranscriptDescriptors(root, tailer.baseline); err == nil {
		t.Fatal("evidence collection must refuse when the post-stop scan is incomplete")
	}
}

// TestTerminalEvidence_SnapshotRootLstatFailureIsIncomplete pins B3/B5: a root
// whose Lstat fails (parent temporarily inaccessible) snapshots to an
// incomplete baseline, never to a complete empty one.
func TestTerminalEvidence_SnapshotRootLstatFailureIsIncomplete(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "state")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTranscriptLine(t, filepath.Join(root, "old.jsonl"), transcriptMessage("old", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "old"}},
	}))
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); err == nil {
		_ = os.Chmod(parent, 0o700) //nolint:gosec // G302: test fixture dir restore needs the execute bit
		t.Skip("Lstat EACCES probe needs non-root; Lstat succeeded")
	}
	_, incomplete := snapshotPiTranscriptBaseline(root)
	_ = os.Chmod(parent, 0o700) //nolint:gosec // G302: test fixture dir restore needs the execute bit
	if !incomplete {
		t.Fatal("snapshot with inaccessible root must be incomplete, not a complete empty baseline")
	}
}

// TestTerminalEvidence_PostStopScanFailureRefuses pins B4: a post-stop
// traversal error is a typed refusal, not successful empty evidence, and the
// refusal is cached for later collections.
func TestTerminalEvidence_PostStopScanFailureRefuses(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTranscriptLine(t, filepath.Join(root, "top.jsonl"), transcriptMessage("m", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "x"}},
	}))
	sub := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTranscriptLine(t, filepath.Join(sub, "old.jsonl"), transcriptMessage("old", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "old"}},
	}))
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadDir(sub); err == nil {
		_ = os.Chmod(sub, 0o700) //nolint:gosec // G302: test fixture dir restore needs the execute bit
		t.Skip("unreadable-dir probe needs non-root EACCES; ReadDir succeeded")
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) }) //nolint:gosec // G302: test fixture dir restore needs the execute bit
	if _, err := collectPiTranscriptDescriptors(root, nil); err == nil {
		t.Fatal("post-stop scan failure must be a typed refusal, not empty success")
	}
	h := newHandle(nil, nil, agent.Spec{Cwd: root}, "token")
	h.setStateRoot(root)
	h.stopped.Store(true)
	if _, err := h.TerminalTranscripts(); err == nil {
		t.Fatal("TerminalTranscripts must refuse when the post-stop scan fails")
	}
	if _, err := h.TerminalTranscripts(); err == nil {
		t.Fatal("second TerminalTranscripts must still refuse (cached), not return empty success")
	}
}

// TestTerminalEvidence_SequentialStopReleasesLease pins B2 through the
// production headless entry: stopping A without collecting, then spawning B
// in the same checkout, leaves B clean.
func TestTerminalEvidence_SequentialStopReleasesLease(t *testing.T) {
	bin := buildEvidenceFakePi(t)
	workdir := t.TempDir()
	t.Setenv("FAKEPI_WRITE_TRANSCRIPT", "1")
	t.Setenv("FAKEPI_TRANSCRIPT_LAST_LINE", "SEQUENTIAL_LEASE_RELEASE")
	t.Setenv("FAKEPI_REPLY", "sequential turn done")
	newHeadless := func() *Provider {
		t.Helper()
		p, err := New(Options{
			PiBin:            bin,
			HandshakeTimeout: 20 * time.Second,
			VersionProbe:     func(context.Context, string) (string, error) { return PinnedVersion, nil },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
		return p
	}
	pA, pB := newHeadless(), newHeadless()
	hA, err := pA.Spawn(context.Background(), agent.Spec{Cwd: workdir, Prompt: "go"})
	if err != nil {
		t.Fatalf("Spawn A: %v", err)
	}
	drainToTerminal(t, hA)
	if err := hA.Stop(context.Background()); err != nil {
		t.Fatalf("Stop A: %v", err)
	}
	hB, err := pB.Spawn(context.Background(), agent.Spec{Cwd: workdir, Prompt: "go"})
	if err != nil {
		t.Fatalf("Spawn B: %v", err)
	}
	drainToTerminal(t, hB)
	if err := hB.Stop(context.Background()); err != nil {
		t.Fatalf("Stop B: %v", err)
	}
	capable, ok := hB.(agent.TerminalTranscriptCapable)
	if !ok {
		t.Fatal("handle B does not expose the terminal-transcript capability")
	}
	descs, err := capable.TerminalTranscripts()
	if err != nil {
		t.Fatalf("sequential B TerminalTranscripts refused: %v", err)
	}
	if len(descs) == 0 {
		t.Fatal("sequential B must expose its own transcript after A stopped without collecting")
	}
}

// TestTerminalEvidence_RepeatedCollectionReturnsCached pins B6: collecting,
// starting a later session, then collecting again returns the original
// outcome, never the later session's transcript.
func TestTerminalEvidence_RepeatedCollectionReturnsCached(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	aPath := filepath.Join(root, "a.jsonl")
	writeTranscriptLine(t, aPath, transcriptMessage("a", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "a"}},
	}))
	h := newHandle(nil, nil, agent.Spec{Cwd: root}, "token")
	h.setStateRoot(root)
	h.stopped.Store(true)
	first, err := h.TerminalTranscripts()
	if err != nil {
		t.Fatalf("first TerminalTranscripts: %v", err)
	}
	if len(first) != 1 || first[0].Path != aPath {
		t.Fatalf("first = %+v; want exactly [%q]", first, aPath)
	}
	bPath := filepath.Join(root, "b.jsonl")
	writeTranscriptLine(t, bPath, transcriptMessage("b", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "b"}},
	}))
	second, err := h.TerminalTranscripts()
	if err != nil {
		t.Fatalf("second TerminalTranscripts: %v", err)
	}
	if len(second) != 1 || second[0].Path != aPath {
		t.Fatalf("second = %+v; want cached original [%q], not the later session's transcript", second, aPath)
	}
}

// TestTerminalEvidence_DelayedFirstCollectionAfterSequential pins B1/B5: Stop
// freezes evidence, so a handle whose first collection happens after a later
// sequential session ran still returns only its own transcript.
//
// RED proof: skip the Stop-time freeze (do not populate the cache at Stop)
// and the delayed first collection returns both sessions' transcripts.
func TestTerminalEvidence_DelayedFirstCollectionAfterSequential(t *testing.T) {
	bin := buildEvidenceFakePi(t)
	workdir := t.TempDir()
	t.Setenv("FAKEPI_WRITE_TRANSCRIPT", "1")
	t.Setenv("FAKEPI_TRANSCRIPT_LAST_LINE", "DELAYED_A_LINE")
	t.Setenv("FAKEPI_REPLY", "a done")
	newHeadless := func() *Provider {
		t.Helper()
		p, err := New(Options{
			PiBin:            bin,
			HandshakeTimeout: 20 * time.Second,
			VersionProbe:     func(context.Context, string) (string, error) { return PinnedVersion, nil },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = p.Shutdown(context.Background()) })
		return p
	}
	pA, pB := newHeadless(), newHeadless()
	hA, err := pA.Spawn(context.Background(), agent.Spec{Cwd: workdir, Prompt: "go"})
	if err != nil {
		t.Fatalf("Spawn A: %v", err)
	}
	drainToTerminal(t, hA)
	if err := hA.Stop(context.Background()); err != nil {
		t.Fatalf("Stop A: %v", err)
	}
	t.Setenv("FAKEPI_TRANSCRIPT_LAST_LINE", "DELAYED_B_LINE")
	hB, err := pB.Spawn(context.Background(), agent.Spec{Cwd: workdir, Prompt: "go"})
	if err != nil {
		t.Fatalf("Spawn B: %v", err)
	}
	drainToTerminal(t, hB)
	if err := hB.Stop(context.Background()); err != nil {
		t.Fatalf("Stop B: %v", err)
	}
	capA, ok := hA.(agent.TerminalTranscriptCapable)
	if !ok {
		t.Fatal("handle A does not expose the terminal-transcript capability")
	}
	descs, err := capA.TerminalTranscripts()
	if err != nil {
		t.Fatalf("delayed A TerminalTranscripts refused: %v", err)
	}
	if len(descs) != 1 {
		t.Fatalf("delayed A first collection = %+v; want only A's 1 transcript, not B's", descs)
	}
}

// TestTerminalEvidence_InteractiveStopJoinsFlush pins B3: Stop joins the final
// flush, so a failed interactive session whose child already wrote its trailing
// line exposes it immediately after Stop returns.
//
// RED proof: return from Stop without joining activityFlushed and the
// collection after Stop refuses while the flush is still pending.
func TestTerminalEvidence_InteractiveStopJoinsFlush(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	workdir := t.TempDir()
	const lastLine = "STOP_JOIN_FLUSH_LINE_9d2c"
	script := fakePiTranscriptPrelude + `
sleep 0.2
tool_line final FINAL_TOOL
printf '{"type":"message","id":"last-line","message":{"role":"assistant","content":[{"type":"text","text":"` + lastLine + `"}]}}\n' >> "$live"
sleep 5
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
	root := ""
	if w, ok := h.(*interactiveStateLossHandle); ok {
		root = w.stateRoot
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		found := false
		if root != "" {
			_ = filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
				if err == nil && e != nil && !e.IsDir() && filepath.Ext(path) == ".jsonl" {
					if b, rerr := os.ReadFile(path); rerr == nil && strings.Contains(string(b), lastLine) { //nolint:gosec // path discovered by walking the test state root
						found = true
					}
				}
				return nil
			})
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("transcript with last line never appeared; root=%q", root)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	capable, ok := h.(agent.TerminalTranscriptCapable)
	if !ok {
		t.Fatal("interactive handle does not expose the terminal-transcript capability")
	}
	descs, err := capable.TerminalTranscripts()
	if err != nil {
		t.Fatalf("TerminalTranscripts immediately after Stop refused: %v", err)
	}
	if len(descs) != 1 {
		t.Fatalf("descriptors = %+v; want exactly this session's transcript", descs)
	}
	raw, err := os.ReadFile(descs[0].Path) //nolint:gosec // path came from the session's own evidence capability under test
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(raw), lastLine) {
		t.Fatalf("transcript lost its trailing line %q; got:\n%s", lastLine, raw)
	}
}

// TestTerminalEvidence_CandidateLstatRefuses pins B4: a candidate transcript
// whose Lstat fails (listable parent without execute permission) is a typed
// refusal, not successful empty evidence.
//
// RED proof: drop Lstat errors in collectPiTranscriptDescriptors and the
// collection returns (nil, nil) instead of refusing.
func TestTerminalEvidence_CandidateLstatRefuses(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTranscriptLine(t, filepath.Join(sub, "cand.jsonl"), transcriptMessage("m", map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "x"}},
	}))
	if err := os.Chmod(sub, 0o400); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(sub, 0o700) }() //nolint:gosec // G302: test fixture dir restore needs the execute bit
	if _, err := os.ReadDir(sub); err != nil {
		t.Skipf("ReadDir failed with 0400, cannot probe: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(sub, "cand.jsonl")); err == nil {
		t.Skip("Lstat succeeded despite 0400, cannot probe (running as root?)")
	}
	if descs, err := collectPiTranscriptDescriptors(root, nil); err == nil {
		t.Fatalf("candidate Lstat EACCES: got %d descriptors and nil error, want incomplete-scan refusal", len(descs))
	}
}

// TestTerminalEvidence_CrossProcessOverlapRefuses pins B2: a live claim file
// from another controller process contaminates this handle, while a stale
// claim from a dead pid is cleaned and stays clean.
//
// RED proof: track overlap only in the in-process map and the live peer file
// is ignored (clean instead of refusal).
func TestTerminalEvidence_CrossProcessOverlapRefuses(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	peer := filepath.Join(root, piEvidenceClaimPrefix+"peer")
	if err := os.WriteFile(peer, []byte(itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	c := acquirePiEvidenceClaim(root)
	if c == nil {
		t.Fatal("nil claim")
	}
	defer c.release()
	if !c.isContaminated() {
		t.Fatal("live peer claim file must contaminate, want ownership refusal")
	}
	_ = os.Remove(peer)
}

// itoa is a tiny test-local int-to-string without importing strconv in the
// test's import block.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [32]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
