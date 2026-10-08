package conformance

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
)

// TestResumeHistoryLoadedRequiresPriorHistory is the red proof for the
// stop-for-resume fixture beside checkResumeContinues: the first session is
// stopped through the suite's stop path, a new process resumes from the
// artifact under the old session id, and the harness must report the prior
// history. A harness that resumes blank fails; one that replays the history
// passes; one that never declared resume is not applicable.
func TestResumeHistoryLoadedRequiresPriorHistory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		cfg        fakeConfig
		wantStatus Status
		wantReason string
		wantTier   bool
	}{
		{
			name:       "a harness that reports the prior history passes",
			cfg:        fakeConfig{supportResume: true, resumeHistory: resumeReportsHistory},
			wantStatus: StatusPass,
			wantTier:   true,
		},
		{
			name:       "a harness that resumes blank fails",
			cfg:        fakeConfig{supportResume: true, resumeHistory: resumeBlank},
			wantStatus: StatusFail,
			wantReason: "resumed blank",
		},
		{
			name:       "resume declared but refused fails",
			cfg:        fakeConfig{supportResume: true, resumeErr: agent.ErrUnsupported},
			wantStatus: StatusFail,
			wantReason: "Resume(",
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
			report := runSubject(t, conformantSubject(tc.cfg))

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
			// The fixture shares the resume probe with session-continues:
			// a blank resume must fail BOTH, so the new check is not a
			// second name for an old verdict — session-continues passes
			// (the resumed stream is conformant) while history-loaded
			// fails (it carries no history).
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
		supportResume: true, resumeHistory: resumeReportsHistory,
	}))
	res := mustResult(t, report, IDResumeHistoryLoaded)
	if res.Status != StatusPass {
		t.Fatalf("resume/history-loaded = %q, want pass (reason %q)\n%s", res.Status, res.Reason, report.Text())
	}
}
