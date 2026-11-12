package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/runtime/state"
)

// TestTruncateToolOutput pins the runner's tool-output bound: content
// within the limit passes through untouched, oversize content is
// middle-truncated around a marker that names the truncation, and the
// cut is rune-safe on both sides.
func TestTruncateToolOutput(t *testing.T) {
	t.Parallel()
	t.Run("within limit passes through", func(t *testing.T) {
		t.Parallel()
		got, changed := truncateToolOutput("ok", 256)
		if changed || got != "ok" {
			t.Fatalf("truncateToolOutput = %q, %v; want %q, false", got, changed, "ok")
		}
	})
	t.Run("empty passes through", func(t *testing.T) {
		t.Parallel()
		got, changed := truncateToolOutput("", 256)
		if changed || got != "" {
			t.Fatalf("truncateToolOutput = %q, %v; want empty, false", got, changed)
		}
	})
	t.Run("non-positive limit passes through", func(t *testing.T) {
		t.Parallel()
		big := strings.Repeat("x", 4096)
		got, changed := truncateToolOutput(big, 0)
		if changed || got != big {
			t.Fatalf("truncateToolOutput with limit 0 changed %d bytes; want passthrough", len(got))
		}
	})
	t.Run("oversize truncates around a marker within the limit", func(t *testing.T) {
		t.Parallel()
		const limit = 256
		content := "HEAD:" + strings.Repeat("a", 4000) + ":TAIL"
		got, changed := truncateToolOutput(content, limit)
		if !changed {
			t.Fatal("truncateToolOutput reported no truncation for oversize content")
		}
		if len(got) > limit {
			t.Fatalf("truncated length = %d; want at most %d", len(got), limit)
		}
		if !strings.Contains(got, toolOutputTruncatedInfix) {
			t.Fatalf("truncated output missing the marker %q: %q", toolOutputTruncatedInfix, got)
		}
		if !strings.Contains(got, "4010-byte output exceeds the 256-byte limit") {
			t.Fatalf("marker must name the sizes; got %q", got)
		}
		if !strings.HasPrefix(got, "HEAD:") {
			t.Fatalf("truncated output lost the head: %q", got)
		}
		if !strings.HasSuffix(got, ":TAIL") {
			t.Fatalf("truncated output lost the tail: %q", got)
		}
		if !utf8.ValidString(got) {
			t.Fatal("truncated output is not valid UTF-8")
		}
	})
	t.Run("multibyte runes never split", func(t *testing.T) {
		t.Parallel()
		const limit = 64
		content := strings.Repeat("é", 200) + strings.Repeat("a", 200)
		got, changed := truncateToolOutput(content, limit)
		if !changed {
			t.Fatal("truncateToolOutput reported no truncation for oversize content")
		}
		if !utf8.ValidString(got) {
			t.Fatalf("truncated output split a rune: %q", got)
		}
		if len(got) > limit {
			t.Fatalf("truncated length = %d; want at most %d", len(got), limit)
		}
		if !strings.Contains(got, toolOutputTruncatedInfix) {
			t.Fatalf("truncated output missing the marker: %q", got)
		}
	})
	t.Run("tiny limit still names the truncation", func(t *testing.T) {
		t.Parallel()
		got, changed := truncateToolOutput(strings.Repeat("x", 4096), 32)
		if !changed {
			t.Fatal("truncateToolOutput reported no truncation for oversize content")
		}
		if len(got) > 32 {
			t.Fatalf("truncated length = %d; want at most 32", len(got))
		}
		if !strings.Contains(got, "tool output") {
			t.Fatalf("degenerate truncation lost the marker: %q", got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("degenerate truncation is not valid UTF-8: %q", got)
		}
	})
}

// TestAbnormalContinueTurnEndCause pins the delivery-gate guard directly:
// an abnormal turn reports its own stable cause, a normally completed
// turn falls through to the delivery check, and an already-recorded
// failure is never relabelled.
func TestAbnormalContinueTurnEndCause(t *testing.T) {
	t.Parallel()
	clean := streamObservation{terminalEvent: &agent.ResultEvent{Success: true}, terminalSuccess: true}
	oversize := streamObservation{
		terminalEvent:       &agent.ResultEvent{Success: true},
		terminalSuccess:     true,
		toolOutputTruncated: true,
		toolOutputBytes:     300000,
		toolOutputLimit:     DefaultMaxToolOutputBytes,
	}
	cases := []struct {
		name        string
		resStatus   string
		resMode     string
		stream      streamObservation
		last        streamObservation
		wantAbnorm  bool
		wantMode    string
		wantContain string
	}{
		{
			name:       "normal completion falls through",
			stream:     clean,
			last:       clean,
			wantAbnorm: false,
		},
		{
			name:        "provider error on the latest turn is the cause",
			stream:      clean,
			last:        streamObservation{terminalEvent: &agent.ResultEvent{Success: true}, terminalSuccess: true, providerError: "model exploded"},
			wantAbnorm:  true,
			wantMode:    FailureProviderError,
			wantContain: "model provider error",
		},
		{
			name:        "provider error on the first turn is the cause",
			stream:      streamObservation{providerError: "gateway 503"},
			last:        clean,
			wantAbnorm:  true,
			wantMode:    FailureProviderError,
			wantContain: "gateway 503",
		},
		{
			name:        "silent close is the cause, not the draft message",
			stream:      streamObservation{},
			last:        streamObservation{},
			wantAbnorm:  true,
			wantMode:    FailureSilentExit,
			wantContain: "without a terminal result",
		},
		{
			name:        "failed terminal is the cause, not the draft message",
			stream:      streamObservation{terminalEvent: &agent.ResultEvent{Success: false, Errors: []string{"exit status 1"}}, terminalSuccess: false},
			last:        streamObservation{terminalEvent: &agent.ResultEvent{Success: false}, terminalSuccess: false},
			wantAbnorm:  true,
			wantMode:    FailureSilentExit,
			wantContain: "reported failure",
		},
		{
			name:        "silent close after oversize names the oversize",
			stream:      streamObservation{toolOutputTruncated: true, toolOutputBytes: 300000, toolOutputLimit: DefaultMaxToolOutputBytes},
			last:        streamObservation{},
			wantAbnorm:  true,
			wantMode:    FailureSilentExit,
			wantContain: "exceeded the 262144-byte limit",
		},
		{
			name:       "already recorded failure is never relabelled",
			resStatus:  "failed",
			resMode:    FailureBudgetExceeded,
			stream:     streamObservation{},
			last:       streamObservation{},
			wantAbnorm: false,
		},
		{
			name:       "oversize alone never fails a normal turn",
			stream:     oversize,
			last:       oversize,
			wantAbnorm: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := &Result{}
			res.Status = tc.resStatus
			res.FailureMode = tc.resMode
			mode, message, abnormal := abnormalContinueTurnEndCause(res, tc.stream, tc.last, 12, DefaultMaxToolOutputBytes)
			if abnormal != tc.wantAbnorm {
				t.Fatalf("abnormal = %v; want %v (mode=%q message=%q)", abnormal, tc.wantAbnorm, mode, message)
			}
			if !tc.wantAbnorm {
				return
			}
			if mode != tc.wantMode {
				t.Errorf("mode = %q; want %q", mode, tc.wantMode)
			}
			if !strings.Contains(message, tc.wantContain) {
				t.Errorf("message = %q; want it to contain %q", message, tc.wantContain)
			}
			if strings.Contains(message, "still a draft") {
				t.Errorf("message = %q; an abnormal turn must never report the draft message", message)
			}
		})
	}
}

