package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/RenseiAI/donmai/provider/harness/activityrelay"
)

// TestActivityHookHashMatchesLiveCLI pins the trust-hash normalization
// against readbacks from a live codex app-server hooks/list (codex-cli
// 0.162.0). Each vector is one configured hook and the currentHash the
// CLI computed for it. A normalization drift — a renamed key, a changed
// default, a different canonical form — fails here before it can park a
// seat on the review modal.
//
// Vectors 1–3 cover shapes beyond the product's own (a matcher, a
// non-default timeout with async and a status line, a matcher-less
// event); vectors 4–5 are the exact product shape (explicit 10s timeout,
// quoted script path carrying a space).
func TestActivityHookHashMatchesLiveCLI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		eventKey      string
		matcher       string
		command       string
		timeoutSec    int
		async         bool
		statusMessage string
		want          string
	}{
		{
			name:       "session start with matcher and default normalization",
			eventKey:   "session_start",
			matcher:    "startup",
			command:    "echo hello",
			timeoutSec: 600,
			want:       "sha256:0e22c8a87597779c5e6055dff4776a751d4b06e9569a278386386a1144f7a77b",
		},
		{
			name:          "permission request with matcher, timeout, async, status",
			eventKey:      "permission_request",
			matcher:       "Bash",
			command:       "echo perm",
			timeoutSec:    30,
			async:         true,
			statusMessage: "checking",
			want:          "sha256:316b8cc4b68892c03466f9aec3a9a7ce3cbf890fecbe9ca566c3c0fc5c1e0e43",
		},
		{
			name:       "prompt submit without matcher",
			eventKey:   "user_prompt_submit",
			command:    "echo hi",
			timeoutSec: 600,
			want:       "sha256:4ac11110e7e52a7ace4a63994f6a554e0c891264e3e3733d1f0541b1cd0b3b3e",
		},
		{
			name:       "stop with short timeout",
			eventKey:   "stop",
			command:    "echo stop",
			timeoutSec: 5,
			want:       "sha256:09401cceb4f683a7677cb3c8f8172ff1d9d712e977b65f194022bee07678f972",
		},
		{
			name:       "product session start with quoted script path",
			eventKey:   "session_start",
			command:    "/bin/sh '/tmp/fake dir/activity-relay.sh' session_start",
			timeoutSec: 10,
			want:       "sha256:261f84607039612fcee9c0fcd86c63d4be845496c5174ed23f507d9715d62c5b",
		},
		{
			name:       "product stop with quoted script path",
			eventKey:   "stop",
			command:    "/bin/sh '/tmp/fake dir/activity-relay.sh' stop",
			timeoutSec: 10,
			want:       "sha256:71e9600d1df65a423b9b2b06dcd4ab219a3b13283217ecd297525be88134ef49",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := hashHookIdentity(tt.eventKey, tt.matcher, tt.command, tt.timeoutSec, tt.async, tt.statusMessage)
			if err != nil {
				t.Fatalf("hashHookIdentity: %v", err)
			}
			if got != tt.want {
				t.Fatalf("hash = %q, want the live CLI readback %q", got, tt.want)
			}
		})
	}
}

// decodeActivityConfig parses a rendered hooks fragment as real TOML and
// returns the hooks table plus the state table, so the test asserts
// semantics rather than string-matching.
func decodeActivityConfig(t *testing.T, fragment string) (hooks map[string]any, state map[string]any) {
	t.Helper()
	var decoded map[string]any
	if _, err := toml.Decode(fragment, &decoded); err != nil {
		t.Fatalf("hooks fragment is not TOML: %v\n%s", err, fragment)
	}
	hooksTable, _ := decoded["hooks"].(map[string]any)
	if hooksTable == nil {
		t.Fatalf("fragment carries no hooks table: %v", decoded)
	}
	state, _ = hooksTable["state"].(map[string]any)
	if state == nil {
		state = map[string]any{}
	}
	hooks = make(map[string]any, len(hooksTable))
	for k, v := range hooksTable {
		if k == "state" {
			continue
		}
		hooks[k] = v
	}
	return hooks, state
}

