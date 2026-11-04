package prompt_test

import (
	"testing"

	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/stagewire"
	"github.com/RenseiAI/donmai/templates"
)

// TestBuilderBuild_StageRoleSelectsTemplate pins the 1.0 template choice:
// with a stage or a card present the user template follows the stage's run
// role (else its dispatch role, else the legacy workTypeRole), and the
// work-type name is never consulted. Each case renders byte-identically to
// the built-in that owns the expected template; the name on each case
// would pick a different template on the legacy path. Both rendering
// paths are covered.
func TestBuilderBuild_StageRoleSelectsTemplate(t *testing.T) {
	t.Parallel()
	reg, err := templates.New()
	if err != nil {
		t.Fatalf("templates.New: %v", err)
	}
	builders := []struct {
		name string
		new  func() *prompt.Builder
	}{
		{name: "legacy", new: prompt.NewBuilder},
		{name: "raymond", new: func() *prompt.Builder { return &prompt.Builder{Registry: reg} }},
	}
	qa, research, development := string(prompt.WorkTypeQA), string(prompt.WorkTypeResearch), string(prompt.WorkTypeDevelopment)
	stage := func(role, runRole stagewire.Role) stagewire.Fields {
		return stagewire.Fields{Stage: &stagewire.Stage{Key: "custom", Role: role, RunRole: runRole}}
	}
	cases := []struct {
		name      string
		workType  string
		role      string
		fields    stagewire.Fields
		reference string
	}{
		{"review renders qa", development, "", stage(stagewire.RoleReview, ""), qa},
		{"intake renders research", qa, "", stage(stagewire.RoleIntake, ""), research},
		{"hold renders research", development, "", stage(stagewire.RoleHold, ""), research},
		{"inflight renders development", qa, "", stage(stagewire.RoleInflight, ""), development},
		{"rework renders development", research, "", stage(stagewire.RoleRework, ""), development},
		{"done renders development", qa, "", stage(stagewire.RoleDone, ""), development},
		{"closed renders development", research, "", stage(stagewire.RoleClosed, ""), development},
		{"run role beats the dispatch role", research, "", stage(stagewire.RoleIntake, stagewire.RoleInflight), development},
		{"stage role beats the legacy role", development, "review", stage(stagewire.RoleIntake, ""), research},
		{"card without stage follows the legacy role", development, "review", stagewire.Fields{Card: &stagewire.Card{ID: "c"}}, qa},
		{"card without any role never reads the name", qa, "", stagewire.Fields{Card: &stagewire.Card{ID: "c"}}, development},
		{"stage without a known role falls back to the legacy role", development, "intake", stagewire.Fields{Stage: &stagewire.Stage{Key: "custom"}}, research},
	}
	for _, builder := range builders {
		for _, tc := range cases {
			t.Run(builder.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				reference := fixtureSession()
				reference.WorkType = tc.reference
				staged := fixtureSession()
				staged.WorkType = tc.workType
				staged.WorkTypeRole = tc.role
				staged.Fields = tc.fields

				_, wantUser, err := builder.new().Build(reference)
				if err != nil {
					t.Fatalf("Build reference: %v", err)
				}
				_, gotUser, err := builder.new().Build(staged)
				if err != nil {
					t.Fatalf("Build staged: %v", err)
				}
				if gotUser != wantUser {
					t.Errorf("work type %q with %+v did not render the %q template", tc.workType, tc.fields, tc.reference)
				}
			})
		}
	}
}
