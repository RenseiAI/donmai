package conformance

import (
	"context"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// toolCallScript is a turn whose work is a side-effecting tool call that
// carries the prompt, and which then reports it: running the turn twice
// appends twice, and the second run's narrative repeats the nonce.
func toolCallScript(sessionID, prompt string) []agent.Event {
	return []agent.Event{
		agent.InitEvent{SessionID: sessionID},
		agent.ToolUseEvent{ToolName: "Bash", ToolUseID: "t1", Input: map[string]any{"command": "printf '%s' '" + prompt + "' >> side-effects.log"}},
		agent.ToolResultEvent{ToolName: "Bash", ToolUseID: "t1", Content: "ok"},
		agent.AssistantTextEvent{Text: "appended: " + prompt},
		agent.ResultEvent{Success: true, Message: "done"},
	}
}

// toolOutputOnlyScript echoes on the first run; on a resume its only trace
// of the prior history is a tool's output (a file read back from the
// workarea), never anything the session itself reports.
func toolOutputOnlyScript(sessionID, prompt string) []agent.Event {
	if !strings.HasPrefix(prompt, "resumed history: ") {
		return defaultScript(sessionID, prompt)
	}
	return []agent.Event{
		agent.InitEvent{SessionID: sessionID},
		agent.ToolUseEvent{ToolName: "Bash", ToolUseID: "t1", Input: map[string]any{"command": "cat side-effects.log"}},
		agent.ToolResultEvent{ToolName: "Bash", ToolUseID: "t1", Content: prompt},
		agent.AssistantTextEvent{Text: "read the log"},
		agent.ResultEvent{Success: true, Message: "done"},
	}
}

// TestResumeHistoryLoadedRequiresPriorHistory is the red proof for the
// stop-for-resume fixture beside checkResumeContinues: the first session is
// stopped through the suite's stop path, a FRESH adapter instance resumes it
// under the old session id, and the harness must report the prior history.
// A harness that resumes blank fails, and so does one whose history lived
// only in the spawning instance's memory or that runs the prior turn again;
// one that replays persisted history passes; one that never declared resume,
// or offers no fresh instance to resume on, is not applicable.
func TestResumeHistoryLoadedRequiresPriorHistory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  fakeConfig
		// persist gives the fake a declared on-disk state dir, the only
		// thing a fresh instance can read the prior history from.
		persist    bool
		subject    func(Subject) Subject
		wantStatus Status
		wantReason string
		wantTier   bool
	}{
		{
			name:       "a harness that reports the prior history passes",
			cfg:        fakeConfig{supportResume: true, resumeHistory: resumeReportsHistory},
			persist:    true,
			wantStatus: StatusPass,
			wantTier:   true,
		},
		{
			name:       "a harness that resumes blank fails",
			cfg:        fakeConfig{supportResume: true, resumeHistory: resumeBlank},
			persist:    true,
			wantStatus: StatusFail,
			wantReason: "resumed blank",
		},
		{
			name:       "history kept only in process memory resumes blank in a new process",
			cfg:        fakeConfig{supportResume: true, resumeHistory: resumeReportsHistory},
			wantStatus: StatusFail,
			wantReason: "resumed blank",
		},
		{
			name:       "a resume that runs the prior turn's tool call again fails",
			cfg:        fakeConfig{supportResume: true, resumeHistory: resumeReruns, script: toolCallScript},
			persist:    true,
			wantStatus: StatusFail,
			wantReason: "tool call again",
		},
		{
			name:       "a tool's output is not history the session reports",
			cfg:        fakeConfig{supportResume: true, resumeHistory: resumeReportsHistory, script: toolOutputOnlyScript},
			persist:    true,
			wantStatus: StatusFail,
			wantReason: "resumed blank",
		},
		{
			name: "a first session that never echoed its nonce establishes no history",
			cfg: fakeConfig{supportResume: true, resumeHistory: resumeReportsHistory, script: func(id, _ string) []agent.Event {
				return defaultScript(id, "a reply that drops the prompt")
			}},
			persist:    true,
			wantStatus: StatusFail,
			wantReason: "never repeated its own prompt nonce",
		},
		{
			name:       "resume declared but refused fails",
			cfg:        fakeConfig{supportResume: true, resumeErr: agent.ErrUnsupported},
			persist:    true,
			wantStatus: StatusFail,
			wantReason: "Resume(",
		},
		{
			name:    "the spawning instance handed back as fresh fails",
			cfg:     fakeConfig{supportResume: true, resumeHistory: resumeReportsHistory},
			persist: true,
			subject: func(s Subject) Subject {
				spawner := s.Provider
				s.ResumeProvider = func(context.Context) (agent.HarnessProvider, error) { return spawner, nil }
				return s
			},
			wantStatus: StatusFail,
			wantReason: "spawning instance",
		},
		{
			name:    "no fresh instance to resume on is not applicable, never a pass",
			cfg:     fakeConfig{supportResume: true, resumeHistory: resumeReportsHistory},
			persist: true,
			subject: func(s Subject) Subject {
				s.ResumeProvider = nil
				return s
			},
			wantStatus: StatusNotApplicable,
			wantReason: "ResumeProvider is unset",
		},
		{
			name:       "no declared resume is not applicable, never a pass",
			cfg:        fakeConfig{},
			wantStatus: StatusNotApplicable,
			wantReason: "does not declare session resume",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			if tc.persist {
				cfg.stateDir = t.TempDir()
			}
			subject := conformantSubject(cfg)
			if tc.subject != nil {
				subject = tc.subject(subject)
			}
			report := runSubject(t, subject)

			res := mustResult(t, report, IDResumeHistoryLoaded)
			if res.Status != tc.wantStatus {
				t.Fatalf("resume/history-loaded = %q, want %q (reason %q)\n%s", res.Status, tc.wantStatus, res.Reason, report.Text())
			}
			if tc.wantReason != "" && !strings.Contains(res.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", res.Reason, tc.wantReason)
			}
			if got := report.Earned(TierResume); got != tc.wantTier {
				t.Errorf("resume tier earned = %v, want %v\n%s", got, tc.wantTier, report.Text())
			}
			// The fixture is not a second name for session-continues: a
			// blank-but-conformant resume passes session-continues (the
			// resumed stream is conformant) while history-loaded fails (it
			// carries no history).
			cont := mustResult(t, report, IDResumeContinues)
			if tc.cfg.resumeHistory == resumeBlank && tc.cfg.resumeErr == nil {
				if cont.Status != StatusPass {
					t.Errorf("resume/session-continues = %q for a blank-but-conformant resume, want pass (reason %q)", cont.Status, cont.Reason)
				}
			}
		})
	}
}

// TestResumeHistoryLoadedSurvivesAResumedPrompt ensures the fixture asks
// about the HISTORY, not the resume call's own fresh prompt: the resumed
// stream carries a new nonce the base session never saw, and the check must
// still demand the base nonce back.
func TestResumeHistoryLoadedSurvivesAResumedPrompt(t *testing.T) {
	t.Parallel()
	report := runSubject(t, conformantSubject(fakeConfig{
		supportResume: true, resumeHistory: resumeReportsHistory, stateDir: t.TempDir(),
	}))
	res := mustResult(t, report, IDResumeHistoryLoaded)
	if res.Status != StatusPass {
		t.Fatalf("resume/history-loaded = %q, want pass (reason %q)\n%s", res.Status, res.Reason, report.Text())
	}
}
