package runner

import (
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/internal/interview"
)

// TestBuildInterviewSystemPrompt_PrependsPersona verifies the persona is
// position-0 (prepended, not appended) so the hard rules are the first thing
// the model reads — surviving a cloned-repo CLAUDE.md that showcases
// developer behaviour (REN-1570 proves the hostile case in a live sandbox).
func TestBuildInterviewSystemPrompt_PrependsPersona(t *testing.T) {
	upstream := "## Phase guidance from the compiled interview definition"
	got := buildInterviewSystemPrompt(upstream, interview.InterviewCompleteSentinel)

	if !strings.HasPrefix(got, "# INTERVIEW MODE") {
		t.Fatalf("persona must be position-0; got prefix %q", got[:min(40, len(got))]) //nolint:gocritic // builtin min
	}
	if !strings.Contains(got, upstream) {
		t.Fatal("upstream system prompt must be preserved after the persona")
	}
	personaIdx := strings.Index(got, "# INTERVIEW MODE")
	upstreamIdx := strings.Index(got, upstream)
	if personaIdx > upstreamIdx {
		t.Fatal("persona must come before the upstream prompt")
	}
}

// TestBuildInterviewSystemPrompt_SubstitutesSentinel verifies the exact
// completion sentinel is embedded so the persona and the runner's exit
// watcher agree on the same string.
func TestBuildInterviewSystemPrompt_SubstitutesSentinel(t *testing.T) {
	got := buildInterviewSystemPrompt("", interview.InterviewCompleteSentinel)
	if !strings.Contains(got, interview.InterviewCompleteSentinel) {
		t.Fatalf("persona must embed the completion sentinel %q", interview.InterviewCompleteSentinel)
	}
	if strings.Contains(got, "%s") {
		t.Fatal("the placeholder must be substituted, not left literal")
	}
}

// TestBuildInterviewSystemPrompt_HardRules asserts the persona states the
// turn-taking + no-code-authoring + no-AskUserQuestion rules so the agent's
// behaviour is pinned regardless of any repository CLAUDE.md.
func TestBuildInterviewSystemPrompt_HardRules(t *testing.T) {
	got := buildInterviewSystemPrompt("", interview.InterviewCompleteSentinel)
	for _, want := range []string{
		"ONE QUESTION PER TURN",
		"NO CODE AUTHORING",
		"AskUserQuestion",
		"CLAUDE.md", // explicitly overrides repo CLAUDE.md
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("persona missing hard-rule marker %q", want)
		}
	}
}

// TestBuildInterviewSystemPrompt_EmptyUpstream verifies the persona stands
// alone when no upstream override was supplied.
func TestBuildInterviewSystemPrompt_EmptyUpstream(t *testing.T) {
	got := buildInterviewSystemPrompt("   \n\t ", interview.InterviewCompleteSentinel)
	if !strings.HasPrefix(got, "# INTERVIEW MODE") {
		t.Fatal("persona must stand alone on empty/whitespace upstream")
	}
	// No trailing upstream block, so the persona's closing divider is the tail.
	if strings.Count(got, "# INTERVIEW MODE") != 1 {
		t.Fatal("persona must not be duplicated")
	}
}
