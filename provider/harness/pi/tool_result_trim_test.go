package pi

// tool_result_trim_test.go — scripted conformance fixture for the bounded
// error-tool-result rail (policy extension's exported trimErrorResultText,
// run against the REAL production extensions/donmai-policy.ts under node —
// no `pi` binary needed).
//
// Failure shape: a provider on the anthropic-messages path rejected a
// request replaying a ~46 KB error tool result with a 400 for invalid
// parameters, ending the whole session on a failure no retry can fix — the
// offending bytes re-enter the context on every attempt. Smaller results
// (including a ~33 KB one the same session replayed repeatedly) passed.
//
// The extension answers with a `tool_result` handler that trims the text of
// an ERROR result past MAX_ERROR_RESULT_CHARS (tail-kept, marker-stamped)
// before pi persists it into the session transcript the next request
// replays, in both lanes. Success results are never touched.
//
// Both fixtures below drive the scripted harness
// testdata/tool-result-trim-harness.mjs against the real extension source:
// the pure-helper boundary cases, and the real registered handler's wiring
// (large error trims, small error and large success pass through, mixed
// blocks trim selectively, trim registers before the tool_call fence).

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// toolResultTrimFixture runs testdata/tool-result-trim-harness.mjs against
// the REAL production extensions/donmai-policy.ts and returns the harness's
// JSON report (helper boundary cases + per-lane wiring outcomes).
func toolResultTrimFixture(t *testing.T) map[string]any {
	t.Helper()
	nodeAvailable(t)

	harness, err := filepath.Abs(filepath.Join("testdata", "tool-result-trim-harness.mjs"))
	if err != nil {
		t.Fatalf("resolve harness path: %v", err)
	}
	extPath, err := filepath.Abs(filepath.Join("extensions", extensionFileName))
	if err != nil {
		t.Fatalf("resolve extension path: %v", err)
	}

	cmd := exec.Command("node", harness, extPath) //nolint:gosec // G204: fixed test-only harness path + node binary resolved from PATH; args are constants.
	cmd.Env = scrubHandshakeTokenFromEnv(os.Environ())
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

// TestToolResultTrim_ErrorBoundTrimsTail keeps the failing shape out of the
// replayed context: a ~46 KB error text (the observed rejection) trims to
// the 40 KB tail plus the marker, while a ~33 KB error text (observed to
// replay cleanly) and anything at or under the bound passes through
// byte-identical. Removing the trim (or raising the bound past the observed
// rejection) turns this red.
func TestToolResultTrim_ErrorBoundTrimsTail(t *testing.T) {
	t.Parallel()
	out := toolResultTrimFixture(t)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("harness did not report ok: %v", out)
	}
	cases, _ := out["helperCases"].(map[string]any)
	if cases == nil {
		t.Fatalf("harness reported no helperCases: %v", out)
	}
	for _, name := range []string{
		"emptyUnchanged", "shortUnchanged", "atBoundUnchanged",
		"mediumUnchanged", "bigTrims", "bigKeepsTail",
	} {
		if got, _ := cases[name].(bool); !got {
			t.Errorf("helper case %q = false; want true (full report: %v)", name, cases)
		}
	}
	if bound, _ := cases["bound"].(float64); bound != 40*1024 {
		t.Errorf("trim bound = %v; want %v", bound, 40*1024)
	}
}

// TestToolResultTrim_WiringBoundsErrorResultsInBothLanes proves, against the
// real extension under node, how the trim is wired:
//
//   - in BOTH lanes a `tool_result` handler is registered, before the lane's
//     tool_call fence;
//   - a large error result is patched to its tail plus the marker (head
//     dropped), a small error result passes through untouched (no patch);
//   - a large SUCCESS result is never touched, and mixed blocks trim
//     selectively (short block kept, large block trimmed).
//
// Dropping the handler from either lane (or trimming success results) turns
// this red.
func TestToolResultTrim_WiringBoundsErrorResultsInBothLanes(t *testing.T) {
	t.Parallel()
	out := toolResultTrimFixture(t)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("harness did not report ok: %v", out)
	}
	raw, err := json.Marshal(out["wiring"])
	if err != nil {
		t.Fatalf("re-encode wiring report: %v", err)
	}
	var wiring map[string]map[string]any
	if err := json.Unmarshal(raw, &wiring); err != nil {
		t.Fatalf("decode wiring report %s: %v", raw, err)
	}
	for _, laneName := range []string{"interactive", "rpc"} {
		lane, ok := wiring[laneName]
		if !ok {
			t.Fatalf("no %s lane in report %s", laneName, raw)
		}
		for _, name := range []string{
			"hasHandler", "largeErrorPatched", "largeErrorKeepsTail",
			"largeErrorDropsHead", "smallErrorUntouched",
			"largeSuccessUntouched", "mixedFirstBlockKept",
			"mixedSecondBlockTrimmed", "trimBeforeFence",
		} {
			if got, _ := lane[name].(bool); !got {
				t.Errorf("%s lane: wiring case %q = false; want true (lane: %v)", laneName, name, lane)
			}
		}
	}
}
