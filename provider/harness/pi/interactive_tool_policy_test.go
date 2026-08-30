package pi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// --- Interactive-lane allowed/disallowed-tools channel
// (agent.ToolDeliveryPiInteractiveLocalToolPolicy) ---
//
// Two halves, matching the pattern the rest of this package's D8 fixture
// families use: a fast Go-only half proving the stamped list actually
// reaches the PTY child's env (this file, fake-pi), and a scripted half
// proving the SAME env, once set, reaches the real production extension's
// enforcement (interactive_local_tool_policy_fixture_test.go, real
// donmai-policy.ts under node — no `pi` binary needed). The generic-plan
// admission half (Spec -> ToolLifecycleReceipt naming the delivery) lives in
// agent/tool_adaptation_test.go
// TestToolLifecyclePiInteractiveAllowedDisallowedToolsAdmitLocally.

// TestInteractiveToolPolicyEnv_JSONEncodesStampedLists is the fast unit-level
// proof that interactiveToolPolicyEnv JSON-encodes exactly the stamped
// Spec.AllowedTools/DisallowedTools lists onto the two documented env vars.
func TestInteractiveToolPolicyEnv_JSONEncodesStampedLists(t *testing.T) {
	t.Parallel()
	spec := agent.Spec{AllowedTools: []string{"Read", "Bash(git:*)"}, DisallowedTools: []string{"Write"}}
	env := interactiveToolPolicyEnv(spec)
	if !containsEnv(env, piAllowedToolsEnvVar, `["Read","Bash(git:*)"]`) {
		t.Errorf("allowed-tools env missing/incorrect: %v", env)
	}
	if !containsEnv(env, piDisallowedToolsEnvVar, `["Write"]`) {
		t.Errorf("disallowed-tools env missing/incorrect: %v", env)
	}
}

// TestInteractiveToolPolicyEnv_UnstampedSpecOmitsBothKeys proves a session
// that stamped neither list carries no local-policy env entry at all — the
// same "nothing configured, nothing blocked" shape RPC mode's own
// NewPolicyEngine has when spec.AllowedTools/DisallowedTools are empty.
func TestInteractiveToolPolicyEnv_UnstampedSpecOmitsBothKeys(t *testing.T) {
	t.Parallel()
	if env := interactiveToolPolicyEnv(agent.Spec{}); len(env) != 0 {
		t.Errorf("want no entries for an unstamped spec, got %v", env)
	}
}

// captureToolPolicyEnvScript is a dedicated fake-pi body (distinct from
// interactive_test.go's captureArgvEnvScript) that additionally dumps the
// two local-tool-policy env vars, so this file's fixtures don't need to
// touch the shared script other tests already pin exact assertions against.
const captureToolPolicyEnvScript = `
{
  printf 'DONMAI_PI_ALLOWED_TOOLS=%s\n' "$DONMAI_PI_ALLOWED_TOOLS"
  printf 'DONMAI_PI_DISALLOWED_TOOLS=%s\n' "$DONMAI_PI_DISALLOWED_TOOLS"
  printf 'DONMAI_PI_HANDSHAKE=[%s]\n' "$DONMAI_PI_HANDSHAKE"
} > "$PWD/env.txt"
`

