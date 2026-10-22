package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/agent"
)

// TestInteractiveHandle_RelayOnlyNoticeChannelIsNil pins B2: a session with
// no Stop-hook channel (relay-only telemetry) reports a real nil channel,
// never a typed-nil interface holding a nil receiver. The runner dead-letters
// only on a real nil; a typed-nil would panic in Offer.
func TestInteractiveHandle_RelayOnlyNoticeChannelIsNil(t *testing.T) {
	t.Parallel()
	h := &interactiveHandle{notices: nil}
	if h.NoticeChannel() != nil {
		t.Fatal("relay-only handle NoticeChannel() is non-nil, want nil (typed-nil panics in Offer)")
	}
	// A handle with a channel still reports it.
	hook := newTestStopHookChannel(t)
	full := &interactiveHandle{notices: hook}
	if full.NoticeChannel() == nil {
		t.Fatal("handle with a channel reports nil, want the channel")
	}
}

// drainActivitySpawnResult reads Events until the terminal ResultEvent,
// collecting harness-lifecycle moments. It fails on timeout.
func drainActivitySpawnResult(t *testing.T, h *interactiveHandle) (moments []string, terminal *agent.ResultEvent) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("events channel closed before a terminal ResultEvent")
			}
			if sys, ok := ev.(agent.SystemEvent); ok && sys.Subtype == agent.SystemSubtypeHarnessLifecycle {
				moments = append(moments, sys.Message)
			}
			if result, ok := ev.(agent.ResultEvent); ok {
				cp := result
				terminal = &cp
				return moments, terminal
			}
		case <-deadline:
			t.Fatal("timed out waiting for the terminal ResultEvent")
		}
	}
}

// readSettingsJSON polls for the fake child's argv dump and returns the
// --settings JSON value.
func readSettingsJSON(t *testing.T, workdir string) map[string]any {
	t.Helper()
	path := filepath.Join(workdir, "argv.txt")
	var raw []byte
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		raw, err = os.ReadFile(path) //nolint:gosec // test-owned fixture
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fake child never wrote argv.txt: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The fake echoes one argv element per line; --settings JSON is one line.
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	idx := -1
	for i, line := range lines {
		if line == "--settings" && i+1 < len(lines) {
			idx = i + 1
			break
		}
	}
	if idx < 0 {
		t.Fatalf("child argv carries no --settings: %q", raw)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(lines[idx]), &decoded); err != nil {
		t.Fatalf("settings JSON is not JSON: %v\n%s", err, lines[idx])
	}
	return decoded
}

// hookCommand extracts the first hook command for event from a decoded
// --settings map.
func hookCommand(t *testing.T, settings map[string]any, event string) string {
	t.Helper()
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings carry no hooks map: %v", settings)
	}
	raw, ok := hooks[event]
	if !ok {
		t.Fatalf("no %s entry in hooks %v", event, hooks)
	}
	groups, ok := raw.([]any)
	if !ok || len(groups) == 0 {
		t.Fatalf("%s entry is not a non-empty group array: %v", event, raw)
	}
	group, ok := groups[0].(map[string]any)
	if !ok {
		t.Fatalf("%s group is not an object: %v", event, groups[0])
	}
	entries, ok := group["hooks"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatalf("%s group carries no hooks array: %v", event, group)
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("%s hook entry is not an object: %v", event, entries[0])
	}
	cmd, ok := entry["command"].(string)
	if !ok || cmd == "" {
		t.Fatalf("%s hook entry carries no command: %v", event, entry)
	}
	return cmd
}

