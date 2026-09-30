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

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