// TestRun_ContinueModeSilentTurnEndRecordsHarnessCause proves the seat-turn
// death path: a harness that dies without a terminal result (channel
// close, as a killed process leaves it) records the real cause instead
// of falling through to the continue-mode contract-check message.
func TestRun_ContinueModeSilentTurnEndRecordsHarnessCause(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "development",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		turns:          []verdictScriptTurn{{silentClose: true, text: "working…"}},
	})
	if res.Status != "failed" || res.FailureMode != FailureSilentExit {
		t.Fatalf("Status = %q (%s: %s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureSilentExit)
	}
	if strings.Contains(res.Error, "still a draft") {
		t.Fatalf("Error = %q; a dead turn must not report the draft message", res.Error)
	}
	if !strings.Contains(res.Error, "without a terminal result") {
		t.Fatalf("Error = %q; want the harness-death cause", res.Error)
	}
}

// TestRun_ContinueModeCrashTurnEndRecordsProviderCause proves the
// harness-error path: a turn that ends on an error event records the
// provider cause instead of the draft message.
func TestRun_ContinueModeCrashTurnEndRecordsProviderCause(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "development",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		turns:          []verdictScriptTurn{{crash: true}},
	})
	if res.Status != "failed" || res.FailureMode != FailureProviderError {
		t.Fatalf("Status = %q (%s: %s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureProviderError)
	}
	if strings.Contains(res.Error, "still a draft") {
		t.Fatalf("Error = %q; a crashed turn must not report the draft message", res.Error)
	}
}

// TestRun_ContinueModeFailedTerminalTurnEndRecordsCause proves the
// harness-reported-failure path: a terminal result that reports failure
// records that cause instead of the draft message.
func TestRun_ContinueModeFailedTerminalTurnEndRecordsCause(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "development",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		turns: []verdictScriptTurn{{
			events: []agent.Event{agent.ResultEvent{Success: false, Errors: []string{"harness gave up: exit status 1"}}},
		}},
	})
	if res.Status != "failed" || res.FailureMode != FailureSilentExit {
		t.Fatalf("Status = %q (%s: %s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureSilentExit)
	}
	if strings.Contains(res.Error, "still a draft") {
		t.Fatalf("Error = %q; a failed turn must not report the draft message", res.Error)
	}
	if !strings.Contains(res.Error, "harness gave up") {
		t.Fatalf("Error = %q; want the harness-reported cause", res.Error)
	}
}

