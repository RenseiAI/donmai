//go:build stophookspike

// Stop-hook live-push spike.
//
// Answers one empirical question a whole message-delivery design rests on: in a
// LIVE INTERACTIVE session under a real PTY (not `-p` headless, which skips
// hooks entirely), does a `Stop` hook of type "command" that prints
// {"decision":"block","reason":"<message>"} actually force the session to keep
// going and act on <message>?
//
// The rig is the same ptyhost.Spawn PTY host the interactive spawn mode uses in
// production, so the answer is about the real terminal path, not a mock.
//
// It is build-tagged out of the default suite because it launches the real CLI,
// issues real model requests, and costs money. Run it deliberately:
//
//	go test -tags=stophookspike -run TestStopHookSpike -v -timeout 20m ./provider/harness/claude/
//
// NEGATIVE CONTROL IS PART OF THE SUITE, not an afterthought: the Red test runs
// an identical session whose hook returns nothing and asserts the proof artefact
// is ABSENT. A green that is not preceded by a red is not evidence.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/ptyhost"
)

// proofToken is the exact byte string the blocked continuation is instructed to
// write. It appears nowhere in the seed prompt, so the model cannot produce it
// by accident in the red run.
const proofToken = "BLOCKED_CONTINUATION_OK"

// seedPrompt ends a turn immediately and uses no tools, so the Stop hook is the
// only thing that could possibly produce further work.
const seedPrompt = "Reply with exactly the word READY and nothing else. Do not use any tools."

// sessionEnvVars are the parent-session markers blanked before spawning, so the
// child is not treated as a nested run of whatever session launched the test.
var sessionEnvVars = []string{
	"CLAUDECODE",
	"CLAUDE_CODE_ENTRYPOINT",
	"CLAUDE_CODE_SESSION_ID",
	"CLAUDE_CODE_MESSAGING_SOCKET",
	"CLAUDE_CODE_BRIDGE_SESSION_ID",
	"CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_PID",
	"CLAUDE_EFFORT",
	"AI_AGENT",
}

type spikeRun struct {
	mode      string // "red" | "green" | "slow"
	dir       string
	ws        string // throwaway workspace = child cwd
	out       string // hook logs, counters, markers
	hookPath  string
	castPath  string
	sleepSecs string

	// hookTimeout is the per-hook "timeout" field in seconds; 0 omits it so the
	// CLI's own default applies.
	hookTimeout int
	// extraEvents are additional hook event names wired to the same probe
	// script, e.g. Notification.
	extraEvents []string
	// settingSources, when non-nil, is passed as --setting-sources (the empty
	// string means "load no user/project/local settings at all").
	settingSources *string
	// isolateConfig points CLAUDE_CONFIG_DIR at a per-run directory so the
	// workspace-trust experiments never read or write the developer's own
	// ~/.claude.json. Trust state lives in that file, NOT in settings, so this
	// is the only way to probe it hermetically.
	isolateConfig bool
	// preSeedTrust writes projects[ws].hasTrustDialogAccepted=true into the
	// isolated config before spawning.
	preSeedTrust bool

	startedAt  time.Time
	modalsSeen map[string]bool
	modalLog   []string
}

func newSpikeRun(t *testing.T, mode string) *spikeRun {
	t.Helper()
	dir := t.TempDir()
	r := &spikeRun{
		mode:       mode,
		dir:        dir,
		ws:         filepath.Join(dir, "ws"),
		out:        filepath.Join(dir, "out"),
		hookPath:   filepath.Join(dir, "hook.sh"),
		castPath:   filepath.Join(dir, "session.cast"),
		startedAt:  time.Now(),
		modalsSeen: map[string]bool{},
	}
	for _, d := range []string{r.ws, r.out} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(r.hookPath, []byte(hookScript), 0o600); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	return r
}

