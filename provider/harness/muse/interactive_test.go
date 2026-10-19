package muse

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func TestInteractiveArgs(t *testing.T) {
	t.Parallel()
	p := &Provider{}
	tests := []struct {
		name string
		spec agent.Spec
		want []string
	}{
		{
			name: "empty spec starts a bare TUI",
			spec: agent.Spec{},
			want: nil,
		},
		{
			name: "non-empty prompt seeds the TUI positionally",
			spec: agent.Spec{Prompt: "fix the bug"},
			want: []string{"fix the bug"},
		},
		{
			name: "read-only lane carries the serve lane's sandbox flags",
			spec: agent.Spec{Prompt: "fix the bug", SandboxLevel: agent.SandboxReadOnly},
			want: []string{"--disable-shell", "--disable-write", "fix the bug"},
		},
		{
			name: "autonomous selects never-ask approval before the prompt",
			spec: agent.Spec{Prompt: "fix the bug", Autonomous: true},
			want: []string{"--approval-mode", "never", "fix the bug"},
		},
		{
			name: "autonomous with empty prompt is still a valid flag-only argv",
			spec: agent.Spec{Autonomous: true},
			want: []string{"--approval-mode", "never"},
		},
		{
			name: "explicit model rides --model",
			spec: agent.Spec{Prompt: "fix the bug", Model: "muse-spark-1.3"},
			want: []string{"--model", "muse-spark-1.3", "fix the bug"},
		},
		{
			name: "default model sentinel emits no flag",
			spec: agent.Spec{Prompt: "fix the bug", Model: "default"},
			want: []string{"fix the bug"},
		},
		{
			name: "effort rides --reasoning-effort in the host wire spelling",
			spec: agent.Spec{Prompt: "fix the bug", Effort: agent.EffortHigh},
			want: []string{"--reasoning-effort", "high", "fix the bug"},
		},
		{
			name: "unset effort emits no flag",
			spec: agent.Spec{Prompt: "fix the bug"},
			want: []string{"fix the bug"},
		},
		{
			name: "system append and initial context prepend the seed in order",
			spec: agent.Spec{Prompt: "fix the bug", SystemPromptAppend: "follow local conventions", InitialContext: "recall: green build"},
			want: []string{"follow local conventions\n\nrecall: green build\n\nfix the bug"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := p.interactiveArgs(tt.spec)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("interactiveArgs(%+v) = %q, want %q", tt.spec, got, tt.want)
			}
		})
	}
}

// TestInteractiveArgs_ApprovalParityWithHeadless pins that the autonomous
// posture does not diverge between spawn modes: headless resolves a
// policy-free autonomous session to allowAll on the wire, and the
// interactive argv must carry its never-ask equivalent — never the CLI
// default. A non-autonomous session keeps the CLI default on both lanes.
func TestInteractiveArgs_ApprovalParityWithHeadless(t *testing.T) {
	t.Parallel()
	p := &Provider{}

	t.Run("autonomous", func(t *testing.T) {
		t.Parallel()
		spec := agent.Spec{Prompt: "ship it", Autonomous: true}
		if got := resolveApprovalMode(spec); got != ApprovalModeAllowAll {
			t.Fatalf("headless resolveApprovalMode(autonomous) = %q, want allowAll", got)
		}
		got := p.interactiveArgs(spec)
		i := slices.Index(got, "--approval-mode")
		if i < 0 || i+1 >= len(got) || got[i+1] != "never" {
			t.Fatalf("interactive argv missing --approval-mode never for an autonomous spec: %q", got)
		}
	})

	t.Run("not autonomous", func(t *testing.T) {
		t.Parallel()
		spec := agent.Spec{Prompt: "ship it"}
		if got := resolveApprovalMode(spec); got != ApprovalModePromptUnmatched {
			t.Fatalf("headless resolveApprovalMode = %q, want promptUnmatched", got)
		}
		if got := p.interactiveArgs(spec); slices.Contains(got, "--approval-mode") {
			t.Fatalf("non-autonomous interactive argv must keep the CLI default, got %q", got)
		}
	})
}

