package pi

// extension_delivery_real_binary_test.go — real `pi` binary conformance
// fixtures for the additional-extension delivery seam (ADR-2026-08-12,
// donmai-architecture). Mirrors real_binary_test.go's scope/CI-gating
// discipline (see that file's doc comment): gated on `pi`/`node` being on
// PATH, real evidence on a machine with pi installed, not currently part of
// donmai's hosted CI.
//
// These fixtures prove, against the REAL pinned binary, the properties the
// scripted tests in extension_delivery_test.go cannot reach because
// skipProcess never execs a real child:
//
//   - tool registration through the seam (both delivery kinds) actually
//     succeeds against the real extension loader — pi.registerTool() does
//     not throw and the session completes normally;
//   - the headless-UI guarantee (D3): a delivered extension's OWN attempted
//     UI round-trip — one the runner has no reason to recognize, since it
//     carries none of the boundary extension's marker — resolves promptly
//     ("cancelled" → undefined) rather than hanging, and the session still
//     reaches a terminal event;
//   - the trust rule (D2): a workspace-discovered extension never loads in
//     an autonomous session (--no-extensions), and an operator-injected one
//     loads via `-e` even when the run explicitly declines project trust;
//   - the boundary extension's own sequential overrides of bash and write:
//     one message's shell calls run in order with max concurrency 1, and
//     the overrides keep every policy-fence rail.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}

func sha256HexOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fixtureMarker is what conformance-fixture.ts writes to DONMAI_FIXTURE_MARKER.
type fixtureMarker struct {
	Loaded             bool   `json:"loaded"`
	HasUI              *bool  `json:"hasUI"`
	ToolExecuted       bool   `json:"toolExecuted"`
	UIRoundTripSettled bool   `json:"uiRoundTripSettled"`
	UIRoundTripMs      int64  `json:"uiRoundTripMs"`
	UIReply            any    `json:"uiReply"`
	UIError            string `json:"uiError"`
}

func readFixtureMarker(t *testing.T, path string) fixtureMarker {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture marker %s: %v — the delivered extension never ran session_start", path, err)
	}
	var m fixtureMarker
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal fixture marker %s: %v (raw: %s)", path, err, b)
	}
	return m
}

// TestRealBinary_AdditionalExtension_ToolRegistersAndHeadlessUIRefusesPromptly
// is the primary conformance fixture: for both delivery kinds (path and
// inline), the extension delivered through Spec.AdditionalExtensions loads
// into the REAL pi process, its pi.registerTool() call succeeds (proven by
// the session completing with no ErrorEvent — a throwing registerTool call
// surfaces as an extension_error the Handle turns into a session abort), and
// its own attempted UI round-trip — carrying no donmai marker — resolves
// promptly with a refusal rather than hanging the session.
func TestRealBinary_AdditionalExtension_ToolRegistersAndHeadlessUIRefusesPromptly(t *testing.T) {
	realBinaryAvailable(t)
	fixture := readTestdata(t, "conformance-fixture.ts")
	digest := sha256HexOf(fixture)

	for _, tt := range []struct {
		name    string
		specify func(t *testing.T, base agent.Spec) agent.Spec
	}{
		{
			name: "path",
			specify: func(t *testing.T, base agent.Spec) agent.Spec {
				t.Helper()
				dir := t.TempDir()
				p := filepath.Join(dir, "conformance-fixture.ts")
				if err := os.WriteFile(p, fixture, 0o600); err != nil {
					t.Fatalf("write path-delivery fixture: %v", err)
				}
				base.AdditionalExtensions = []agent.ExtensionDelivery{
					{ID: "conformance-fixture", Kind: agent.ExtensionDeliveryPath, Path: p, Digest: digest, Required: true},
				}
				return base
			},
		},
		{
			name: "inline",
			specify: func(_ *testing.T, base agent.Spec) agent.Spec {
				base.AdditionalExtensions = []agent.ExtensionDelivery{
					{ID: "conformance-fixture", Kind: agent.ExtensionDeliveryInline, Source: fixture, Basename: "conformance-fixture-inline.ts", Digest: digest, Required: true},
				}
				return base
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := newRealBinaryStub(t, realBinaryModel)
			cwd := t.TempDir()
			markerPath := filepath.Join(t.TempDir(), "marker.json")

			spec := realBinarySpec(cwd, "reply with a short greeting and nothing else", stub.baseURL())
			spec.Env = map[string]string{fixtureMarkerEnvVar: markerPath}
			spec = tt.specify(t, spec)

			p, err := New(Options{HandshakeTimeout: 30 * time.Second})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			h, err := p.Spawn(ctx, spec)
			if err != nil {
				t.Fatalf("Spawn: %v — a delivered extension must not break spawn", err)
			}
			t.Cleanup(func() { _ = h.Stop(context.Background()) })

			for _, ev := range drainToResult(t, h, 30*time.Second) {
				if e, ok := ev.(agent.ErrorEvent); ok {
					t.Fatalf("session ended with an ErrorEvent instead of completing: %+v — the delivered extension's registerTool call likely threw", e)
				}
			}

			m := readFixtureMarker(t, markerPath)
			if !m.Loaded {
				t.Fatal("fixture marker reports loaded=false — the delivered extension's session_start handler never ran")
			}
			if m.HasUI == nil || !*m.HasUI {
				t.Fatalf("fixture marker hasUI = %v; the headless RPC lane reports hasUI=true (pi's own documented behavior — see docs/extensions.md ctx.hasUI), so a nil/false reading means the probe never observed a real RPC context", m.HasUI)
			}
			if !m.UIRoundTripSettled {
				t.Fatal("fixture's UI round-trip never settled within the drained session — this is the exact hang D3 forbids: a UI call from an extension the runner does not recognize must resolve promptly (as a refusal), never hang")
			}
			if m.UIRoundTripMs > 10000 {
				t.Errorf("fixture's UI round-trip took %dms to settle; want prompt cancellation (typically single-digit ms), not something indistinguishable from a hang", m.UIRoundTripMs)
			}
			if m.UIError == "" && m.UIReply != nil {
				t.Errorf("fixture's UI round-trip resolved to a non-null reply (%v) with no error — an unrecognized extension's round-trip must come back refused (pi's own contract: input() resolves to undefined on cancellation), never as if it were answered", m.UIReply)
			}
		})
	}
}

