package runner

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/stagewire"
)

// skillHarness is a provider whose manifest declares a skill profile.
type skillHarness struct {
	profile agent.SkillDeliveryProfile
}

func (skillHarness) Name() agent.ProviderName         { return agent.ProviderPi }
func (skillHarness) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (skillHarness) Shutdown(context.Context) error   { return nil }
func (s skillHarness) Manifest() agent.HarnessManifest {
	return agent.HarnessManifest{Name: agent.HarnessPi, Skills: s.profile}
}
func (skillHarness) Spawn(context.Context, agent.Spec) (agent.Handle, error) { return nil, nil }
func (skillHarness) Resume(context.Context, string, agent.Spec) (agent.Handle, error) {
	return nil, nil
}

func legacySkills() []prompt.SkillSpec {
	return []prompt.SkillSpec{
		{ID: "a", Body: "alpha body", DisallowedTools: []string{"Bash(rm:*)"}},
		{ID: "b", Body: "   ", DisallowedTools: []string{"WebFetch"}},
		{ID: "c", Body: "gamma body"},
	}
}

// TestPlanSessionSkills_FallbackMatchesFoldInlineSkills is the byte-equality
// guarantee for the prompt fallback: for every skill the plan leaves on the
// prompt, the SkillAppend and disallowed set are exactly what
// foldInlineSkills produced from the raw wire skills before planning
// existed, on a prompt-only harness and for body-only skills on a native
// one.
func TestPlanSessionSkills_FallbackMatchesFoldInlineSkills(t *testing.T) {
	t.Parallel()
	providers := map[string]agent.Provider{
		"prompt-only harness":              skillHarness{},
		"native harness, body-only skills": skillHarness{profile: agent.SkillDeliveryProfile{Layout: agent.SkillLayoutStateDir, RepoSkills: agent.RepoSkillsByFlag}},
	}
	for name, provider := range providers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, existing := range []string{"", "kit skill text"} {
				var qw QueuedWork
				qw.Skills = legacySkills()
				skills, err := planSessionSkills(qw, provider, agent.PromptModeAutonomous)
				if err != nil {
					t.Fatal(err)
				}
				wantAppend, wantDisallow, wantCount := foldInlineSkills(existing, qw.Skills)
				gotAppend, _, gotCount := foldInlineSkills(existing, skills.residue)
				if gotAppend != wantAppend || gotCount != wantCount {
					t.Fatalf("existing=%q: append = %q (%d), want %q (%d)", existing, gotAppend, gotCount, wantAppend, wantCount)
				}
				if !reflect.DeepEqual(skills.disallowed, wantDisallow) {
					t.Fatalf("disallowed = %v, want %v", skills.disallowed, wantDisallow)
				}
			}
		})
	}
}

