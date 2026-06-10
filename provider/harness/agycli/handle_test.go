package agycli

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

// newFakeProvider builds a Provider whose binary is a fake-agy script. The
// script is run under a real pty by the handle, so this exercises the genuine
// pty lifecycle without consuming agy's OAuth quota.
func newFakeProvider(t *testing.T, script string, opts Options) *Provider {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-agy")
	if err := os.WriteFile(bin, []byte("#!/bin/bash\n"+script), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	opts.Binary = bin
	opts.LookPath = func(string) (string, error) { return bin, nil }
	if opts.StateHome == "" {
		opts.StateHome = t.TempDir()
	}
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New(fake): %v", err)
	}
	return p
}

// collectEvents drains a handle until a terminal ResultEvent, channel close, or
// timeout.
func collectEvents(t *testing.T, h agent.Handle) []agent.Event {
	t.Helper()
	var out []agent.Event
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
			if _, terminal := ev.(agent.ResultEvent); terminal {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out collecting events; got %d so far", len(out))
			return out
		}
	}
}

func eventsOfKind[T agent.Event](evs []agent.Event) []T {
	var out []T
	for _, e := range evs {
		if t, ok := e.(T); ok {
			out = append(out, t)
		}
	}
	return out
}

func TestSpawn_StdoutSpine_PassedEnvelope(t *testing.T) {
	t.Parallel()
	script := `
echo "I will read the file."
echo '<<<DONMAI_RESULT>>>'
echo '{"status":"passed","summary":"did it"}'
echo '<<<END_DONMAI_RESULT>>>'
echo "WORK_RESULT:passed"
`
	p := newFakeProvider(t, script, Options{DisableTranscriptEnrichment: true})
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "task", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	evs := collectEvents(t, h)

	if len(eventsOfKind[agent.InitEvent](evs)) != 1 {
		t.Errorf("expected exactly one InitEvent: %#v", evs)
	}
	texts := eventsOfKind[agent.AssistantTextEvent](evs)
	joined := ""
	for _, te := range texts {
		joined += te.Text + "\n"
	}
	if !strings.Contains(joined, "I will read the file.") {
		t.Errorf("narration not emitted as assistant text: %q", joined)
	}
	if !strings.Contains(joined, "WORK_RESULT:passed") {
		t.Errorf("WORK_RESULT marker not present in assistant text stream (runner scans this): %q", joined)
	}
	// Envelope lines are retained for buildResult but must NOT surface as
	// assistant text — raw envelope JSON would render as thought spam.
	for _, frag := range []string{resultEnvelopeBegin, resultEnvelopeEnd, `"status":"passed"`} {
		if strings.Contains(joined, frag) {
			t.Errorf("envelope fragment %q leaked into assistant text stream: %q", frag, joined)
		}
	}

	results := eventsOfKind[agent.ResultEvent](evs)
	if len(results) != 1 {
		t.Fatalf("expected one terminal ResultEvent, got %d", len(results))
	}
	if !results[0].Success {
		t.Errorf("expected Success=true, got %#v", results[0])
	}
	if results[0].Message != "did it" {
		t.Errorf("expected envelope summary as Message, got %q", results[0].Message)
	}
}

func TestSpawn_EnvelopeFailed(t *testing.T) {
	t.Parallel()
	script := `
echo '<<<DONMAI_RESULT>>>'
echo '{"status":"failed","summary":"could not"}'
echo '<<<END_DONMAI_RESULT>>>'
`
	p := newFakeProvider(t, script, Options{DisableTranscriptEnrichment: true})
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "x", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	results := eventsOfKind[agent.ResultEvent](collectEvents(t, h))
	if len(results) != 1 || results[0].Success {
		t.Fatalf("expected one failed ResultEvent, got %#v", results)
	}
}

