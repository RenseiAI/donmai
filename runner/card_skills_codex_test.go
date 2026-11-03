package runner

import (
	"slices"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/internal/interview"
	"github.com/RenseiAI/donmai/prompt"
)

// TestCodexNativeCardSkills_Routing pins which sessions route card-skill
// bodies to the harness-native surface: headless codex only. Interactive
// codex keeps the prompt fallback (the interactive lane has ambient-home
// sub-paths where isolated delivery is impossible), and every other
// provider never leaves the prompt.
func TestCodexNativeCardSkills_Routing(t *testing.T) {
	t.Parallel()

	codex := &selectorFakeProvider{name: agent.ProviderCodex, harness: agent.HarnessCodex}
	claude := &selectorFakeProvider{name: agent.ProviderClaude, harness: agent.HarnessCodex}
	shell := &selectorFakeProvider{name: agent.ProviderShell, harness: agent.HarnessCodex}

	headless := QueuedWork{}
	interactive := QueuedWork{}
	interactive.Mode = interactiveRunMode
	interviewMode := QueuedWork{}
	interviewMode.Mode = interview.InterviewRunMode

	cases := []struct {
		name     string
		provider agent.Provider
		qw       QueuedWork
		want     bool
	}{
		{"codex headless routes natively", codex, headless, true},
		{"codex interview routes natively", codex, interviewMode, true},
		{"codex interactive keeps the prompt", codex, interactive, false},
		{"claude keeps the prompt", claude, headless, false},
		{"shell keeps the prompt", shell, headless, false},
		{"nil provider keeps the prompt", nil, headless, false},
	}
	for _, tc := range cases {
		if got := codexNativeCardSkills(tc.provider, tc.qw); got != tc.want {
			t.Errorf("%s: codexNativeCardSkills = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestToCardSkills_Conversion pins the wire → Spec skill shape: ids,
// bodies, and disallow lists convert, empty input stays nil, and the
// disallow slices are copies (mutating the wire afterwards must not
// move the Spec).
func TestToCardSkills_Conversion(t *testing.T) {
	t.Parallel()

	if got := toCardSkills(nil); got != nil {
		t.Errorf("toCardSkills(nil) = %v, want nil", got)
	}
	wire := []prompt.SkillSpec{
		{ID: "a", Body: "BODY-A", DisallowedTools: []string{"Bash(rm:*)"}},
		{ID: "b", Body: "   "},
	}
	got := toCardSkills(wire)
	if len(got) != 2 || got[0].ID != "a" || got[0].Body != "BODY-A" || got[1].ID != "b" {
		t.Fatalf("toCardSkills = %+v", got)
	}
	if !slices.Equal(got[0].DisallowedTools, []string{"Bash(rm:*)"}) {
		t.Fatalf("disallows = %v", got[0].DisallowedTools)
	}
	wire[0].DisallowedTools[0] = "MUTATED"
	if got[0].DisallowedTools[0] != "Bash(rm:*)" {
		t.Errorf("Spec shares the wire disallow slice: %v", got[0].DisallowedTools)
	}
}

// TestCollectInlineSkillDisallows_Union pins the runner-side enforcement
// rule shared by the prompt and native lanes: every skill's restrictions
// count, in order — including a whitespace-only body, which carries no
// guidance but still narrows the tool surface.
func TestCollectInlineSkillDisallows_Union(t *testing.T) {
	t.Parallel()

	if got := collectInlineSkillDisallows(nil); got != nil {
		t.Errorf("collectInlineSkillDisallows(nil) = %v, want nil", got)
	}
	skills := []prompt.SkillSpec{
		{ID: "a", Body: "guidance", DisallowedTools: []string{"Bash(rm:*)"}},
		{ID: "blank", Body: "   ", DisallowedTools: []string{"Bash(sudo:*)"}},
		{ID: "plain", Body: "more guidance"},
	}
	want := []string{"Bash(rm:*)", "Bash(sudo:*)"}
	if got := collectInlineSkillDisallows(skills); !slices.Equal(got, want) {
		t.Errorf("collectInlineSkillDisallows = %v, want %v", got, want)
	}
}

// preparedSourceSpecForSkillTest builds a receipted prepared spec for a
// provider name over work carrying one card skill. Server-free.
func preparedSourceSpecForSkillTest(t *testing.T, name agent.ProviderName, mode string) (agent.Spec, QueuedWork) {
	t.Helper()

	qw := exactReceiptQueuedWork("card-skill-prepared-" + string(name) + mode)
	qw.Body = "exercise card-skill prompt routing"
	qw.Mode = mode
	qw.Skills = []prompt.SkillSpec{{
		ID:              "prep-skill",
		Body:            "PREPARED_SKILL_BODY_MARKER",
		DisallowedTools: []string{"Bash(prepared-deny:*)"},
	}}
	cell := exactReceiptCell("harness/v2", "gpt-test", executioncell.SessionAutonomous, nil)
	qw = attachAdmittedExecutionCell(t, qw, cell)
	operational, err := CanonicalOperationalPayload(qw)
	if err != nil {
		t.Fatalf("CanonicalOperationalPayload: %v", err)
	}
	qw.OperationalPayload = operational
	provider := &manifestSelectorProvider{
		selectorFakeProvider: &selectorFakeProvider{name: name, harness: agent.HarnessCodex},
		manifest:             codexManifestForTest(),
		capabilities:         codexCapabilitiesForTest(),
	}
	selection := harnessSelection{
		Provider:      provider,
		receipt:       mustAdmissionReceipt(t, qw.AdmissionReceipt),
		effectiveCell: cell,
	}
	spec, _, err := buildPreparedSourceSpec(qw, selection, nil)
	if err != nil {
		t.Fatalf("buildPreparedSourceSpec: %v", err)
	}
	return spec, qw
}

// TestBuildPreparedSourceSpec_CodexHeadlessSkipsPromptFold pins the
// prepared lane's half of native routing: a headless codex prepared
// spec carries no card-skill bodies in the prompt (they ride Spec.Skills
// on the live spec), while the skill's tool restriction still lands in
// the approval bridge. Force the fold back on and this goes red.
func TestBuildPreparedSourceSpec_CodexHeadlessSkipsPromptFold(t *testing.T) {
	t.Parallel()

	spec, _ := preparedSourceSpecForSkillTest(t, agent.ProviderCodex, "")
	if spec.PromptPlan == nil || spec.PromptPlan.HarnessProtocol == nil {
		t.Fatal("prepared spec has no harness protocol prompt")
	}
	if strings.Contains(spec.PromptPlan.HarnessProtocol.Text, "PREPARED_SKILL_BODY_MARKER") {
		t.Error("headless codex prepared prompt contains the card-skill body")
	}
	if strings.Contains(spec.SystemPromptAppend, "PREPARED_SKILL_BODY_MARKER") {
		t.Error("headless codex prepared SystemPromptAppend contains the card-skill body")
	}
	if spec.PermissionConfig == nil || !slices.Contains(spec.PermissionConfig.DisallowPatterns, "Bash(prepared-deny:*)") {
		t.Errorf("headless codex prepared spec lost the skill tool restriction: %+v", spec.PermissionConfig)
	}
}

// TestBuildPreparedSourceSpec_NonCodexKeepsPromptFold pins the other
// half: providers without a native skill surface still receive the
// card-skill body in the prepared prompt.
func TestBuildPreparedSourceSpec_NonCodexKeepsPromptFold(t *testing.T) {
	t.Parallel()

	spec, _ := preparedSourceSpecForSkillTest(t, agent.ProviderClaude, "")
	if spec.PromptPlan == nil || spec.PromptPlan.HarnessProtocol == nil {
		t.Fatal("prepared spec has no harness protocol prompt")
	}
	if !strings.Contains(spec.PromptPlan.HarnessProtocol.Text, "PREPARED_SKILL_BODY_MARKER") {
		t.Error("non-codex prepared prompt lost the card-skill body")
	}
}
