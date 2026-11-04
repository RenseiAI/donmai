package runner

import (
	"fmt"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/prompt"
)

// sessionSkills is a session's skill delivery as both spawn lanes consume it:
// the plan and native content for Spec.Skills, the prompt residue whose
// bodies foldInlineSkills appends to the system prompt, and the session-level
// union of every skill's disallowed tools.
type sessionSkills struct {
	delivery   *agent.SkillDelivery
	residue    []prompt.SkillSpec
	disallowed []string
}

// planSessionSkills plans qw's skills for the exact harness selected for the
// session. The prepared-source lane (buildPreparedSourceSpec) and the spawn
// lane (runLoop) both call it on the same work item, provider and mode, so
// the plan, and therefore the prompt residue and the authority digest's
// "skills" field, are identical on both sides. A provider without a harness
// manifest plans prompt-only, exactly like a harness whose manifest declares
// no skill layout.
//
// Disallowed tools are the union over EVERY skill, whatever its delivery:
// a natively delivered or omitted skill narrows the tool surface exactly as
// a prompt-delivered one always has.
func planSessionSkills(qw QueuedWork, provider agent.Provider, mode agent.PromptSessionMode) (sessionSkills, error) {
	in := agent.SkillPlanInput{
		Mode:       mode,
		Levels:     agent.EffectiveExecutionSecurityLevels(qw.ExecutionSecurity),
		Skills:     qw.Skills,
		RepoSkills: qw.RepoSkills,
	}
	if harness, ok := provider.(agent.HarnessProvider); ok {
		manifest := harness.Manifest()
		in.Harness, in.Profile = manifest.Name, manifest.Skills
	}
	delivery, residue, err := agent.PlanSkillDelivery(in)
	if err != nil {
		return sessionSkills{}, fmt.Errorf("runner: plan skill delivery: %w", err)
	}
	return sessionSkills{delivery: delivery, residue: residue, disallowed: agent.SkillDisallowedTools(qw.Skills)}, nil
}

// skillDeliverySummary renders the plan as log attributes: counts per
// delivery and the repository-skill policy achieved. Identity and digests
// only, never content.
func skillDeliverySummary(delivery *agent.SkillDelivery) []any {
	if delivery == nil {
		return nil
	}
	counts := map[agent.SkillDeliveryMethod]int{}
	for _, entry := range delivery.Plan.Entries {
		counts[entry.Delivery]++
	}
	return []any{
		"skillsNative", counts[agent.SkillDeliveredNative],
		"skillsPrompt", counts[agent.SkillDeliveredPrompt],
		"skillsOmitted", counts[agent.SkillNotDelivered],
		"skillLayout", string(delivery.Plan.Layout),
		"repoSkillsRequested", string(delivery.Plan.RepoSkills.Requested),
		"repoSkillsAchieved", string(delivery.Plan.RepoSkills.Achieved),
	}
}
