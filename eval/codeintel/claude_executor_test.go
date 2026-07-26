package codeintel

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeSpawn is the hermetic exec seam: it returns a scripted stream + wait
// outcome instead of spawning a real `claude`, and records the argv/env the
// executor built so the wiring can be asserted without a live process.
type fakeSpawn struct {
	stream  string
	waitErr error
	stderr  string
	spawnEr error

	called  bool
	gotDir  string
	gotArgv []string
	gotEnv  []string
}

func (f *fakeSpawn) spawn(_ context.Context, dir string, argv []string, env []string) (io.ReadCloser, func() (string, error), error) {
	f.called = true
	f.gotDir = dir
	f.gotArgv = append([]string(nil), argv...)
	f.gotEnv = append([]string(nil), env...)
	if f.spawnEr != nil {
		return nil, nil, f.spawnEr
	}
	return io.NopCloser(strings.NewReader(f.stream)), func() (string, error) { return f.stderr, f.waitErr }, nil
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// TestClaudeExecutor_With_ParsesSymbolSearch proves the WITH arm parses a real
// claude stream-json session that calls the af-code-intelligence MCP tool then
// answers: the tool call (name+args+result), turn count, tokens INCLUDING
// cache-read, the final answer, and the advertised-tool passthrough.
func TestClaudeExecutor_With_ParsesSymbolSearch(t *testing.T) {
	fs := &fakeSpawn{stream: readFixture(t, "claude_with_search_symbols.jsonl")}
	exec := newClaudeExecutorWithSpawner(fs.spawn)
	waWith := t.TempDir()
	spec := ArmSpec{
		Arm:             ArmWith,
		UseCodeIntel:    true,
		Case:            fsCaseFor("BuildClaudeInvocation"),
		Workarea:        waWith,
		Env:             []string{"PATH=/with/bin"},
		Budget:          Budget{MaxTurns: 8},
		AdvertiseMode:   AdvertiseMCP,
		MCPConfigPath:   "/tmp/mcp.json",
		AdvertisedTools: []string{"mcp__af-code-intelligence__af_code_search_symbols"},
		SnapshotID:      "wa-with",
	}
	tr, err := exec.Execute(context.Background(), spec)
	if err != nil {
		t.Fatalf("WITH execute: %v", err)
	}
	if len(tr.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d (%+v)", len(tr.ToolCalls), tr.ToolCalls)
	}
	tc := tr.ToolCalls[0]
	if tc.Name != "mcp__af-code-intelligence__af_code_search_symbols" {
		t.Errorf("tool name = %q", tc.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("decode tool args %q: %v", string(tc.Arguments), err)
	}
	if args["query"] != "BuildClaudeInvocation" {
		t.Errorf("tool args query = %v", args["query"])
	}
	if !strings.Contains(tc.ResultText, "executor.go") {
		t.Errorf("tool result should contain the hit; got %q", tc.ResultText)
	}
	if tc.IsError {
		t.Error("tool call should not be an error")
	}
	if tr.TurnCount != 2 {
		t.Errorf("TurnCount = %d, want 2 (two assistant turns)", tr.TurnCount)
	}
	// Tokens from the terminal usage: input(1280)+cache_creation(3000)=4280,
	// output 62, cache_read 4200. Cache-read MUST be counted (W5 finding).
	if tr.TokenCounts.Input != 4280 || tr.TokenCounts.Output != 62 || tr.TokenCounts.CacheRead != 4200 {
		t.Errorf("TokenCounts = %+v, want {4280 62 4200}", tr.TokenCounts)
	}
	if tr.TokenCounts.CacheRead == 0 {
		t.Error("cache-read must be counted so WITH-arm context overhead is visible")
	}
	if !strings.Contains(tr.FinalAnswer, "executor.go:519") {
		t.Errorf("FinalAnswer = %q", tr.FinalAnswer)
	}
	if len(tr.AdvertisedTools) != 1 || tr.AdvertisedTools[0] != "mcp__af-code-intelligence__af_code_search_symbols" {
		t.Errorf("AdvertisedTools not carried through: %v", tr.AdvertisedTools)
	}
	if tr.SnapshotRef == nil || tr.SnapshotRef.Retain != RetainEvalPermanent {
		t.Errorf("snapshotRef must be eval-permanent; got %+v", tr.SnapshotRef)
	}
	// The executor must feed the stream-json flags on top of BuildClaudeInvocation.
	joined := strings.Join(fs.gotArgv, " ")
	for _, want := range []string{"--output-format stream-json", "--verbose", "--mcp-config /tmp/mcp.json", "--strict-mcp-config"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q; got %s", want, joined)
		}
	}
	if strings.Join(fs.gotEnv, " ") != "PATH=/with/bin" {
		t.Errorf("arm env not carried verbatim: %v", fs.gotEnv)
	}
	// The agent MUST be spawned inside the provisioned workarea (its native
	// Read/Grep/Bash operate on the process cwd), not the operator's checkout.
	if fs.gotDir != waWith {
		t.Errorf("claude cwd = %q, want the provisioned workarea %q", fs.gotDir, waWith)
	}
}

// TestClaudeExecutor_Without_GuardAndGrep proves the WITHOUT arm runs the
// mandatory contamination guard BEFORE spawning and, on a clean env, parses a
// baseline Grep session.
func TestClaudeExecutor_Without_GuardAndGrep(t *testing.T) {
	// (a) Dirty control env: donmai reachable → guard fails, claude never spawns.
	donmaiDir := writeFakeBinary(t, "donmai")
	fs := &fakeSpawn{stream: readFixture(t, "claude_without_grep.jsonl")}
	exec := newClaudeExecutorWithSpawner(fs.spawn)
	dirty := ArmSpec{Arm: ArmWithout, Case: fsCaseFor("BuildClaudeInvocation"), Workarea: t.TempDir(), Env: []string{"PATH=" + donmaiDir}, SnapshotID: "wa-wo"}
	if _, err := exec.Execute(context.Background(), dirty); err == nil {
		t.Fatal("WITHOUT arm must fail when donmai is reachable")
	} else if !strings.Contains(err.Error(), "contamination") {
		t.Errorf("want contamination error, got %v", err)
	}
	if fs.called {
		t.Error("claude must NOT be spawned when the contamination guard fails")
	}

	// (b) Clean env → parses the Grep tool call + final answer.
	clean := ArmSpec{Arm: ArmWithout, Case: fsCaseFor("BuildClaudeInvocation"), Workarea: t.TempDir(), Env: []string{"PATH=/clean/bin"}, SnapshotID: "wa-wo"}
	tr, err := exec.Execute(context.Background(), clean)
	if err != nil {
		t.Fatalf("clean WITHOUT execute: %v", err)
	}
	if len(tr.ToolCalls) != 1 || tr.ToolCalls[0].Name != "Grep" {
		t.Fatalf("want one Grep tool call, got %+v", tr.ToolCalls)
	}
	if !strings.Contains(tr.ToolCalls[0].ResultText, "executor.go") {
		t.Errorf("grep result should name the file; got %q", tr.ToolCalls[0].ResultText)
	}
	if tr.TokenCounts.Input != 3460 || tr.TokenCounts.Output != 32 || tr.TokenCounts.CacheRead != 3600 {
		t.Errorf("TokenCounts = %+v, want {3460 32 3600}", tr.TokenCounts)
	}
	if !strings.Contains(tr.FinalAnswer, "executor.go") {
		t.Errorf("FinalAnswer = %q", tr.FinalAnswer)
	}
	if len(tr.AdvertisedTools) != 0 {
		t.Errorf("control arm advertises no tools; got %v", tr.AdvertisedTools)
	}
	// The control invocation must NOT wire an MCP config but MUST isolate MCP.
	joined := strings.Join(fs.gotArgv, " ")
	if strings.Contains(joined, "--mcp-config") {
		t.Errorf("control must not wire --mcp-config; got %s", joined)
	}
	if !strings.Contains(joined, "--strict-mcp-config") {
		t.Errorf("control must pass --strict-mcp-config; got %s", joined)
	}
}

// TestClaudeExecutor_ZeroToolSession proves a no-tool session (text answer only)
// parses cleanly: no tool calls, one turn, tokens + answer captured, no error.
func TestClaudeExecutor_ZeroToolSession(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"z1","tools":["Read"]}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The answer is 42."}]},"session_id":"z1"}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"The answer is 42.","usage":{"input_tokens":100,"output_tokens":8,"cache_read_input_tokens":0}}`,
	}, "\n")
	fs := &fakeSpawn{stream: stream}
	exec := newClaudeExecutorWithSpawner(fs.spawn)
	tr, err := exec.Execute(context.Background(), ArmSpec{Arm: ArmWith, UseCodeIntel: true, Case: fsCaseFor("X"), Workarea: t.TempDir(), Env: []string{"PATH=/x"}, SnapshotID: "z"})
	if err != nil {
		t.Fatalf("zero-tool execute: %v", err)
	}
	if len(tr.ToolCalls) != 0 {
		t.Errorf("want no tool calls, got %+v", tr.ToolCalls)
	}
	if tr.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1", tr.TurnCount)
	}
	if tr.FinalAnswer != "The answer is 42." {
		t.Errorf("FinalAnswer = %q", tr.FinalAnswer)
	}
	if tr.TokenCounts.Input != 100 || tr.TokenCounts.Output != 8 {
		t.Errorf("TokenCounts = %+v", tr.TokenCounts)
	}
}

