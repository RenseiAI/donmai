package claude

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/activityrelay"
)

// decodeSettingsHooks decodes the hooks map out of a --settings string,
// failing the test on any shape surprise.
func decodeSettingsHooks(t *testing.T, rendered string) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(rendered), &decoded); err != nil {
		t.Fatalf("settings render is not JSON: %v", err)
	}
	hooks, ok := decoded["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings carry no hooks map: %v", decoded)
	}
	return hooks
}

// hookCommands flattens every command string out of one event's group array.
func hookCommands(t *testing.T, hooks map[string]any, event string) []string {
	t.Helper()
	raw, ok := hooks[event]
	if !ok {
		t.Fatalf("no %s entry in hooks %v", event, hooks)
	}
	groups, ok := raw.([]any)
	if !ok || len(groups) == 0 {
		t.Fatalf("%s entry is not a non-empty group array: %v", event, raw)
	}
	var commands []string
	for _, g := range groups {
		group, ok := g.(map[string]any)
		if !ok {
			t.Fatalf("%s group is not an object: %v", event, g)
		}
		entries, ok := group["hooks"].([]any)
		if !ok {
			t.Fatalf("%s group carries no hooks array: %v", event, group)
		}
		for _, e := range entries {
			entry, ok := e.(map[string]any)
			if !ok {
				t.Fatalf("%s hook entry is not an object: %v", event, e)
			}
			if entry["type"] != "command" {
				t.Fatalf("%s hook entry type = %v, want command", event, entry["type"])
			}
			command, ok := entry["command"].(string)
			if !ok || command == "" {
				t.Fatalf("%s hook entry carries no command: %v", event, entry)
			}
			commands = append(commands, command)
		}
	}
	return commands
}

func TestActivitySettingsEntriesCoverFourMoments(t *testing.T) {
	t.Parallel()
	relay, err := activityrelay.New()
	if err != nil {
		t.Fatalf("New relay: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	entries := activitySettingsEntries(relay)
	if len(entries) != 4 {
		t.Fatalf("telemetry entries cover %d moments, want SessionStart, UserPromptSubmit, Stop, PermissionRequest", len(entries))
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "PermissionRequest"} {
		commands := hookCommands(t, entries, event)
		if len(commands) != 1 {
			t.Fatalf("%s has %d hook commands, want exactly the relay invocation", event, len(commands))
		}
		if !strings.Contains(commands[0], relay.ScriptPath()) {
			t.Fatalf("%s command = %q, want the relay script", event, commands[0])
		}
		if !strings.Contains(commands[0], "session_start") && event == "SessionStart" {
			t.Fatalf("SessionStart command = %q, want the moment on argv", commands[0])
		}
	}
}

func TestMergedSettingsKeepNoticeStopAndPin(t *testing.T) {
	t.Parallel()
	channel, err := newStopHookChannel()
	if err != nil {
		t.Fatalf("newStopHookChannel: %v", err)
	}
	t.Cleanup(func() { _ = channel.close() })
	relay, err := activityrelay.New()
	if err != nil {
		t.Fatalf("New relay: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	rendered, err := channel.settingsJSONWithPinAndActivity(map[string]string{
		EnvBaseURL: "http://127.0.0.1:1/v1",
	}, activitySettingsEntries(relay))
	if err != nil {
		t.Fatalf("settingsJSONWithPinAndActivity: %v", err)
	}
	hooks := decodeSettingsHooks(t, rendered)

	// The Stop array carries BOTH the notice claim script and the
	// telemetry relay, notice first.
	stopCommands := hookCommands(t, hooks, "Stop")
	if len(stopCommands) != 2 {
		t.Fatalf("Stop has %d commands, want notice + telemetry: %q", len(stopCommands), stopCommands)
	}
	if !strings.Contains(stopCommands[0], stopHookScriptFile) {
		t.Fatalf("first Stop command = %q, want the notice claim script first", stopCommands[0])
	}
	if !strings.Contains(stopCommands[1], relay.ScriptPath()) {
		t.Fatalf("second Stop command = %q, want the telemetry relay", stopCommands[1])
	}

	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PermissionRequest"} {
		if got := hookCommands(t, hooks, event); len(got) != 1 {
			t.Fatalf("%s has %d commands, want 1: %q", event, len(got), got)
		}
	}

	// The routing pin still rides the winning layer beside the hooks.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(rendered), &decoded); err != nil {
		t.Fatal(err)
	}
	env, _ := decoded["env"].(map[string]any)
	if env[EnvBaseURL] != "http://127.0.0.1:1/v1" {
		t.Fatalf("pin lost under merged hooks: %v", env)
	}
	if !decoded["skipDangerousModePermissionPrompt"].(bool) {
		t.Fatalf("modal answer lost under merged hooks: %v", decoded)
	}
}