func TestActivityHooksBlockShape(t *testing.T) {
	t.Parallel()
	relay, err := activityrelay.New()
	if err != nil {
		t.Fatalf("New relay: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	block := activityHooksBlock(relay)
	hooks, _ := decodeActivityConfig(t, block)
	if len(hooks) != 4 {
		t.Fatalf("hooks block covers %d events, want SessionStart, UserPromptSubmit, PermissionRequest, Stop", len(hooks))
	}
	for event, key := range map[string]string{
		"SessionStart":      "session_start",
		"UserPromptSubmit":  "user_prompt_submit",
		"PermissionRequest": "permission_request",
		"Stop":              "stop",
	} {
		groups, ok := hooks[event].([]map[string]any)
		if !ok || len(groups) != 1 {
			t.Fatalf("%s is not one matcher group: %v", event, hooks[event])
		}
		if _, hasMatcher := groups[0]["matcher"]; hasMatcher {
			t.Fatalf("%s carries a matcher — telemetry observes the whole moment", event)
		}
		entries, ok := groups[0]["hooks"].([]map[string]any)
		if !ok || len(entries) != 1 {
			t.Fatalf("%s carries %v hook entries, want exactly the relay invocation", event, groups[0]["hooks"])
		}
		entry := entries[0]
		if entry["type"] != "command" {
			t.Fatalf("%s type = %v, want command", event, entry["type"])
		}
		command, _ := entry["command"].(string)
		if !strings.Contains(command, relay.ScriptPath()) || !strings.HasSuffix(command, " "+key) {
			t.Fatalf("%s command = %q, want the relay script with the moment on argv", event, command)
		}
		if timeout, _ := entry["timeout"].(int64); timeout != codexActivityHookTimeoutSec {
			t.Fatalf("%s timeout = %v, want %d", event, entry["timeout"], codexActivityHookTimeoutSec)
		}
	}
}

// TestActivityHookStateTrustsExactlyWhatRuns is the self-consistency
// proof the live readback cannot give: for every rendered hook, the
// pre-seeded trusted_hash equals the hash recomputed from the RENDERED
// command, under the RENDERED config path. A render/hash skew —
// different commands, different paths — fails here.
func TestActivityHookStateTrustsExactlyWhatRuns(t *testing.T) {
	t.Parallel()
	relay, err := activityrelay.New()
	if err != nil {
		t.Fatalf("New relay: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	configPath := filepath.Join(t.TempDir(), "config.toml")
	state, err := activityHookStateEntries(configPath, relay)
	if err != nil {
		t.Fatalf("activityHookStateEntries: %v", err)
	}
	block := activityHooksBlock(relay)
	hooks, parsedState := decodeActivityConfig(t, block+"\n"+state)
	if len(parsedState) != 4 {
		t.Fatalf("state covers %d hooks, want 4", len(parsedState))
	}
	for event, key := range map[string]string{
		"SessionStart":      "session_start",
		"UserPromptSubmit":  "user_prompt_submit",
		"PermissionRequest": "permission_request",
		"Stop":              "stop",
	} {
		groups := hooks[event].([]map[string]any)
		command := groups[0]["hooks"].([]map[string]any)[0]["command"].(string)
		wantKey := activityHookStateKey(configPath, key, 0, 0)
		entry, ok := parsedState[wantKey].(map[string]any)
		if !ok {
			t.Fatalf("no state entry for key %q (have %v)", wantKey, activityStateKeys(parsedState))
		}
		trusted, _ := entry["trusted_hash"].(string)
		recomputed, err := activityHookHash(key, command)
		if err != nil {
			t.Fatal(err)
		}
		if trusted != recomputed {
			t.Fatalf("%s trusted_hash = %q, recomputed from the rendered command = %q", event, trusted, recomputed)
		}
	}
}

func activityStateKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestAppendActivityHooksRoundTrip appends the fragment to a real
// boundary config and proves the result still parses with the hooks and
// their trust intact.
func TestAppendActivityHooksRoundTrip(t *testing.T) {
	t.Parallel()
	boundary, err := newCodexConfigBoundary("", false)
	if err != nil {
		t.Fatalf("newCodexConfigBoundary: %v", err)
	}
	t.Cleanup(func() { _ = boundary.remove() })
	relay, err := activityrelay.New()
	if err != nil {
		t.Fatalf("New relay: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	if err := boundary.appendActivityHooks(relay); err != nil {
		t.Fatalf("appendActivityHooks: %v", err)
	}
	raw, err := os.ReadFile(boundary.configPath)
	if err != nil {
		t.Fatal(err)
	}
	hooks, state := decodeActivityConfig(t, string(raw))
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PermissionRequest", "Stop"} {
		if _, ok := hooks[event]; !ok {
			t.Fatalf("%s missing after append: %v", event, activityStateKeys(hooks))
		}
	}
	if len(state) != 4 {
		t.Fatalf("state has %d entries after append, want 4", len(state))
	}
	if !strings.Contains(string(raw), "mcp_servers = {}") {
		t.Fatalf("append clobbered the baseline config:\n%s", raw)
	}
}