// TestPlanSessionSkills_NativeLeavesPromptAndKeepsDisallow proves a natively
// delivered skill's body leaves the prompt while its disallowed tools still
// narrow the session (the union is session-level).
func TestPlanSessionSkills_NativeLeavesPromptAndKeepsDisallow(t *testing.T) {
	t.Parallel()
	native := agent.SealSkillPackage(stagewire.Skill{
		ID: "review", Body: "review body", DisallowedTools: []string{"Bash(git push:*)"},
		Files: []stagewire.SkillFile{agent.NewSkillFile("SKILL.md", []byte("---\nname: review\ndescription: d\n---\n"), 0o644)},
	})
	var qw QueuedWork
	qw.Skills = []prompt.SkillSpec{native, {ID: "legacy", Body: "legacy body", DisallowedTools: []string{"WebFetch"}}}
	qw.RepoSkills = &stagewire.RepoSkills{Policy: stagewire.RepoSkillsNone}
	provider := skillHarness{profile: agent.SkillDeliveryProfile{Layout: agent.SkillLayoutStateDir, RepoSkills: agent.RepoSkillsByFlag}}
	skills, err := planSessionSkills(qw, provider, agent.PromptModeAutonomous)
	if err != nil {
		t.Fatal(err)
	}
	appendText, _, _ := foldInlineSkills("", skills.residue)
	if appendText != "legacy body" {
		t.Fatalf("SkillAppend = %q, want only the prompt-delivered body", appendText)
	}
	if want := []string{"Bash(git push:*)", "WebFetch"}; !reflect.DeepEqual(skills.disallowed, want) {
		t.Fatalf("disallowed = %v, want %v", skills.disallowed, want)
	}
	if !skills.delivery.HasNative() || skills.delivery.Plan.RepoSkills.Achieved != stagewire.RepoSkillsNone {
		t.Fatalf("plan = %s", skills.delivery.Plan.Summary())
	}

	// The same work item in an interactive session falls back to the prompt.
	interactive, err := planSessionSkills(qw, provider, agent.PromptModeHumanControlled)
	if err != nil {
		t.Fatal(err)
	}
	if text, _, _ := foldInlineSkills("", interactive.residue); text != "review body\n\nlegacy body" {
		t.Fatalf("interactive SkillAppend = %q", text)
	}
}

// TestPlanSessionSkills_PromptFallbackIsCapped proves an oversized fallback
// no longer rides the system prompt whole: the skill that does not fit is
// omitted and recorded, and the rest still lands.
func TestPlanSessionSkills_PromptFallbackIsCapped(t *testing.T) {
	t.Parallel()
	var qw QueuedWork
	qw.Skills = []prompt.SkillSpec{
		{ID: "fits", Body: "small body"},
		{ID: "huge", Body: strings.Repeat("x", agent.MaxPromptSkillBytes)},
	}
	skills, err := planSessionSkills(qw, skillHarness{}, agent.PromptModeAutonomous)
	if err != nil {
		t.Fatal(err)
	}
	appendText, _, injected := foldInlineSkills("", skills.residue)
	if appendText != "small body" || injected != 1 {
		t.Fatalf("SkillAppend = %d bytes (%d skills), want only the skill that fits", len(appendText), injected)
	}
	if entry := skills.delivery.Plan.Entries[1]; entry.Delivery != agent.SkillNotDelivered || entry.Reason != agent.SkillReasonPromptCap {
		t.Fatalf("oversized entry = %+v, want omitted for the prompt cap", entry)
	}
}

// TestPlanSessionSkills_NativeRequiredRefuses proves a required native
// skill on a harness that cannot deliver it refuses the run.
func TestPlanSessionSkills_NativeRequiredRefuses(t *testing.T) {
	t.Parallel()
	var qw QueuedWork
	qw.Skills = []prompt.SkillSpec{{ID: "must", Body: "b", Delivery: stagewire.SkillDeliveryNativeRequired}}
	if _, err := planSessionSkills(qw, skillHarness{}, agent.PromptModeAutonomous); err == nil || !strings.Contains(err.Error(), "must") {
		t.Fatalf("err = %v, want a refusal naming the skill", err)
	}
}

// TestApplyPreparedSourceAuthority_CarriesSkills proves the spawn lane takes
// the prepared source's skill delivery (and a copy, not an alias).
func TestApplyPreparedSourceAuthority_CarriesSkills(t *testing.T) {
	t.Parallel()
	source := agent.Spec{Skills: &agent.SkillDelivery{Plan: agent.SkillDeliveryPlan{Harness: agent.HarnessPi, Entries: []agent.SkillDeliveryEntry{{ID: "x", Delivery: agent.SkillDeliveredPrompt}}}}}
	target := applyPreparedSourceAuthority(agent.Spec{}, source, nil)
	if target.Skills == nil || target.Skills == source.Skills || !reflect.DeepEqual(target.Skills.Plan, source.Skills.Plan) {
		t.Fatalf("target.Skills = %+v, want a copy of the source's", target.Skills)
	}
}