// TestClaudeExecutor_GarbledStream proves a truncated/garbled stream returns a
// clear error (never a panic) while preserving whatever partial output parsed.
func TestClaudeExecutor_GarbledStream(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"g1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"x.go"}}]},"session_id":"g1"}`,
		`{"type":"result","subtype":"success", GARBLE-not-json`,
	}, "\n")
	fs := &fakeSpawn{stream: stream}
	exec := newClaudeExecutorWithSpawner(fs.spawn)
	tr, err := exec.Execute(context.Background(), ArmSpec{Arm: ArmWith, UseCodeIntel: true, Case: fsCaseFor("X"), Workarea: t.TempDir(), Env: []string{"PATH=/x"}, SnapshotID: "g"})
	if err == nil {
		t.Fatal("garbled/truncated stream must yield an error")
	}
	// Partial output preserved: the one parsed tool_use survives the error.
	if len(tr.ToolCalls) != 1 || tr.ToolCalls[0].Name != "Read" {
		t.Errorf("partial tool calls should survive; got %+v", tr.ToolCalls)
	}
}

// TestClaudeExecutor_NonZeroExit proves a non-zero claude exit yields an error
// (not a panic) with the partial transcript still populated.
func TestClaudeExecutor_NonZeroExit(t *testing.T) {
	fs := &fakeSpawn{stream: readFixture(t, "claude_without_grep.jsonl"), waitErr: &fakeExitErr{}, stderr: "boom"}
	exec := newClaudeExecutorWithSpawner(fs.spawn)
	tr, err := exec.Execute(context.Background(), ArmSpec{Arm: ArmWith, UseCodeIntel: true, Case: fsCaseFor("X"), Workarea: t.TempDir(), Env: []string{"PATH=/x"}, SnapshotID: "e"})
	if err == nil {
		t.Fatal("non-zero claude exit must yield an error")
	}
	if len(tr.ToolCalls) != 1 {
		t.Errorf("partial transcript should still carry the parsed tool call; got %+v", tr.ToolCalls)
	}
}