func TestSpawn_NoOutputNonzeroExit(t *testing.T) {
	t.Parallel()
	p := newFakeProvider(t, "exit 3\n", Options{DisableTranscriptEnrichment: true})
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "x", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	results := eventsOfKind[agent.ResultEvent](collectEvents(t, h))
	if len(results) != 1 {
		t.Fatalf("expected one ResultEvent, got %#v", results)
	}
	if results[0].Success || results[0].ErrorSubtype != "no_output" {
		t.Errorf("expected failed no_output result, got %#v", results[0])
	}
}

func TestSpawn_TranscriptEnrichment(t *testing.T) {
	t.Parallel()
	// StateHome with a fixture transcript that will be discovered as the single
	// fresh conversation created during the run.
	stateHome := t.TempDir()
	fixture, err := os.ReadFile(filepath.Join("testdata", "transcript_sample.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// The fake script writes the transcript itself (simulating agy) so it
	// appears AFTER the pre-spawn snapshot.
	convDir := filepath.Join(stateHome, "antigravity-cli", "brain", "fresh-conv", ".system_generated", "logs")
	script := "mkdir -p '" + convDir + "'\n" +
		"cat > '" + filepath.Join(convDir, "transcript.jsonl") + "' <<'EOF'\n" + string(fixture) + "\nEOF\n" +
		"echo done\n"

	p := newFakeProvider(t, script, Options{StateHome: stateHome})
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "x", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	evs := collectEvents(t, h)
	if len(eventsOfKind[agent.ToolUseEvent](evs)) == 0 {
		t.Errorf("expected transcript enrichment to emit ToolUseEvents: %#v", evs)
	}
	if h.SessionID() != "fresh-conv" {
		t.Errorf("SessionID = %q, want fresh-conv (discovered conv-id)", h.SessionID())
	}
}

func TestSpawn_TrustWorkspaceOptIn(t *testing.T) {
	t.Parallel()
	stateHome := t.TempDir()
	cwd := t.TempDir()
	p := newFakeProvider(t, "echo hi\n", Options{
		StateHome:                   stateHome,
		TrustWorkspace:              true,
		DisableTranscriptEnrichment: true,
	})
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "x", Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	collectEvents(t, h)

	// With TrustWorkspace=true the cwd must have been written to settings.json.
	data, err := os.ReadFile(filepath.Join(stateHome, "antigravity-cli", "settings.json"))
	if err != nil {
		t.Fatalf("expected settings.json written when TrustWorkspace=true: %v", err)
	}
	if !strings.Contains(string(data), cwd) {
		t.Errorf("trusted cwd not written: %s", data)
	}
}

func TestStop_Idempotent(t *testing.T) {
	t.Parallel()
	// A script that would run a while; Stop should terminate it promptly.
	p := newFakeProvider(t, "sleep 30\n", Options{DisableTranscriptEnrichment: true})
	h, err := p.Spawn(context.Background(), agent.Spec{Prompt: "x", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Errorf("Stop #1: %v", err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Errorf("Stop #2 (idempotent): %v", err)
	}
	// Events channel must close after Stop.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return // closed — good
			}
		case <-deadline:
			t.Fatal("events channel did not close after Stop")
		}
	}
}

func TestInject_Unsupported(t *testing.T) {
	t.Parallel()
	h := &Handle{}
	if err := h.Inject(context.Background(), "msg"); err == nil {
		t.Error("Inject should be unsupported")
	}
}

func TestSanitizeLine(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plain":                   "plain",
		"trailing cr\r":           "trailing cr",
		"\x1b[31mred\x1b[0m text": "red text",
		"\x1b[1;33mbold\x1b[0m\r": "bold",
	}
	for in, want := range cases {
		if got := sanitizeLine(in); got != want {
			t.Errorf("sanitizeLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCappedBuffer_FrontDrop(t *testing.T) {
	t.Parallel()
	b := newCappedBuffer(10)
	b.WriteString("123456")
	b.WriteString("7890ABCDE") // total 15 > cap 10 → keep last 10
	got := b.String()
	if len(got) != 10 || !strings.HasSuffix(got, "0ABCDE") {
		t.Errorf("capped buffer = %q (len %d), want last 10 bytes", got, len(got))
	}
}
