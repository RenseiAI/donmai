package prompt_test

import (
	"testing"

	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/templates"
)

// TestBuilderBuild_CustomWorkTypeFollowsRole pins the role-aware user
// template selection for organisation-defined names: a custom name with
// role review renders byte-identically to qa, with role intake to
// research, and with role inflight (or no/invalid role) to development.
// Named built-ins keep their template regardless of any role passed, so
// a stray role can never move a built-in off its legacy prompt. Both
// rendering paths (legacy text/template and the raymond shim) are
// covered.
func TestBuilderBuild_CustomWorkTypeFollowsRole(t *testing.T) {
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

	// reference renders the named built-in verbatim; custom renders the
	// same session with an organisation-defined name plus a role. The
	// user prompts must be byte-identical.
	cases := []struct {
		name      string
		reference string // named built-in work type
		custom    string // organisation-defined name
		role      string
	}{
		{name: "review-renders-qa", reference: string(prompt.WorkTypeQA), custom: "peer-review", role: "review"},
		{name: "intake-renders-research", reference: string(prompt.WorkTypeResearch), custom: "triage-desk", role: "intake"},
		{name: "inflight-renders-development", reference: string(prompt.WorkTypeDevelopment), custom: "chief-of-staff", role: "inflight"},
		{name: "terminal-renders-development", reference: string(prompt.WorkTypeDevelopment), custom: "garden-tend", role: "terminal"},
		{name: "absent-role-renders-development", reference: string(prompt.WorkTypeDevelopment), custom: "chief-of-staff", role: ""},
		{name: "invalid-role-renders-development", reference: string(prompt.WorkTypeDevelopment), custom: "chief-of-staff", role: "not-a-role"},
	}
	for _, builder := range builders {
		for _, tc := range cases {
			t.Run(builder.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				reference := fixtureSession()
				reference.WorkType = tc.reference
				custom := fixtureSession()
				custom.WorkType = tc.custom
				custom.WorkTypeRole = tc.role

				_, wantUser, err := builder.new().Build(reference)
				if err != nil {
					t.Fatalf("Build reference: %v", err)
				}
				_, gotUser, err := builder.new().Build(custom)
				if err != nil {
					t.Fatalf("Build custom: %v", err)
				}
				if gotUser != wantUser {
					t.Errorf("custom work type %q with role %q differs from %q\n--- want ---\n%s\n--- got ---\n%s",
						tc.custom, tc.role, tc.reference, wantUser, gotUser)
				}
			})
		}
	}
}

// TestBuilderBuild_NamedWorkTypeIgnoresRole proves the legacy switch is
// authoritative for the names it knows: passing any role alongside a
// built-in name renders byte-identically to the role-less build. A stray
// or mismatched role must never move a built-in off its legacy prompt
// (the RED case is a named case moving).
func TestBuilderBuild_NamedWorkTypeIgnoresRole(t *testing.T) {
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
	named := []string{
		string(prompt.WorkTypeDevelopment),
		string(prompt.WorkTypeQA),
		string(prompt.WorkTypeAcceptance),
		string(prompt.WorkTypeResearch),
	}
	roles := []string{"", "inflight", "review", "intake", "terminal", "not-a-role"}
	for _, builder := range builders {
		for _, workType := range named {
			for _, role := range roles {
				t.Run(builder.name+"/"+workType+"/role="+role, func(t *testing.T) {
					t.Parallel()
					plain := fixtureSession()
					plain.WorkType = workType
					withRole := fixtureSession()
					withRole.WorkType = workType
					withRole.WorkTypeRole = role

					_, wantUser, err := builder.new().Build(plain)
					if err != nil {
						t.Fatalf("Build plain: %v", err)
					}
					_, gotUser, err := builder.new().Build(withRole)
					if err != nil {
						t.Fatalf("Build with role: %v", err)
					}
					if gotUser != wantUser {
						t.Errorf("named work type %q with role %q moved off its legacy prompt", workType, role)
					}
				})
			}
		}
	}
}