// TestClaudeExecutor_ErrorMaxTurns_RecordedNotAborted proves an agent-error
// terminal result (subtype=error_max_turns, is_error=true) delivered with a CLEAN
// process exit (waitErr nil, exit 0 — the observed 2.1.201 headless behavior) is a
// completed-but-FAILED task, NOT a harness error: Execute returns nil error (so
// the driver records the trial instead of aborting the whole A/B matrix), its
// empty result is NOT masked by a stale mid-session assistant line (so the
// task-success grader scores it pass=false), and its token cost is reconstructed
// from the per-turn assistant usage rather than reported as zero. Regression for
// the W6 review blocker: runError used to return a fatal error here, discarding
// the entire report on the first — disproportionately control-arm — max-turns.
func TestClaudeExecutor_ErrorMaxTurns_RecordedNotAborted(t *testing.T) {
	// The fixture's terminal result carries NO usage object; only the per-turn
	// assistant events do (input 5000 + cache_read 800, output 120). A clean exit
	// (fakeSpawn waitErr defaults to nil) reproduces the exact headless scenario.
	fs := &fakeSpawn{stream: readFixture(t, "claude_error_max_turns.jsonl")}
	exec := newClaudeExecutorWithSpawner(fs.spawn)
	tr, err := exec.Execute(context.Background(), ArmSpec{Arm: ArmWith, UseCodeIntel: true, Case: fsCaseFor("X"), Workarea: t.TempDir(), Env: []string{"PATH=/x"}, SnapshotID: "emt"})
	if err != nil {
		t.Fatalf("error_max_turns is a recorded task failure, not a harness error: %v", err)
	}
	// The mid-session line must NOT be presented as the final answer — otherwise
	// the task-success grader (which only rejects EMPTY answers) would score an
	// unfinished session pass on a stale, hedged line.
	if strings.Contains(tr.FinalAnswer, "foo.go:42") {
		t.Errorf("mid-session text must not be graded as the final answer; got %q", tr.FinalAnswer)
	}
	if strings.TrimSpace(tr.FinalAnswer) != "" {
		t.Errorf("an errored result with empty result text should leave FinalAnswer empty; got %q", tr.FinalAnswer)
	}
	// Token cost must fall back to the per-turn assistant usage, not zero — a
	// zeroed control-arm max-turns cost would inflate the WITH/WITHOUT token gate.
	if tr.TokenCounts.Input != 5000 || tr.TokenCounts.Output != 120 || tr.TokenCounts.CacheRead != 800 {
		t.Errorf("TokenCounts = %+v, want {5000 120 800} (fallback from assistant usage)", tr.TokenCounts)
	}
}