func TestMergedSettingsWithoutActivityIsUnchanged(t *testing.T) {
	t.Parallel()
	channel, err := newStopHookChannel()
	if err != nil {
		t.Fatalf("newStopHookChannel: %v", err)
	}
	t.Cleanup(func() { _ = channel.close() })

	pin := map[string]string{EnvBaseURL: "http://127.0.0.1:1/v1"}
	plain, err := channel.settingsJSONWithPin(pin)
	if err != nil {
		t.Fatalf("settingsJSONWithPin: %v", err)
	}
	merged, err := channel.settingsJSONWithPinAndActivity(pin, nil)
	if err != nil {
		t.Fatalf("settingsJSONWithPinAndActivity(nil): %v", err)
	}
	if plain != merged {
		t.Fatalf("nil activity changed the render:\n%s\n%s", plain, merged)
	}
	hooks := decodeSettingsHooks(t, merged)
	if got := hookCommands(t, hooks, "Stop"); len(got) != 1 {
		t.Fatalf("Stop has %d commands without telemetry, want only the notice hook", len(got))
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PermissionRequest"} {
		if _, present := hooks[event]; present {
			t.Fatalf("%s present without telemetry — the headless shape must stay hook-free", event)
		}
	}
}

func TestPinActivitySettingsFallback(t *testing.T) {
	t.Parallel()
	relay, err := activityrelay.New()
	if err != nil {
		t.Fatalf("New relay: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	// Pin plus telemetry: the fail-closed render when the notice channel
	// could not be created.
	rendered := pinActivitySettings(map[string]string{EnvBaseURL: "http://127.0.0.1:1/v1"}, activitySettingsEntries(relay))
	hooks := decodeSettingsHooks(t, rendered)
	if got := hookCommands(t, hooks, "Stop"); len(got) != 1 {
		t.Fatalf("fallback Stop has %d commands, want only telemetry: %q", len(got), got)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(rendered), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["env"].(map[string]any)[EnvBaseURL] != "http://127.0.0.1:1/v1" {
		t.Fatalf("fallback lost the pin: %v", decoded)
	}

	// Bare host-login spawn stays bare: no pin, no telemetry, no settings.
	if got := pinActivitySettings(nil, nil); got != "" {
		t.Fatalf("bare fallback = %q, want empty", got)
	}
	// Telemetry alone still declares hooks without inventing an env layer.
	alone := pinActivitySettings(nil, activitySettingsEntries(relay))
	aloneHooks := decodeSettingsHooks(t, alone)
	hookCommands(t, aloneHooks, "SessionStart")
	var aloneDecoded map[string]any
	if err := json.Unmarshal([]byte(alone), &aloneDecoded); err != nil {
		t.Fatal(err)
	}
	if _, present := aloneDecoded["env"]; present {
		t.Fatalf("telemetry-only fallback invented an env layer: %v", aloneDecoded)
	}
}

func TestSweepActivityRelayMapsToLifecycleEvents(t *testing.T) {
	t.Parallel()
	relay, err := activityrelay.New()
	if err != nil {
		t.Fatalf("New relay: %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	appendRelayLines(t, relay.EventsPath(),
		`{"v":1,"event":"session_start","ts":"t","payload":{"session_id":"s"}}`,
		`{"v":1,"event":"permission_request","ts":"t","payload":{"tool_name":"Bash"}}`,
	)
	events := sweepActivityRelay(activityrelay.NewTailer(relay.EventsPath()))
	if len(events) != 2 {
		t.Fatalf("sweep = %d events, want 2", len(events))
	}
	for i, want := range []string{
		agent.HarnessLifecycleSessionStart,
		agent.HarnessLifecyclePermissionRequest,
	} {
		system, ok := events[i].(agent.SystemEvent)
		if !ok {
			t.Fatalf("event %d is %T, want SystemEvent", i, events[i])
		}
		if system.Subtype != agent.SystemSubtypeHarnessLifecycle || system.Message != want {
			t.Fatalf("event %d = %+v, want harness_lifecycle/%s", i, system, want)
		}
	}
	if got := sweepActivityRelay(nil); len(got) != 0 {
		t.Fatalf("nil tailer sweep = %d events, want none", len(got))
	}
}

func appendRelayLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}
