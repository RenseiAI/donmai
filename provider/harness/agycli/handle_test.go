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
	// Write WITHOUT the exec bit, then chmod-add it after close.
	// Linux can throw ETXTBSY on fork+exec when a writable FD is open on an
	// executable inode — writing 0o600 then chmodding to 0o755 post-close
	// means the file never carries the exec bit while any writable FD exists.
	if err := os.WriteFile(bin, []byte("#!/bin/bash\n"+script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // test fixture needs exec bit
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

// spawnFake retries the Linux ETXTBSY race that can occur when a concurrently
// forking sibling test inherits the recently closed write descriptor for the
// executable fixture. The policy matches the bounded fixture retry used by the
// Amp harness tests.
func spawnFake(ctx context.Context, t *testing.T, p *Provider, spec agent.Spec) (agent.Handle, error) {
	t.Helper()
	for attempt := 0; ; attempt++ {
		h, err := p.Spawn(ctx, spec)
		if err == nil || attempt >= 3 || !strings.Contains(err.Error(), "text file busy") {
			return h, err
		}
		time.Sleep(time.Duration(25*(attempt+1)) * time.Millisecond)
	}
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
	h, err := spawnFake(context.Background(), t, p, agent.Spec{Prompt: "task", Cwd: t.TempDir()})
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
	h, err := spawnFake(context.Background(), t, p, agent.Spec{Prompt: "x", Cwd: t.TempDir()})
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
	h, err := spawnFake(context.Background(), t, p, agent.Spec{Prompt: "x", Cwd: t.TempDir()})
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
	h, err := spawnFake(context.Background(), t, p, agent.Spec{Prompt: "x", Cwd: t.TempDir()})
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

// TestSpawn_TranscriptLiveStreaming proves tool structure streams DURING the
// run, not as an after-exit replay: the fake agy writes a tool-call line,
// then blocks until a flag file appears. The test creates the flag only
// AFTER observing the live ToolUseEvent — under the old EOF-replay contract
// the event could never arrive while the process was alive, and this test
// would deadlock into its timeout.
func TestSpawn_TranscriptLiveStreaming(t *testing.T) {
	t.Parallel()
	stateHome := t.TempDir()
	flag := filepath.Join(t.TempDir(), "release")
	convDir := filepath.Join(stateHome, "antigravity-cli", "brain", "live-conv", ".system_generated", "logs")
	transcript := filepath.Join(convDir, "transcript.jsonl")

	script := "mkdir -p '" + convDir + "'\n" +
		"printf '%s\\n' '" + tailPlannerLine + "' > '" + transcript + "'\n" +
		"echo working\n" +
		"while [ ! -f '" + flag + "' ]; do sleep 0.05; done\n" +
		"printf '%s\\n' '" + tailResultLine + "' >> '" + transcript + "'\n" +
		"echo done\n"

	p := newFakeProvider(t, script, Options{StateHome: stateHome})
	h, err := spawnFake(context.Background(), t, p, agent.Spec{Prompt: "x", Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Stop(context.Background()) }()

	deadline := time.After(20 * time.Second)
	var evs []agent.Event
	released := false
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatalf("events channel closed before terminal ResultEvent: %#v", evs)
			}
			evs = append(evs, ev)
			if _, isUse := ev.(agent.ToolUseEvent); isUse && !released {
				// Live ToolUse observed while the subprocess is still
				// blocked on the flag file → tailing streamed it mid-run.
				// The conv-id must already be discovered at this point.
				if h.SessionID() != "live-conv" {
					t.Errorf("SessionID during run = %q, want live-conv (corrective InitEvent should precede tool events)", h.SessionID())
				}
				if err := os.WriteFile(flag, nil, 0o600); err != nil { //nolint:gosec // test flag file
					t.Fatal(err)
				}
				released = true
			}
			if _, terminal := ev.(agent.ResultEvent); terminal {
				goto drained
			}
		case <-deadline:
			t.Fatalf("timed out waiting for live-streamed events (EOF-replay regression?); got %#v", evs)
		}
	}
drained:
	if !released {
		t.Fatal("never observed a live ToolUseEvent")
	}
	if n := len(eventsOfKind[agent.ToolUseEvent](evs)); n != 1 {
		t.Errorf("ToolUseEvents = %d, want exactly 1 (no duplicate EOF replay)", n)
	}
	results := eventsOfKind[agent.ToolResultEvent](evs)
	if len(results) != 1 || !strings.Contains(results[0].Content, "notes body") {
		t.Errorf("want exactly one ToolResultEvent carrying the tool output, got %#v", results)
	}
	inits := eventsOfKind[agent.InitEvent](evs)
	if len(inits) != 2 || inits[1].SessionID != "live-conv" {
		t.Errorf("want initial + corrective InitEvent (live-conv), got %#v", inits)
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
	h, err := spawnFake(context.Background(), t, p, agent.Spec{Prompt: "x", Cwd: cwd})
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
	h, err := spawnFake(context.Background(), t, p, agent.Spec{Prompt: "x", Cwd: t.TempDir()})
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