// TestRun_ContinueModeSilentTurnEndAfterOversizeNamesBoth proves the
// oversize path: a turn that dies after a tool result went over the size
// limit names the oversize in the terminal reason, so the death reads as
// the oversized output it followed.
func TestRun_ContinueModeSilentTurnEndAfterOversizeNamesBoth(t *testing.T) {
	big := "log line\n" + strings.Repeat("x", 4096) + "\nhttps://github.com/example/repo/pull/7\n"
	res, _ := runScriptedSession(t, scriptedSession{
		workType:        "development",
		skipSteering:    true,
		repository:      "https://github.com/example/repo",
		continueNumber:  12,
		toolOutputLimit: 256,
		turns: []verdictScriptTurn{{
			silentClose: true,
			events: []agent.Event{
				agent.ToolUseEvent{ToolName: "bash", ToolUseID: "call-1"},
				agent.ToolResultEvent{ToolName: "bash", ToolUseID: "call-1", Content: big},
			},
		}},
	})
	if res.Status != "failed" || res.FailureMode != FailureSilentExit {
		t.Fatalf("Status = %q (%s: %s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureSilentExit)
	}
	if strings.Contains(res.Error, "still a draft") {
		t.Fatalf("Error = %q; a dead turn must not report the draft message", res.Error)
	}
	if !strings.Contains(res.Error, "exceeded the 256-byte limit") {
		t.Fatalf("Error = %q; want the oversize cause", res.Error)
	}
	if !sawToolOutputTruncationWarning(res) {
		t.Fatalf("PostSessionWarnings = %q; want the truncation warning", res.PostSessionWarnings)
	}
}

// TestRun_ToolOutputOversizeTruncatesAndContinues proves the protective
// path: an oversized tool result is truncated with a marker and the seat
// keeps going on the bounded copy — oversize alone never fails the turn.
func TestRun_ToolOutputOversizeTruncatesAndContinues(t *testing.T) {
	big := "command echo\n" + strings.Repeat("x", 4096) + "\nhttps://github.com/example/repo/pull/7\n"
	res, _ := runScriptedSession(t, scriptedSession{
		workType:        "development",
		skipSteering:    true,
		repository:      "https://github.com/example/repo",
		continueNumber:  12,
		toolOutputLimit: 256,
		turns: []verdictScriptTurn{{
			manifest: passedManifest,
			text:     "WORK_RESULT:passed",
			events: []agent.Event{
				agent.ToolUseEvent{ToolName: "bash", ToolUseID: "call-1"},
				agent.ToolResultEvent{ToolName: "bash", ToolUseID: "call-1", Content: big},
			},
		}},
	})
	if res.Status != "completed" {
		t.Fatalf("Status = %q (%s: %s); oversize alone must not fail the turn", res.Status, res.FailureMode, res.Error)
	}
	if res.FailureMode != "" || res.Error != "" {
		t.Fatalf("FailureMode = %q Error = %q; want both empty on completed", res.FailureMode, res.Error)
	}
	if !sawToolOutputTruncationWarning(res) {
		t.Fatalf("PostSessionWarnings = %q; want the truncation warning", res.PostSessionWarnings)
	}
	// The retained audit copy carries the marker, not the flood: the
	// 4 KiB run of output never lands verbatim in the event log.
	body, err := os.ReadFile(filepath.Join(res.WorktreePath, state.AgentDirName, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events.jsonl: %v", err)
	}
	if !strings.Contains(string(body), toolOutputTruncatedInfix) {
		t.Fatal("events.jsonl missing the truncation marker")
	}
	if strings.Contains(string(body), strings.Repeat("x", 1000)) {
		t.Fatal("events.jsonl carries the unbounded tool output verbatim")
	}
}

// TestRun_ContinueModeNormalDraftTurnStillReportsDraft pins the other
// side of the guard: a turn that completed normally but delivered no code
// on a draft still reports the draft message — only abnormal turns bypass
// it.
func TestRun_ContinueModeNormalDraftTurnStillReportsDraft(t *testing.T) {
	res, _ := runScriptedSession(t, scriptedSession{
		workType:       "development",
		skipSteering:   true,
		repository:     "https://github.com/example/repo",
		continueNumber: 12,
		backstop:       true,
		draft:          func(context.Context, string, string) (bool, error) { return true, nil },
		turns: []verdictScriptTurn{{
			manifest: failedManifest,
			text:     "WORK_RESULT:failed",
			files:    map[string]string{"fix.go": "package fix\n"},
		}},
	})
	if res.Status != "failed" || res.FailureMode != FailureBackstop {
		t.Fatalf("Status = %q (%s: %s); want failed/%s", res.Status, res.FailureMode, res.Error, FailureBackstop)
	}
	if want := "continued pull request #12 is still a draft"; res.Error != want {
		t.Fatalf("Error = %q; want exactly %q", res.Error, want)
	}
}

// sawToolOutputTruncationWarning reports whether the terminal result
// carries the tool-output truncation warning.
func sawToolOutputTruncationWarning(res *Result) bool {
	for _, w := range res.PostSessionWarnings {
		if strings.HasPrefix(w, toolOutputTruncationWarningPrefix) {
			return true
		}
	}
	return false
}
