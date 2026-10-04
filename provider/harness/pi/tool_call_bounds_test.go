package pi

// tool_call_bounds_test.go — scripted conformance fixtures for the bounded
// shell-tool execution rail (policy extension's exported
// resolveBashTimeoutSeconds / withPipefailPrelude, run against the REAL
// production extensions/donmai-policy.ts under node — no `pi` binary
// needed).
//
// Two halves, matching the pattern the rest of this package's D8 fixture
// families use: a fast Go-side half is unnecessary here (the helpers are
// pure functions of their inputs, owned entirely by the extension), so both
// fixtures below drive the scripted harness testdata/tool-call-bounds-
// harness.mjs. The in-RPC application half (mutation of event.input after
// an allow verdict) is proved by the existing Go adjudication fixtures plus
// the harness's own wiring; this file proves the BOUNDS the wiring applies.
//
// The same harness also drives the sequential shell and file-write overrides
// through the extension's real activation (both lanes, stub host package),
// so CI — which has node but no pi — guards that mechanism too. The
// real-binary half (ordering on a live pi, and the fence holding through the
// overrides) is extension_delivery_real_binary_test.go.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// toolCallBoundsFixture runs testdata/tool-call-bounds-harness.mjs against
// the REAL production extensions/donmai-policy.ts and returns the harness's
// JSON report (timeout cases + prelude cases).
func toolCallBoundsFixture(t *testing.T) map[string]any {
	t.Helper()
	nodeAvailable(t)

	harness, err := filepath.Abs(filepath.Join("testdata", "tool-call-bounds-harness.mjs"))
	if err != nil {
		t.Fatalf("resolve harness path: %v", err)
	}
	extPath, err := filepath.Abs(filepath.Join("extensions", extensionFileName))
	if err != nil {
		t.Fatalf("resolve extension path: %v", err)
	}

	cmd := exec.Command("node", harness, extPath) //nolint:gosec // G204: fixed test-only harness path + node binary resolved from PATH; args are constants.
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("fixture harness failed: %v\nstderr: %s", err, stderr.String())
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode fixture output %q: %v", stdout.String(), err)
	}
	return out
}

// TestToolCallBounds_TimeoutDefaultsAndClamps proves the 300s bound against
// the real extension: a missing, zero, negative, NaN, or non-numeric timeout
// resolves to 300; a small timeout is preserved; anything above 300 clamps
// to 300. The bound sits below the runner's 12-minute no-progress window so
// a bounded call always ends (as a tool error the agent sees) before the
// session timer could fire on its silence.
func TestToolCallBounds_TimeoutDefaultsAndClamps(t *testing.T) {
	t.Parallel()
	out := toolCallBoundsFixture(t)
	ok, _ := out["ok"].(bool)
	if !ok {
		t.Fatalf("harness did not report ok: %v", out)
	}
	cases, _ := out["timeoutCases"].(map[string]any)
	if cases == nil {
		t.Fatalf("harness reported no timeoutCases: %v", out)
	}
	want := map[string]float64{
		"missing": 300, "zero": 300, "negative": 300, "nan": 300,
		"string": 300, "small": 60, "atBound": 300, "aboveBound": 300,
	}
	for name, wantVal := range want {
		got, _ := cases[name].(float64)
		if got != wantVal {
			t.Errorf("timeout case %q = %v; want %v", name, got, wantVal)
		}
	}
}

// TestToolCallBounds_PipefailPreludeIsIdempotent proves the pipefail prelude
// against the real extension: a plain piped gate gains the prelude (so it
// reports its own exit code, not tail's), an already-prefixed command is
// left alone (the bound is applied once even if the hook ever ran twice),
// and the prelude carries a guard so a non-bash fallback shell runs the
// command unchanged instead of failing on an unknown option.
func TestToolCallBounds_PipefailPreludeIsIdempotent(t *testing.T) {
	t.Parallel()
	out := toolCallBoundsFixture(t)
	ok, _ := out["ok"].(bool)
	if !ok {
		t.Fatalf("harness did not report ok: %v", out)
	}
	cases, _ := out["preludeCases"].(map[string]any)
	if cases == nil {
		t.Fatalf("harness reported no preludeCases: %v", out)
	}
	plain, _ := cases["plain"].(string)
	const wantPlain = "set -o pipefail 2>/dev/null; false | tail -1; echo $?"
	if plain != wantPlain {
		t.Errorf("plain prelude = %q; want %q", plain, wantPlain)
	}
	prefixed, _ := cases["alreadyPrefixed"].(string)
	const wantPrefixed = "set -o pipefail 2>/dev/null; echo hi"
	if prefixed != wantPrefixed {
		t.Errorf("already-prefixed prelude = %q; want %q (idempotent)", prefixed, wantPrefixed)
	}
}