// TestInteractiveArgs_SandboxParityWithServeArgs pins the read-only lane
// byte-identical to serveArgs': the OS boundary composes outside whatever
// the host enforces itself, so the interactive argv must carry the same
// lane flags the serve argv does — and a provider-level DisableSandbox
// must never widen a read-only spec.
func TestInteractiveArgs_SandboxParityWithServeArgs(t *testing.T) {
	t.Parallel()

	t.Run("read-only", func(t *testing.T) {
		t.Parallel()
		spec := agent.Spec{Prompt: "x", SandboxLevel: agent.SandboxReadOnly}
		var serve []string
		for _, flag := range serveArgs(spec, false, false) {
			if flag != "serve" {
				serve = append(serve, flag)
			}
		}
		if !slices.Equal(serve, []string{"--disable-shell", "--disable-write"}) {
			t.Fatalf("serveArgs read-only lane = %q, want the shell+write pair", serve)
		}
		p := &Provider{}
		got := p.interactiveArgs(spec)
		if !slices.Contains(got, "--disable-shell") || !slices.Contains(got, "--disable-write") {
			t.Fatalf("interactive argv missing the read-only pair: %q", got)
		}
	})

	t.Run("disable-sandbox never widens read-only", func(t *testing.T) {
		t.Parallel()
		p := &Provider{opts: Options{DisableSandbox: true}}
		got := p.interactiveArgs(agent.Spec{Prompt: "x", SandboxLevel: agent.SandboxReadOnly})
		if slices.Contains(got, "--disable-sandbox") {
			t.Fatalf("read-only interactive argv carries --disable-sandbox: %q", got)
		}
	})

	t.Run("trust workspace seeds the trust record", func(t *testing.T) {
		t.Parallel()
		p := &Provider{opts: Options{TrustWorkspace: true}}
		got := p.interactiveArgs(agent.Spec{Prompt: "x"})
		if !slices.Contains(got, "--trust-workspace") {
			t.Fatalf("trusted interactive argv missing --trust-workspace: %q", got)
		}
	})
}

// TestInteractiveArgs_PromptIsAlwaysLast pins the positional-argument
// invariant: the CLI parses a bare prompt positionally, so any flag emitted
// after it would swallow it (or the prompt would be read as a flag value).
func TestInteractiveArgs_PromptIsAlwaysLast(t *testing.T) {
	t.Parallel()
	p := &Provider{opts: Options{TrustWorkspace: true}}
	const prompt = "fix the bug"
	for _, autonomous := range []bool{false, true} {
		name := map[bool]string{false: "not autonomous", true: "autonomous"}[autonomous]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := p.interactiveArgs(agent.Spec{
				Prompt: prompt, Autonomous: autonomous,
				SandboxLevel: agent.SandboxReadOnly, Model: "muse-spark-1.3",
				Effort: agent.EffortLow, SystemPromptAppend: "sys", InitialContext: "ctx",
			})
			if len(got) == 0 {
				t.Fatal("interactiveArgs returned an empty argv for a non-empty prompt")
			}
			last := got[len(got)-1]
			if !strings.HasSuffix(last, prompt) {
				t.Errorf("last argv element = %q, want it to end with the prompt %q (full argv: %q)", last, prompt, got)
			}
			if n := slices.Index(got, last); n != len(got)-1 {
				t.Errorf("seed appears at index %d, want %d (full argv: %q)", n, len(got)-1, got)
			}
		})
	}
}

// newFakeInteractiveProvider builds a Provider whose binary is a fake-muse
// script, run under a real PTY by ptycli. The script answers `--version`
// with the pinned shape (so New's probe passes) and `exec` with the login
// probe's model-validation failure (so the host login check passes); any
// other invocation runs the TUI stub body.
//
// Mirrors the PATH-shim technique in provider/harness/clijsonl's
// handle_test.go: write without the exec bit, then chmod after close.
func newFakeInteractiveProvider(t *testing.T, tuiBody string) *Provider {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("pty spawn tests are unix-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-muse")
	script := "#!/bin/bash\n" +
		"if [[ \"$1\" == \"--version\" ]]; then echo \"Muse Code 1.4.6 (1.4.6-R6000.1)\"; exit 0; fi\n" +
		"if [[ \"$1\" == \"exec\" ]]; then echo \"agent loop failed: model failed: model " + hostLoginProbeModel + " does not exist or you lack access\" >&2; exit 1; fi\n" +
		tuiBody + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o755); err != nil { //nolint:gosec // test fixture needs exec bit
		t.Fatal(err)
	}
	// Point the login check at a shape-valid credential fixture so the
	// file leg passes without touching the operator's real login.
	t.Setenv(museAuthPathEnv, writeTestAuthFile(t))
	p, err := New(Options{MuseBin: bin})
	if err != nil {
		t.Fatalf("New(fake): %v", err)
	}
	return p
}

