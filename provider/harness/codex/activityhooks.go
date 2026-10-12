package codex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/RenseiAI/donmai/provider/harness/activityrelay"
)

// Activity-hook telemetry for platform seats.
//
// A platform session runs its TUI against a private, process-owned
// CODEX_HOME whose config.toml the harness writes. That ownership is
// what makes per-launch hooks possible without touching the operator's
// persistent configuration: the hooks block below is appended to the
// private config, and the trust the TUI would otherwise stop and ask
// for is pre-seeded beside it.
//
// Four moments are relayed — session start, prompt submit, stop, and
// permission request — each invoking the shared relay script with its
// moment on argv. The script appends one JSON line to the session's
// relay file and prints nothing, so codex treats every fire as an
// abstention: the session proceeds exactly as it would with no hooks
// installed.
//
// # Trust without a dialog
//
// Codex runs only hooks whose recorded trust matches the hook's current
// hash, where the hash is sha256 over the canonical JSON of the
// normalized hook identity (event, matcher, and the single handler with
// defaults applied: timeout 600s except SessionEnd/Interrupt at 1s,
// matcher dropped for UserPromptSubmit/Stop/Interrupt). An untrusted
// hook does not run, and at startup the TUI parks on the review modal
// for it — the hang this pre-seeding removes.
//
// The state lives in the same private config.toml under [hooks.state],
// keyed `<config-path>:<event>:<group>:<handler>` with
// trusted_hash = "sha256:<hex>". The key incorporates the config path,
// so the hash and the key are computed against the boundary's own
// configPath after it exists. Only the User layer (this file) may carry
// this state — project and plugin layers can discover hooks but never
// write trust — which is exactly the layer a private CODEX_HOME is.
//
// Provenance gates the installation (see trust.go): the platform may
// pre-answer trust only for something the platform itself provisioned.
// These hooks qualify — the harness renders both the command and the
// hash — while repo hooks stay exactly as the hooks policy leaves them.
// Repository-authority seats keep the hooks feature off entirely (hooks
// execute outside the sandbox), so they carry no telemetry; platform
// seats without repository authority run untrusted projects, whose
// project hooks codex skips on its own, leaving only these trusted
// hooks to run with no dialog.
//
// Verified against codex-cli 0.162.0: the hooks block shape
// ([[hooks.SessionStart]] tables with matcher + hooks arrays), the
// [hooks.state] key format, and the hash algorithm were all confirmed
// against a live `codex app-server` hooks/list readback. The TUI
// honors the same trust status (it shares the discovery code); the
// --dangerously-bypass-hook-trust flag is deliberately NOT used — it
// prints a warning into every session and waives review for all hooks,
// not just the platform's own.
// codexActivityHookEvent pairs a hooks TOML table with the snake_case
// label the trust hash and the state key use.
type codexActivityHookEvent struct {
	// event is the TOML table name under [hooks.*].
	event string
	// key is the snake_case label the trust hash and the state key use.
	key string
}

// No group carries a matcher: telemetry wants the moment, not a subset
// of it. SessionStart fires on startup, resume, clear, and compact
// alike; PermissionRequest observes every tool's approval. (Codex
// ignores matchers on UserPromptSubmit and Stop in any case.)
var codexActivityHookEvents = []codexActivityHookEvent{
	{event: "SessionStart", key: activityrelay.EventSessionStart},
	{event: "UserPromptSubmit", key: activityrelay.EventUserPromptSubmit},
	{event: "PermissionRequest", key: activityrelay.EventPermissionRequest},
	{event: "Stop", key: activityrelay.EventStop},
}

// codexActivityHookTimeoutSec is the explicit per-hook timeout. The CLI
// default is 600s; stating 10s keeps a wedged telemetry append from
// stalling a turn, matching the notice channel's bound.
const codexActivityHookTimeoutSec = 10

// activityHooksBlock renders the hooks block appended to the private
// config.toml: one matcher group per moment, each invoking the relay
// script with its moment on argv. The command embeds absolute paths to
// the relay script only — no environment, no interpolation — because
// hook commands run through $SHELL -lc with the session snapshot, and
// a channel that depends on a variable surviving that is a channel that
// fails silently on the day it does not.
func activityHooksBlock(relay *activityrelay.Relay) string {
	var b strings.Builder
	for _, hook := range codexActivityHookEvents {
		b.WriteString("[[hooks." + hook.event + "]]\n")
		b.WriteString("[[hooks." + hook.event + ".hooks]]\n")
		b.WriteString("type = \"command\"\n")
		b.WriteString("command = " + tomlBasicString(relay.Command(canonicalRelayMoment(hook.key))) + "\n")
		b.WriteString(fmt.Sprintf("timeout = %d\n", codexActivityHookTimeoutSec))
		b.WriteString("\n")
	}
	return b.String()
}

// canonicalRelayMoment folds a state-key moment to the relay argv form.
// They are already identical; the function exists so the two vocabularies
// cannot drift without touching this line.
func canonicalRelayMoment(key string) string { return key }