// settingsJSON is passed to `claude --settings <json-string>`. The CLI documents
// the flag as "Path to a settings JSON file OR a JSON string to load additional
// settings from" — the string form is what keeps a hook definition out of any
// tree the agent could commit, which a settings FILE inside the worktree would
// not.
func (r *spikeRun) settingsJSON(t *testing.T) string {
	t.Helper()
	// Invoked through bash rather than as an executable so the probe script
	// never needs the execute bit.
	hook := map[string]any{"type": "command", "command": "/bin/bash " + r.hookPath}
	if r.hookTimeout > 0 {
		hook["timeout"] = r.hookTimeout
	}
	entry := []any{map[string]any{"hooks": []any{hook}}}

	hooks := map[string]any{"Stop": entry}
	for _, ev := range r.extraEvents {
		hooks[ev] = entry
	}
	// skipDangerousModePermissionPrompt is what stops --dangerously-skip-permissions
	// raising its OWN inline modal. Carrying it here rather than relying on the
	// developer's user settings is what makes the recipe reproducible on a
	// machine that has never run the CLI interactively.
	b, err := json.Marshal(map[string]any{
		"hooks":                             hooks,
		"skipDangerousModePermissionPrompt": true,
	})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	return string(b)
}

func (r *spikeRun) env() []string {
	env := []string{
		"SPIKE_OUT=" + r.out,
		"SPIKE_WS=" + r.ws,
		"SPIKE_MODE=" + r.mode,
		"TERM=xterm-256color",
	}
	if r.sleepSecs != "" {
		env = append(env, "SPIKE_SLEEP="+r.sleepSecs)
	}
	if r.isolateConfig {
		env = append(env, "CLAUDE_CONFIG_DIR="+r.configDir())
	}
	for _, k := range sessionEnvVars {
		env = append(env, k+"=")
	}
	return env
}

func (r *spikeRun) configDir() string { return filepath.Join(r.dir, "cfg") }

// seedConfig materializes the isolated CLAUDE_CONFIG_DIR. Workspace trust is
// recorded per-cwd as projects[<cwd>].hasTrustDialogAccepted — there is no
// settings key, CLI flag, or env var for it on this build, so pre-seeding this
// file is the ONLY way to launch a session that is reachable immediately
// instead of parked on a modal.
func (r *spikeRun) seedConfig(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(r.configDir(), 0o750); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	projects := map[string]any{}
	if r.preSeedTrust {
		// The CLI keys trust on the RESOLVED cwd. On macOS t.TempDir() hands
		// back /var/folders/... while the child reports /private/var/folders/...,
		// so seeding only the unresolved path silently misses and the modal
		// still fires. Seed both.
		for _, p := range []string{r.ws, resolvePath(r.ws)} {
			projects[p] = map[string]any{"hasTrustDialogAccepted": true}
		}
	}
	cfg := map[string]any{
		"hasCompletedOnboarding": true,
		"projects":               projects,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.configDir(), ".claude.json"), b, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func (r *spikeRun) trustModalSeen() bool { return r.modalsSeen["workspace-trust"] }

func resolvePath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

func (r *spikeRun) proofPath() string { return filepath.Join(r.ws, "PROOF.txt") }

func (r *spikeRun) proofFound() bool {
	b, err := os.ReadFile(r.proofPath())
	return err == nil && strings.Contains(string(b), proofToken)
}

func (r *spikeRun) readOut(name string) string {
	b, _ := os.ReadFile(filepath.Join(r.out, name))
	return string(b)
}

func (r *spikeRun) hookEvents() string { return r.readOut("hook-events.log") }
func (r *spikeRun) hookStdin() string  { return r.readOut("hook-stdin.log") }

func (r *spikeRun) hookFired() bool {
	return strings.Contains(r.hookEvents(), "fire=1")
}

// reFiredWithLoopGuard reports whether a second Stop hook fire carried
// stop_hook_active=true — the field that makes a forced-continuation loop
// guardable instead of infinite.
func (r *spikeRun) reFiredWithLoopGuard() bool {
	return strings.Contains(r.hookEvents(), "stop_hook_active=True")
}

func (r *spikeRun) notificationFired() bool {
	return strings.Contains(r.hookEvents(), "event=Notification")
}

// run spawns the interactive REPL under a PTY and polls until stop() reports the
// experiment has produced its observation, or the budget expires. A nil stop
// runs the full budget, which is what the idle probe needs.
func (r *spikeRun) run(t *testing.T, budget time.Duration, stop func() bool) string {
	t.Helper()

	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}

	argv := []string{
		bin,
		"--settings", r.settingsJSON(t),
		"--dangerously-skip-permissions",
		"--model", "sonnet",
	}
	if r.settingSources != nil {
		argv = append(argv, "--setting-sources", *r.settingSources)
	}
	argv = append(argv, seedPrompt) // positional prompt LAST, matching interactiveArgs

	sess, err := ptyhost.Spawn(ptyhost.Spec{
		Command:    argv,
		Env:        r.env(),
		Cwd:        r.ws,
		Cols:       120,
		Rows:       40,
		RecordPath: r.castPath,
	})
	if err != nil {
		t.Fatalf("ptyhost.Spawn: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = sess.Stop(ctx)
	})

	deadline := time.Now().Add(budget)
	var lastScreen string
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		lastScreen = screenText(sess)
		r.answerBlockingModal(sess, lastScreen)
		if stop != nil && stop() {
			break
		}
		select {
		case <-sess.Done():
			return lastScreen
		default:
		}
	}
	return lastScreen
}