// writeTestAuthFile materializes a shape-valid login fixture: a meta entry
// with a login mechanism, mirroring the observed on-disk shape.
func writeTestAuthFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	body := `{"schema_version":1,"providers":{"meta":{"mechanism":"oauth","storage":"keychain"}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSpawn_Interactive_RoutesThroughPTYNotServe proves Spec.Interactive
// routes muse's Spawn through the ptycli path — not the headless
// `muse serve` MSP path — end to end: the fake binary records its own
// argv[0] (so a stray "serve" would be visible) and the returned handle
// must satisfy agent.InteractiveCapable with the coarse Init/Result event
// contract.
func TestSpawn_Interactive_RoutesThroughPTYNotServe(t *testing.T) {
	workdir := t.TempDir()
	p := newFakeInteractiveProvider(t, `printf '%s\n' "$@" > "$PWD/argv.txt"`)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:      "hello",
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	if _, ok := h.(agent.InteractiveCapable); !ok {
		t.Fatal("handle does not implement agent.InteractiveCapable")
	}

	deadline := time.After(20 * time.Second)
	gotInit, gotResult := false, false
	for !gotResult {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events channel closed before a terminal ResultEvent")
			}
			switch ev.(type) {
			case agent.InitEvent:
				gotInit = true
			case agent.ResultEvent:
				gotResult = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for events")
		}
	}
	if !gotInit {
		t.Error("never observed an InitEvent")
	}

	// The headless lane would have execed `muse serve`: the child's argv
	// must never carry the serve subcommand on the interactive path.
	body, err := os.ReadFile(filepath.Join(workdir, "argv.txt"))
	if err != nil {
		t.Fatalf("read echoed argv: %v", err)
	}
	for line := range strings.Lines(string(body)) {
		if strings.TrimSpace(line) == "serve" {
			t.Errorf("interactive child argv carries the headless serve subcommand; got:\n%s", body)
		}
	}
	if last := lastArgvLine(string(body)); last != "hello" {
		t.Errorf("last child argv element = %q, want the positional seed %q", last, "hello")
	}
}

func lastArgvLine(body string) string {
	var last string
	for line := range strings.Lines(strings.TrimSpace(body)) {
		last = line
	}
	return last
}

// TestSpawn_Interactive_SeedReachesChildArgvLast is the load-bearing proof
// that the composed seed (system append + context + prompt) reaches the PTY
// child's argv as the LAST element: the fake binary echoes its full argv to
// a file, and the test asserts the seed carries every layer in order with
// nothing after it. RED proof: drop the seed composition in interactiveArgs
// and the layers vanish from the child's argv.
func TestSpawn_Interactive_SeedReachesChildArgvLast(t *testing.T) {
	workdir := t.TempDir()
	p := newFakeInteractiveProvider(t, `printf '%s\n' "$@" > "$PWD/argv.txt"`)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:             "hello",
		SystemPromptAppend: "follow local conventions",
		InitialContext:     "recall: green build",
		Cwd:                workdir,
		Interactive:        &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })

	deadline := time.After(20 * time.Second)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				goto exited
			}
		case <-deadline:
			t.Fatal("timed out waiting for fake TUI exit")
		}
	}
exited:
	body, err := os.ReadFile(filepath.Join(workdir, "argv.txt"))
	if err != nil {
		t.Fatalf("read echoed argv: %v", err)
	}
	// printf '%s\n' splits the multi-line seed across lines, so assert on
	// the whole argv text: every layer present in order, seed last.
	want := "follow local conventions\n\nrecall: green build\n\nhello"
	argv := strings.TrimSpace(string(body))
	if !strings.HasSuffix(argv, want) {
		t.Errorf("child argv does not end with the composed seed %q (full argv:\n%s)", want, body)
	}
	if i, j := strings.Index(argv, "follow local conventions"), strings.Index(argv, "recall: green build"); i < 0 || j < 0 || i > j {
		t.Errorf("seed layers out of order in child argv:\n%s", body)
	}
}