// sequentialLane is one activation of the extension as the harness saw it:
// every pi.on / pi.registerTool call in order, and the tools registered.
type sequentialLane struct {
	Sequence []string `json:"sequence"`
	Tools    []struct {
		Name          string  `json:"name"`
		ExecutionMode *string `json:"executionMode"`
		PromptSnippet *string `json:"promptSnippet"`
	} `json:"tools"`
}

type sequentialReport struct {
	Lanes          map[string]sequentialLane `json:"lanes"`
	UnresolvedHost sequentialLane            `json:"unresolvedHost"`
	Delegation     *struct {
		Cwd        string `json:"cwd"`
		ToolCallID string `json:"toolCallId"`
		SameParams bool   `json:"sameParams"`
	} `json:"delegation"`
	Rejects struct {
		NotAFunction bool    `json:"notAFunction"`
		WrongTool    bool    `json:"wrongTool"`
		NoExecute    bool    `json:"noExecute"`
		Accepted     *string `json:"accepted"`
	} `json:"rejects"`
}

// TestToolCallBounds_SequentialOverridesLayerUnderTheFence proves, against
// the real extension under node, how the sequential overrides are wired:
//
//   - in BOTH lanes, exactly bash, write and edit are re-registered with
//     executionMode "sequential", keeping pi's own tool metadata (read and
//     the other read-only tools stay parallel);
//   - they are registered only after the lane's tool_call handler, so the
//     fence is in place before the overrides are even attempted;
//   - an override runs pi's own factory for the session's cwd with the very
//     params object the tool_call hook mutated (the bounds rail reaches it);
//   - when the host package does not resolve, nothing is registered and the
//     fence handlers are still installed;
//   - an unusable factory yields no override rather than a broken tool.
func TestToolCallBounds_SequentialOverridesLayerUnderTheFence(t *testing.T) {
	t.Parallel()
	out := toolCallBoundsFixture(t)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("harness did not report ok: %v", out)
	}
	raw, err := json.Marshal(out["sequential"])
	if err != nil {
		t.Fatalf("re-encode sequential report: %v", err)
	}
	var report sequentialReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode sequential report %s: %v", raw, err)
	}

	wantTools := []string{"bash", "write", "edit"}
	for _, laneName := range []string{"interactive", "rpc"} {
		lane, ok := report.Lanes[laneName]
		if !ok {
			t.Fatalf("no %s lane in report %s", laneName, raw)
		}
		var names []string
		for _, tool := range lane.Tools {
			names = append(names, tool.Name)
			if tool.ExecutionMode == nil || *tool.ExecutionMode != "sequential" {
				t.Errorf("%s lane: %s registered with executionMode %v, want sequential", laneName, tool.Name, tool.ExecutionMode)
			}
			if tool.PromptSnippet == nil || *tool.PromptSnippet != "snippet "+tool.Name {
				t.Errorf("%s lane: %s lost pi's own prompt metadata: %v", laneName, tool.Name, tool.PromptSnippet)
			}
		}
		if !slices.Equal(names, wantTools) {
			t.Errorf("%s lane registered %q, want %q", laneName, names, wantTools)
		}
		fence := slices.Index(lane.Sequence, "on:tool_call")
		firstTool := slices.IndexFunc(lane.Sequence, func(entry string) bool { return strings.HasPrefix(entry, "tool:") })
		if fence < 0 || (firstTool >= 0 && firstTool < fence) {
			t.Errorf("%s lane: overrides registered before the tool_call fence: %q", laneName, lane.Sequence)
		}
	}
	if !slices.Contains(report.Lanes["rpc"].Sequence, "on:session_start") {
		t.Errorf("rpc lane lost its handshake handler: %q", report.Lanes["rpc"].Sequence)
	}

	if len(report.UnresolvedHost.Tools) != 0 {
		t.Errorf("unresolved host package still registered tools: %+v", report.UnresolvedHost.Tools)
	}
	if want := []string{"on:session_start", "on:tool_call"}; !slices.Equal(report.UnresolvedHost.Sequence, want) {
		t.Errorf("unresolved host package: handlers %q, want the fence %q", report.UnresolvedHost.Sequence, want)
	}

	if d := report.Delegation; d == nil || d.Cwd != "/fixture/session-cwd" || d.ToolCallID != "fixture-call" || !d.SameParams {
		t.Errorf("override did not delegate to pi's factory for the session cwd with the hook's params: %+v", d)
	}

	r := report.Rejects
	if !r.NotAFunction || !r.WrongTool || !r.NoExecute {
		t.Errorf("an unusable factory produced an override: %+v", r)
	}
	if r.Accepted == nil || *r.Accepted != "sequential" {
		t.Errorf("a usable factory produced executionMode %v, want sequential", r.Accepted)
	}
}