// fixtureMarkerEnvVar is DONMAI_FIXTURE_MARKER — named as a constant
// so a typo cannot silently desync this file from testdata/conformance-fixture.ts.
const fixtureMarkerEnvVar = "DONMAI_FIXTURE_MARKER"

// sequentialShellCommand logs "start <label>", holds the shell for a
// second, then logs "end <label>", all into order.log in the session cwd.
// Three of these issued in one assistant message leave interleaved lines
// unless pi runs them one at a time.
func sequentialShellCommand(label string) string {
	return fmt.Sprintf("echo start %s >> order.log; sleep 1; echo end %s >> order.log", label, label)
}

// spawnRealBinaryBatch runs one autonomous real-binary session whose first
// model turn answers with calls (one assistant message) and whose second
// turn completes, and returns the handle and every event up to the terminal.
func spawnRealBinaryBatch(t *testing.T, workdir string, calls []stubToolCall) (*Handle, []agent.Event) {
	t.Helper()
	stub := newRealBinaryStub(t, realBinaryModel)
	stub.mu.Lock()
	stub.responses = []stubResponse{{ToolCalls: calls}, {Text: "complete"}}
	stub.mu.Unlock()

	spec := realBinarySpec(workdir, "run the requested tool calls", stub.baseURL())
	spec.Autonomous = true
	p, err := New(Options{HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	events := drainToResult(t, h, 60*time.Second)
	for _, ev := range events {
		if e, ok := ev.(agent.ErrorEvent); ok {
			t.Fatalf("session raised an ErrorEvent instead of completing: %+v", e)
		}
	}
	return h.(*Handle), events
}

// toolExecutionStream flattens the tool lifecycle into "start <id>" and
// "end <id>" entries in event order, and the ids of calls ending in error.
func toolExecutionStream(events []agent.Event) (stream []string, failed map[string]bool) {
	failed = map[string]bool{}
	for _, ev := range events {
		switch e := ev.(type) {
		case agent.ToolUseEvent:
			stream = append(stream, "start "+e.ToolUseID)
		case agent.ToolResultEvent:
			stream = append(stream, "end "+e.ToolUseID)
			failed[e.ToolUseID] = e.IsError
		}
	}
	return stream, failed
}

// oneAtATime is the stream a batch run strictly one call at a time, in the
// order the model wrote it, produces.
func oneAtATime(calls []stubToolCall) []string {
	out := make([]string, 0, 2*len(calls))
	for _, call := range calls {
		out = append(out, "start "+call.ID, "end "+call.ID)
	}
	return out
}

// TestRealBinary_SequentialShellTools_RunInOrder proves, against the REAL
// binary, that three bash calls issued together in one assistant message
// run in order with max concurrency 1. The policy extension re-registers
// bash with executionMode "sequential" (registerSequentialTools in
// extensions/donmai-policy.ts), which moves the agent loop onto its
// sequential path for the whole batch. Two independent witnesses must agree:
//
//   - the commands' own log (process level): each call writes "start", holds
//     the shell for a second, then writes "end";
//   - the event stream: tool_execution_start/end strictly alternate, in the
//     order the model wrote the calls.
//
// Removing the sequential setting turns both red: pi then runs every call's
// tool_call hook first and starts all three shells together.
func TestRealBinary_SequentialShellTools_RunInOrder(t *testing.T) {
	realBinaryAvailable(t)

	workdir := t.TempDir()
	labels := []string{"one", "two", "three"}
	calls := make([]stubToolCall, 0, len(labels))
	for _, label := range labels {
		args, err := json.Marshal(map[string]any{"command": sequentialShellCommand(label)})
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		calls = append(calls, stubToolCall{ID: "call-" + label, Name: "bash", Arguments: string(args)})
	}
	_, events := spawnRealBinaryBatch(t, workdir, calls)

	var wantLog []string
	for _, label := range labels {
		wantLog = append(wantLog, "start "+label, "end "+label)
	}

	raw, err := os.ReadFile(filepath.Join(workdir, "order.log"))
	if err != nil {
		t.Fatalf("read order.log: %v", err)
	}
	gotLog := strings.Split(strings.TrimSpace(string(raw)), "\n")
	running, maxRunning := 0, 0
	for _, line := range gotLog {
		if strings.HasPrefix(line, "start ") {
			running++
			maxRunning = max(maxRunning, running)
		} else {
			running--
		}
	}
	if maxRunning != 1 || !slices.Equal(gotLog, wantLog) {
		t.Fatalf("shell executions: max concurrency %d, log %q; want concurrency 1 and log %q", maxRunning, gotLog, wantLog)
	}

	stream, failed := toolExecutionStream(events)
	if want := oneAtATime(calls); !slices.Equal(stream, want) {
		t.Fatalf("tool execution stream %q, want %q", stream, want)
	}
	for id, isError := range failed {
		if isError {
			t.Fatalf("bash call %s ended as an error", id)
		}
	}
}

// TestRealBinary_SequentialToolOverride_KeepsPolicyFence proves, against the
// REAL binary, that the sequential overrides of bash and write sit under the
// policy fence rather than around it. One assistant message carries an
// allowed write, a piped bash command that only fails under the pipefail
// prelude, a write outside the workarea, and a deletion of the harness state
// directory. The batch must run one call at a time (so the overrides are the
// tools that ran), every call must still be adjudicated by the Go side, the
// prelude must still reach the shell, and both denials must have no effect.
func TestRealBinary_SequentialToolOverride_KeepsPolicyFence(t *testing.T) {
	realBinaryAvailable(t)

	workdir := t.TempDir()
	inside := filepath.Join(workdir, "inside.txt")
	outside := filepath.Join(t.TempDir(), "must-not-exist.txt")
	marshal := func(v map[string]any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		return string(b)
	}
	calls := []stubToolCall{
		{ID: "call-write-inside", Name: "write", Arguments: marshal(map[string]any{"path": inside, "content": "inside\n"})},
		{ID: "call-pipefail", Name: "bash", Arguments: marshal(map[string]any{"command": "false | true"})},
		{ID: "call-write-outside", Name: "write", Arguments: marshal(map[string]any{"path": outside, "content": "forbidden\n"})},
		{ID: "call-delete-state", Name: "bash", Arguments: marshal(map[string]any{"command": "rm -rf .pi"})},
	}
	h, events := spawnRealBinaryBatch(t, workdir, calls)

	stream, isError := toolExecutionStream(events)
	if want := oneAtATime(calls); !slices.Equal(stream, want) {
		t.Fatalf("tool execution stream %q, want %q: the sequential overrides were not the tools that ran", stream, want)
	}
	for _, call := range calls {
		if !h.wasAdjudicated(call.ID) {
			t.Errorf("%s reached no Go-side adjudication through the override", call.ID)
		}
		if _, ok := isError[call.ID]; !ok {
			t.Errorf("%s produced no tool result", call.ID)
		}
	}
	if isError["call-write-inside"] {
		t.Error("allowed write ended as an error")
	}
	if content, err := os.ReadFile(inside); err != nil || string(content) != "inside\n" {
		t.Errorf("allowed write: content %q, err %v", content, err)
	}
	if !isError["call-pipefail"] {
		t.Error("`false | true` succeeded: the pipefail prelude did not reach the overridden bash")
	}
	if !isError["call-write-outside"] {
		t.Error("write outside the workarea was not refused")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("refused write outside the workarea had an effect: %v", err)
	}
	if !isError["call-delete-state"] {
		t.Error("deletion of the harness state directory was not refused")
	}
	if info, err := os.Stat(filepath.Join(workdir, piStateDir)); err != nil || !info.IsDir() {
		t.Errorf("harness state directory is gone after a refused deletion: %v", err)
	}
}

// TestRealBinary_WorkspaceDiscovery_StaysDisabled plants a workspace-local
// auto-discovered extension (<cwd>/.pi/extensions/canary.ts — the exact
// location docs/extensions.md names for project-local auto-discovery) and
// proves it never loads through a normal Provider.Spawn: `--no-extensions`
// disables that discovery source outright for an autonomous session, which
// is the other half of D2's trust rule (workspace-discovered extensions
// never bypass trust — they are not merely gated, they are disabled).
func TestRealBinary_WorkspaceDiscovery_StaysDisabled(t *testing.T) {
	realBinaryAvailable(t)
	canary := readTestdata(t, "workspace-discovery-canary.ts")

	stub := newRealBinaryStub(t, realBinaryModel)
	cwd := t.TempDir()
	canaryDir := filepath.Join(cwd, ".pi", "extensions")
	if err := os.MkdirAll(canaryDir, 0o750); err != nil {
		t.Fatalf("mkdir workspace extensions dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(canaryDir, "canary.ts"), canary, 0o600); err != nil {
		t.Fatalf("plant workspace canary: %v", err)
	}
	canaryMarker := filepath.Join(t.TempDir(), "canary-marker.json")

	spec := realBinarySpec(cwd, "reply with a short greeting and nothing else", stub.baseURL())
	spec.Env = map[string]string{"DONMAI_CANARY_MARKER": canaryMarker}

	p, err := New(Options{HandshakeTimeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := p.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	for _, ev := range drainToResult(t, h, 30*time.Second) {
		if e, ok := ev.(agent.ErrorEvent); ok {
			t.Fatalf("session ended with an ErrorEvent: %+v", e)
		}
	}

	if _, err := os.Stat(canaryMarker); err == nil {
		t.Fatal("the workspace-discovered canary extension LOADED — --no-extensions failed to disable auto-discovery, which means the trust rule's disabled-not-merely-gated half is broken")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat canary marker: %v", err)
	}
}

// TestRealBinary_TrustBypass_OperatorInjectedExtensionLoadsWithoutApprove
// drives the real pi binary DIRECTLY (bypassing this package's own argv
// construction, which always passes --approve as belt-and-suspenders) to
// prove pi's own documented behavior the seam's trust rule depends on: an
// extension loaded by explicit `-e` path loads regardless of project trust —
// even when the run EXPLICITLY declines it with --no-approve. This is D2's
// "operator-injected extensions bypass project trust" claim, tested at its
// true source rather than assumed from our own default flag choice.
func TestRealBinary_TrustBypass_OperatorInjectedExtensionLoadsWithoutApprove(t *testing.T) {
	realBinaryAvailable(t)
	fixture := readTestdata(t, "conformance-fixture.ts")

	cwd := t.TempDir()
	extPath := filepath.Join(cwd, "injected.ts")
	if err := os.WriteFile(extPath, fixture, 0o600); err != nil {
		t.Fatalf("write injected extension: %v", err)
	}
	sessionDir := filepath.Join(cwd, ".pi-session")
	markerPath := filepath.Join(t.TempDir(), "trust-bypass-marker.json")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// nolint:gosec // G204: fixed argv plus test-owned temp paths.
	cmd := exec.CommandContext(ctx, "pi",
		"--mode", "rpc",
		"-e", extPath,
		"--no-extensions",
		"--no-approve", // explicitly DECLINE project trust — the operator-injected
		// path must load anyway; only workspace-discovered resources are gated on
		// this decision.
		"--session-dir", sessionDir,
		"--model", "x",
	)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(),
		"DONMAI_FIXTURE_MARKER="+markerPath,
		piOfflineEnvVar+"=1",
		piSkipVersionCheckEnvVar+"=1",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start pi: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// A single get_state round trip is enough to prove the extension loaded
	// (session_start already ran by the time pi answers anything); no model
	// turn is needed for this fixture.
	_, _ = stdin.Write([]byte(`{"type":"get_state"}` + "\n"))
	time.Sleep(2 * time.Second)
	_ = stdin.Close()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	m := readFixtureMarker(t, markerPath)
	if !m.Loaded {
		t.Fatal("operator-injected extension (-e, --no-approve) never ran session_start — the trust bypass D2 depends on did not hold against the real binary")
	}
}