// activityHookStateEntries renders the [hooks.state] table pre-seeding
// trust for every activity hook: one entry per (event, group 0,
// handler 0) key with the pre-computed trusted_hash. configPath is the
// boundary's own config.toml path — the same string codex displays as
// the hook source, which is what the state key incorporates.
func activityHookStateEntries(configPath string, relay *activityrelay.Relay) (string, error) {
	var b strings.Builder
	for _, hook := range codexActivityHookEvents {
		hash, err := activityHookHash(hook.key, relay.Command(canonicalRelayMoment(hook.key)))
		if err != nil {
			return "", err
		}
		key := activityHookStateKey(configPath, hook.key, 0, 0)
		b.WriteString("[hooks.state." + tomlBasicString(key) + "]\n")
		b.WriteString("trusted_hash = " + tomlBasicString(hash) + "\n")
		b.WriteString("\n")
	}
	return b.String(), nil
}

// activityHookStateKey renders one hook-state key:
// `<config-path>:<event>:<group>:<handler>`.
func activityHookStateKey(configPath, eventKey string, group, handler int) string {
	return fmt.Sprintf("%s:%s:%d:%d", configPath, eventKey, group, handler)
}

// activityHookHash computes the trusted_hash for one telemetry hook: the
// product shape (explicit 10s timeout, sync, no matcher, no status line).
func activityHookHash(eventKey, command string) (string, error) {
	return hashHookIdentity(eventKey, "", command, codexActivityHookTimeoutSec, false, "")
}

// hashHookIdentity computes the trusted_hash for one command hook: sha256
// over the canonical JSON of the normalized hook identity
// {event_name, matcher?, hooks:[{type,command,timeout,async,statusMessage?}]}
// with absent optionals omitted and keys sorted. It replicates codex's own
// normalization so the pre-seeded hash equals the current hash the CLI
// computes at startup; any divergence marks the hook modified and the seat
// parks on the review modal. The extra parameters exist for the fixture
// tests, which pin the normalization against live CLI readbacks beyond
// the product shape (matchers, non-default timeouts, async, status).
func hashHookIdentity(eventKey, matcher, command string, timeoutSec int, async bool, statusMessage string) (string, error) {
	handler := map[string]any{
		"type":    "command",
		"command": command,
		"timeout": timeoutSec,
		"async":   async,
	}
	if statusMessage != "" {
		handler["statusMessage"] = statusMessage
	}
	identity := map[string]any{
		"event_name": eventKey,
		"hooks":      []any{handler},
	}
	if matcher != "" {
		identity["matcher"] = matcher
	}
	canonical, err := canonicalHookJSON(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// canonicalHookJSON renders the canonical bytes the hash covers: JSON
// with object keys sorted and no whitespace, matching serde_json's
// sorted-object serialization over the TOML value. encoding/json sorts
// map keys and emits compact separators; HTML escaping is disabled so
// commands carrying <, >, or & hash exactly as codex sees them.
func canonicalHookJSON(v any) ([]byte, error) {
	normalized, err := normalizeHookJSONValue(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalized); err != nil {
		return nil, fmt.Errorf("codex: canonicalize hook identity: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// normalizeHookJSONValue sorts every object by key so the encoding is
// canonical regardless of map iteration order. Slices keep their order
// (hook handlers are positional); scalars pass through.
func normalizeHookJSONValue(v any) (any, error) {
	switch typed := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(typed))
		for _, k := range keys {
			normalized, err := normalizeHookJSONValue(typed[k])
			if err != nil {
				return nil, err
			}
			out[k] = normalized
		}
		return out, nil
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			normalized, err := normalizeHookJSONValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = normalized
		}
		return out, nil
	case string, bool, json.Number, float64, int, int64, uint64, nil:
		return v, nil
	default:
		return nil, fmt.Errorf("codex: unsupported hook identity value %T", v)
	}
}

// appendActivityHooks installs the activity hooks block and its trust
// state into the boundary's private config.toml. It validates the
// boundary afterwards so a half-written config never reaches the child.
func (b *codexConfigBoundary) appendActivityHooks(relay *activityrelay.Relay) error {
	if b == nil {
		return fmt.Errorf("codex: isolated config boundary is missing")
	}
	if relay == nil {
		return fmt.Errorf("codex: activity relay is missing")
	}
	state, err := activityHookStateEntries(b.configPath, relay)
	if err != nil {
		return err
	}
	block := activityHooksBlock(relay)
	f, err := os.OpenFile(b.configPath, os.O_WRONLY|os.O_APPEND, codexConfigMode)
	if err != nil {
		return fmt.Errorf("codex: append activity hooks: %w", err)
	}
	if _, err := f.WriteString("\n" + block + state); err != nil {
		_ = f.Close()
		return fmt.Errorf("codex: append activity hooks: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("codex: close activity hooks config: %w", err)
	}
	if err := os.Chmod(b.configPath, codexConfigMode); err != nil {
		return fmt.Errorf("codex: secure activity hooks config: %w", err)
	}
	return b.validate()
}
