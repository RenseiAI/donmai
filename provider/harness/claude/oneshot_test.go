package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// fakeCLIDispatcher is written in TestMain before any test can fork. Each
// per-test fake CLI is a hard link to it, with the test-specific shell source in
// a non-executable sidecar file. This keeps an inherited writer for a newly
// created fixture from ever referring to the inode execve uses.
var (
	fakeCLIDispatcher    string
	fakeCLIDispatcherErr string
)

func TestMain(m *testing.M) {
	code := func() int {
		if runtime.GOOS == "windows" {
			return m.Run()
		}

		dir, err := os.MkdirTemp("", "claude-fake-cli-dispatcher-")
		if err != nil {
			fakeCLIDispatcherErr = "create fake CLI dispatcher directory: " + err.Error()
			return m.Run()
		}
		defer func() { _ = os.RemoveAll(dir) }()

		path := filepath.Join(dir, "fake-cli-dispatcher.sh")
		// $0 is the per-test symlink path, so its sidecar names that test's
		// script. /bin/sh reads the sidecar as data rather than execve-ing it.
		const dispatcher = "#!/bin/sh\nexec /bin/sh \"$0.fixture\" \"$@\"\n"
		if err := writeFakeCLIFile(path, dispatcher); err != nil {
			fakeCLIDispatcherErr = "write fake CLI dispatcher: " + err.Error()
			return m.Run()
		}
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // test fixture dispatcher needs exec bit
			fakeCLIDispatcherErr = "chmod fake CLI dispatcher: " + err.Error()
			return m.Run()
		}
		fakeCLIDispatcher = path
		return m.Run()
	}()
	os.Exit(code)
}

// writeFakeCLIFile creates a fixture file without execute permission, writes
// and syncs it, then closes the writable descriptor before publication.
func writeFakeCLIFile(path, contents string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // isolated test fixture
	if err != nil {
		return err
	}
	if _, err := file.WriteString(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// writeFakeCLI writes a per-test fake claude CLI and returns its path.
//
// It must not return a newly-written executable script. A sibling test can
// fork while that script's writable FD is still open; its child then inherits
// the writer and execve of the script can fail with ETXTBSY. The dispatcher is
// immutable before tests begin, while each test script is only shell input.
func writeFakeCLI(t *testing.T, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses /bin/sh; skip on windows")
	}
	if fakeCLIDispatcherErr != "" {
		t.Fatalf("prepare fake CLI dispatcher: %s", fakeCLIDispatcherErr)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := writeFakeCLIFile(path+".fixture", script); err != nil {
		t.Fatalf("write fake cli fixture: %v", err)
	}
	if err := os.Link(fakeCLIDispatcher, path); err != nil { //nolint:gosec // atomically publishes a hard-linked test fixture
		t.Fatalf("publish fake cli dispatcher: %v", err)
	}
	return path
}

// TestWriteFakeCLI_ExecutesWhileFixtureIsWritable keeps the script source open
// while the provider forks it. With the old direct-script helper that source
// was the executable inode, and Linux returned ETXTBSY. The dispatcher safely
// asks /bin/sh to read the source as a non-executable sidecar instead.
func TestWriteFakeCLI_ExecutesWhileFixtureIsWritable(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "linux" {
		t.Run("old direct executable is the literal red control", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old-direct-fake-claude.sh")
			writer, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // deliberate ETXTBSY control
			if err != nil {
				t.Fatalf("create old direct fake CLI: %v", err)
			}
			t.Cleanup(func() { _ = writer.Close() })
			if _, err := writer.WriteString("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"is_error\":false,\"result\":\"old\"}'\n"); err != nil {
				t.Fatalf("write old direct fake CLI: %v", err)
			}
			if err := writer.Sync(); err != nil {
				t.Fatalf("sync old direct fake CLI: %v", err)
			}
			if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // executable control intentionally retains its writer
				t.Fatalf("chmod old direct fake CLI: %v", err)
			}

			p, err := New(Options{Binary: path, LookPath: func(name string) (string, error) { return name, nil }})
			if err != nil {
				t.Fatalf("New old direct control: %v", err)
			}
			_, err = p.Complete(t.Context(), agent.OneShotRequest{Messages: []agent.Message{{Content: "c"}}})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "text file busy") {
				t.Fatalf("old direct executable control error = %v, want ETXTBSY/text file busy", err)
			}
		})
	}

	cli := writeFakeCLI(t, "fake-claude-writable-fixture.sh", "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"is_error\":false,\"result\":\"ok\"}'\n")
	fixture, err := os.OpenFile(cli+".fixture", os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // deliberately holds the fake CLI source open for writing
	if err != nil {
		t.Fatalf("open fake cli source for writing: %v", err)
	}
	t.Cleanup(func() { _ = fixture.Close() })

	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := p.Complete(t.Context(), agent.OneShotRequest{Messages: []agent.Message{{Content: "c"}}})
	if err != nil {
		t.Fatalf("Complete while fixture writer is open: %v", err)
	}
	if res.Text != "ok" {
		t.Errorf("Text = %q, want ok", res.Text)
	}
}

