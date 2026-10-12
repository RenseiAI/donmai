package claude

import (
	"encoding/json"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/provider/harness/activityrelay"
)

// Activity-hook telemetry for interactive seats.
//
// The interactive REPL fires the CLI's own lifecycle hooks; the headless
// lane does not need them (its JSONL stream already carries the turn), so
// these entries ride only the interactive --settings string built in
// spawnInteractive — never buildArgs' headless argv, which stays
// byte-identical to before telemetry existed.
//
// Four moments are relayed, in the snake_case vocabulary the runner maps
// to activity (see agent.HarnessLifecycleEvent): session start, prompt
// submit, stop, and permission request. Each entry invokes the shared
// relay script with its moment on argv; the script appends one JSON line
// to the session's relay file and prints nothing, so the CLI treats every
// fire as an abstention — the session proceeds exactly as it would with
// no hooks installed. The Stop entry sits BESIDE the notice channel's
// entry in the same Stop array: the notice hook may block a turn to
// deliver a message, while the telemetry hook only observes that the turn
// ended.
//
// Verified against claude 2.1.296: settings hooks delivered per launch via
// --settings <json>, no trust dialog (a --settings string is inherently
// per-launch — nothing is written to the user's settings). Unknown event
// names are silently ignored by the CLI, so a harness version that has
// not heard of one of these moments loses that telemetry line rather
// than failing the spawn; PermissionRequest additionally fires only in
// sessions that actually raise permission prompts (bypassPermissions
// seats never request, so they never emit it).
var activityHookEvents = []string{
	activityrelay.EventSessionStart,
	activityrelay.EventUserPromptSubmit,
	activityrelay.EventStop,
	activityrelay.EventPermissionRequest,
}

// activitySettingsEntries renders the telemetry hook entries for one
// session's relay: event name → group array, in the shape the CLI's
// settings schema takes (see the spike rig's settingsJSON). No matcher
// narrows any entry — SessionStart fires on startup, resume, clear and
// compact alike, and PermissionRequest observes every tool's prompt —
// because telemetry wants the moment, not a subset of it.
func activitySettingsEntries(r *activityrelay.Relay) map[string]any {
	entries := make(map[string]any, len(activityHookEvents))
	for _, event := range activityHookEvents {
		entries[eventTitle(event)] = []any{
			map[string]any{
				"hooks": []any{
					map[string]any{
						"type":    "command",
						"command": r.Command(event),
						"timeout": activityrelay.TimeoutSeconds(),
					},
				},
			},
		}
	}
	return entries
}

// eventTitle folds a canonical relay moment back to the settings key the
// CLI matches on. The wire casing is PascalCase (SessionStart); the relay
// uses snake_case throughout.
func eventTitle(event string) string {
	switch event {
	case activityrelay.EventSessionStart:
		return "SessionStart"
	case activityrelay.EventUserPromptSubmit:
		return "UserPromptSubmit"
	case activityrelay.EventStop:
		return "Stop"
	case activityrelay.EventPermissionRequest:
		return "PermissionRequest"
	}
	return event
}

// mergeActivityHooks adds the telemetry entries into a settings hooks map
// that already carries the notice channel's Stop entry. The Stop arrays
// concatenate — the notice entry stays first so a pending delivery is
// claimed on the same fire the telemetry observes — while the other three
// moments are new keys.
func mergeActivityHooks(hooks map[string]any, entries map[string]any) map[string]any {
	if hooks == nil {
		hooks = make(map[string]any)
	}
	for event, groups := range entries {
		if existing, ok := hooks[event]; ok {
			if oldGroups, ok := existing.([]any); ok {
				if newGroups, ok := groups.([]any); ok {
					hooks[event] = append(oldGroups, newGroups...)
					continue
				}
			}
		}
		hooks[event] = groups
	}
	return hooks
}

// sweepActivityRelay maps newly relayed hook fires to runner events. It is
// the telemetry leg of the interactive handle's tailer: best-effort and
// non-blocking like the usage sweep beside it.
func sweepActivityRelay(tailer *activityrelay.Tailer) []agent.Event {
	if tailer == nil {
		return nil
	}
	return tailer.Sweep(activityrelay.MaxLinesPerSweep, activityrelay.MaxEventsPerSweep)
}

// pinActivitySettings renders the fail-closed fallback when the notice
// channel could not be created: the routing pin plus the telemetry
// entries, without the Stop-hook claim script. It returns "" when there
// is neither a pin nor telemetry to declare, so a bare host-login spawn
// stays bare.
func pinActivitySettings(pin map[string]string, activity map[string]any) string {
	settings := map[string]any{}
	if pinJSON, err := marshalRoutingPin(pin); err == nil && pinJSON != "" {
		var env map[string]any
		if err := json.Unmarshal([]byte(pinJSON), &env); err == nil {
			for k, v := range env {
				settings[k] = v
			}
		}
	}
	if len(activity) > 0 {
		settings["hooks"] = mergeActivityHooks(nil, activity)
	}
	if len(settings) == 0 {
		return ""
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return ""
	}
	return string(b)
}