// TestSpawn_Interactive_AllowedDisallowedToolsReachChildEnv is the load-
// bearing proof that a stamped Spec.AllowedTools/DisallowedTools list
// reaches the PTY child's env: interactiveChildEnv now carries
// DONMAI_PI_ALLOWED_TOOLS/DONMAI_PI_DISALLOWED_TOOLS, and the interactive
// lane still never sets the handshake token. RED proof: remove the
// interactiveToolPolicyEnv loop from interactiveChildEnv and the two JSON
// values vanish from the child while the handshake omission stays intact.
func TestSpawn_Interactive_AllowedDisallowedToolsReachChildEnv(t *testing.T) {
	workdir := t.TempDir()
	p := newFakeInteractivePiProvider(t, captureToolPolicyEnvScript)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:             workdir,
		Interactive:     &agent.InteractiveSpec{Cols: 80, Rows: 24},
		AllowedTools:    []string{"Read"},
		DisallowedTools: []string{"Bash"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	env := readCapturedFile(t, workdir, "env.txt")
	if !strings.Contains(env, `DONMAI_PI_ALLOWED_TOOLS=["Read"]`) {
		t.Errorf("interactive child env missing the stamped allowed-tools list; got:\n%s", env)
	}
	if !strings.Contains(env, `DONMAI_PI_DISALLOWED_TOOLS=["Bash"]`) {
		t.Errorf("interactive child env missing the stamped disallowed-tools list; got:\n%s", env)
	}
	if !strings.Contains(env, "DONMAI_PI_HANDSHAKE=[]") {
		t.Errorf("interactive child unexpectedly carries a handshake token; got:\n%s", env)
	}
}

// TestSpawn_Interactive_NoStampedToolsOmitsEnvEntirely is the negative
// counterpart: a session that stamps neither list must not carry either env
// key at all (not even empty-array), so the extension's local gate never
// activates for an ordinary unstamped interactive session.
func TestSpawn_Interactive_NoStampedToolsOmitsEnvEntirely(t *testing.T) {
	workdir := t.TempDir()
	p := newFakeInteractivePiProvider(t, captureToolPolicyEnvScript)

	h, err := p.Spawn(context.Background(), agent.Spec{
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	awaitPTYExit(t, h)

	env := readCapturedFile(t, workdir, "env.txt")
	if strings.Contains(env, "DONMAI_PI_ALLOWED_TOOLS=[") || strings.Contains(env, "DONMAI_PI_DISALLOWED_TOOLS=[") {
		t.Errorf("unstamped interactive session must not carry a local tool-policy env entry; got:\n%s", env)
	}
}

// --- Scripted conformance fixture: the stamped list reaches the REAL
// production extension's enforcement (no `pi` binary needed) ---

// nodeAvailable skips the scripted fixture on a machine with no node —
// unlike realBinaryAvailable (handle_test.go et al.), this fixture never
// needs the real `pi` binary, only node, so it is expected to run in
// donmai's own hosted CI (which already execs node for other build steps)
// even though realBinaryAvailable-gated tests skip there.
func nodeAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("scripted interactive-local-tool-policy fixture: `node` not on PATH — skipping")
	}
}

// localToolPolicyFixtureVerdict runs testdata/interactive-local-tool-policy-harness.mjs
// against the REAL production extensions/donmai-policy.ts, simulating one
// guarded tool_call under the exact env interactive.go's interactiveChildEnv
// sets (DONMAI_PI_ALLOWED_TOOLS/DONMAI_PI_DISALLOWED_TOOLS,
// DONMAI_PI_HANDSHAKE absent), and returns the captured registration+verdict.
// This is the scripted conformance fixture ADR-2026-08-12 D3.1 calls for on
// this lane: proof that a stamped list reaches the SAME extension's
// enforcement without spawning the real `pi` binary.
func localToolPolicyFixtureVerdict(t *testing.T, allowedJSON, disallowedJSON, toolName, inputJSON string) map[string]any {
	t.Helper()
	nodeAvailable(t)

	harness, err := filepath.Abs(filepath.Join("testdata", "interactive-local-tool-policy-harness.mjs"))
	if err != nil {
		t.Fatalf("resolve harness path: %v", err)
	}
	extPath, err := filepath.Abs(filepath.Join("extensions", extensionFileName))
	if err != nil {
		t.Fatalf("resolve extension path: %v", err)
	}

	cmd := exec.Command("node", harness, extPath, toolName, inputJSON) //nolint:gosec // G204: fixed test-only harness path + node binary resolved from PATH; args are constants and this test's own JSON literals.
	cmd.Env = append(os.Environ(),
		"DONMAI_PI_ALLOWED_TOOLS="+allowedJSON,
		"DONMAI_PI_DISALLOWED_TOOLS="+disallowedJSON,
	)
	// DONMAI_PI_HANDSHAKE deliberately absent from cmd.Env beyond whatever the
	// test process itself has (none, in CI) — the interactive lane never sets
	// it, and this harness's job is to prove behavior under that exact
	// absence.
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

// TestInteractiveLocalToolPolicyFixture_DisallowedToolBlocksLocally proves a
// stamped DisallowedTools entry actually reaches the extension's tool_call
// gate and blocks, naming the disallowed-tools channel in its reason —
// mirroring policy.go's Evaluate step 3 message shape.
func TestInteractiveLocalToolPolicyFixture_DisallowedToolBlocksLocally(t *testing.T) {
	t.Parallel()
	out := localToolPolicyFixtureVerdict(t, "[]", `["Bash"]`, "bash", `{"command":"git status"}`)
	if registered, _ := out["registered"].(bool); !registered {
		t.Fatalf("tool_call handler was not registered with a stamped disallowed-tools list: %v", out)
	}
	verdict, _ := out["verdict"].(map[string]any)
	if verdict == nil {
		t.Fatalf("want a block verdict for a disallowed bash call, got none: %v", out)
	}
	if block, _ := verdict["block"].(bool); !block {
		t.Errorf("want block=true, got %v", verdict)
	}
	if reason, _ := verdict["reason"].(string); !strings.Contains(reason, "disallowed-tools") {
		t.Errorf("reason %q does not name the disallowed-tools channel", reason)
	}
}

// TestInteractiveLocalToolPolicyFixture_AllowedToolPassesLocally proves a
// tool matching a stamped AllowedTools entry is let through (no block
// verdict) by the real extension.
func TestInteractiveLocalToolPolicyFixture_AllowedToolPassesLocally(t *testing.T) {
	t.Parallel()
	out := localToolPolicyFixtureVerdict(t, `["Read"]`, "[]", "read", `{"path":"README.md"}`)
	if verdict := out["verdict"]; verdict != nil {
		t.Errorf("want no block verdict for an allowed read call, got %v", verdict)
	}
}

// TestInteractiveLocalToolPolicyFixture_AllowGateDeniesUnlistedTool proves
// the allow-gate shape: once ANY AllowedTools entry is stamped, a tool that
// does not match one is blocked, mirroring policy.go's Evaluate step 5.
func TestInteractiveLocalToolPolicyFixture_AllowGateDeniesUnlistedTool(t *testing.T) {
	t.Parallel()
	out := localToolPolicyFixtureVerdict(t, `["Read"]`, "[]", "bash", `{"command":"ls"}`)
	verdict, _ := out["verdict"].(map[string]any)
	if verdict == nil {
		t.Fatalf("want a block verdict for a tool outside a configured allow-list, got none: %v", out)
	}
	if block, _ := verdict["block"].(bool); !block {
		t.Errorf("want block=true, got %v", verdict)
	}
}

// TestInteractiveLocalToolPolicyFixture_NoStampedListRegistersNoHandler
// proves the extension registers NO tool_call handler at all in PTY mode
// when neither list is stamped — an ordinary interactive session (the
// overwhelming common case) pays no local-gate cost and blocks nothing.
func TestInteractiveLocalToolPolicyFixture_NoStampedListRegistersNoHandler(t *testing.T) {
	t.Parallel()
	out := localToolPolicyFixtureVerdict(t, "[]", "[]", "bash", `{"command":"git status"}`)
	if registered, _ := out["registered"].(bool); !registered {
		t.Fatalf("the always-on state-directory safety handler was not registered: %v", out)
	}
	if verdict := out["verdict"]; verdict != nil {
		t.Errorf("ordinary bash must remain allowed without a stamped list, got %v", verdict)
	}
}

// TestInteractiveLocalToolPolicyFixture_StateDirDeletionDenied proves the
// interactive embedded extension keeps the same non-overridable local safety
// rail as the headless Go adjudicator. This is deliberately unstamped: the
// state-dir guard protects a pi session even when no allowed/disallowed tool
// policy was requested.
func TestInteractiveLocalToolPolicyFixture_StateDirDeletionDenied(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		command string
		blocked bool
	}{
		{name: "rm", command: "rm -rf .pi", blocked: true},
		{name: "rmdir", command: "rmdir .pi", blocked: true},
		{name: "forced git clean", command: "git clean -fd", blocked: true},
		{name: "find root delete", command: "find .pi -name '*.jsonl' -delete", blocked: true},
		// `.pi` after the first find predicate is an expression operand,
		// not a search root. Go's findSearchRoots stops at that predicate;
		// the local extension must preserve the same discrimination.
		{name: "find predicate mentions state dir", command: "find build -path .pi -delete", blocked: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := localToolPolicyFixtureVerdict(t, "[]", "[]", "bash", `{"command":`+strconv.Quote(tc.command)+`}`)
			verdict, _ := out["verdict"].(map[string]any)
			if !tc.blocked {
				if verdict != nil {
					t.Fatalf("unrelated find root was blocked: %v", verdict)
				}
				return
			}
			if verdict == nil {
				t.Fatalf("want local refusal for deleting the pi state dir, got %v", out)
			}
			if block, _ := verdict["block"].(bool); !block {
				t.Errorf("want block=true, got %v", verdict)
			}
			reason, _ := verdict["reason"].(string)
			if !strings.Contains(reason, stateDirGuardReasonPrefix) || !strings.Contains(reason, "harness state") {
				t.Errorf("state-dir refusal must retain the typed headless reason, got %q", reason)
			}
		})
	}
}