// fakeArgvRecorderCLI writes a fake claude CLI that records its argv (one arg
// per line, NUL-free) and its stdin to files under dir, then prints a valid
// `--output-format json` envelope. It lets a test assert the EXACT invocation
// shape the real CLI would have received.
func fakeArgvRecorderCLI(t *testing.T, dir, resultText string) string {
	t.Helper()
	argvPath := filepath.Join(dir, "argv")
	stdinPath := filepath.Join(dir, "stdin")
	body, err := json.Marshal(map[string]any{
		"type":           "result",
		"subtype":        "success",
		"is_error":       false,
		"result":         resultText,
		"num_turns":      1,
		"session_id":     "sess-oneshot-1",
		"duration_ms":    1234,
		"total_cost_usd": 0.001,
		"usage": map[string]any{
			"input_tokens":            11,
			"output_tokens":           22,
			"cache_read_input_tokens": 3,
		},
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	// `printf '%s\n' "$@"` writes each argument on its own line, preserving an
	// EMPTY argument as an empty line — which is exactly what `--tools ""` must
	// deliver, and what a naive "$*" join would erase.
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + shQuote(argvPath) + "\n" +
		"cat > " + shQuote(stdinPath) + "\n" +
		"cat <<'DONMAI_EOF'\n" + string(body) + "\nDONMAI_EOF\n"
	return writeFakeCLI(t, "fake-claude-oneshot.sh", script)
}

// shQuote single-quotes a path for embedding in the fake CLI script.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readLines reads a file written by the fake CLI as a line list, dropping the
// trailing empty element produced by the final newline (but preserving genuine
// empty arguments in the middle).
func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// TestComplete_InvokesNonAgenticSingleShotArgv pins the EXACT argv the one-shot
// lane hands the claude CLI.
//
// This is the assertion the KG fleet-extraction lane was missing. The emit ran
// on the AGENTIC invocation (`--output-format stream-json --verbose
// --dangerously-skip-permissions --add-dir <cwd> --permission-mode
// bypassPermissions --append-system-prompt`), which boots the full Claude Code
// agent — tools, MCP servers, project memory — to produce one JSON object, and
// blew a 120s per-observation deadline on every observation in production.
//
// Reverting Complete to the agentic shape must fail HERE, not in a daemon log
// three hours later, so the assertion is on the argv byte-for-byte rather than
// on "it produced some text".
func TestComplete_InvokesNonAgenticSingleShotArgv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cli := fakeArgvRecorderCLI(t, dir, `{"nodes":[],"edges":[]}`)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := contextWithTimeout(t, 30*time.Second)
	defer cancel()
	res, err := p.Complete(ctx, agent.OneShotRequest{
		System:   "extract triples",
		Messages: []agent.Message{{Role: "user", Content: "the auth service uses postgres"}},
		Model:    "claude-test-model",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Text != `{"nodes":[],"edges":[]}` {
		t.Errorf("Text = %q, want the envelope's result field", res.Text)
	}

	want := []string{
		"-p",
		"--output-format", "json",
		"--max-turns", "1",
		"--strict-mcp-config",
		"--no-session-persistence",
		"--system-prompt", "extract triples",
		"--model", "claude-test-model",
		"--tools", "",
	}
	got := readLines(t, filepath.Join(dir, "argv"))
	if len(got) != len(want) {
		t.Fatalf("argv = %q (%d args), want %q (%d args)", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q\nfull argv = %q", i, got[i], want[i], got)
		}
	}

	// The agentic flags must be ABSENT — each one is a distinct cost or hazard a
	// pure completion has no use for.
	for _, banned := range []string{
		"stream-json",                    // the agent lane's JSONL framing
		"--verbose",                      //
		"--dangerously-skip-permissions", // refused under root; nothing to permit with no tools
		"--permission-mode",              //
		"--add-dir",                      // no filesystem access to grant
		"--append-system-prompt",         // APPENDS to the agent system prompt instead of replacing it
		"--mcp-config",                   //
		"--bare",                         // would force an API key and break host-session auth
		"--setting-sources",              // could drop a host's apiKeyHelper
	} {
		for _, arg := range got {
			if arg == banned {
				t.Errorf("argv contains agentic/unsafe flag %q: %q", banned, got)
			}
		}
	}

	// The prompt rides stdin, never argv — observation text stays off the
	// process listing and clear of argv length limits.
	if stdin := string(mustRead(t, filepath.Join(dir, "stdin"))); stdin != "the auth service uses postgres" {
		t.Errorf("stdin = %q, want the flattened message content", stdin)
	}
	for _, arg := range got {
		if strings.Contains(arg, "the auth service uses postgres") {
			t.Errorf("prompt leaked into argv: %q", got)
		}
	}
}

// TestComplete_ToolsFlagIsLastAndEmpty pins the two properties of `--tools ""`
// that make the invocation non-agentic AND parseable: the value is the EMPTY
// string (the CLI's "disable all tools" spelling — "default" or an omitted flag
// enables the full built-in tool set), and the flag is LAST, because the CLI
// declares it variadic (`--tools <tools...>`) and a following flag could
// otherwise be swallowed into its value list.
func TestComplete_ToolsFlagIsLastAndEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cli := fakeArgvRecorderCLI(t, dir, "{}")
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := contextWithTimeout(t, 30*time.Second)
	defer cancel()
	if _, err := p.Complete(ctx, agent.OneShotRequest{
		System:   "sys",
		Model:    "m",
		Effort:   agent.EffortLevel("high"),
		Messages: []agent.Message{{Content: "c"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got := readLines(t, filepath.Join(dir, "argv"))
	if len(got) < 2 {
		t.Fatalf("argv too short: %q", got)
	}
	if got[len(got)-2] != "--tools" || got[len(got)-1] != "" {
		t.Errorf("argv must end with `--tools` then an EMPTY value; got tail %q from %q", got[len(got)-2:], got)
	}
	// --effort must have been threaded (and, being before --tools, cannot be
	// swallowed by the variadic).
	if !containsPair(got, "--effort", "high") {
		t.Errorf("argv missing `--effort high`: %q", got)
	}
}

// TestComplete_EndpointModelWinsOverBareModel mirrors specFromOneShot's
// precedence so the two one-shot lanes cannot disagree about which model runs.
func TestComplete_EndpointModelWinsOverBareModel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cli := fakeArgvRecorderCLI(t, dir, "{}")
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := contextWithTimeout(t, 30*time.Second)
	defer cancel()
	if _, err := p.Complete(ctx, agent.OneShotRequest{
		Messages: []agent.Message{{Content: "c"}},
		Model:    "bare-model",
		Endpoint: &agent.EndpointBinding{Company: agent.CompanyAnthropic, Host: agent.HostDirect, Model: "bound-model"},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got := readLines(t, filepath.Join(dir, "argv"))
	if !containsPair(got, "--model", "bound-model") {
		t.Errorf("bound endpoint model must win; argv = %q", got)
	}
}

// TestComplete_IsErrorEnvelopeIsAFailure is the anti-silent-success assertion.
//
// Measured against claude CLI 2.1.226, a request against a nonexistent model
// exits 0 with `subtype:"success"` and `is_error:true`. A reader that trusted the
// exit code or the subtype would hand the caller an apology sentence as if it
// were a successful completion — the same "the writer reported success and the
// reader has no row" shape that hid this lane's earlier defects. Only is_error
// may decide.
func TestComplete_IsErrorEnvelopeIsAFailure(t *testing.T) {
	t.Parallel()

	envelope := `{"type":"result","subtype":"success","is_error":true,` +
		`"terminal_reason":"api_error","api_error_status":404,` +
		`"result":"There's an issue with the selected model (no-such-model)."}`
	cli := writeFakeCLI(t, "fake-claude-apierr.sh", "#!/bin/sh\ncat <<'DONMAI_EOF'\n"+envelope+"\nDONMAI_EOF\n")
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := contextWithTimeout(t, 30*time.Second)
	defer cancel()
	_, err = p.Complete(ctx, agent.OneShotRequest{Messages: []agent.Message{{Content: "c"}}})
	if err == nil {
		t.Fatal("expected an error for an is_error:true envelope printed with exit status 0")
	}
	for _, want := range []string{"api_error", "404"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must carry %q so the failure is diagnosable", err, want)
		}
	}
}

// TestComplete_UnparseableOutputIsAFailure proves a CLI that prints nothing
// usable fails loudly rather than returning empty text the caller would treat
// as "the model found no triples".
func TestComplete_UnparseableOutputIsAFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		script string
	}{
		{"no output", "#!/bin/sh\nexit 0\n"},
		{"not json", "#!/bin/sh\necho 'command not found'\n"},
		{"nonzero exit with stderr", "#!/bin/sh\necho 'boom' >&2\nexit 127\n"},
		{"wrong envelope type", `#!/bin/sh` + "\n" + `echo '{"type":"system","subtype":"init"}'` + "\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cli := writeFakeCLI(t, "fake-claude-bad.sh", tc.script)
			p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := contextWithTimeout(t, 30*time.Second)
			t.Cleanup(cancel)
			if _, err := p.Complete(ctx, agent.OneShotRequest{Messages: []agent.Message{{Content: "c"}}}); err == nil {
				t.Fatal("expected an error, got a successful completion")
			}
		})
	}
}

// TestComplete_TolerantOfLeadingNoise proves a warning line ahead of the
// envelope does not fail the parse — the CLI occasionally prefixes stdout, and a
// whole batch failing on a deprecation notice would be an absurd way to lose a
// graph.
func TestComplete_TolerantOfLeadingNoise(t *testing.T) {
	t.Parallel()

	script := "#!/bin/sh\n" +
		"echo 'warning: something advisory'\n" +
		`echo '{"type":"result","is_error":false,"result":"{\"nodes\":[],\"edges\":[]}"}'` + "\n"
	cli := writeFakeCLI(t, "fake-claude-noise.sh", script)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := contextWithTimeout(t, 30*time.Second)
	defer cancel()
	res, err := p.Complete(ctx, agent.OneShotRequest{Messages: []agent.Message{{Content: "c"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Text != `{"nodes":[],"edges":[]}` {
		t.Errorf("Text = %q, want the envelope result despite the leading warning", res.Text)
	}
}

// TestComplete_ProjectsCostAndTransport proves the OneShotResult projection is
// the SHARED one (agent.ProjectOneShot), so a caller sees the same cost /
// transport / schema semantics whichever one-shot lane ran.
func TestComplete_ProjectsCostAndTransport(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cli := fakeArgvRecorderCLI(t, dir, `{"nodes":[],"edges":[]}`)
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := contextWithTimeout(t, 30*time.Second)
	defer cancel()
	res, err := p.Complete(ctx, agent.OneShotRequest{
		Messages:       []agent.Message{{Content: "c"}},
		ResponseSchema: []byte(`{"type":"object","required":["nodes"],"properties":{"nodes":{"type":"array"}}}`),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !res.SchemaOK {
		t.Errorf("SchemaOK = false, want true — the shared projection must validate the emitted object")
	}
	if res.TransportUsed != p.Manifest().Caps.Transport {
		t.Errorf("TransportUsed = %q, want the manifest transport %q", res.TransportUsed, p.Manifest().Caps.Transport)
	}
	if res.Cost == nil {
		t.Fatal("Cost = nil, want the envelope's usage projected")
	}
	if res.Cost.InputTokens != 11 || res.Cost.OutputTokens != 22 || res.Cost.CachedInputTokens != 3 {
		t.Errorf("Cost tokens = %+v, want 11/22/3 from the envelope usage", res.Cost)
	}
}

// TestComplete_ResponseSchemaReachesThePrompt proves the soft-JSON instruction
// is delivered on this lane exactly as SpawnComplete delivers it — otherwise a
// caller that passed a schema would silently get an unconstrained completion
// depending on which lane resolved.
func TestComplete_ResponseSchemaReachesThePrompt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cli := fakeArgvRecorderCLI(t, dir, "{}")
	p, err := New(Options{Binary: cli, LookPath: func(name string) (string, error) { return name, nil }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := contextWithTimeout(t, 30*time.Second)
	defer cancel()
	req := agent.OneShotRequest{
		Messages:       []agent.Message{{Content: "observation text"}},
		ResponseSchema: []byte(`{"type":"object"}`),
	}
	if _, err := p.Complete(ctx, req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got := string(mustRead(t, filepath.Join(dir, "stdin")))
	if got != agent.OneShotPrompt(req) {
		t.Errorf("stdin = %q, want agent.OneShotPrompt(req) = %q", got, agent.OneShotPrompt(req))
	}
}

// TestProvider_ImplementsOneShotProvider proves agent.Complete resolves to the
// non-agentic lane for this harness rather than falling back to
// agent.SpawnComplete's agent-harness projection. Without this the fallback is
// invisible: SpawnComplete "works", it just boots an agent to do it.
func TestProvider_ImplementsOneShotProvider(t *testing.T) {
	t.Parallel()

	var p any = &Provider{}
	if _, ok := p.(agent.OneShotProvider); !ok {
		t.Fatal("claude Provider must implement agent.OneShotProvider so agent.Complete takes the non-agentic lane")
	}
}

// contextWithTimeout bounds a fake-CLI invocation so a wedged fixture fails the
// test instead of hanging the suite.
func contextWithTimeout(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(t.Context(), d)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
