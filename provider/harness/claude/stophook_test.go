package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// newTestStopHookChannel builds a channel and tears its drop directory down.
func newTestStopHookChannel(t *testing.T) *stopHookChannel {
	t.Helper()
	c, err := newStopHookChannel()
	if err != nil {
		t.Fatalf("newStopHookChannel: %v", err)
	}
	t.Cleanup(func() {
		if err := c.close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return c
}

// fireHook runs the GENERATED script through a real /bin/sh with payload on
// stdin, exactly as the CLI invokes it, and returns what the CLI would read
// from its stdout.
//
// The script is executed rather than simulated on purpose: its claim is a
// rename and its loop guard is that rename's single-winner property, neither of
// which a Go reimplementation of "what the script probably does" would test.
func fireHook(t *testing.T, c *stopHookChannel, payload string) string {
	t.Helper()
	cmd := exec.Command("/bin/sh", c.path(stopHookScriptFile)) //nolint:gosec // the script is this test's own fixture
	cmd.Stdin = strings.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook script exited non-zero (%v); stderr=%q", err, stderr.String())
	}
	return stdout.String()
}

// hookStdin is a realistic Stop payload. Only transcript_path is read; the rest
// is present so the test exercises the parse against the real shape rather than
// a one-field stub.
func hookStdin(transcriptPath string, stopHookActive bool) string {
	b, _ := json.Marshal(map[string]any{
		"session_id":             "a0fe5c03-fd5c-4113-849c-4f7a81d73ee4",
		"transcript_path":        transcriptPath,
		"cwd":                    "/tmp/ws",
		"permission_mode":        "bypassPermissions",
		"hook_event_name":        "Stop",
		"stop_hook_active":       stopHookActive,
		"last_assistant_message": "READY",
	})
	return string(b)
}

// TestStopHookScript_ClaimsOnceAndIsItsOwnLoopGuard pins the two properties the
// whole channel rests on.
//
// EMITS THE DECISION VERBATIM: the CLI reads the hook's stdout as JSON. The
// runner renders that JSON in Go, where escaping is correct, and the script's
// only job is to hand it over unchanged. A script that reformatted, re-quoted,
// or line-wrapped it would corrupt every message containing a quote.
//
// CLAIMS EXACTLY ONCE: a `decision:block` makes the CLI re-fire the Stop hook
// after the forced continuation. If that second fire emitted the same message
// again the session would loop forever. The guard is the rename — only one
// process can win it — so it does not depend on the hook reading, or the CLI
// supplying, stop_hook_active. The re-fire below therefore declares
// stop_hook_active=true AND is proven inert without it being consulted.
func TestStopHookScript_ClaimsOnceAndIsItsOwnLoopGuard(t *testing.T) {
	c := newTestStopHookChannel(t)
	const text = `check the "build" — it's red`

	// A fire before anything is offered must emit nothing at all: an empty
	// stdout is what tells the CLI to let the turn end normally.
	if got := fireHook(t, c, hookStdin("/tmp/t.jsonl", false)); got != "" {
		t.Fatalf("hook emitted %q with nothing offered; want empty stdout", got)
	}

	if err := c.Offer("dlv-1", text); err != nil {
		t.Fatalf("Offer: %v", err)
	}

	out := fireHook(t, c, hookStdin("/tmp/t.jsonl", false))
	var decision struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &decision); err != nil {
		t.Fatalf("hook stdout is not the JSON the CLI parses: %v (stdout=%q)", err, out)
	}
	if decision.Decision != "block" {
		t.Fatalf("decision = %q; want block — anything else lets the turn end", decision.Decision)
	}
	if decision.Reason != text {
		t.Fatalf("reason = %q; want the message verbatim %q", decision.Reason, text)
	}

	// The offer is gone from the drop, so the re-fire has nothing to emit.
	if _, err := os.Stat(c.path(stopHookPendingFile)); !os.IsNotExist(err) {
		t.Fatalf("pending payload survived the claim (stat err = %v)", err)
	}
	if got := fireHook(t, c, hookStdin("/tmp/t.jsonl", true)); got != "" {
		t.Fatalf("the re-fire emitted %q; a forced-continuation loop would never terminate", got)
	}

	// The claim receipt is the FIRST fire's payload, not the re-fire's: it is
	// the one that identifies the transcript this message landed in.
	b, err := os.ReadFile(c.path(stopHookStdinFile))
	if err != nil {
		t.Fatalf("read claim receipt: %v", err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(b, &receipt); err != nil {
		t.Fatalf("claim receipt is not the harness payload verbatim: %v", err)
	}
	if receipt["stop_hook_active"] != false {
		t.Fatalf("claim receipt came from the re-fire, not the claiming fire: %v", receipt["stop_hook_active"])
	}
}

// TestStopHookChannel_ConsumedNeedsTheTranscriptRecord is the ack contract.
//
// Every row below is a state a REAL session reaches, and only one of them means
// the message reached the model. The three that do not are the dangerous ones,
// because each is silent from the session's side:
//
//   - the hook has not fired yet (the turn is still running);
//   - the hook fired and its output was DISCARDED for overrunning its timeout,
//     which the CLI records only as hook_cancelled and shows the user nothing;
//   - the CLI wrote its own bookkeeping about the hook but no message.
//
// A channel that acked on any of these would report a lost message as
// delivered, which is exactly the failure this axis exists to prevent.
func TestStopHookChannel_ConsumedNeedsTheTranscriptRecord(t *testing.T) {
	const text = "the deploy finished"

	feedback := func(body string) string {
		return jsonLine(t, map[string]any{
			"type": "user", "isMeta": true,
			"message": map[string]any{"role": "user", "content": body},
		})
	}

	tests := []struct {
		name       string
		claimed    bool // the hook fired and left a receipt
		transcript []string
		want       bool
	}{
		{
			name:    "hook has not fired yet",
			claimed: false,
			want:    false,
		},
		{
			name:    "hook fired but the transcript records no message",
			claimed: true,
			transcript: []string{
				jsonLine(t, map[string]any{"type": "assistant", "message": map[string]any{"role": "assistant", "content": "READY"}}),
			},
			want: false,
		},
		{
			name:    "hook output was discarded on timeout",
			claimed: true,
			transcript: []string{
				jsonLine(t, map[string]any{"type": "attachment", "attachment": map[string]any{
					"type": "hook_cancelled", "hookName": "Stop", "timedOut": true, "timeoutMs": 5000,
				}}),
				jsonLine(t, map[string]any{
					"type": "system", "subtype": "stop_hook_summary",
					"hookErrors": []string{}, "preventedContinuation": false, "hasOutput": false,
				}),
			},
			want: false,
		},
		{
			name:    "only the CLI's own hook bookkeeping, no message",
			claimed: true,
			transcript: []string{
				jsonLine(t, map[string]any{"type": "attachment", "attachment": map[string]any{
					"type": "hook_blocking_error", "hookName": "Stop",
					"blockingError": map[string]any{"blockingError": text},
				}}),
				jsonLine(t, map[string]any{
					"type": "system", "subtype": "stop_hook_summary",
					"hookErrors": []string{text}, "hasOutput": true,
				}),
			},
			want: false,
		},
		{
			name:       "the message entered the conversation",
			claimed:    true,
			transcript: []string{feedback("Stop hook feedback:\n" + text)},
			want:       true,
		},
		{
			name:    "the message entered the conversation as content blocks",
			claimed: true,
			transcript: []string{jsonLine(t, map[string]any{
				"type": "user", "isMeta": true,
				"message": map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "Stop hook feedback:\n" + text},
				}},
			})},
			want: true,
		},
		{
			name:    "a same-shaped record that is not meta is not our message",
			claimed: true,
			transcript: []string{jsonLine(t, map[string]any{
				"type": "user", "isMeta": false,
				"message": map[string]any{"role": "user", "content": text},
			})},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestStopHookChannel(t)
			if err := c.Offer("dlv-1", text); err != nil {
				t.Fatalf("Offer: %v", err)
			}
			if tc.claimed {
				path := writeTranscript(t, tc.transcript)
				fireHook(t, c, hookStdin(path, false))
			}
			got, err := c.Consumed()
			if err != nil {
				t.Fatalf("Consumed: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Consumed() = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestStopHookChannel_RepeatedMessageDoesNotAckOnItsPredecessor is the subtle
// one, and the reason Consumed carries a cursor at all.
//
// Evidence matching is on the message body, because the body is what the CLI
// records. Two identical messages in one session — "the build is red", twice —
// therefore produce two indistinguishable records, and a matcher that rescans
// from the top would credit the SECOND delivery with the FIRST's record the
// instant it was offered, acking a message that has not been collected and may
// never be.
func TestStopHookChannel_RepeatedMessageDoesNotAckOnItsPredecessor(t *testing.T) {
	const text = "the build is red"
	feedback := jsonLine(t, map[string]any{
		"type": "user", "isMeta": true,
		"message": map[string]any{"role": "user", "content": "Stop hook feedback:\n" + text},
	})

	c := newTestStopHookChannel(t)
	path := writeTranscript(t, []string{feedback})

	// First delivery: collected, recorded, acked.
	if err := c.Offer("dlv-1", text); err != nil {
		t.Fatalf("Offer 1: %v", err)
	}
	fireHook(t, c, hookStdin(path, false))
	if got, err := c.Consumed(); err != nil || !got {
		t.Fatalf("first delivery Consumed() = %v, %v; want true, nil", got, err)
	}

	// Second delivery of the SAME text. The hook fires and claims it — so a
	// receipt exists and the transcript is readable — but the CLI writes no
	// record, which is precisely what happens when the hook overruns its
	// timeout and its output is discarded.
	//
	// This is the state that isolates the cursor. The predecessor's record is
	// still sitting in the transcript, identical in every byte, and a matcher
	// that rescanned from the top would credit it and ack a message that was
	// silently dropped.
	if err := c.Offer("dlv-2", text); err != nil {
		t.Fatalf("Offer 2: %v", err)
	}
	fireHook(t, c, hookStdin(path, false))
	if got, err := c.Consumed(); err != nil || got {
		t.Fatalf("second delivery Consumed() = %v, %v with no record of its own; want false, nil — "+
			"it was acked against its predecessor's identical record", got, err)
	}

	// Its own record lands: only now is it consumed.
	appendTranscript(t, path, feedback)
	if got, err := c.Consumed(); err != nil || !got {
		t.Fatalf("second delivery Consumed() = %v, %v after its own record landed; want true, nil", got, err)
	}
}

// TestStopHookChannel_ConsumedIgnoresAHalfWrittenTrailingLine covers the
// transcript being appended to live: a poll can land mid-write, and a matcher
// that credited a truncated line would advance its cursor past a record it
// never actually read.
func TestStopHookChannel_ConsumedIgnoresAHalfWrittenTrailingLine(t *testing.T) {
	const text = "half-written"
	full := jsonLine(t, map[string]any{
		"type": "user", "isMeta": true,
		"message": map[string]any{"role": "user", "content": "Stop hook feedback:\n" + text},
	})

	c := newTestStopHookChannel(t)
	// The record is present but its line has no terminator yet.
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if err := c.Offer("dlv-1", text); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	fireHook(t, c, hookStdin(path, false))

	if got, err := c.Consumed(); err != nil || got {
		t.Fatalf("Consumed() = %v, %v on a half-written line; want false, nil", got, err)
	}
	// Completing the line makes the same record count, from the same cursor.
	appendTranscript(t, path, "")
	if got, err := c.Consumed(); err != nil || !got {
		t.Fatalf("Consumed() = %v, %v once the line was terminated; want true, nil", got, err)
	}
}

// TestStopHookChannel_RetractDistinguishesUnclaimedFromClaimed pins the fact
// the runner's dead-letter path acts on.
//
// retracted == true is a guarantee: the message was still in the drop and is
// now gone, so no later hook fire can emit it. retracted == false is the
// opposite fact and must never be reported as the first one — the harness has
// already taken the message, and whether it landed is Consumed's question.
func TestStopHookChannel_RetractDistinguishesUnclaimedFromClaimed(t *testing.T) {
	t.Run("unclaimed offer is withdrawn", func(t *testing.T) {
		c := newTestStopHookChannel(t)
		if err := c.Offer("dlv-1", "hello"); err != nil {
			t.Fatalf("Offer: %v", err)
		}
		retracted, err := c.Retract()
		if err != nil || !retracted {
			t.Fatalf("Retract() = %v, %v; want true, nil", retracted, err)
		}
		if got := fireHook(t, c, hookStdin("/tmp/t.jsonl", false)); got != "" {
			t.Fatalf("a retracted message was still emitted to the harness: %q", got)
		}
	})

	t.Run("already-claimed offer cannot be withdrawn", func(t *testing.T) {
		c := newTestStopHookChannel(t)
		if err := c.Offer("dlv-1", "hello"); err != nil {
			t.Fatalf("Offer: %v", err)
		}
		fireHook(t, c, hookStdin("/tmp/t.jsonl", false))
		retracted, err := c.Retract()
		if err != nil {
			t.Fatalf("Retract: %v", err)
		}
		if retracted {
			t.Fatal("Retract() claimed to withdraw a message the harness had already taken")
		}
	})

	t.Run("nothing outstanding", func(t *testing.T) {
		c := newTestStopHookChannel(t)
		retracted, err := c.Retract()
		if err != nil || retracted {
			t.Fatalf("Retract() = %v, %v with nothing offered; want false, nil", retracted, err)
		}
	})
}

// TestStopHookChannel_OfferClearsThePreviousDeliverysEvidence stops the
// cheapest possible false ack: a fresh offer answered by the last delivery's
// claim receipt, which would report the new message consumed before the harness
// had been near it.
func TestStopHookChannel_OfferClearsThePreviousDeliverysEvidence(t *testing.T) {
	c := newTestStopHookChannel(t)
	path := writeTranscript(t, []string{jsonLine(t, map[string]any{
		"type": "user", "isMeta": true,
		"message": map[string]any{"role": "user", "content": "Stop hook feedback:\nfirst"},
	})})

	if err := c.Offer("dlv-1", "first"); err != nil {
		t.Fatalf("Offer 1: %v", err)
	}
	fireHook(t, c, hookStdin(path, false))
	if got, _ := c.Consumed(); !got {
		t.Fatal("first delivery was never consumed; the fixture is wrong")
	}

	if err := c.Offer("dlv-2", "second"); err != nil {
		t.Fatalf("Offer 2: %v", err)
	}
	for _, f := range []string{stopHookClaimedFile, stopHookStdinFile} {
		if _, err := os.Stat(c.path(f)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the next offer (stat err = %v) — the new message can be "+
				"answered by the old one's receipt", f, err)
		}
	}
	if got, err := c.Consumed(); err != nil || got {
		t.Fatalf("Consumed() = %v, %v on a freshly-offered message; want false, nil", got, err)
	}
}

// TestStopHookSettings_DeclareTheStopHookAndNothingElse checks the one flag
// value that reaches the CLI.
//
// It declares ONLY `hooks`. --settings is applied additively on top of whichever
// base layer the CLI selects, so anything else declared here would silently
// override an operator's own setting for every interactive session.
func TestStopHookSettings_DeclareTheStopHookAndNothingElse(t *testing.T) {
	c := newTestStopHookChannel(t)
	raw, err := c.settingsJSON()
	if err != nil {
		t.Fatalf("settingsJSON: %v", err)
	}

	var settings map[string]any
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		t.Fatalf("settings is not a JSON string the CLI can parse: %v (%q)", err, raw)
	}
	if keys := sortedKeys(settings); !slices.Equal(keys, []string{"hooks"}) {
		t.Fatalf("settings declares %v; want only [hooks] — --settings merges, so anything "+
			"else here overrides the operator's own configuration", keys)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if keys := sortedKeys(hooks); !slices.Equal(keys, []string{"Stop"}) {
		t.Fatalf("hooks declares %v; want only [Stop]", keys)
	}

	entry := hooks["Stop"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if entry["type"] != "command" {
		t.Fatalf("hook type = %v; want command", entry["type"])
	}
	if got, ok := entry["timeout"].(float64); !ok || got <= 0 {
		t.Fatalf("hook timeout = %v; an absent or zero timeout inherits the CLI's 60 s default, "+
			"which is 60 s of a stalled turn when the filesystem hangs", entry["timeout"])
	}
	cmd, _ := entry["command"].(string)
	if !strings.Contains(cmd, stopHookScriptFile) {
		t.Fatalf("hook command %q does not invoke the generated script", cmd)
	}
	if _, err := os.Stat(c.path(stopHookScriptFile)); err != nil {
		t.Fatalf("the hook command points at a script that is not on disk: %v", err)
	}
}

// TestStopHookChannel_LivesOutsideTheWorkspace keeps the channel's machinery out
// of any tree the session's agent can see.
//
// A hook script or settings file inside the worktree is a file the agent can
// read, edit, or `git add -A` by accident — and the CLI accepts its --settings
// as a JSON string, so nothing about this channel needs to exist there.
func TestStopHookChannel_LivesOutsideTheWorkspace(t *testing.T) {
	c := newTestStopHookChannel(t)
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	dir, err := filepath.EvalSymlinks(c.dir)
	if err != nil {
		t.Fatalf("resolve drop dir: %v", err)
	}
	if !strings.HasPrefix(dir, tmp) {
		t.Fatalf("drop dir %q is not under the system temp dir %q", dir, tmp)
	}

	raw, err := c.settingsJSON()
	if err != nil {
		t.Fatalf("settingsJSON: %v", err)
	}
	if !strings.Contains(raw, "{") {
		t.Fatalf("settings must be passed as a JSON STRING, not a file path: %q", raw)
	}

	if err := c.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(c.dir); !os.IsNotExist(err) {
		t.Fatalf("the drop directory outlived the session (stat err = %v)", err)
	}
	// close is wired into per-session cleanup, which can run more than once.
	if err := c.close(); err != nil {
		t.Fatalf("close is not idempotent: %v", err)
	}
	t.Cleanup(func() {}) // the harness cleanup already ran above
}

// TestClaudeDeclaresAndImplementsItsNoticeChannel ties the declaration to the
// implementation.
//
// The manifest saying `hook` is what the runner routes on — it never looks at a
// harness's name — so a manifest that declares a mechanism the harness cannot
// serve produces a session that accepts messages into a door that does not
// exist. Both halves are asserted here so neither can move alone.
func TestClaudeDeclaresAndImplementsItsNoticeChannel(t *testing.T) {
	if got := (&Provider{}).Manifest().Caps.NoticeDelivery; got != agent.NoticeDeliveryHook {
		t.Fatalf("claude declares notice delivery %q; want %q", got, agent.NoticeDeliveryHook)
	}
	var h any = &interactiveHandle{}
	if _, ok := h.(agent.NoticeChannelCapable); !ok {
		t.Fatal("the claude interactive handle does not expose agent.NoticeChannelCapable, " +
			"so every message routed to its declared channel would be dead-lettered")
	}
	if _, ok := h.(agent.InteractiveCapable); !ok {
		t.Fatal("wrapping the PTY handle lost agent.InteractiveCapable — the session would spawn " +
			"with no live PTY surface at all")
	}
}

// TestInteractiveArgs_NoticeSettingsPrecedeThePositionalPrompt pins the two
// argv properties that make the flag work at all.
//
// ORDER: the CLI takes the seed prompt as a POSITIONAL argument, so every
// flag-shaped argument must precede it. A --settings emitted after the prompt
// would consume the prompt as its own value and the session would start bare.
//
// ABSENCE: a session with no notice channel must not carry the flag. Passing
// an empty --settings is not a no-op — it is a malformed value the CLI has to
// interpret, on a spawn path that has nothing to gain from it.
func TestInteractiveArgs_NoticeSettingsPrecedeThePositionalPrompt(t *testing.T) {
	const settings = `{"hooks":{"Stop":[]}}`
	spec := agent.Spec{Prompt: "fix the bug", Autonomous: true}

	got := interactiveArgsWith(spec, "", settings)
	want := []string{
		"--settings", settings,
		"--permission-mode", "bypassPermissions",
		"fix the bug",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("interactiveArgsWith = %q; want %q", got, want)
	}

	if bare := interactiveArgs(spec); slices.Contains(bare, "--settings") {
		t.Fatalf("a session with no notice channel still carried --settings: %q", bare)
	}
	// --setting-sources selects the BASE layer; --settings merges on top of it.
	// Passing it would strip the operator's own configuration from every
	// interactive session, which is not this flag's job.
	if slices.Contains(got, "--setting-sources") {
		t.Fatalf("the notice channel suppressed the operator's base settings: %q", got)
	}
}

// ─── fixtures ──────────────────────────────────────────────────────────────

func jsonLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(b)
}

func writeTranscript(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func appendTranscript(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer f.Close() //nolint:errcheck // test fixture
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("append transcript: %v", err)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