// blockingModals are the inline REPL prompts that stall a freshly spawned
// session before any turn can run. Each is recorded the first time it is seen —
// the record is the point, because a modal that appears here is a modal that a
// notice written into the same PTY would have answered by accident.
var blockingModals = []struct {
	name   string
	needle string
	keys   string
}{
	{"workspace-trust", "Yes, I trust this folder", "\r"},
	{"bypass-permissions", "Yes, I accept", "2\r"},
}

func (r *spikeRun) answerBlockingModal(sess *ptyhost.Session, screen string) {
	for _, m := range blockingModals {
		if !strings.Contains(screen, m.needle) || r.modalsSeen[m.name] {
			continue
		}
		r.modalsSeen[m.name] = true
		r.modalLog = append(r.modalLog, fmt.Sprintf("%s modal appeared at +%s — answered with %q",
			m.name, time.Since(r.startedAt).Round(time.Second), m.keys))
		_, _ = sess.WriteInput([]byte(m.keys))
		return
	}
}

func screenText(sess *ptyhost.Session) string {
	scr, _, err := sess.Snapshot()
	if err != nil {
		return fmt.Sprintf("<snapshot error: %v>", err)
	}
	cells := scr.Primary
	if scr.ActiveBuffer == attachwire.BufferAlt && scr.AltPresent {
		cells = scr.Alt
	}
	if scr.Cols == 0 || scr.Cols > math.MaxInt32 || scr.Cols > uint64(len(cells)) {
		return "<empty screen>"
	}
	cols := int(scr.Cols)
	var b strings.Builder
	for row := 0; row < len(cells)/cols; row++ {
		var line strings.Builder
		for col := range cols {
			c := cells[row*cols+col]
			if len(c.RuneBytes) == 0 {
				line.WriteByte(' ')
				continue
			}
			line.Write(c.RuneBytes)
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

func report(t *testing.T, label string, r *spikeRun, screen string) {
	t.Helper()
	t.Logf("\n========== %s ==========\nproof present: %v\nmodals: %v\nhook events:\n%s\nfinal screen:\n%s\n---- hook stdin payloads ----\n%s",
		label, r.proofFound(), r.modalLog, r.hookEvents(), screen, r.hookStdin())
}

// TestStopHookSpike_Red is the NEGATIVE CONTROL and must be believed before any
// green: an identical session whose Stop hook prints nothing. If this ever goes
// green the harness is not measuring what it claims to measure.
func TestStopHookSpike_Red(t *testing.T) {
	r := newSpikeRun(t, "red")
	r.hookTimeout = 30
	screen := r.run(t, 150*time.Second, r.proofFound)
	report(t, "RED (no-op hook)", r, screen)

	if !r.hookFired() {
		t.Fatalf("negative control never fired the hook at all — the rig is not exercising Stop hooks:\n%s", r.hookEvents())
	}
	if r.proofFound() {
		t.Fatalf("RED FAILED: proof artefact %s exists with a no-op hook — the assertion cannot detect failure", r.proofPath())
	}
}

// TestStopHookSpike_Green is only believable after Red above.
func TestStopHookSpike_Green(t *testing.T) {
	r := newSpikeRun(t, "green")
	r.hookTimeout = 30
	// Wait for BOTH observations: the forced continuation itself, and the
	// re-fire that carries stop_hook_active=true. Breaking on the proof alone
	// races the second Stop hook and makes the loop-guard assertion flaky.
	screen := r.run(t, 180*time.Second, func() bool {
		return r.proofFound() && r.reFiredWithLoopGuard()
	})
	report(t, "GREEN (decision:block hook)", r, screen)

	if !r.hookFired() {
		t.Fatalf("hook never fired:\n%s", r.hookEvents())
	}
	if !r.proofFound() {
		t.Fatalf("GREEN FAILED: decision:block did not force a continuation — %s absent", r.proofPath())
	}
	if !r.reFiredWithLoopGuard() {
		t.Fatalf("no re-fire carried stop_hook_active=true — a forced-continuation loop would be unguardable:\n%s", r.hookEvents())
	}
}

// TestStopHookSpike_Timeout measures the per-hook `timeout` field: a hook that
// sleeps past its ceiling must be abandoned, and its decision:block must NOT
// take effect. A rail that silently drops the message when the mailbox read is
// slow is the ack-on-the-wrong-event failure class in another costume.
func TestStopHookSpike_Timeout(t *testing.T) {
	r := newSpikeRun(t, "slow")
	r.hookTimeout = 5
	r.sleepSecs = "25"
	screen := r.run(t, 90*time.Second, nil)
	report(t, "TIMEOUT (5s ceiling, 25s hook)", r, screen)

	if !r.hookFired() {
		t.Fatalf("hook never fired:\n%s", r.hookEvents())
	}
	if r.proofFound() {
		t.Fatalf("a hook that overran its 5s timeout still delivered its block — the timeout field is not enforced")
	}
}

// TestStopHookSpike_IdleNotification asks the open question the ruling could not
// close: once a turn has ended and the session sits at an empty prompt, is there
// any hook event left to ride? Stop has already fired and been allowed. This
// wires Notification alongside Stop and simply waits out the budget.
func TestStopHookSpike_IdleNotification(t *testing.T) {
	r := newSpikeRun(t, "red") // Stop returns nothing, so the session reaches idle
	r.hookTimeout = 30
	r.extraEvents = []string{"Notification"}
	screen := r.run(t, 210*time.Second, nil)
	report(t, "IDLE / Notification (210s at an empty prompt)", r, screen)

	if !r.hookFired() {
		t.Fatalf("Stop hook never fired, so the session never reached idle:\n%s", r.hookEvents())
	}
	t.Logf("IDLE RESULT: Notification fired while idle = %v", r.notificationFired())
}

// TestStopHookSpike_SettingSourcesNone establishes whether --settings is
// ADDITIVE to the user's settings or a replacement for them, by cutting the base
// sources away and checking the injected hook still runs. Read it against the
// Red/Green runs, whose status line rendered the user's own configured
// statusLine command even though --settings never mentioned one.
func TestStopHookSpike_SettingSourcesNone(t *testing.T) {
	none := ""
	r := newSpikeRun(t, "red")
	r.hookTimeout = 30
	r.settingSources = &none
	screen := r.run(t, 90*time.Second, r.hookFired)
	report(t, "SETTING-SOURCES='' (base settings suppressed)", r, screen)

	if !r.hookFired() {
		t.Fatalf("--settings hooks did not survive --setting-sources='' — the flag is not additive:\n%s", r.hookEvents())
	}
}

// TestWorkspaceTrust_ModalStillAppears is the RED half of the workspace-trust
// pair, and the finding the whole delivery ruling rests on: with an untrusted
// cwd, a session launched with --dangerously-skip-permissions STILL parks on an
// inline 1./2./Enter modal before any turn can run. Confirm it has not been
// fixed upstream before trusting anything downstream of it.
func TestWorkspaceTrust_ModalStillAppears(t *testing.T) {
	r := newSpikeRun(t, "red")
	r.isolateConfig = true
	r.preSeedTrust = false
	r.seedConfig(t)
	screen := r.run(t, 40*time.Second, r.trustModalSeen)
	report(t, "TRUST RED (untrusted cwd)", r, screen)

	if !r.trustModalSeen() {
		t.Fatalf("workspace-trust modal did NOT appear on an untrusted cwd — upstream may have fixed it; re-derive every downstream claim")
	}
}

// TestWorkspaceTrust_PreSeedSuppresses is the GREEN half: the same session,
// with hasTrustDialogAccepted pre-seeded for that cwd, never shows the modal.
// Both halves run against an isolated CLAUDE_CONFIG_DIR, so neither touches the
// developer's real config — and because trust is checked before auth, neither
// needs working credentials.
func TestWorkspaceTrust_PreSeedSuppresses(t *testing.T) {
	r := newSpikeRun(t, "red")
	r.isolateConfig = true
	r.preSeedTrust = true
	r.seedConfig(t)
	screen := r.run(t, 40*time.Second, nil)
	report(t, "TRUST GREEN (hasTrustDialogAccepted pre-seeded)", r, screen)

	if r.trustModalSeen() {
		t.Fatalf("pre-seeding hasTrustDialogAccepted did not suppress the modal — there is no headless way to launch a reachable session")
	}
}

const hookScript = `#!/bin/bash
set -u
OUT="${SPIKE_OUT:?}"
MODE="${SPIKE_MODE:-red}"
WS="${SPIKE_WS:?}"

payload="$(cat)"

n=$(cat "$OUT/fire-count" 2>/dev/null || echo 0)
n=$((n + 1))
printf '%s' "$n" >"$OUT/fire-count"

{
  printf '===== fire %s (mode=%s) at %s =====\n' "$n" "$MODE" "$(date -u +%FT%TZ)"
  printf '%s\n' "$payload"
} >>"$OUT/hook-stdin.log"

read -r event active <<<"$(printf '%s' "$payload" | /usr/bin/python3 -c 'import sys,json
try:
    d=json.load(sys.stdin)
except Exception:
    print("PARSE_ERR PARSE_ERR"); raise SystemExit(0)
print(d.get("hook_event_name"), d.get("stop_hook_active"))' 2>/dev/null || printf 'PY_ERR PY_ERR')"

printf 'fire=%s mode=%s event=%s stop_hook_active=%s at=%s\n' \
  "$n" "$MODE" "$event" "$active" "$(date -u +%FT%TZ)" >>"$OUT/hook-events.log"

# Only Stop can force a continuation; every other event just records that it fired.
if [ "$event" != "Stop" ]; then
  exit 0
fi

# Loop guard: honour stop_hook_active when the harness supplies it, and belt-and-
# braces on the local fire counter so a missing field cannot spin forever.
if [ "$active" = "True" ] || [ "$n" -gt 1 ]; then
  printf 'fire=%s SUPPRESSED (loop guard)\n' "$n" >>"$OUT/hook-events.log"
  exit 0
fi

case "$MODE" in
red)
  exit 0
  ;;
slow)
  sleep "${SPIKE_SLEEP:-20}"
  printf 'fire=%s SLEPT %ss then emitted block at %s\n' "$n" "${SPIKE_SLEEP:-20}" "$(date -u +%FT%TZ)" >>"$OUT/hook-events.log"
  ;;
esac

cat <<JSON
{"decision":"block","reason":"STOP BLOCKED BY HOOK. Do not end your turn yet. You have one remaining instruction: use the Bash tool to run exactly this command, with no modifications:\n\nprintf 'BLOCKED_CONTINUATION_OK' > $WS/PROOF.txt\n\nAfter the command succeeds, reply with the single word DONE and stop."}
JSON
`