// TestSpawnInteractive_ActivityTelemetryReachesEvents pins B3: the production
// SpawnInteractive path carries the four telemetry hooks in --settings beside
// the notice Stop entry, and a SessionStart fire arrives as a
// harness_lifecycle event before the terminal ResultEvent, with
// ActivityFlushed closing first.
func TestSpawnInteractive_ActivityTelemetryReachesEvents(t *testing.T) {
	workdir := t.TempDir()
	// The fake records its argv then sleeps, so the test can fire a hook
	// while the session is still running (before Done and the final flush).
	p := newFakeInteractiveProvider(t, `printf '%s\n' "$@" > "$PWD/argv.txt"; sleep 2`)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:      "hello",
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	wrapper, ok := h.(*interactiveHandle)
	if !ok {
		t.Fatalf("Spawn returned %T, want *interactiveHandle", h)
	}
	settings := readSettingsJSON(t, workdir)
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings carry no hooks map: %v", settings)
	}
	// Four moments beside the notice Stop entry.
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PermissionRequest"} {
		raw, present := hooks[event]
		if !present {
			t.Fatalf("%s missing from --settings hooks %v", event, hooks)
		}
		if groups, ok := raw.([]any); !ok || len(groups) != 1 {
			t.Fatalf("%s is not one matcher group: %v", event, raw)
		}
	}
	stopRaw, present := hooks["Stop"]
	if !present {
		t.Fatalf("Stop missing from --settings hooks %v", hooks)
	}
	stopGroups, ok := stopRaw.([]any)
	if !ok || len(stopGroups) != 2 {
		t.Fatalf("Stop has %d groups, want notice + telemetry (2 groups): %v", len(stopGroups), stopRaw)
	}
	var stopCommands []string
	for _, g := range stopGroups {
		group, _ := g.(map[string]any)
		entries, _ := group["hooks"].([]any)
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			if cmd, _ := entry["command"].(string); cmd != "" {
				stopCommands = append(stopCommands, cmd)
			}
		}
	}
	if len(stopCommands) != 2 {
		t.Fatalf("Stop has %d hook commands, want notice + telemetry: %q", len(stopCommands), stopCommands)
	}
	if !strings.Contains(stopCommands[0], stopHookScriptFile) {
		t.Fatalf("first Stop command = %q, want the notice claim script first", stopCommands[0])
	}
	// Fire SessionStart through the installed command (simulating the CLI).
	cmd := hookCommand(t, settings, "SessionStart")
	if out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", cmd).CombinedOutput(); err != nil { //nolint:gosec // test executes the harness's own relay command
		t.Fatalf("fire installed SessionStart hook: %s: %v", out, err)
	}
	moments, terminal := drainActivitySpawnResult(t, wrapper)
	found := false
	for _, m := range moments {
		if m == agent.HarnessLifecycleSessionStart {
			found = true
		}
	}
	if !found {
		t.Fatalf("harness_lifecycle moments = %v, want session_start before the terminal", moments)
	}
	if terminal == nil {
		t.Fatal("no terminal ResultEvent")
	}
	select {
	case <-wrapper.ActivityFlushed():
	default:
		t.Fatal("terminal ResultEvent arrived before ActivityFlushed closed")
	}
	if wrapper.NoticeChannel() == nil {
		t.Fatal("full session reports no notice channel, want the Stop-hook channel")
	}
}

// TestSpawnInteractive_RelayOnlyWithoutHookChannel pins the relay-only
// production path (it also covers B2): when the Stop-hook channel cannot be
// created, the session still spawns wrapped with telemetry, reports a nil
// notice channel, and delivers hook fires.
func TestSpawnInteractive_RelayOnlyWithoutHookChannel(t *testing.T) {
	old := newStopHookChannelFn
	newStopHookChannelFn = func() (*stopHookChannel, error) {
		return nil, context.DeadlineExceeded
	}
	t.Cleanup(func() { newStopHookChannelFn = old })
	workdir := t.TempDir()
	p := newFakeInteractiveProvider(t, `printf '%s\n' "$@" > "$PWD/argv.txt"; sleep 2`)
	h, err := p.Spawn(context.Background(), agent.Spec{
		Prompt:      "hello",
		Cwd:         workdir,
		Interactive: &agent.InteractiveSpec{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatalf("relay-only Spawn: %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background()) })
	wrapper, ok := h.(*interactiveHandle)
	if !ok {
		t.Fatalf("relay-only Spawn returned %T, want *interactiveHandle (relay-only still wrapped)", h)
	}
	if wrapper.NoticeChannel() != nil {
		t.Fatal("relay-only handle NoticeChannel() is non-nil, want nil (typed-nil panics in Offer)")
	}
	settings := readSettingsJSON(t, workdir)
	// Without the notice channel there is only the telemetry Stop entry.
	stopRaw, present := settings["hooks"].(map[string]any)["Stop"]
	if !present {
		t.Fatalf("Stop missing from relay-only settings %v", settings)
	}
	stopGroups, ok := stopRaw.([]any)
	if !ok || len(stopGroups) != 1 {
		t.Fatalf("relay-only Stop is not one group: %v", stopRaw)
	}
	stopGroup, _ := stopGroups[0].(map[string]any)
	stopEntries, ok := stopGroup["hooks"].([]any)
	if !ok || len(stopEntries) != 1 {
		t.Fatalf("relay-only Stop has %d entries, want only telemetry", len(stopEntries))
	}
	cmd := hookCommand(t, settings, "SessionStart")
	if out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", cmd).CombinedOutput(); err != nil { //nolint:gosec // test executes the harness's own relay command
		t.Fatalf("fire installed SessionStart hook (relay-only): %s: %v", out, err)
	}
	moments, terminal := drainActivitySpawnResult(t, wrapper)
	found := false
	for _, m := range moments {
		if m == agent.HarnessLifecycleSessionStart {
			found = true
		}
	}
	if !found {
		t.Fatalf("relay-only moments = %v, want session_start", moments)
	}
	if terminal == nil {
		t.Fatal("no terminal ResultEvent on the relay-only path")
	}
}