// TestClaudeExecutor_ErrorDuringExecution_RecordedNotAborted proves the sibling
// error_during_execution subtype (is_error=true, clean exit, usage present on the
// result) is likewise a recorded failed trial, not a harness error: nil error,
// empty FinalAnswer (mid-session text suppressed), and tokens taken from the
// result's own usage. Mirrors the frozen clijsonl decoder's Success =
// (subtype=="success" && !is_error).
func TestClaudeExecutor_ErrorDuringExecution_RecordedNotAborted(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"ede1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"partial reasoning, not a final answer"}]},"session_id":"ede1"}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","usage":{"input_tokens":500,"output_tokens":10,"cache_read_input_tokens":100}}`,
	}, "\n")
	fs := &fakeSpawn{stream: stream}
	exec := newClaudeExecutorWithSpawner(fs.spawn)
	tr, err := exec.Execute(context.Background(), ArmSpec{Arm: ArmWith, UseCodeIntel: true, Case: fsCaseFor("X"), Workarea: t.TempDir(), Env: []string{"PATH=/x"}, SnapshotID: "ede"})
	if err != nil {
		t.Fatalf("error_during_execution is a recorded task failure, not a harness error: %v", err)
	}
	if strings.TrimSpace(tr.FinalAnswer) != "" {
		t.Errorf("mid-session text must not become the final answer; got %q", tr.FinalAnswer)
	}
	// Result usage is present, so it is authoritative (not the assistant fallback).
	if tr.TokenCounts.Input != 500 || tr.TokenCounts.Output != 10 || tr.TokenCounts.CacheRead != 100 {
		t.Errorf("TokenCounts = %+v, want {500 10 100} (from the result usage)", tr.TokenCounts)
	}
}

type fakeExitErr struct{}

func (fakeExitErr) Error() string { return "exit status 1" }

// TestSpawnClaude_RealSubprocess exercises the real exec seam (never touched by
// the fakeSpawn tests): it stages a fake `claude` on a temp PATH and asserts the
// two contamination-critical invariants of spawnClaude — the child runs in the
// provisioned workarea (cmd.Dir), and it receives the safe arm env without a
// merge from os.Environ or runner-only attach controls — plus the argv/lookup guards. Without this, an
// env- or cwd-handling regression would silently defeat the A/B isolation and no
// fake-spawner test would go red.
func TestSpawnClaude_RealSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a POSIX shell script")
	}
	workarea := t.TempDir()
	binDir := t.TempDir()
	// Fake claude: prove cwd by dropping a marker file in it, prove safe arm env
	// passthrough, and prove runner-only attach controls are absent.
	// Use only shell builtins (`:` redirection, printf) — the fake claude's PATH is
	// just binDir, so an external `touch` would not resolve. The redirect creating
	// ./ran-here is relative to the child cwd, which spawnClaude must set to the
	// workarea.
	script := "#!/bin/sh\n: > ./ran-here\nattach=leaked\nif [ \"${ATTACH_TOKEN+x}${ATTACH_TOKEN_FILE+x}${ATTACH_URL+x}\" = \"\" ]; then attach=clean; fi\nprintf '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"marker=%s,attach=%s\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"cache_read_input_tokens\":0}}\\n' \"$CI_EVAL_MARKER\" \"$attach\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil { // nolint:gosec // test fixture must be executable
		t.Fatalf("write fake claude: %v", err)
	}
	env := []string{
		"PATH=" + binDir,
		"CI_EVAL_MARKER=env-ok",
		"ATTACH_TOKEN=explicit-secret",
		"ATTACH_TOKEN_FILE=/explicit/token",
		"ATTACH_URL=wss://explicit.invalid/v1/rooms/room-1",
	}

	stdout, wait, err := spawnClaude(context.Background(), workarea, []string{"claude"}, env)
	if err != nil {
		t.Fatalf("spawnClaude: %v", err)
	}
	ps := parseClaudeStream(stdout)
	_ = stdout.Close()
	tail, werr := wait()
	if werr != nil {
		t.Fatalf("claude exited non-zero: %v (stderr: %q)", werr, tail)
	}
	// cmd.Dir was set to the workarea: the marker landed there, not in the test cwd.
	if _, statErr := os.Stat(filepath.Join(workarea, "ran-here")); statErr != nil {
		t.Errorf("claude did not run in the provisioned workarea: %v", statErr)
	}
	// cmd.Env carried the safe arm value but removed runner-only controls.
	if ps.finalAnswer != "marker=env-ok,attach=clean" {
		t.Errorf("arm env boundary mismatch; result = %q", ps.finalAnswer)
	}

	// Guard: empty argv is an error, not a panic.
	if _, _, e := spawnClaude(context.Background(), workarea, nil, env); e == nil {
		t.Error("empty argv must error")
	}
	// Guard: a binary resolvable on neither the arm PATH nor the process PATH errors.
	if _, _, e := spawnClaude(context.Background(), workarea, []string{"claude-nonexistent-xyz-123"}, []string{"PATH=" + binDir}); e == nil {
		t.Error("an unresolvable binary must error")
	}
}

// TestClaudeExecutor_Name pins the executor identifier used in reports.
func TestClaudeExecutor_Name(t *testing.T) {
	if NewClaudeExecutor().Name() != "claude" {
		t.Errorf("Name() = %q, want claude", NewClaudeExecutor().Name())
	}
}
